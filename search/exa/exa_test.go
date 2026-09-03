package exa

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

	"github.com/shepard-labs/go-clients/search"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body string, headers map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	h.Set("Content-Type", "application/json")
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

// newClient wraps rt with an atomic call counter and stubs out sleeping so
// retries happen without wall-clock delays.
func newClient(rt rtFunc, calls *int64) *Client {
	wrapped := rt
	if calls != nil {
		inner := rt
		wrapped = func(r *http.Request) (*http.Response, error) {
			atomic.AddInt64(calls, 1)
			return inner(r)
		}
	}
	c := New("test-key", zap.NewNop(), WithHTTPClient(&http.Client{Transport: wrapped})).(*Client)
	c.sleeper = func(context.Context, time.Duration) bool { return true }
	return c
}

const searchFixture = `{"results":[` +
	`{"url":"https://a.example/1","title":"First","score":0.9,"publishedDate":"2024-01-01","text":"text-body","summary":"summary-body","highlights":["h1","h2"]},` +
	`{"url":"https://a.example/2","title":"Second","score":0.5,"publishedDate":"2024-02-02","text":"","summary":"summary-body","highlights":["h1"]},` +
	`{"url":"https://a.example/3","title":"Third","score":0.1,"publishedDate":"2024-03-03","text":"","summary":"","highlights":["h1","h2"]}` +
	`]}`

func TestSearchSendsAuthAndParsesSnippetPreference(t *testing.T) {
	var method, path, auth string
	var reqBody map[string]any
	var calls int64
	c := newClient(func(r *http.Request) (*http.Response, error) {
		method = r.Method
		path = r.URL.Path
		auth = r.Header.Get("x-api-key")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &reqBody); err != nil {
			t.Errorf("unmarshal search request body %q: %v", raw, err)
		}
		return jsonResp(http.StatusOK, searchFixture, nil), nil
	}, &calls)

	page, err := c.Search(context.Background(), &search.SearchQuery{Query: "golang", NumResults: 3})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if method != http.MethodPost {
		t.Fatalf("method = %q, want POST", method)
	}
	if path != "/search" {
		t.Fatalf("path = %q, want /search", path)
	}
	if auth != "test-key" {
		t.Fatalf("x-api-key = %q, want test-key", auth)
	}
	if reqBody["query"] != "golang" {
		t.Fatalf("query = %v, want golang", reqBody["query"])
	}
	if reqBody["numResults"] != float64(3) {
		t.Fatalf("numResults = %v, want 3", reqBody["numResults"])
	}
	contents, ok := reqBody["contents"].(map[string]any)
	if !ok || contents["text"] != true {
		t.Fatalf("contents = %v, want {text:true}", reqBody["contents"])
	}

	if len(page.Results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(page.Results))
	}
	// Snippet preference: text wins, then summary, then joined highlights.
	if got := page.Results[0].Snippet; got != "text-body" {
		t.Fatalf("results[0].Snippet = %q, want text-body", got)
	}
	if got := page.Results[1].Snippet; got != "summary-body" {
		t.Fatalf("results[1].Snippet = %q, want summary-body", got)
	}
	if got := page.Results[2].Snippet; got != "h1\nh2" {
		t.Fatalf("results[2].Snippet = %q, want joined highlights", got)
	}
	first := page.Results[0]
	if first.URL != "https://a.example/1" || first.Title != "First" || first.Score != 0.9 || first.PublishedAt != "2024-01-01" {
		t.Fatalf("results[0] fields not mapped: %+v", first)
	}
	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("calls = %d, want 1", atomic.LoadInt64(&calls))
	}
}

func TestSearchInvalidQueryMakesNoHTTPCall(t *testing.T) {
	var calls int64
	c := newClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(http.StatusOK, `{"results":[]}`, nil), nil
	}, &calls)

	if _, err := c.Search(context.Background(), &search.SearchQuery{Query: "   ", NumResults: 5}); !errors.Is(err, search.ErrInvalidQuery) {
		t.Fatalf("expected ErrInvalidQuery, got %v", err)
	}
	if _, err := c.Search(context.Background(), nil); !errors.Is(err, search.ErrInvalidQuery) {
		t.Fatalf("expected ErrInvalidQuery for nil query, got %v", err)
	}
	if got := atomic.LoadInt64(&calls); got != 0 {
		t.Fatalf("calls = %d, want 0", got)
	}
}

func TestSearchEmptyKeyMakesNoHTTPCall(t *testing.T) {
	var calls int64
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt64(&calls, 1)
		return jsonResp(http.StatusOK, `{"results":[]}`, nil), nil
	})
	c := New("", zap.NewNop(), WithHTTPClient(&http.Client{Transport: rt})).(*Client)
	c.sleeper = func(context.Context, time.Duration) bool { return true }

	if _, err := c.Search(context.Background(), &search.SearchQuery{Query: "golang", NumResults: 5}); err == nil {
		t.Fatalf("expected error for empty api key, got nil")
	}
	if got := atomic.LoadInt64(&calls); got != 0 {
		t.Fatalf("calls = %d, want 0", got)
	}
}

