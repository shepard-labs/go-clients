package firecrawl

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	search "github.com/shepard-labs/go-clients/search"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func resp(status int, body string, h http.Header) *http.Response {
	if h == nil {
		h = make(http.Header)
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: h}
}

// stubClient builds an in-package *Client over rt with an instant sleeper.
// It returns the client and a counter of HTTP calls made through rt.
func stubClient(rt http.RoundTripper) (*Client, *atomic.Int64) {
	calls := &atomic.Int64{}
	wrapped := rtFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return rt.RoundTrip(r)
	})
	c := New("k", zap.NewNop(), WithHTTPClient(&http.Client{Transport: wrapped})).(*Client)
	c.sleeper = func(context.Context, time.Duration) bool { return true }
	return c, calls
}

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		t.Fatalf("failed to decode request body: %v", err)
	}
	return m
}

func asAPIError(t *testing.T, err error) *APIError {
	t.Helper()
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	return apiErr
}

func TestSearchSendsBearerAndParsesGrouped(t *testing.T) {
	c, calls := stubClient(rtFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost {
			return nil, errors.New("unexpected method " + r.Method)
		}
		if r.URL.Path != "/v2/search" {
			return nil, errors.New("unexpected path " + r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			return nil, errors.New("unexpected auth header " + got)
		}
		body := decodeBody(t, r)
		if body["query"] != "golang" || body["limit"] != float64(5) {
			return nil, errors.New("unexpected search payload")
		}
		return resp(http.StatusOK, `{"success":true,"data":{`+
			`"web":[{"url":"https://a.example","title":"A","description":"desc-a","score":0.9,"publishedAt":"2024-01-01"}],`+
			`"news":[{"url":"https://b.example","title":"B","snippet":"snip-b","date":"2024-02-02"}],`+
			`"images":[]}}`, nil), nil
	}))

	page, err := c.Search(context.Background(), &search.SearchQuery{Query: "golang", NumResults: 5})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 HTTP call, got %d", calls.Load())
	}
	if len(page.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(page.Results))
	}
	if page.Results[0].URL != "https://a.example" || page.Results[0].Title != "A" ||
		page.Results[0].Snippet != "desc-a" || page.Results[0].PublishedAt != "2024-01-01" ||
		page.Results[0].Score != 0.9 {
		t.Fatalf("unexpected web result: %#v", page.Results[0])
	}
	if page.Results[1].URL != "https://b.example" || page.Results[1].Snippet != "snip-b" ||
		page.Results[1].PublishedAt != "2024-02-02" {
		t.Fatalf("unexpected news result: %#v", page.Results[1])
	}
}

func TestSearchValidationMakesNoCalls(t *testing.T) {
	tests := []struct {
		name  string
		query *search.SearchQuery
	}{
		{"nil query", nil},
		{"empty query", &search.SearchQuery{Query: "", NumResults: 5}},
		{"whitespace query", &search.SearchQuery{Query: "   ", NumResults: 5}},
		{"zero results", &search.SearchQuery{Query: "golang", NumResults: 0}},
		{"too many results", &search.SearchQuery{Query: "golang", NumResults: 101}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, calls := stubClient(rtFunc(func(*http.Request) (*http.Response, error) {
				return resp(http.StatusOK, `{}`, nil), nil
			}))
			if _, err := c.Search(context.Background(), tt.query); !errors.Is(err, search.ErrInvalidQuery) {
				t.Fatalf("expected ErrInvalidQuery, got %v", err)
			}
			if calls.Load() != 0 {
				t.Fatalf("expected 0 HTTP calls, got %d", calls.Load())
			}
		})
	}
}

