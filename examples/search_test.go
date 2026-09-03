package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/shepard-labs/go-clients/search"
)

const (
	testAPIKey   = "test-api-key-0123456789"
	secretDetail = "boom-secret-detail"
)

// fakeSearchClient is a search.Client with per-method func fields so each test
// can drive exactly one handler outcome.
type fakeSearchClient struct {
	searchFn func(ctx context.Context, q *search.SearchQuery) (*search.SearchPage, error)
	scrapeFn func(ctx context.Context, r *search.ScrapeRequest) (*search.Document, error)
	crawlFn  func(ctx context.Context, r *search.CrawlRequest) (*search.CrawlPage, error)
}

func (f *fakeSearchClient) Search(ctx context.Context, q *search.SearchQuery) (*search.SearchPage, error) {
	return f.searchFn(ctx, q)
}

func (f *fakeSearchClient) Scrape(ctx context.Context, r *search.ScrapeRequest) (*search.Document, error) {
	return f.scrapeFn(ctx, r)
}

func (f *fakeSearchClient) Crawl(ctx context.Context, r *search.CrawlRequest) (*search.CrawlPage, error) {
	return f.crawlFn(ctx, r)
}

func (f *fakeSearchClient) Close() error { return nil }

func newSearchTestRouter(searcher search.Client) *gin.Engine {
	cfg := &config{APIKey: testAPIKey, MaxRequestBytes: defaultMaxRequestBytes}
	srv := &server{logger: zap.NewNop(), searcher: searcher}
	return buildRouter(cfg, srv)
}

