package crawl4ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/shepard-labs/go-clients/search"
)

// Local test doubles. crawl4ai_test.go already defines rtFunc, jsonResp, and
// newTestClient; these opt-prefixed variants avoid redeclaration while keeping
// this file self-contained.
type optRoundTripper func(*http.Request) (*http.Response, error)

func (f optRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func optJSONResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// newOptClient builds a *Client with a counting transport, Bearer token "t",
// and an instant sleeper so polls never actually wait.
func newOptClient(rt optRoundTripper) (*Client, *atomic.Int64) {
	calls := &atomic.Int64{}
	counting := optRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return rt(r)
	})
	c := New("http://localhost:11235", zap.NewNop(), WithToken("t"), WithHTTPClient(&http.Client{Transport: counting})).(*Client)
	c.sleeper = func(context.Context, time.Duration) bool { return true }
	return c, calls
}

func optRequireBearer(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer t" {
		t.Errorf("expected Authorization Bearer t, got %q", got)
	}
}

// optCaptureServe records every POST /crawl/job body and completes the job on
// poll with result (a JSON-encoded result payload). mu guards bodies.
func optCaptureServe(t *testing.T, mu *sync.Mutex, bodies *[][]byte, result string) optRoundTripper {
	t.Helper()
	return func(r *http.Request) (*http.Response, error) {
		optRequireBearer(t, r)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/crawl/job":
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			*bodies = append(*bodies, body)
			mu.Unlock()
			return optJSONResp(202, `{"task_id":"crawl_opt"}`), nil
		case r.Method == http.MethodGet && r.URL.Path == "/crawl/job/crawl_opt":
			return optJSONResp(200, `{"task_id":"crawl_opt","status":"completed","result":`+result+`}`), nil
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			return optJSONResp(500, `{}`), nil
		}
	}
}

func optDecodeBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("submit body is not a JSON object: %v (%s)", err, raw)
	}
	return m
}

// optDeepFreeze snapshots an option map through a JSON round-trip so the
// caller can prove the client never mutated it.
func optDeepFreeze(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("freeze marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("freeze unmarshal: %v", err)
	}
	return out
}

func TestNewReturnsOptionsCapableClient(t *testing.T) {
	var sc search.Client = New("http://localhost:11235", zap.NewNop(), WithToken("t"))
	cli, ok := sc.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client to reach ScrapeWithOptions/CrawlWithOptions", sc)
	}
	// Prove the asserted client is functional: drive an option variant
	// through it and check the configs arrive on the submit body.
	var mu sync.Mutex
	var bodies [][]byte
	cli.httpClient = &http.Client{Transport: optCaptureServe(t, &mu, &bodies,
		`{"url":"https://example.com/page","markdown":"# Hi"}`)}
	cli.sleeper = func(context.Context, time.Duration) bool { return true }
	doc, err := cli.ScrapeWithOptions(context.Background(),
		&search.ScrapeRequest{URL: "https://example.com/page"},
		&RunOptions{CrawlerConfig: map[string]any{"only_text": true}})
	if err != nil {
		t.Fatalf("ScrapeWithOptions through asserted client: %v", err)
	}
	if doc.Markdown != "# Hi" {
		t.Fatalf("expected markdown %q, got %q", "# Hi", doc.Markdown)
	}
	if len(bodies) != 1 {
		t.Fatalf("expected 1 submit body, got %d", len(bodies))
	}
	cc, ok := optDecodeBody(t, bodies[0])["crawler_config"].(map[string]any)
	if !ok || cc["only_text"] != true {
		t.Fatalf("expected crawler_config.only_text=true on submit, got %s", bodies[0])
	}
}