func TestSearchEmptyKeyMakesNoCalls(t *testing.T) {
	c, calls := stubClient(rtFunc(func(*http.Request) (*http.Response, error) {
		return resp(http.StatusOK, `{}`, nil), nil
	}))
	c.apiKey = ""
	if _, err := c.Search(context.Background(), &search.SearchQuery{Query: "golang", NumResults: 5}); err == nil {
		t.Fatalf("expected error for empty key, got nil")
	}
	if calls.Load() != 0 {
		t.Fatalf("expected 0 HTTP calls, got %d", calls.Load())
	}
}

func TestSearchHTTPFailures(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		body         string
		wantCalls    int64
		unauthorized bool
	}{
		{"unauthorized", http.StatusUnauthorized, `{"error":"invalid key"}`, 1, true},
		{"billing 402 not retried", http.StatusPaymentRequired, `{"error":"quota exceeded"}`, 1, false},
		{"success false envelope", http.StatusOK, `{"success":false,"error":"bad request"}`, 1, false},
		{"malformed json", http.StatusOK, `{"success":true,"data":{`, 1, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, calls := stubClient(rtFunc(func(*http.Request) (*http.Response, error) {
				return resp(tt.status, tt.body, nil), nil
			}))
			_, err := c.Search(context.Background(), &search.SearchQuery{Query: "golang", NumResults: 5})
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if calls.Load() != tt.wantCalls {
				t.Fatalf("expected %d HTTP calls, got %d", tt.wantCalls, calls.Load())
			}
			if tt.name == "malformed json" {
				var apiErr *APIError
				if errors.As(err, &apiErr) {
					t.Fatalf("expected plain decode error, got %v", apiErr)
				}
				return
			}
			apiErr := asAPIError(t, err)
			if apiErr.IsUnauthorized() != tt.unauthorized {
				t.Fatalf("IsUnauthorized() = %v, want %v (%v)", apiErr.IsUnauthorized(), tt.unauthorized, apiErr)
			}
		})
	}
}

func TestSearchFlatDataShape(t *testing.T) {
	c, _ := stubClient(rtFunc(func(*http.Request) (*http.Response, error) {
		return resp(http.StatusOK, `{"success":true,"data":[`+
			`{"url":"https://flat.example","title":"Flat","description":"flat-desc"}]}`, nil), nil
	}))

	page, err := c.Search(context.Background(), &search.SearchQuery{Query: "golang", NumResults: 5})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(page.Results) != 1 || page.Results[0].URL != "https://flat.example" ||
		page.Results[0].Snippet != "flat-desc" {
		t.Fatalf("unexpected flat results: %#v", page.Results)
	}
}

func TestSearchRateLimitedThenSuccess(t *testing.T) {
	seen := &atomic.Int64{}
	c, calls := stubClient(rtFunc(func(r *http.Request) (*http.Response, error) {
		if seen.Add(1) == 1 {
			return resp(http.StatusTooManyRequests, `{"error":"slow down"}`,
				http.Header{"Retry-After": []string{"0"}}), nil
		}
		return resp(http.StatusOK, `{"success":true,"data":{"web":[`+
			`{"url":"https://a.example","title":"A","description":"d"}]}}`, nil), nil
	}))
	sleeps := &atomic.Int64{}
	c.sleeper = func(context.Context, time.Duration) bool { sleeps.Add(1); return true }

	page, err := c.Search(context.Background(), &search.SearchQuery{Query: "golang", NumResults: 5})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 HTTP calls, got %d", calls.Load())
	}
	if sleeps.Load() != 1 {
		t.Fatalf("expected 1 retry sleep, got %d", sleeps.Load())
	}
	if len(page.Results) != 1 || page.Results[0].URL != "https://a.example" {
		t.Fatalf("unexpected results after retry: %#v", page.Results)
	}
}

func TestSearchResponseTooLarge(t *testing.T) {
	c, _ := stubClient(rtFunc(func(*http.Request) (*http.Response, error) {
		return resp(http.StatusOK, strings.Repeat("x", int(search.MaxResponseBytes)+10), nil), nil
	}))
	if _, err := c.Search(context.Background(), &search.SearchQuery{Query: "golang", NumResults: 5}); !errors.Is(err, search.ErrResponseTooLarge) {
		t.Fatalf("expected ErrResponseTooLarge, got %v", err)
	}
}

