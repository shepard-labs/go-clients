package crawl4ai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/shepard-labs/go-clients/search"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// newTestClient builds a *Client with a counting transport, Bearer token "t",
// and an instant sleeper so polls never actually wait.
func newTestClient(rt rtFunc) (*Client, *atomic.Int64) {
	calls := &atomic.Int64{}
	counting := rtFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return rt(r)
	})
	c := New("http://localhost:11235", zap.NewNop(), WithToken("t"), WithHTTPClient(&http.Client{Transport: counting})).(*Client)
	c.sleeper = func(context.Context, time.Duration) bool { return true }
	return c, calls
}

func failIfNoBearer(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer t" {
		t.Fatalf("expected Authorization Bearer t, got %q", got)
	}
}

func TestSearchNotSupported(t *testing.T) {
	c, calls := newTestClient(func(r *http.Request) (*http.Response, error) {
		t.Errorf("Search must not issue HTTP requests, got %s %s", r.Method, r.URL.Path)
		return jsonResp(200, `{}`), nil
	})

	queries := []*search.SearchQuery{
		{Query: "golang", NumResults: 10},
		nil,
	}
	for i, q := range queries {
		if _, err := c.Search(context.Background(), q); !errors.Is(err, search.ErrNotSupported) {
			t.Fatalf("query %d: expected ErrNotSupported, got %v", i, err)
		}
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("expected 0 HTTP calls, got %d", got)
	}
}

func TestScrapeSuccess(t *testing.T) {
	var polls atomic.Int64
	var authSeen atomic.Int64
	c, calls := newTestClient(func(r *http.Request) (*http.Response, error) {
		failIfNoBearer(t, r)
		authSeen.Add(1)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/crawl/job":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "https://example.com/page") {
				t.Errorf("submit body missing URL, got %s", body)
			}
			return jsonResp(202, `{"task_id":"crawl_abc123"}`), nil
		case r.Method == http.MethodGet && r.URL.Path == "/crawl/job/crawl_abc123":
			if polls.Add(1) == 1 {
				return jsonResp(200, `{"task_id":"crawl_abc123","status":"processing"}`), nil
			}
			return jsonResp(200, `{"task_id":"crawl_abc123","status":"completed","result":{"url":"https://example.com/page","markdown":"# Hello","links":{"internal":["https://example.com/a"],"external":["https://example.org/b"]}}}`), nil
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			return jsonResp(500, `{}`), nil
		}
	})

	doc, err := c.Scrape(context.Background(), &search.ScrapeRequest{URL: "https://example.com/page"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc.URL != "https://example.com/page" {
		t.Fatalf("expected doc URL backfill, got %q", doc.URL)
	}
	if doc.Markdown != "# Hello" {
		t.Fatalf("expected markdown %q, got %q", "# Hello", doc.Markdown)
	}
	wantLinks := []string{"https://example.com/a", "https://example.org/b"}
	if len(doc.Links) != len(wantLinks) {
		t.Fatalf("expected links %v, got %v", wantLinks, doc.Links)
	}
	for i := range wantLinks {
		if doc.Links[i] != wantLinks[i] {
			t.Fatalf("expected links %v, got %v", wantLinks, doc.Links)
		}
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("expected 3 HTTP calls (1 submit + 2 polls), got %d", got)
	}
	if got := authSeen.Load(); got != calls.Load() {
		t.Fatalf("expected Bearer on every request, saw %d of %d", got, calls.Load())
	}
}

func TestScrapeInvalidRequest(t *testing.T) {
	c, calls := newTestClient(func(r *http.Request) (*http.Response, error) {
		t.Errorf("invalid request must not issue HTTP calls, got %s %s", r.Method, r.URL.Path)
		return jsonResp(200, `{}`), nil
	})

	reqs := []*search.ScrapeRequest{
		nil,
		{URL: "not-a-url"},
		{URL: ""},
	}
	for i, r := range reqs {
		if _, err := c.Scrape(context.Background(), r); !errors.Is(err, search.ErrInvalidRequest) {
			t.Fatalf("req %d: expected ErrInvalidRequest, got %v", i, err)
		}
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("expected 0 HTTP calls, got %d", got)
	}
}

func TestCrawlMultiResult(t *testing.T) {
	c, calls := newTestClient(func(r *http.Request) (*http.Response, error) {
		failIfNoBearer(t, r)
		if r.Method == http.MethodPost && r.URL.Path == "/crawl/job" {
			return jsonResp(202, `{"task_id":"crawl_multi"}`), nil
		}
		if r.Method == http.MethodGet && r.URL.Path == "/crawl/job/crawl_multi" {
			return jsonResp(200, `{"task_id":"crawl_multi","status":"completed","result":{"success":true,"results":[{"success":true,"url":"https://example.com/1","markdown":"# One"},{"success":true,"url":"https://example.com/2","markdown":"# Two","links":["https://example.com/3"]},{"success":false,"url":"https://example.com/bad","markdown":"skipped"}]}}`), nil
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		return jsonResp(500, `{}`), nil
	})

	page, err := c.Crawl(context.Background(), &search.CrawlRequest{StartURL: "https://example.com", MaxPages: 5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(page.Documents) != 2 {
		t.Fatalf("expected 2 documents (failed item skipped), got %d", len(page.Documents))
	}
	if page.Documents[0].Markdown != "# One" || page.Documents[0].URL != "https://example.com/1" {
		t.Fatalf("unexpected first doc: %+v", page.Documents[0])
	}
	if page.Documents[1].Markdown != "# Two" {
		t.Fatalf("unexpected second doc: %+v", page.Documents[1])
	}
	if len(page.Documents[1].Links) != 1 || page.Documents[1].Links[0] != "https://example.com/3" {
		t.Fatalf("unexpected second doc links: %v", page.Documents[1].Links)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 HTTP calls (1 submit + 1 poll), got %d", got)
	}
}

func TestCrawlFailedTerminal(t *testing.T) {
	c, _ := newTestClient(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			return jsonResp(202, `{"task_id":"crawl_bad"}`), nil
		}
		return jsonResp(200, `{"task_id":"crawl_bad","status":"failed","error":"crawler exploded"}`), nil
	})

	_, err := c.Crawl(context.Background(), &search.CrawlRequest{StartURL: "https://example.com", MaxPages: 2})
	if err == nil {
		t.Fatal("expected error for failed job, got nil")
	}
	if !strings.Contains(err.Error(), "crawler exploded") {
		t.Fatalf("expected server failure message, got %v", err)
	}
}

func TestCrawlPollTimeout(t *testing.T) {
	polls := &atomic.Int64{}
	c, _ := newTestClient(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			return jsonResp(202, `{"task_id":"crawl_slow"}`), nil
		}
		polls.Add(1)
		return jsonResp(200, `{"task_id":"crawl_slow","status":"processing"}`), nil
	})
	c.pollInterval = time.Nanosecond
	c.maxPollWait = 5 * time.Millisecond

	_, err := c.Crawl(context.Background(), &search.CrawlRequest{StartURL: "https://example.com", MaxPages: 2})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
	if polls.Load() == 0 {
		t.Fatalf("expected at least one status poll before timing out")
	}
}