func postSearchJSON(t *testing.T, r *gin.Engine, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestSearchQueryNumResultsLimit(t *testing.T) {
	called := false
	fake := &fakeSearchClient{
		searchFn: func(ctx context.Context, q *search.SearchQuery) (*search.SearchPage, error) {
			called = true
			return &search.SearchPage{}, nil
		},
	}
	w := postSearchJSON(t, newSearchTestRouter(fake), "/search/query", `{"query":"gophers","num_results":21}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d (body %s)", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "num_results too large") {
		t.Fatalf("body %q should mention num_results limit", w.Body.String())
	}
	if called {
		t.Fatal("searcher must not be called when num_results exceeds the limit")
	}
}

func TestSearchCrawlMaxPagesLimit(t *testing.T) {
	called := false
	fake := &fakeSearchClient{
		crawlFn: func(ctx context.Context, r *search.CrawlRequest) (*search.CrawlPage, error) {
			called = true
			return &search.CrawlPage{}, nil
		},
	}
	w := postSearchJSON(t, newSearchTestRouter(fake), "/search/crawl", `{"start_url":"https://example.com","max_pages":51}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d (body %s)", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "max_pages too large") {
		t.Fatalf("body %q should mention max_pages limit", w.Body.String())
	}
	if called {
		t.Fatal("searcher must not be called when max_pages exceeds the limit")
	}
}

func TestSearchHandlerStatusMapping(t *testing.T) {
	notSupported := func() *fakeSearchClient {
		return &fakeSearchClient{
			searchFn: func(ctx context.Context, q *search.SearchQuery) (*search.SearchPage, error) {
				return nil, fmt.Errorf("exa %s: %w", secretDetail, search.ErrNotSupported)
			},
			scrapeFn: func(ctx context.Context, r *search.ScrapeRequest) (*search.Document, error) {
				return nil, fmt.Errorf("exa %s: %w", secretDetail, search.ErrNotSupported)
			},
			crawlFn: func(ctx context.Context, r *search.CrawlRequest) (*search.CrawlPage, error) {
				return nil, fmt.Errorf("exa %s: %w", secretDetail, search.ErrNotSupported)
			},
		}
	}
	genericErr := func() *fakeSearchClient {
		return &fakeSearchClient{
			searchFn: func(ctx context.Context, q *search.SearchQuery) (*search.SearchPage, error) {
				return nil, errors.New(secretDetail)
			},
			scrapeFn: func(ctx context.Context, r *search.ScrapeRequest) (*search.Document, error) {
				return nil, errors.New(secretDetail)
			},
			crawlFn: func(ctx context.Context, r *search.CrawlRequest) (*search.CrawlPage, error) {
				return nil, errors.New(secretDetail)
			},
		}
	}
	invalidErr := func() *fakeSearchClient {
		return &fakeSearchClient{
			searchFn: func(ctx context.Context, q *search.SearchQuery) (*search.SearchPage, error) {
				return nil, fmt.Errorf("%s: %w", secretDetail, search.ErrInvalidQuery)
			},
			scrapeFn: func(ctx context.Context, r *search.ScrapeRequest) (*search.Document, error) {
				return nil, fmt.Errorf("%s: %w", secretDetail, search.ErrInvalidRequest)
			},
			crawlFn: func(ctx context.Context, r *search.CrawlRequest) (*search.CrawlPage, error) {
				return nil, fmt.Errorf("%s: %w", secretDetail, search.ErrInvalidRequest)
			},
		}
	}

	tests := []struct {
		name         string
		path         string
		body         string
		fake         func() *fakeSearchClient
		wantStatus   int
		wantContains string
	}{
		{"query invalid", "/search/query", `{"query":"","num_results":5}`, invalidErr, http.StatusBadRequest, "invalid search query"},
		{"query not supported", "/search/query", `{"query":"gophers","num_results":5}`, notSupported, http.StatusNotImplemented, "search not supported by provider"},
		{"query upstream failure", "/search/query", `{"query":"gophers","num_results":5}`, genericErr, http.StatusBadGateway, "search query failed"},
		{"scrape invalid", "/search/scrape", `{"url":"https://example.com"}`, invalidErr, http.StatusBadRequest, "invalid scrape request"},
		{"scrape upstream failure", "/search/scrape", `{"url":"https://example.com"}`, genericErr, http.StatusBadGateway, "search scrape failed"},
		{"crawl invalid", "/search/crawl", `{"start_url":"https://example.com","max_pages":3}`, invalidErr, http.StatusBadRequest, "invalid crawl request"},
		{"crawl not supported", "/search/crawl", `{"start_url":"https://example.com","max_pages":3}`, notSupported, http.StatusNotImplemented, "crawl not supported by provider"},
		{"crawl upstream failure", "/search/crawl", `{"start_url":"https://example.com","max_pages":3}`, genericErr, http.StatusBadGateway, "search crawl failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := postSearchJSON(t, newSearchTestRouter(tt.fake()), tt.path, tt.body)
			if w.Code != tt.wantStatus {
				t.Fatalf("got status %d, want %d (body %s)", w.Code, tt.wantStatus, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tt.wantContains) {
				t.Fatalf("body %q should contain %q", w.Body.String(), tt.wantContains)
			}
			if strings.Contains(w.Body.String(), secretDetail) {
				t.Fatalf("body %q leaks provider detail", w.Body.String())
			}
		})
	}
}

func TestSearchCrawlDeadlineMapsTo504(t *testing.T) {
	fake := &fakeSearchClient{
		crawlFn: func(ctx context.Context, r *search.CrawlRequest) (*search.CrawlPage, error) {
			return nil, fmt.Errorf("%s: %w", secretDetail, context.DeadlineExceeded)
		},
	}
	w := postSearchJSON(t, newSearchTestRouter(fake), "/search/crawl", `{"start_url":"https://example.com","max_pages":3}`)
	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("got status %d, want %d (body %s)", w.Code, http.StatusGatewayTimeout, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "search crawl timed out") {
		t.Fatalf("body %q should mention the crawl timeout", w.Body.String())
	}
	if strings.Contains(w.Body.String(), secretDetail) {
		t.Fatalf("body %q leaks provider detail", w.Body.String())
	}
}

func TestSearchHandlerSuccess(t *testing.T) {
	fake := &fakeSearchClient{
		searchFn: func(ctx context.Context, q *search.SearchQuery) (*search.SearchPage, error) {
			return &search.SearchPage{Results: []search.SearchResult{
				{URL: "https://example.com/a", Title: "Example A", Snippet: "a snippet"},
			}}, nil
		},
		scrapeFn: func(ctx context.Context, r *search.ScrapeRequest) (*search.Document, error) {
			return &search.Document{URL: "https://example.com", Markdown: "hello-markdown"}, nil
		},
		crawlFn: func(ctx context.Context, r *search.CrawlRequest) (*search.CrawlPage, error) {
			return &search.CrawlPage{Documents: []search.Document{
				{URL: "https://example.com", Markdown: "crawled-markdown"},
			}}, nil
		},
	}
	router := newSearchTestRouter(fake)

	tests := []struct {
		name         string
		path         string
		body         string
		wantContains string
	}{
		{"query ok", "/search/query", `{"query":"gophers","num_results":5}`, "https://example.com/a"},
		{"scrape ok", "/search/scrape", `{"url":"https://example.com"}`, "hello-markdown"},
		{"crawl ok", "/search/crawl", `{"start_url":"https://example.com","max_pages":3}`, "crawled-markdown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := postSearchJSON(t, router, tt.path, tt.body)
			if w.Code != http.StatusOK {
				t.Fatalf("got status %d, want %d (body %s)", w.Code, http.StatusOK, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tt.wantContains) {
				t.Fatalf("body %q should contain echoed payload %q", w.Body.String(), tt.wantContains)
			}
		})
	}
}

// setSearchTestBaseEnv establishes a valid baseline for every loadConfig test
// and clears provider keys so ambient exported secrets cannot leak between
// cases. Individual tests then set exactly the SEARCH_PROVIDER keys they need.
func setSearchTestBaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("API_KEY", testAPIKey)
	t.Setenv("KMS_KEY_NAME", "projects/p/locations/global/keyRings/r/cryptoKeys/k")
	t.Setenv("EMAIL_PROVIDER", "postmark")
	t.Setenv("POSTMARK_SERVER_TOKEN", "dummy-postmark-token")
	t.Setenv("STORAGE_PROVIDER", "gcs")
	t.Setenv("GCS_BUCKET", "dummy-bucket")
	for _, k := range []string{
		"SEARCH_PROVIDER", "FIRECRAWL_API_KEY", "EXA_API_KEY",
		"CRAWL4AI_TOKEN", "CRAWL4AI_BASE_URL",
		"SES_ACCESS_KEY_ID", "SES_SECRET_ACCESS_KEY", "SES_REGION",
		"R2_ACCOUNT_ID", "R2_ACCESS_KEY_ID", "R2_SECRET_KEY", "R2_BUCKET",
		"GCS_SERVICE_ACCOUNT", "KMS_SERVICE_ACCOUNT", "ADDR", "LOG_LEVEL",
	} {
		t.Setenv(k, "")
	}
}