func TestScrapeMapsDocument(t *testing.T) {
	c, calls := stubClient(rtFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/scrape" {
			return nil, errors.New("unexpected " + r.Method + " " + r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			return nil, errors.New("unexpected auth header " + got)
		}
		body := decodeBody(t, r)
		if body["url"] != "https://example.com" {
			return nil, errors.New("unexpected scrape payload")
		}
		return resp(http.StatusOK, `{"success":true,"data":{`+
			`"url":"https://example.com","markdown":"# hi","html":"<h1>hi</h1>",`+
			`"metadata":{"title":"T","keywords":["a","b"],"count":3},`+
			`"links":["https://example.com/x"]}}`, nil), nil
	}))

	doc, err := c.Scrape(context.Background(), &search.ScrapeRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatalf("Scrape failed: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 HTTP call, got %d", calls.Load())
	}
	if doc.URL != "https://example.com" || doc.Markdown != "# hi" || doc.HTML != "<h1>hi</h1>" {
		t.Fatalf("unexpected document: %#v", doc)
	}
	if doc.Metadata["title"] != "T" || doc.Metadata["keywords"] != "a, b" || doc.Metadata["count"] != "3" {
		t.Fatalf("unexpected metadata: %#v", doc.Metadata)
	}
	if len(doc.Links) != 1 || doc.Links[0] != "https://example.com/x" {
		t.Fatalf("unexpected links: %#v", doc.Links)
	}
}

func TestScrapeRawHTMLFallback(t *testing.T) {
	c, _ := stubClient(rtFunc(func(*http.Request) (*http.Response, error) {
		return resp(http.StatusOK, `{"success":true,"data":{`+
			`"url":"https://example.com","markdown":"m","rawHtml":"<p>raw</p>"}}`, nil), nil
	}))

	doc, err := c.Scrape(context.Background(), &search.ScrapeRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatalf("Scrape failed: %v", err)
	}
	if doc.HTML != "<p>raw</p>" {
		t.Fatalf("expected rawHtml fallback, got %q", doc.HTML)
	}
}

func TestScrapeBadURLMakesNoCalls(t *testing.T) {
	c, calls := stubClient(rtFunc(func(*http.Request) (*http.Response, error) {
		return resp(http.StatusOK, `{}`, nil), nil
	}))
	if _, err := c.Scrape(context.Background(), &search.ScrapeRequest{URL: "://bad"}); !errors.Is(err, search.ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, got %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("expected 0 HTTP calls, got %d", calls.Load())
	}
}

func TestCrawlPollsToCompletion(t *testing.T) {
	statusCalls := &atomic.Int64{}
	c, _ := stubClient(rtFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/crawl":
			body := decodeBody(t, r)
			if body["url"] != "https://example.com" || body["limit"] != float64(5) {
				return nil, errors.New("unexpected crawl payload")
			}
			return resp(http.StatusOK, `{"success":true,"id":"job1"}`, nil), nil
		case r.Method == http.MethodGet && r.URL.Path == "/v2/crawl/job1":
			if statusCalls.Add(1) == 1 {
				return resp(http.StatusOK, `{"success":true,"status":"scraping",`+
					`"data":[{"url":"https://example.com","markdown":"one"}]}`, nil), nil
			}
			return resp(http.StatusOK, `{"success":true,"status":"completed",`+
				`"data":[{"url":"https://example.com","markdown":"one"},`+
				`{"url":"https://example.com/two","markdown":"two"}]}`, nil), nil
		default:
			return nil, errors.New("unexpected " + r.Method + " " + r.URL.Path)
		}
	}))

	page, err := c.Crawl(context.Background(), &search.CrawlRequest{StartURL: "https://example.com", MaxPages: 5})
	if err != nil {
		t.Fatalf("Crawl failed: %v", err)
	}
	if statusCalls.Load() != 2 {
		t.Fatalf("expected 2 status polls, got %d", statusCalls.Load())
	}
	if len(page.Documents) != 2 || page.Documents[0].Markdown != "one" ||
		page.Documents[1].URL != "https://example.com/two" {
		t.Fatalf("unexpected crawled documents: %#v", page.Documents)
	}
}

