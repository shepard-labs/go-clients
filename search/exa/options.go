// Optional parameters for SearchWithOptions and ScrapeWithOptions.
//
// Reachability: New returns a search.Client, so callers that need options
// assert back to the concrete type:
//
//	c := exa.New(key, logger).(*exa.Client)
//	page, err := c.SearchWithOptions(ctx, q, &exa.SearchOptions{Type: "neural"})
//	doc, err := c.ScrapeWithOptions(ctx, r, &exa.ScrapeOptions{Subpages: &n})
//
// Wire reconciliation (exa-js npm 2.19.0, exa-labs/exa-js src/index.ts @
// v2.19.0, inspected live via raw.githubusercontent.com — no clones, no
// deps, no fallback shapes used):
//   - Search keys type, category, includeDomains, excludeDomains,
//     startPublishedDate, endPublishedDate are top-level /search fields
//     (BaseSearchOptions / NonDeepSearchOptions; type is
//     "keyword"|"neural"|"auto"|"hybrid"|"fast"|"instant" there — this
//     package accepts only ""|"auto"|"neural"|"keyword" and rejects the
//     rest). livecrawl/livecrawlTimeout passed alongside them ride the
//     top-level ...requestOptions spread in buildSearchRequestBody.
//   - Scrape (getContents) posts {urls, ...options} to /contents, so text,
//     livecrawl, livecrawlTimeout, subpages, subpageTarget are all top-level
//     fields (ContentsOptions: livecrawl is
//     "never"|"fallback"|"always"|"auto"|"preferred"; subpages counts
//     subpages per result; subpageTarget fuzzy-matches them).
package exa

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/shepard-labs/go-clients/search"
)

// publishedDateLayout is the only accepted date shape for
// StartPublishedDate/EndPublishedDate (matches the Exa API's YYYY-MM-DD).
const publishedDateLayout = "2006-01-02"

// maxLivecrawlTimeout caps livecrawlTimeout in milliseconds.
const maxLivecrawlTimeout = 30000

// maxSubpages caps subpages per scraped result.
const maxSubpages = 5

// SearchOptions carries optional /search parameters. The zero value (or a nil
// *SearchOptions) sends exactly the default body: query, numResults, and
// contents.text. Every validation failure wraps search.ErrInvalidQuery.
type SearchOptions struct {
	// Type is the search type: "" (unset), "auto", "neural", or "keyword".
	Type string
	// Category focuses the search on a data category (e.g. "news",
	// "company"). Passed through verbatim.
	Category string
	// IncludeDomains restricts results to these bare hosts.
	IncludeDomains []string
	// ExcludeDomains excludes results from these bare hosts.
	ExcludeDomains []string
	// StartPublishedDate / EndPublishedDate bound result publish dates
	// (YYYY-MM-DD).
	StartPublishedDate string
	EndPublishedDate   string
	// Livecrawl controls live crawling: "" (unset), "never", "always",
	// "preferred", "fallback", or "auto".
	Livecrawl string
	// LivecrawlTimeout caps a live crawl in milliseconds (0..30000).
	// Setting it without Livecrawl is rejected.
	LivecrawlTimeout *int
}

// ScrapeOptions carries optional /contents parameters. A nil *ScrapeOptions
// sends exactly the default body: urls and text=true. Every validation
// failure wraps search.ErrInvalidRequest.
type ScrapeOptions struct {
	// Text controls text extraction. Nil means true (text is requested);
	// an explicit false opts out.
	Text *bool
	// Livecrawl controls live crawling: "" (unset), "never", "always",
	// "preferred", "fallback", or "auto".
	Livecrawl string
	// LivecrawlTimeout caps a live crawl in milliseconds (0..30000).
	// Setting it without Livecrawl is rejected.
	LivecrawlTimeout *int
	// Subpages requests subpages per result (1..5). Nil leaves it unset.
	Subpages *int
	// SubpageTarget fuzzy-matches which subpages to return. Setting it
	// without Subpages is rejected.
	SubpageTarget string
}