func TestNilOptionsSubmitParity(t *testing.T) {
	ctx := context.Background()

	t.Run("scrape", func(t *testing.T) {
		var mu sync.Mutex
		var bodies [][]byte
		c, _ := newOptClient(optCaptureServe(t, &mu, &bodies,
			`{"url":"https://example.com/page","markdown":"# Hi"}`))
		req := &search.ScrapeRequest{URL: "https://example.com/page"}
		if _, err := c.Scrape(ctx, req); err != nil {
			t.Fatalf("Scrape: %v", err)
		}
		if _, err := c.ScrapeWithOptions(ctx, req, nil); err != nil {
			t.Fatalf("ScrapeWithOptions(nil): %v", err)
		}
		if len(bodies) != 2 {
			t.Fatalf("expected 2 submit bodies, got %d", len(bodies))
		}
		a, b := optDecodeBody(t, bodies[0]), optDecodeBody(t, bodies[1])
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("nil-parity bodies differ:\n%s\n%s", bodies[0], bodies[1])
		}
		// Nil opts keeps today's Scrape shape: bare {"urls":[...]}.
		if _, ok := a["browser_config"]; ok {
			t.Fatalf("expected no browser_config in nil-options body, got %s", bodies[0])
		}
		if _, ok := a["crawler_config"]; ok {
			t.Fatalf("expected no crawler_config in nil-options body, got %s", bodies[0])
		}
	})

	t.Run("crawl", func(t *testing.T) {
		var mu sync.Mutex
		var bodies [][]byte
		c, _ := newOptClient(optCaptureServe(t, &mu, &bodies,
			`{"success":true,"results":[{"success":true,"url":"https://example.com","markdown":"# C"}]}`))
		req := &search.CrawlRequest{StartURL: "https://example.com", MaxPages: 5}
		if _, err := c.Crawl(ctx, req); err != nil {
			t.Fatalf("Crawl: %v", err)
		}
		if _, err := c.CrawlWithOptions(ctx, req, nil); err != nil {
			t.Fatalf("CrawlWithOptions(nil): %v", err)
		}
		if len(bodies) != 2 {
			t.Fatalf("expected 2 submit bodies, got %d", len(bodies))
		}
		a, b := optDecodeBody(t, bodies[0]), optDecodeBody(t, bodies[1])
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("nil-parity bodies differ:\n%s\n%s", bodies[0], bodies[1])
		}
		// Nil opts keeps today's Crawl shape: crawler_config carries only the
		// MaxPages hint.
		cc, ok := a["crawler_config"].(map[string]any)
		if !ok || cc["max_pages"] != float64(5) || len(cc) != 1 {
			t.Fatalf("expected crawler_config {\"max_pages\":5} in nil-options body, got %s", bodies[0])
		}
	})
}

func TestRunOptionsPassthrough(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	c, calls := newOptClient(optCaptureServe(t, &mu, &bodies,
		`{"url":"https://example.com/page","markdown":"# Hi"}`))
	opts := &RunOptions{
		BrowserConfig: map[string]any{"headless": true, "userAgent": "opts-test"},
		CrawlerConfig: map[string]any{"only_text": true},
	}
	doc, err := c.ScrapeWithOptions(context.Background(),
		&search.ScrapeRequest{URL: "https://example.com/page"}, opts)
	if err != nil {
		t.Fatalf("ScrapeWithOptions: %v", err)
	}
	if doc.Markdown != "# Hi" {
		t.Fatalf("expected markdown %q, got %q", "# Hi", doc.Markdown)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 HTTP calls (1 submit + 1 poll), got %d", got)
	}
	body := optDecodeBody(t, bodies[0])
	bc, ok := body["browser_config"].(map[string]any)
	if !ok || bc["headless"] != true || bc["userAgent"] != "opts-test" {
		t.Fatalf("browser_config keys missing on submit, got %s", bodies[0])
	}
	cc, ok := body["crawler_config"].(map[string]any)
	if !ok || cc["only_text"] != true {
		t.Fatalf("crawler_config keys missing on submit, got %s", bodies[0])
	}
}