func TestCrawlFailedTerminal(t *testing.T) {
	c, _ := stubClient(rtFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			return resp(http.StatusOK, `{"success":true,"id":"job9"}`, nil), nil
		}
		return resp(http.StatusOK, `{"success":true,"status":"failed","error":"boom"}`, nil), nil
	}))

	_, err := c.Crawl(context.Background(), &search.CrawlRequest{StartURL: "https://example.com", MaxPages: 5})
	apiErr := asAPIError(t, err)
	if apiErr.StatusCode != http.StatusOK || apiErr.Code != "crawl_failed" {
		t.Fatalf("unexpected terminal error: %#v", apiErr)
	}
}

func TestCrawlStatusTerminalStates(t *testing.T) {
	tests := []struct {
		name   string
		status string
		code   string
	}{
		{"failed", "failed", "crawl_failed"},
		{"cancelled", "cancelled", "crawl_cancelled"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, calls := stubClient(rtFunc(func(*http.Request) (*http.Response, error) {
				return resp(http.StatusOK, `{"success":true,"status":"`+tt.status+`"}`, nil), nil
			}))
			page, done, err := c.CrawlStatus(context.Background(), "job1")
			if !done {
				t.Fatalf("expected terminal done=true for %q", tt.status)
			}
			if page != nil {
				t.Fatalf("expected nil page for %q, got %#v", tt.status, page)
			}
			apiErr := asAPIError(t, err)
			if apiErr.Code != tt.code || apiErr.StatusCode != http.StatusOK {
				t.Fatalf("unexpected terminal error: %#v", apiErr)
			}
			if calls.Load() != 1 {
				t.Fatalf("expected 1 HTTP call, got %d", calls.Load())
			}
		})
	}
}

func TestCrawlPollTimeout(t *testing.T) {
	statusCalls := &atomic.Int64{}
	c, _ := stubClient(rtFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			return resp(http.StatusOK, `{"success":true,"id":"job-timeout"}`, nil), nil
		}
		statusCalls.Add(1)
		return resp(http.StatusOK, `{"success":true,"status":"scraping","data":[]}`, nil), nil
	}))
	c.pollInterval = time.Nanosecond
	c.maxPollWait = 5 * time.Millisecond

	_, err := c.Crawl(context.Background(), &search.CrawlRequest{StartURL: "https://example.com", MaxPages: 5})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
	if statusCalls.Load() == 0 {
		t.Fatalf("expected at least one status poll before timing out")
	}
}