func TestSearchUnauthorized(t *testing.T) {
	var calls int64
	c := newClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(http.StatusUnauthorized, `{"code":"unauthorized","message":"bad key"}`, nil), nil
	}, &calls)

	_, err := c.Search(context.Background(), &search.SearchQuery{Query: "golang", NumResults: 5})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if !apiErr.IsUnauthorized() {
		t.Fatalf("IsUnauthorized = false for 401: %+v", apiErr)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("StatusCode = %d, want 401", apiErr.StatusCode)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("calls = %d, want 1 (401 must not retry)", got)
	}
}

func TestSearchPaymentRequiredNotRetried(t *testing.T) {
	var calls int64
	c := newClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(http.StatusPaymentRequired, `{"code":"billing","message":"top up"}`, nil), nil
	}, &calls)

	if _, err := c.Search(context.Background(), &search.SearchQuery{Query: "golang", NumResults: 5}); err == nil {
		t.Fatalf("expected error, got nil")
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("calls = %d, want exactly 1 (402 must not retry)", got)
	}
}

func TestSearchRateLimitedRetriesThenSucceeds(t *testing.T) {
	var calls int64
	c := newClient(func(r *http.Request) (*http.Response, error) {
		if atomic.LoadInt64(&calls) == 1 {
			return jsonResp(http.StatusTooManyRequests, `{"message":"slow down"}`, map[string]string{"Retry-After": "0"}), nil
		}
		return jsonResp(http.StatusOK, searchFixture, nil), nil
	}, &calls)

	page, err := c.Search(context.Background(), &search.SearchQuery{Query: "golang", NumResults: 3})
	if err != nil {
		t.Fatalf("Search after 429: %v", err)
	}
	if len(page.Results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(page.Results))
	}
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("calls = %d, want 2 (429 then success)", got)
	}
}

func TestSearchMalformedJSON(t *testing.T) {
	c := newClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(http.StatusOK, `{"results":[oops`, nil), nil
	}, nil)

	if _, err := c.Search(context.Background(), &search.SearchQuery{Query: "golang", NumResults: 5}); err == nil {
		t.Fatalf("expected decode error, got nil")
	}
}

func TestSearchResponseTooLarge(t *testing.T) {
	big := strings.Repeat("x", int(search.MaxResponseBytes)+1)
	c := newClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(http.StatusOK, big, nil), nil
	}, nil)

	_, err := c.Search(context.Background(), &search.SearchQuery{Query: "golang", NumResults: 5})
	if !errors.Is(err, search.ErrResponseTooLarge) {
		t.Fatalf("expected ErrResponseTooLarge, got %v", err)
	}
}

func TestScrapeSendsContentsWithURLsKey(t *testing.T) {
	var method, path string
	var reqBody map[string]any
	c := newClient(func(r *http.Request) (*http.Response, error) {
		method = r.Method
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &reqBody); err != nil {
			t.Errorf("unmarshal scrape request body %q: %v", raw, err)
		}
		return jsonResp(http.StatusOK, `{"results":[{"url":"https://a.example/1","text":"# hello","author":"ada","publishedDate":"2024-01-01"}]}`, nil), nil
	}, nil)

	doc, err := c.Scrape(context.Background(), &search.ScrapeRequest{URL: "https://a.example/1"})
	if err != nil {
		t.Fatalf("Scrape: %v", err)
	}
	if method != http.MethodPost {
		t.Fatalf("method = %q, want POST", method)
	}
	if path != "/contents" {
		t.Fatalf("path = %q, want /contents", path)
	}
	urls, ok := reqBody["urls"].([]any)
	if !ok || len(urls) != 1 || urls[0] != "https://a.example/1" {
		t.Fatalf("urls = %v, want [https://a.example/1]", reqBody["urls"])
	}
	if reqBody["text"] != true {
		t.Fatalf("text = %v, want true", reqBody["text"])
	}
	if doc.Markdown != "# hello" {
		t.Fatalf("Markdown = %q, want # hello", doc.Markdown)
	}
	if doc.URL != "https://a.example/1" {
		t.Fatalf("URL = %q, want scraped url", doc.URL)
	}
	if doc.Metadata["author"] != "ada" || doc.Metadata["publishedDate"] != "2024-01-01" {
		t.Fatalf("Metadata = %v, want author and publishedDate", doc.Metadata)
	}
}