func TestCrawlContextCancelMidPoll(t *testing.T) {
	c, _ := newTestClient(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			return jsonResp(202, `{"task_id":"crawl_slow"}`), nil
		}
		return jsonResp(200, `{"task_id":"crawl_slow","status":"processing"}`), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	c.sleeper = func(ctx context.Context, d time.Duration) bool {
		cancel()
		<-ctx.Done()
		return false
	}

	_, err := c.Crawl(ctx, &search.CrawlRequest{StartURL: "https://example.com", MaxPages: 2})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestStartCrawl(t *testing.T) {
	c, calls := newTestClient(func(r *http.Request) (*http.Response, error) {
		failIfNoBearer(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/crawl/job" {
			t.Errorf("expected POST /crawl/job, got %s %s", r.Method, r.URL.Path)
			return jsonResp(500, `{}`), nil
		}
		return jsonResp(202, `{"task_id":"crawl_deadbeef"}`), nil
	})

	id, err := c.StartCrawl(context.Background(), &search.CrawlRequest{StartURL: "https://example.com", MaxPages: 3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "crawl_deadbeef" {
		t.Fatalf("expected task id crawl_deadbeef, got %q", id)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 HTTP call, got %d", got)
	}

	if _, err := c.StartCrawl(context.Background(), &search.CrawlRequest{StartURL: "://bad", MaxPages: 3}); !errors.Is(err, search.ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest for bad StartURL, got %v", err)
	}
}

func TestJobStatus(t *testing.T) {
	c, _ := newTestClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/crawl_waiting"):
			return jsonResp(200, `{"task_id":"crawl_waiting","status":"processing"}`), nil
		case strings.HasSuffix(r.URL.Path, "/crawl_done"):
			return jsonResp(200, `{"task_id":"crawl_done","status":"completed","result":{"url":"https://example.com","markdown":"# Done"}}`), nil
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			return jsonResp(500, `{}`), nil
		}
	})
	ctx := context.Background()

	if _, _, err := c.JobStatus(ctx, ""); err == nil {
		t.Fatal("expected error for empty task id, got nil")
	} else if !errors.Is(err, search.ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest for empty task id, got %v", err)
	}

	page, status, err := c.JobStatus(ctx, "crawl_waiting")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if page != nil {
		t.Fatalf("expected nil page while processing, got %+v", page)
	}
	if status != "processing" {
		t.Fatalf("expected status processing, got %q", status)
	}

	page, status, err = c.JobStatus(ctx, "crawl_done")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "completed" {
		t.Fatalf("expected status completed, got %q", status)
	}
	if page == nil || len(page.Documents) != 1 || page.Documents[0].Markdown != "# Done" {
		t.Fatalf("unexpected completed page: %+v", page)
	}
}