func TestCrawlContextCancelledMidPoll(t *testing.T) {
	c, _ := stubClient(rtFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			return resp(http.StatusOK, `{"success":true,"id":"job1"}`, nil), nil
		}
		return resp(http.StatusOK, `{"success":true,"status":"scraping","data":[]}`, nil), nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.sleeper = func(ctx context.Context, d time.Duration) bool { cancel(); return sleepCtx(ctx, d) }

	_, err := c.Crawl(ctx, &search.CrawlRequest{StartURL: "https://example.com", MaxPages: 5})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestStartCrawlMissingID(t *testing.T) {
	c, calls := stubClient(rtFunc(func(*http.Request) (*http.Response, error) {
		return resp(http.StatusOK, `{"success":true}`, nil), nil
	}))
	if _, err := c.StartCrawl(context.Background(), &search.CrawlRequest{StartURL: "https://example.com", MaxPages: 5}); err == nil {
		t.Fatalf("expected error for missing job id, got nil")
	} else if !strings.Contains(err.Error(), "missing job id") {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 HTTP call, got %d", calls.Load())
	}
}

func TestCrawlStatusEmptyJobID(t *testing.T) {
	for _, id := range []string{"", "   "} {
		c, calls := stubClient(rtFunc(func(*http.Request) (*http.Response, error) {
			return resp(http.StatusOK, `{}`, nil), nil
		}))
		if _, _, err := c.CrawlStatus(context.Background(), id); !errors.Is(err, search.ErrInvalidRequest) {
			t.Fatalf("jobID %q: expected ErrInvalidRequest, got %v", id, err)
		}
		if calls.Load() != 0 {
			t.Fatalf("jobID %q: expected 0 HTTP calls, got %d", id, calls.Load())
		}
	}
}

func TestMapParsesStringAndObjectLinks(t *testing.T) {
	c, _ := stubClient(rtFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/map" {
			return nil, errors.New("unexpected " + r.Method + " " + r.URL.Path)
		}
		body := decodeBody(t, r)
		if body["url"] != "https://example.com" || body["limit"] != float64(10) {
			return nil, errors.New("unexpected map payload")
		}
		return resp(http.StatusOK, `{"success":true,"links":[`+
			`"https://a.example",`+
			`{"url":"https://b.example","title":"B"},`+
			`"",`+
			`{"url":""},`+
			`42]}`, nil), nil
	}))

	links, err := c.Map(context.Background(), "https://example.com", 10)
	if err != nil {
		t.Fatalf("Map failed: %v", err)
	}
	if len(links) != 2 || links[0] != "https://a.example" || links[1] != "https://b.example" {
		t.Fatalf("unexpected links: %#v", links)
	}
}

func TestMapInvalidURLMakesNoCalls(t *testing.T) {
	for _, u := range []string{"", "not-a-url", "ftp://example.com/file"} {
		c, calls := stubClient(rtFunc(func(*http.Request) (*http.Response, error) {
			return resp(http.StatusOK, `{}`, nil), nil
		}))
		if _, err := c.Map(context.Background(), u, 10); !errors.Is(err, search.ErrInvalidRequest) {
			t.Fatalf("url %q: expected ErrInvalidRequest, got %v", u, err)
		}
		if calls.Load() != 0 {
			t.Fatalf("url %q: expected 0 HTTP calls, got %d", u, calls.Load())
		}
	}
}

func TestRetryExhaustionPersistent500(t *testing.T) {
	c, calls := stubClient(rtFunc(func(*http.Request) (*http.Response, error) {
		return resp(http.StatusInternalServerError, `oops`, nil), nil
	}))

	_, err := c.Search(context.Background(), &search.SearchQuery{Query: "golang", NumResults: 5})
	if err == nil {
		t.Fatalf("expected error after retries, got nil")
	}
	apiErr := asAPIError(t, err)
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d (%v)", apiErr.StatusCode, apiErr)
	}
	if calls.Load() != 3 {
		t.Fatalf("expected exactly 3 HTTP calls, got %d", calls.Load())
	}
}

func TestConcurrentUse(t *testing.T) {
	c, calls := stubClient(rtFunc(func(*http.Request) (*http.Response, error) {
		return resp(http.StatusOK, `{"success":true,"data":[`+
			`{"url":"https://a.example","title":"A","description":"d"}]}`, nil), nil
	}))

	const workers = 10
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			page, err := c.Search(context.Background(), &search.SearchQuery{Query: "golang", NumResults: 5})
			if err == nil && len(page.Results) != 1 {
				err = errors.New("unexpected result count")
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d failed: %v", i, err)
		}
	}
	if calls.Load() != workers {
		t.Fatalf("expected %d HTTP calls, got %d", workers, calls.Load())
	}
}

func TestClose(t *testing.T) {
	c, _ := stubClient(rtFunc(func(*http.Request) (*http.Response, error) {
		return resp(http.StatusOK, `{}`, nil), nil
	}))
	if err := c.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}