func TestCrawlMaxPagesWinsOverOption(t *testing.T) {
	ctx := context.Background()
	for _, pre := range []any{"ten", 3.5, 99, nil} {
		t.Run(fmt.Sprintf("pre=%#v", pre), func(t *testing.T) {
			var mu sync.Mutex
			var bodies [][]byte
			c, _ := newOptClient(optCaptureServe(t, &mu, &bodies,
				`{"success":true,"results":[{"success":true,"url":"https://example.com","markdown":"# C"}]}`))
			opts := &RunOptions{CrawlerConfig: map[string]any{"max_pages": pre, "only_text": true}}
			if _, err := c.CrawlWithOptions(ctx,
				&search.CrawlRequest{StartURL: "https://example.com", MaxPages: 5}, opts); err != nil {
				t.Fatalf("CrawlWithOptions: %v", err)
			}
			cc, ok := optDecodeBody(t, bodies[0])["crawler_config"].(map[string]any)
			if !ok {
				t.Fatalf("expected crawler_config on submit, got %s", bodies[0])
			}
			// Request wins even over wrong-type ("ten") and float pre-values.
			if cc["max_pages"] != float64(5) {
				t.Fatalf("pre-existing max_pages %#v not overwritten by MaxPages=5, got %s", pre, bodies[0])
			}
			if cc["only_text"] != true {
				t.Fatalf("sibling crawler_config key lost, got %s", bodies[0])
			}
		})
	}
}

func TestCallerOptionMapsNotMutated(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	// Single transport serves both the scrape and the crawl below.
	c, _ := newOptClient(optCaptureServe(t, &mu, &bodies,
		`{"success":true,"results":[{"success":true,"url":"https://example.com/page","markdown":"# Hi"}]}`))
	// JSON-stable fixture: values that survive a marshal round-trip with
	// identical dynamic types, so DeepEqual is exact.
	opts := &RunOptions{
		BrowserConfig: map[string]any{
			"headless": true,
			"locale":   "en-US",
			"viewport": map[string]any{"width": float64(1280), "height": float64(800)},
		},
		CrawlerConfig: map[string]any{"only_text": true, "max_pages": "ten", "tags": []any{"a", "b"}},
	}
	frozenBrowser := optDeepFreeze(t, opts.BrowserConfig)
	frozenCrawler := optDeepFreeze(t, opts.CrawlerConfig)

	ctx := context.Background()
	if _, err := c.ScrapeWithOptions(ctx,
		&search.ScrapeRequest{URL: "https://example.com/page"}, opts); err != nil {
		t.Fatalf("ScrapeWithOptions: %v", err)
	}
	if _, err := c.CrawlWithOptions(ctx,
		&search.CrawlRequest{StartURL: "https://example.com", MaxPages: 7}, opts); err != nil {
		t.Fatalf("CrawlWithOptions: %v", err)
	}
	if !reflect.DeepEqual(opts.BrowserConfig, frozenBrowser) {
		t.Fatalf("BrowserConfig mutated: %#v", opts.BrowserConfig)
	}
	if !reflect.DeepEqual(opts.CrawlerConfig, frozenCrawler) {
		t.Fatalf("CrawlerConfig mutated: %#v", opts.CrawlerConfig)
	}
	// The caller's wrong-type max_pages survives: the request won on the
	// submitted copy only.
	if opts.CrawlerConfig["max_pages"] != "ten" {
		t.Fatalf("caller max_pages changed to %#v", opts.CrawlerConfig["max_pages"])
	}
}