func TestLoadConfigSearchProviders(t *testing.T) {
	t.Run("bogus provider rejected", func(t *testing.T) {
		setSearchTestBaseEnv(t)
		t.Setenv("SEARCH_PROVIDER", "bogus")
		t.Setenv("FIRECRAWL_API_KEY", "dummy")
		_, err := loadConfig()
		if err == nil || !strings.Contains(err.Error(), "invalid SEARCH_PROVIDER") {
			t.Fatalf("got err %v, want invalid SEARCH_PROVIDER error", err)
		}
	})

	t.Run("exa missing key", func(t *testing.T) {
		setSearchTestBaseEnv(t)
		t.Setenv("SEARCH_PROVIDER", "exa")
		_, err := loadConfig()
		if err == nil || !strings.Contains(err.Error(), "EXA_API_KEY") {
			t.Fatalf("got err %v, want missing-var error naming EXA_API_KEY", err)
		}
	})

	t.Run("crawl4ai minimal valid", func(t *testing.T) {
		setSearchTestBaseEnv(t)
		t.Setenv("SEARCH_PROVIDER", "crawl4ai")
		t.Setenv("CRAWL4AI_TOKEN", "dummy-token")
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig failed: %v", err)
		}
		if cfg.SearchProvider != "crawl4ai" {
			t.Fatalf("got provider %q, want crawl4ai", cfg.SearchProvider)
		}
		if cfg.Crawl4AI.BaseURL != "http://localhost:11235" {
			t.Fatalf("got base URL %q, want default http://localhost:11235", cfg.Crawl4AI.BaseURL)
		}
	})

	t.Run("default requires firecrawl key", func(t *testing.T) {
		setSearchTestBaseEnv(t)
		_, err := loadConfig()
		if err == nil || !strings.Contains(err.Error(), "FIRECRAWL_API_KEY") {
			t.Fatalf("got err %v, want missing-var error naming FIRECRAWL_API_KEY", err)
		}

		t.Setenv("FIRECRAWL_API_KEY", "dummy-firecrawl-key")
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig failed: %v", err)
		}
		if cfg.SearchProvider != "firecrawl" {
			t.Fatalf("got provider %q, want default firecrawl", cfg.SearchProvider)
		}
	})
}