func TestJobStatusEscapesTaskID(t *testing.T) {
	const taskID = "job/with space"
	var gotEscaped string
	c, _ := newTestClient(func(r *http.Request) (*http.Response, error) {
		gotEscaped = r.URL.EscapedPath()
		return jsonResp(200, `{"task_id":"job/with space","status":"processing"}`), nil
	})

	page, status, err := c.JobStatus(context.Background(), taskID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if page != nil {
		t.Fatalf("expected nil page while processing, got %+v", page)
	}
	if status != "processing" {
		t.Fatalf("expected status processing, got %q", status)
	}
	if want := "/crawl/job/" + url.PathEscape(taskID); gotEscaped != want {
		t.Fatalf("escaped path = %q, want %q (task id must stay a single segment)", gotEscaped, want)
	}
}

func TestVerifyAuth(t *testing.T) {
	okClient, okCalls := newTestClient(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != "/health" {
			t.Errorf("expected GET /health, got %s %s", r.Method, r.URL.Path)
			return jsonResp(500, `{}`), nil
		}
		return jsonResp(200, `{"status":"ok"}`), nil
	})
	if err := okClient.VerifyAuth(context.Background()); err != nil {
		t.Fatalf("expected nil for 2xx, got %v", err)
	}
	if got := okCalls.Load(); got != 1 {
		t.Fatalf("expected 1 HTTP call, got %d", got)
	}

	unauthClient, _ := newTestClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(401, `{"detail":"invalid token"}`), nil
	})
	err := unauthClient.VerifyAuth(context.Background())
	if err == nil {
		t.Fatal("expected error for 401, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T (%v)", err, err)
	}
	if !apiErr.IsUnauthorized() {
		t.Fatalf("expected IsUnauthorized, got status %d", apiErr.StatusCode)
	}
}

func TestRetryExhaustionPersistent500(t *testing.T) {
	c, calls := newTestClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(500, `{"detail":"boom"}`), nil
	})

	if err := c.VerifyAuth(context.Background()); err == nil {
		t.Fatal("expected error after retries, got nil")
	} else if !strings.Contains(err.Error(), "after retries") {
		t.Fatalf("expected retry-exhaustion error, got %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("expected exactly 3 calls, got %d", got)
	}
}

func TestNoRetryOn402(t *testing.T) {
	c, calls := newTestClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(402, `{"detail":"payment required"}`), nil
	})

	err := c.VerifyAuth(context.Background())
	if err == nil {
		t.Fatal("expected error for 402, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T (%v)", err, err)
	}
	if apiErr.StatusCode != 402 {
		t.Fatalf("expected status 402, got %d", apiErr.StatusCode)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 call (no retry on 4xx), got %d", got)
	}
}

func TestMalformedJSON(t *testing.T) {
	c, _ := newTestClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(202, `this is not json{`), nil
	})

	if _, err := c.StartCrawl(context.Background(), &search.CrawlRequest{StartURL: "https://example.com", MaxPages: 1}); err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	} else if !strings.Contains(err.Error(), "unmarshal") {
		t.Fatalf("expected unmarshal error, got %v", err)
	}
}

func TestResponseTooLarge(t *testing.T) {
	big := strings.Repeat("x", int(search.MaxResponseBytes)+1)
	c, _ := newTestClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, big), nil
	})

	if err := c.VerifyAuth(context.Background()); !errors.Is(err, search.ErrResponseTooLarge) {
		t.Fatalf("expected ErrResponseTooLarge, got %v", err)
	}
}

func TestConcurrentSmoke(t *testing.T) {
	c, _ := newTestClient(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			return jsonResp(202, `{"task_id":"crawl_shared"}`), nil
		}
		return jsonResp(200, `{"task_id":"crawl_shared","status":"completed","result":{"url":"https://example.com","markdown":"# Shared"}}`), nil
	})

	const n = 8
	var wg sync.WaitGroup
	var ok atomic.Int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			doc, err := c.Scrape(context.Background(), &search.ScrapeRequest{URL: "https://example.com"})
			if err != nil {
				t.Errorf("concurrent scrape failed: %v", err)
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
}

func TestCloseAndDefaults(t *testing.T) {
	c, _ := newTestClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{}`), nil
	})
	if err := c.Close(); err != nil {
		t.Fatalf("expected nil Close, got %v", err)
	}

	def := New("", nil).(*Client)
	if def.baseURL != "http://localhost:11235" {
		t.Fatalf("expected default baseURL, got %q", def.baseURL)
	}
	if err := def.Close(); err != nil {
		t.Fatalf("expected nil Close, got %v", err)
	}
}