func TestSharedOptionsConcurrentScrape(t *testing.T) {
	shared := &RunOptions{
		BrowserConfig: map[string]any{"headless": true},
		CrawlerConfig: map[string]any{"only_text": true},
	}
	var mu sync.Mutex
	var bodies [][]byte
	c, _ := newOptClient(optCaptureServe(t, &mu, &bodies,
		`{"url":"https://example.com","markdown":"# Shared"}`))

	const n = 8
	var wg sync.WaitGroup
	var ok atomic.Int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			doc, err := c.ScrapeWithOptions(context.Background(),
				&search.ScrapeRequest{URL: "https://example.com"}, shared)
			if err != nil {
				t.Errorf("concurrent ScrapeWithOptions failed: %v", err)
				return
			}
			if doc.Markdown != "# Shared" {
				t.Errorf("unexpected markdown %q", doc.Markdown)
				return
			}
			ok.Add(1)
		}()
	}
	wg.Wait()
	if got := ok.Load(); got != n {
		t.Fatalf("expected %d successful scrapes, got %d", n, got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != n {
		t.Fatalf("expected %d submit bodies, got %d", n, len(bodies))
	}
	for i, raw := range bodies {
		bc, ok := optDecodeBody(t, raw)["browser_config"].(map[string]any)
		if !ok || bc["headless"] != true {
			t.Fatalf("body %d missing shared browser_config, got %s", i, raw)
		}
	}
}

func TestRunOptionsValidation(t *testing.T) {
	deepNest := func(levels int) map[string]any {
		m := map[string]any{"leaf": "x"}
		for i := 1; i < levels; i++ {
			m = map[string]any{"nest": m}
		}
		return m
	}
	manyKeys := func(n int) map[string]any {
		m := make(map[string]any, n)
		for i := 0; i < n; i++ {
			m["k"+strconv.Itoa(i)] = i
		}
		return m
	}
	big := strings.Repeat("x", 33*1024) // ~33KB each; combined > 64KB cap
	cases := []struct {
		name    string
		opts    *RunOptions
		wantErr bool
	}{
		{"unmarshalable chan value", &RunOptions{BrowserConfig: map[string]any{"ch": make(chan int)}}, true},
		{"depth 9 nesting", &RunOptions{CrawlerConfig: deepNest(9)}, true},
		{"depth 7 chain ok", &RunOptions{CrawlerConfig: deepNest(7)}, false},
		{"257 keys", &RunOptions{BrowserConfig: manyKeys(257)}, true},
		{"256 keys ok", &RunOptions{BrowserConfig: manyKeys(256)}, false},
		{"combined over 64KB", &RunOptions{
			BrowserConfig: map[string]any{"big": big},
			CrawlerConfig: map[string]any{"big": big},
		}, true},
	}
	ctx := context.Background()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var bodies [][]byte
			c, calls := newOptClient(optCaptureServe(t, &mu, &bodies,
				`{"success":true,"results":[{"success":true,"url":"https://example.com","markdown":"# C"}]}`))
			scrapeReq := &search.ScrapeRequest{URL: "https://example.com/page"}
			crawlReq := &search.CrawlRequest{StartURL: "https://example.com", MaxPages: 5}
			scrapeErr := func() error {
				_, err := c.ScrapeWithOptions(ctx, scrapeReq, tc.opts)
				return err
			}()
			crawlErr := func() error {
				_, err := c.CrawlWithOptions(ctx, crawlReq, tc.opts)
				return err
			}()
			if tc.wantErr {
				for i, err := range []error{scrapeErr, crawlErr} {
					if !errors.Is(err, search.ErrInvalidRequest) {
						t.Fatalf("call %d: expected ErrInvalidRequest, got %v", i, err)
					}
					var apiErr *APIError
					if errors.As(err, &apiErr) {
						t.Fatalf("call %d: validation failure must not be an *APIError, got %v", i, err)
					}
				}
				if got := calls.Load(); got != 0 {
					t.Fatalf("expected 0 HTTP calls for invalid options, got %d", got)
				}
				return
			}
			if scrapeErr != nil {
				t.Fatalf("valid boundary options rejected by scrape: %v", scrapeErr)
			}
			if crawlErr != nil {
				t.Fatalf("valid boundary options rejected by crawl: %v", crawlErr)
			}
			if got := calls.Load(); got != 4 {
				t.Fatalf("expected 4 HTTP calls (2 submits + 2 polls), got %d", got)
			}
		})
	}
}

func TestValidatedCopyRejectsNonObject(t *testing.T) {
	// marshalOptionMap always emits a JSON object for any map[string]any, so
	// its "not a JSON object" branch is unreachable from public input. The
	// observable decode failure is the deep-copy step rejecting non-object
	// JSON instead of forwarding it.
	if _, err := copyValidated([]byte(`[1,2,3]`)); err == nil {
		t.Fatal("expected error decoding top-level array into option map, got nil")
	}
	if m, err := copyValidated(nil); err != nil || m != nil {
		t.Fatalf("expected (nil, nil) for omitted config, got %#v, %v", m, err)
	}
}