func TestScrapeZeroResults(t *testing.T) {
	c := newClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(http.StatusOK, `{"results":[]}`, nil), nil
	}, nil)

	_, err := c.Scrape(context.Background(), &search.ScrapeRequest{URL: "https://a.example/1"})
	if err == nil {
		t.Fatalf("expected no-results error, got nil")
	}
	if !strings.Contains(err.Error(), "no results") {
		t.Fatalf("error = %q, want it to mention no results", err.Error())
	}
}

func TestScrapeBadURLMakesNoHTTPCall(t *testing.T) {
	var calls int64
	c := newClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(http.StatusOK, `{"results":[]}`, nil), nil
	}, &calls)

	if _, err := c.Scrape(context.Background(), &search.ScrapeRequest{URL: "not-a-url"}); !errors.Is(err, search.ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, got %v", err)
	}
	if got := atomic.LoadInt64(&calls); got != 0 {
		t.Fatalf("calls = %d, want 0", got)
	}
}

func TestCrawlNotSupported(t *testing.T) {
	var calls int64
	c := newClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(http.StatusOK, `{}`, nil), nil
	}, &calls)

	if _, err := c.Crawl(context.Background(), &search.CrawlRequest{StartURL: "https://a.example", MaxPages: 5}); !errors.Is(err, search.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported for valid request, got %v", err)
	}
	if _, err := c.Crawl(context.Background(), nil); !errors.Is(err, search.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported for nil request, got %v", err)
	}
	if got := atomic.LoadInt64(&calls); got != 0 {
		t.Fatalf("calls = %d, want 0", got)
	}
}

func TestAnswerReturnsString(t *testing.T) {
	var method, path string
	var reqBody map[string]any
	c := newClient(func(r *http.Request) (*http.Response, error) {
		method = r.Method
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &reqBody); err != nil {
			t.Errorf("unmarshal answer request body %q: %v", raw, err)
		}
		return jsonResp(http.StatusOK, `{"answer":"the sky is blue","citations":[]}`, nil), nil
	}, nil)

	ans, err := c.Answer(context.Background(), "what color is the sky?")
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if ans != "the sky is blue" {
		t.Fatalf("answer = %q, want the sky is blue", ans)
	}
	if method != http.MethodPost {
		t.Fatalf("method = %q, want POST", method)
	}
	if path != "/answer" {
		t.Fatalf("path = %q, want /answer", path)
	}
	if reqBody["query"] != "what color is the sky?" {
		t.Fatalf("query = %v, want the question", reqBody["query"])
	}
	if reqBody["stream"] != false || reqBody["model"] != "exa" {
		t.Fatalf("body = %v, want stream:false model:exa", reqBody)
	}
}

func TestAnswerBlankQueryMakesNoHTTPCall(t *testing.T) {
	var calls int64
	c := newClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(http.StatusOK, `{"answer":"x"}`, nil), nil
	}, &calls)

	if _, err := c.Answer(context.Background(), "   "); !errors.Is(err, search.ErrInvalidQuery) {
		t.Fatalf("expected ErrInvalidQuery, got %v", err)
	}
	if got := atomic.LoadInt64(&calls); got != 0 {
		t.Fatalf("calls = %d, want 0", got)
	}
}

func TestRetryExhaustionPersistent500(t *testing.T) {
	var calls int64
	c := newClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(http.StatusInternalServerError, `{"message":"boom"}`, nil), nil
	}, &calls)

	_, err := c.Search(context.Background(), &search.SearchQuery{Query: "golang", NumResults: 5})
	if err == nil {
		t.Fatalf("expected error after retries, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("StatusCode = %d, want 500", apiErr.StatusCode)
	}
	if got := atomic.LoadInt64(&calls); got != 3 {
		t.Fatalf("calls = %d, want exactly 3", got)
	}
}

func TestContextCanceled(t *testing.T) {
	c := newClient(func(r *http.Request) (*http.Response, error) {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		return jsonResp(http.StatusOK, searchFixture, nil), nil
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Search(ctx, &search.SearchQuery{Query: "golang", NumResults: 5}); err == nil {
		t.Fatalf("expected context error, got nil")
	}
}

func TestConcurrentSmoke(t *testing.T) {
	var calls int64
	c := newClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(http.StatusOK, searchFixture, nil), nil
	}, &calls)

	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			page, err := c.Search(context.Background(), &search.SearchQuery{Query: "golang", NumResults: 3})
			if err != nil {
				errs[i] = err
				return
			}
			if len(page.Results) != 3 {
				t.Errorf("goroutine %d: len(results) = %d, want 3", i, len(page.Results))
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt64(&calls); got != n {
		t.Fatalf("calls = %d, want %d", got, n)
	}
}

func TestClose(t *testing.T) {
	c := newClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(http.StatusOK, `{"results":[]}`, nil), nil
	}, nil)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