// validate reports whether the search options are well-formed. A nil receiver
// is valid (default body). Failures wrap search.ErrInvalidQuery.
func (o *SearchOptions) validate() error {
	if o == nil {
		return nil
	}
	switch o.Type {
	case "", "auto", "neural", "keyword":
	default:
		return fmt.Errorf("exa: invalid search options: type must be auto|neural|keyword, got %q: %w", o.Type, search.ErrInvalidQuery)
	}
	include, err := normalizeDomains(o.IncludeDomains)
	if err != nil {
		return fmt.Errorf("exa: invalid search options: %w: %w", err, search.ErrInvalidQuery)
	}
	exclude, err := normalizeDomains(o.ExcludeDomains)
	if err != nil {
		return fmt.Errorf("exa: invalid search options: %w: %w", err, search.ErrInvalidQuery)
	}
	inSet := make(map[string]struct{}, len(include))
	for _, d := range include {
		inSet[d] = struct{}{}
	}
	for _, d := range exclude {
		if _, ok := inSet[d]; ok {
			return fmt.Errorf("exa: invalid search options: domain %q in both includeDomains and excludeDomains: %w", d, search.ErrInvalidQuery)
		}
	}
	var start, end time.Time
	var hasStart, hasEnd bool
	if o.StartPublishedDate != "" {
		t, err := time.Parse(publishedDateLayout, o.StartPublishedDate)
		if err != nil {
			return fmt.Errorf("exa: invalid search options: startPublishedDate must be YYYY-MM-DD, got %q: %w", o.StartPublishedDate, search.ErrInvalidQuery)
		}
		start, hasStart = t, true
	}
	if o.EndPublishedDate != "" {
		t, err := time.Parse(publishedDateLayout, o.EndPublishedDate)
		if err != nil {
			return fmt.Errorf("exa: invalid search options: endPublishedDate must be YYYY-MM-DD, got %q: %w", o.EndPublishedDate, search.ErrInvalidQuery)
		}
		end, hasEnd = t, true
	}
	if hasStart && hasEnd && start.After(end) {
		return fmt.Errorf("exa: invalid search options: startPublishedDate %q is after endPublishedDate %q: %w", o.StartPublishedDate, o.EndPublishedDate, search.ErrInvalidQuery)
	}
	if reason := checkLivecrawl(o.Livecrawl, o.LivecrawlTimeout); reason != "" {
		return fmt.Errorf("exa: invalid search options: %s: %w", reason, search.ErrInvalidQuery)
	}
	return nil
}

// validate reports whether the scrape options are well-formed. A nil receiver
// is valid (default body). Failures wrap search.ErrInvalidRequest.
func (o *ScrapeOptions) validate() error {
	if o == nil {
		return nil
	}
	if reason := checkLivecrawl(o.Livecrawl, o.LivecrawlTimeout); reason != "" {
		return fmt.Errorf("exa: invalid scrape options: %s: %w", reason, search.ErrInvalidRequest)
	}
	if o.Subpages != nil && (*o.Subpages < 1 || *o.Subpages > maxSubpages) {
		return fmt.Errorf("exa: invalid scrape options: subpages must be 1..%d, got %d: %w", maxSubpages, *o.Subpages, search.ErrInvalidRequest)
	}
	if strings.TrimSpace(o.SubpageTarget) != "" && o.Subpages == nil {
		return fmt.Errorf("exa: invalid scrape options: subpageTarget requires subpages: %w", search.ErrInvalidRequest)
	}
	return nil
}

// checkLivecrawl validates a livecrawl/livecrawlTimeout pair, returning a
// non-empty reason when invalid. The caller wraps it with its own sentinel.
func checkLivecrawl(livecrawl string, timeout *int) string {
	switch livecrawl {
	case "", "never", "always", "preferred", "fallback", "auto":
	default:
		return fmt.Sprintf("livecrawl must be never|always|preferred|fallback|auto, got %q", livecrawl)
	}
	if timeout == nil {
		return ""
	}
	if livecrawl == "" {
		return "livecrawlTimeout requires livecrawl"
	}
	if *timeout < 0 || *timeout > maxLivecrawlTimeout {
		return fmt.Sprintf("livecrawlTimeout must be 0..%d, got %d", maxLivecrawlTimeout, *timeout)
	}
	return ""
}

// normalizeDomains lowercases and trims each entry, rejecting anything that
// is not a bare host: empty entries, schemes, ports, paths, userinfo,
// interior whitespace, or non-ASCII (unicode) input.
func normalizeDomains(domains []string) ([]string, error) {
	out := make([]string, 0, len(domains))
	for _, d := range domains {
		t := strings.ToLower(strings.TrimSpace(d))
		if t == "" {
			return nil, fmt.Errorf("empty domain")
		}
		if strings.Contains(t, "://") || strings.ContainsAny(t, "/:@") {
			return nil, fmt.Errorf("invalid domain %q: must be a bare host", d)
		}
		if strings.IndexFunc(t, func(r rune) bool {
			return unicode.IsSpace(r) || r < 33 || r == 127 || r > 127
		}) >= 0 {
			return nil, fmt.Errorf("invalid domain %q: must be a bare host", d)
		}
		out = append(out, t)
	}
	return out, nil
}
