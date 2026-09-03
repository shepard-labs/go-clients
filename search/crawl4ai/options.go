package crawl4ai

import (
	"encoding/json"
	"fmt"

	"github.com/shepard-labs/go-clients/search"
)

// Run-config limits for RunOptions. The pinned server is
// unclecode/crawl4ai:0.9.3; its POST /crawl/job body is free-form JSON
// (browser_config/crawler_config are Dict), so these client-side caps exist
// to reject runaway payloads before any I/O.
const (
	// maxOptionDepth is the deepest allowed nesting inside one config map.
	// The top-level object is depth 1; each nested map or slice adds one.
	maxOptionDepth = 8
	// maxOptionKeys is the maximum number of keys counted recursively
	// across all nested maps inside one config map.
	maxOptionKeys = 256
	// maxOptionBytes bounds the combined compact-JSON size of both configs.
	maxOptionBytes = 64 * 1024
)

// RunOptions carries per-request Crawl4AI run configuration. It maps 1:1
// onto the POST /crawl/job envelope of the pinned server
// (unclecode/crawl4ai:0.9.3):
//
//   - deploy/docker/schemas.py (v0.9.3): class CrawlRequest with
//     urls: List[str], browser_config: Optional[Dict],
//     crawler_config: Optional[Dict].
//   - deploy/docker/job.py (v0.9.3): POST /crawl/job (202 {"task_id"}) reads
//     payload.browser_config / payload.crawler_config; GET
//     /crawl/job/{task_id} reports the job status.
//   - deploy/docker/server.py (v0.9.3): handle_crawl_request(urls,
//     browser_config, crawler_config, ...) — the same two dicts.
//
// A nil *RunOptions means "no overrides": the submitted body keeps today's
// exact shapes (Scrape submits {"urls": [...]}, Crawl adds
// crawler_config {"max_pages": N}). A non-nil *RunOptions with nil maps
// behaves the same; only non-nil maps are forwarded.
//
// Reachability: New returns a search.Client whose dynamic type is
// *crawl4ai.Client, so callers type-assert to reach the option variants:
//
//	c, _ := crawl4ai.New("http://localhost:11235", logger,
//		crawl4ai.WithToken(os.Getenv("CRAWL4AI_API_TOKEN"))).(*crawl4ai.Client)
//	doc, err := c.ScrapeWithOptions(ctx,
//		&search.ScrapeRequest{URL: "https://example.com/page"},
//		&crawl4ai.RunOptions{
//			CrawlerConfig: map[string]any{"only_text": true},
//		})
//	page, err := c.CrawlWithOptions(ctx,
//		&search.CrawlRequest{StartURL: "https://example.com", MaxPages: 5},
//		&crawl4ai.RunOptions{
//			BrowserConfig: map[string]any{"headless": true},
//		})
//
// The maps are deep-copied at call entry (via the validated compact JSON),
// so the caller may reuse or mutate them — including sharing one *RunOptions
// across goroutines — without affecting in-flight requests, and the request's
// MaxPages always wins over a caller-supplied "max_pages" entry.
type RunOptions struct {
	// BrowserConfig is forwarded as POST /crawl/job "browser_config".
	BrowserConfig map[string]any
	// CrawlerConfig is forwarded as POST /crawl/job "crawler_config".
	CrawlerConfig map[string]any
}

// marshalValidated validates both config maps and returns their compact
// JSON encodings (nil per omitted map) for reuse by the caller: one marshal
// per map, no double cost. A nil receiver returns (nil, nil, nil).
func (o *RunOptions) marshalValidated() (browser, crawler []byte, err error) {
	if o == nil {
		return nil, nil, nil
	}
	if browser, err = marshalOptionMap("browser_config", o.BrowserConfig); err != nil {
		return nil, nil, err
	}
	if crawler, err = marshalOptionMap("crawler_config", o.CrawlerConfig); err != nil {
		return nil, nil, err
	}
	if len(browser)+len(crawler) >= maxOptionBytes {
		return nil, nil, fmt.Errorf("crawl4ai: run options: combined configs exceed %d bytes: %w", maxOptionBytes, search.ErrInvalidRequest)
	}
	return browser, crawler, nil
}

// marshalOptionMap validates one config map and returns its compact JSON
// encoding, or nil when m is nil (key omitted from the submit body).
func marshalOptionMap(name string, m map[string]any) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("crawl4ai: run options: %s: %v: %w", name, err, search.ErrInvalidRequest)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		return nil, fmt.Errorf("crawl4ai: run options: %s is not a JSON object: %w", name, search.ErrInvalidRequest)
	}
	keys := 0
	if err := checkOptionValue(name, back, 1, &keys); err != nil {
		return nil, err
	}
	return b, nil
}

// checkOptionValue walks one decoded config value enforcing the depth and
// key-count caps. depth is 1 at the top-level object.
func checkOptionValue(name string, v any, depth int, keys *int) error {
	if depth > maxOptionDepth {
		return fmt.Errorf("crawl4ai: run options: %s: nesting depth exceeds %d: %w", name, maxOptionDepth, search.ErrInvalidRequest)
	}
	switch t := v.(type) {
	case map[string]any:
		for _, val := range t {
			*keys++
			if *keys > maxOptionKeys {
				return fmt.Errorf("crawl4ai: run options: %s: more than %d keys: %w", name, maxOptionKeys, search.ErrInvalidRequest)
			}
			if err := checkOptionValue(name, val, depth+1, keys); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range t {
			if err := checkOptionValue(name, e, depth+1, keys); err != nil {
				return err
			}
		}
	}
	return nil
}

// copyValidated decodes previously validated compact JSON back into a fresh
// map detached from the caller's map. It returns nil for nil input, so
// omitted maps stay omitted (omitempty) in the submit body.
func copyValidated(b []byte) (map[string]any, error) {
	if b == nil {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("crawl4ai: run options: decode validated config: %w", err)
	}
	return m, nil
}