func TestRoutesRegistered(t *testing.T) {
	router := newSearchTestRouter(&fakeSearchClient{
		searchFn: func(ctx context.Context, q *search.SearchQuery) (*search.SearchPage, error) {
			return &search.SearchPage{}, nil
		},
		scrapeFn: func(ctx context.Context, r *search.ScrapeRequest) (*search.Document, error) {
			return &search.Document{}, nil
		},
		crawlFn: func(ctx context.Context, r *search.CrawlRequest) (*search.CrawlPage, error) {
			return &search.CrawlPage{}, nil
		},
	})
	present := map[string]bool{}
	for _, r := range router.Routes() {
		present[r.Method+" "+r.Path] = true
	}
	for _, want := range []string{
		"GET /healthz",
		"POST /email/send",
		"POST /storage/upload",
		"POST /kms/encrypt",
		"POST /search/query",
		"POST /search/scrape",
		"POST /search/crawl",
	} {
		if !present[want] {
			t.Fatalf("route %q not registered (have %v)", want, present)
		}
	}
}

func TestLoadConfigEmailStorageProviders(t *testing.T) {
	setSearchTestBaseEnv(t)
	t.Setenv("EMAIL_PROVIDER", "ses")
	t.Setenv("SES_ACCESS_KEY_ID", "dummy-access")
	t.Setenv("SES_SECRET_ACCESS_KEY", "dummy-secret")
	t.Setenv("SES_REGION", "us-east-1")
	t.Setenv("STORAGE_PROVIDER", "r2")
	t.Setenv("R2_ACCOUNT_ID", "dummy-account")
	t.Setenv("R2_ACCESS_KEY_ID", "dummy-access")
	t.Setenv("R2_SECRET_KEY", "dummy-secret")
	t.Setenv("R2_BUCKET", "dummy-bucket")
	t.Setenv("SEARCH_PROVIDER", "firecrawl")
	t.Setenv("FIRECRAWL_API_KEY", "dummy-firecrawl-key")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig failed: %v", err)
	}
	if cfg.EmailProvider != "ses" {
		t.Fatalf("got email provider %q, want ses", cfg.EmailProvider)
	}
	if cfg.StorageProvider != "r2" {
		t.Fatalf("got storage provider %q, want r2", cfg.StorageProvider)
	}
	if cfg.SES.Region != "us-east-1" || cfg.R2.Bucket != "dummy-bucket" {
		t.Fatalf("provider fields not parsed: %+v %+v", cfg.SES, cfg.R2)
	}
}
