package exa

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"

	"github.com/shepard-labs/go-clients/search"
)

func boolPtr(b bool) *bool { return &b }

func intPtr(n int) *int { return &n }

// captureBodies runs fn twice (or once per client call) capturing the decoded
// JSON body of each request.
func captureBody(t *testing.T, calls *int64, resp string, do func(c *Client) error) map[string]any {
	t.Helper()
	var got map[string]any
	c := newClient(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("unmarshal request body %q: %v", raw, err)
		}
		return jsonResp(http.StatusOK, resp, nil), nil
	}, calls)
	if err := do(c); err != nil {
		t.Fatalf("request: %v", err)
	}
	return got
}

func TestNewReturnsConcreteClient(t *testing.T) {
	got := New("test-key", zap.NewNop())
	c, ok := got.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", got)
	}
	if c == nil {
		t.Fatalf("New returned nil *Client")
	}
}

func TestSearchWithOptionsNilParity(t *testing.T) {
	q := &search.SearchQuery{Query: "golang", NumResults: 3}
	oldBody := captureBody(t, nil, searchFixture, func(c *Client) error {
		_, err := c.Search(context.Background(), q)
		return err
	})
	newBody := captureBody(t, nil, searchFixture, func(c *Client) error {
		_, err := c.SearchWithOptions(context.Background(), q, nil)
		return err
	})
	if !reflect.DeepEqual(oldBody, newBody) {
		t.Fatalf("SearchWithOptions(nil) body %v != Search body %v", newBody, oldBody)
	}
	want := map[string]any{
		"query":      "golang",
		"numResults": float64(3),
		"contents":   map[string]any{"text": true},
	}
	if !reflect.DeepEqual(newBody, want) {
		t.Fatalf("default search body = %v, want %v", newBody, want)
	}
}

func TestScrapeWithOptionsNilParity(t *testing.T) {
	r := &search.ScrapeRequest{URL: "https://a.example/1"}
	scrapeOK := `{"results":[{"url":"https://a.example/1","text":"# hello"}]}`
	oldBody := captureBody(t, nil, scrapeOK, func(c *Client) error {
		_, err := c.Scrape(context.Background(), r)
		return err
	})
	newBody := captureBody(t, nil, scrapeOK, func(c *Client) error {
		_, err := c.ScrapeWithOptions(context.Background(), r, nil)
		return err
	})
	if !reflect.DeepEqual(oldBody, newBody) {
		t.Fatalf("ScrapeWithOptions(nil) body %v != Scrape body %v", newBody, oldBody)
	}
	want := map[string]any{
		"urls": []any{"https://a.example/1"},
		"text": true,
	}
	if !reflect.DeepEqual(newBody, want) {
		t.Fatalf("default scrape body = %v, want %v", newBody, want)
	}
}

func TestSearchWithOptionsPerFieldKeys(t *testing.T) {
	timeout := 1500
	tests := []struct {
		name  string
		opts  *SearchOptions
		check func(t *testing.T, body map[string]any)
	}{
		{
			name: "type",
			opts: &SearchOptions{Type: "neural"},
			check: func(t *testing.T, body map[string]any) {
				if body["type"] != "neural" {
					t.Fatalf("type = %v, want neural", body["type"])
				}
			},
		},
		{
			name: "category",
			opts: &SearchOptions{Category: "news"},
			check: func(t *testing.T, body map[string]any) {
				if body["category"] != "news" {
					t.Fatalf("category = %v, want news", body["category"])
				}
			},
		},
		{
			name: "domains",
			opts: &SearchOptions{IncludeDomains: []string{" Example.COM "}, ExcludeDomains: []string{"other.com"}},
			check: func(t *testing.T, body map[string]any) {
				if !reflect.DeepEqual(body["includeDomains"], []any{"example.com"}) {
					t.Fatalf("includeDomains = %v, want [example.com]", body["includeDomains"])
				}
				if !reflect.DeepEqual(body["excludeDomains"], []any{"other.com"}) {
					t.Fatalf("excludeDomains = %v, want [other.com]", body["excludeDomains"])
				}
			},
		},
		{
			name: "livecrawl",
			opts: &SearchOptions{Livecrawl: "preferred"},
			check: func(t *testing.T, body map[string]any) {
				if body["livecrawl"] != "preferred" {
					t.Fatalf("livecrawl = %v, want preferred", body["livecrawl"])
				}
			},
		},
		{
			name: "livecrawlTimeout",
			opts: &SearchOptions{Livecrawl: "auto", LivecrawlTimeout: &timeout},
			check: func(t *testing.T, body map[string]any) {
				if body["livecrawl"] != "auto" {
					t.Fatalf("livecrawl = %v, want auto", body["livecrawl"])
				}
				if body["livecrawlTimeout"] != float64(1500) {
					t.Fatalf("livecrawlTimeout = %v, want 1500", body["livecrawlTimeout"])
				}
			},
		},
		{
			name: "dates",
			opts: &SearchOptions{StartPublishedDate: "2024-01-01", EndPublishedDate: "2024-12-31"},
			check: func(t *testing.T, body map[string]any) {
				if body["startPublishedDate"] != "2024-01-01" {
					t.Fatalf("startPublishedDate = %v, want 2024-01-01", body["startPublishedDate"])
				}
				if body["endPublishedDate"] != "2024-12-31" {
					t.Fatalf("endPublishedDate = %v, want 2024-12-31", body["endPublishedDate"])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := captureBody(t, nil, searchFixture, func(c *Client) error {
				_, err := c.SearchWithOptions(context.Background(), &search.SearchQuery{Query: "golang", NumResults: 3}, tt.opts)
				return err
			})
			tt.check(t, body)
		})
	}
}

func TestSearchWithOptionsUnsetFieldsOmitted(t *testing.T) {
	body := captureBody(t, nil, searchFixture, func(c *Client) error {
		_, err := c.SearchWithOptions(context.Background(), &search.SearchQuery{Query: "golang", NumResults: 3}, &SearchOptions{})
		return err
	})
	for _, k := range []string{"type", "category", "includeDomains", "excludeDomains", "startPublishedDate", "endPublishedDate", "livecrawl", "livecrawlTimeout"} {
		if _, ok := body[k]; ok {
			t.Fatalf("body[%q] = %v, want it omitted", k, body[k])
		}
	}
}

func TestScrapeWithOptionsPerFieldKeys(t *testing.T) {
	subpages := 3
	timeout := 2000
	tests := []struct {
		name  string
		opts  *ScrapeOptions
		check func(t *testing.T, body map[string]any)
	}{
		{
			name: "text false",
			opts: &ScrapeOptions{Text: boolPtr(false)},
			check: func(t *testing.T, body map[string]any) {
				if body["text"] != false {
					t.Fatalf("text = %v, want false", body["text"])
				}
			},
		},
		{
			name: "livecrawl",
			opts: &ScrapeOptions{Livecrawl: "always", LivecrawlTimeout: &timeout},
			check: func(t *testing.T, body map[string]any) {
				if body["livecrawl"] != "always" {
					t.Fatalf("livecrawl = %v, want always", body["livecrawl"])
				}
				if body["livecrawlTimeout"] != float64(2000) {
					t.Fatalf("livecrawlTimeout = %v, want 2000", body["livecrawlTimeout"])
				}
			},
		},
		{
			name: "subpages",
			opts: &ScrapeOptions{Subpages: &subpages},
			check: func(t *testing.T, body map[string]any) {
				if body["subpages"] != float64(3) {
					t.Fatalf("subpages = %v, want 3", body["subpages"])
				}
			},
		},
		{
			name: "subpageTarget",
			opts: &ScrapeOptions{Subpages: &subpages, SubpageTarget: "pricing"},
			check: func(t *testing.T, body map[string]any) {
				if body["subpages"] != float64(3) {
					t.Fatalf("subpages = %v, want 3", body["subpages"])
				}
				if body["subpageTarget"] != "pricing" {
					t.Fatalf("subpageTarget = %v, want pricing", body["subpageTarget"])
				}
			},
		},
	}

	scrapeOK := `{"results":[{"url":"https://a.example/1","text":"# hello"}]}`
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := captureBody(t, nil, scrapeOK, func(c *Client) error {
				_, err := c.ScrapeWithOptions(context.Background(), &search.ScrapeRequest{URL: "https://a.example/1"}, tt.opts)
				return err
			})
			tt.check(t, body)
		})
	}
}

func TestScrapeWithOptionsUnsetFieldsOmitted(t *testing.T) {
	scrapeOK := `{"results":[{"url":"https://a.example/1","text":"# hello"}]}`
	body := captureBody(t, nil, scrapeOK, func(c *Client) error {
		_, err := c.ScrapeWithOptions(context.Background(), &search.ScrapeRequest{URL: "https://a.example/1"}, &ScrapeOptions{})
		return err
	})
	for _, k := range []string{"livecrawl", "livecrawlTimeout", "subpages", "subpageTarget"} {
		if _, ok := body[k]; ok {
			t.Fatalf("body[%q] = %v, want it omitted", k, body[k])
		}
	}
	if body["text"] != true {
		t.Fatalf("text = %v, want true (nil Text defaults to true)", body["text"])
	}
}

func TestSearchWithOptionsValidation(t *testing.T) {
	timeout := 1000
	tests := []struct {
		name string
		opts *SearchOptions
	}{
		{name: "bad type", opts: &SearchOptions{Type: "hybrid"}},
		{name: "bad livecrawl", opts: &SearchOptions{Livecrawl: "sometimes"}},
		{name: "timeout without mode", opts: &SearchOptions{LivecrawlTimeout: &timeout}},
		{name: "timeout below range", opts: &SearchOptions{Livecrawl: "auto", LivecrawlTimeout: intPtr(-1)}},
		{name: "timeout above range", opts: &SearchOptions{Livecrawl: "auto", LivecrawlTimeout: intPtr(30001)}},
		{name: "malformed start date", opts: &SearchOptions{StartPublishedDate: "01-02-2024"}},
		{name: "malformed end date", opts: &SearchOptions{EndPublishedDate: "2024/12/31"}},
		{name: "start after end", opts: &SearchOptions{StartPublishedDate: "2024-12-31", EndPublishedDate: "2024-01-01"}},
		{name: "domain with scheme", opts: &SearchOptions{IncludeDomains: []string{"https://example.com"}}},
		{name: "domain with port", opts: &SearchOptions{IncludeDomains: []string{"example.com:8080"}}},
		{name: "domain with userinfo", opts: &SearchOptions{IncludeDomains: []string{"user@example.com"}}},
		{name: "domain with path", opts: &SearchOptions{ExcludeDomains: []string{"example.com/docs"}}},
		{name: "empty domain", opts: &SearchOptions{IncludeDomains: []string{"   "}}},
		{name: "unicode domain", opts: &SearchOptions{IncludeDomains: []string{"exämple.com"}}},
		{name: "overlap", opts: &SearchOptions{IncludeDomains: []string{"Example.COM"}, ExcludeDomains: []string{"example.com"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls int64
			c := newClient(func(r *http.Request) (*http.Response, error) {
				return jsonResp(http.StatusOK, searchFixture, nil), nil
			}, &calls)
			if _, err := c.SearchWithOptions(context.Background(), &search.SearchQuery{Query: "golang", NumResults: 3}, tt.opts); !errors.Is(err, search.ErrInvalidQuery) {
				t.Fatalf("expected ErrInvalidQuery, got %v", err)
			}
			if got := atomic.LoadInt64(&calls); got != 0 {
				t.Fatalf("calls = %d, want 0", got)
			}
		})
	}
}

func TestScrapeWithOptionsValidation(t *testing.T) {
	timeout := 1000
	tests := []struct {
		name string
		opts *ScrapeOptions
	}{
		{name: "bad livecrawl", opts: &ScrapeOptions{Livecrawl: "sometimes"}},
		{name: "timeout without mode", opts: &ScrapeOptions{LivecrawlTimeout: &timeout}},
		{name: "timeout below range", opts: &ScrapeOptions{Livecrawl: "auto", LivecrawlTimeout: intPtr(-1)}},
		{name: "timeout above range", opts: &ScrapeOptions{Livecrawl: "auto", LivecrawlTimeout: intPtr(30001)}},
		{name: "subpages zero", opts: &ScrapeOptions{Subpages: intPtr(0)}},
		{name: "subpages above max", opts: &ScrapeOptions{Subpages: intPtr(6)}},
		{name: "subpageTarget without subpages", opts: &ScrapeOptions{SubpageTarget: "pricing"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls int64
			c := newClient(func(r *http.Request) (*http.Response, error) {
				return jsonResp(http.StatusOK, `{"results":[{"url":"https://a.example/1","text":"x"}]}`, nil), nil
			}, &calls)
			if _, err := c.ScrapeWithOptions(context.Background(), &search.ScrapeRequest{URL: "https://a.example/1"}, tt.opts); !errors.Is(err, search.ErrInvalidRequest) {
				t.Fatalf("expected ErrInvalidRequest, got %v", err)
			}
			if got := atomic.LoadInt64(&calls); got != 0 {
				t.Fatalf("calls = %d, want 0", got)
			}
		})
	}
}

// The Answer path takes no options struct, so there is no Answer-path options
// validation to cover here (blank-query rejection is covered in exa_test.go).

func TestSearchWithOptionsBlankCategoryOmitted(t *testing.T) {
	var calls int64
	body := captureBody(t, &calls, searchFixture, func(c *Client) error {
		_, err := c.SearchWithOptions(context.Background(), &search.SearchQuery{Query: "golang", NumResults: 3}, &SearchOptions{Category: "  "})
		return err
	})
	if _, ok := body["category"]; ok {
		t.Fatalf("body[%q] = %v, want it omitted", "category", body["category"])
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("calls = %d, want 1 (blank category must not fail validation)", got)
	}

	// Non-blank values still sent.
	body = captureBody(t, nil, searchFixture, func(c *Client) error {
		_, err := c.SearchWithOptions(context.Background(), &search.SearchQuery{Query: "golang", NumResults: 3}, &SearchOptions{Category: "news"})
		return err
	})
	if body["category"] != "news" {
		t.Fatalf("category = %v, want news", body["category"])
	}
}

func TestSearchWithOptionsCategoryPaddedTrimmed(t *testing.T) {
	body := captureBody(t, nil, searchFixture, func(c *Client) error {
		_, err := c.SearchWithOptions(context.Background(), &search.SearchQuery{Query: "golang", NumResults: 3}, &SearchOptions{Category: "  news  "})
		return err
	})
	if body["category"] != "news" {
		t.Fatalf("category = %v, want news", body["category"])
	}
}

func TestScrapeWithOptionsBlankSubpageTargetOmitted(t *testing.T) {
	scrapeOK := `{"results":[{"url":"https://a.example/1","text":"# hello"}]}`
	subpages := 2
	for _, opts := range []*ScrapeOptions{
		{SubpageTarget: "   "},
		{Subpages: &subpages, SubpageTarget: "   "},
	} {
		var calls int64
		body := captureBody(t, &calls, scrapeOK, func(c *Client) error {
			_, err := c.ScrapeWithOptions(context.Background(), &search.ScrapeRequest{URL: "https://a.example/1"}, opts)
			return err
		})
		if _, ok := body["subpageTarget"]; ok {
			t.Fatalf("body[%q] = %v, want it omitted for opts %+v", "subpageTarget", body["subpageTarget"], opts)
		}
		if got := atomic.LoadInt64(&calls); got != 1 {
			t.Fatalf("calls = %d, want 1 (blank subpageTarget must not fail validation)", got)
		}
	}

	// Non-blank values still sent.
	body := captureBody(t, nil, scrapeOK, func(c *Client) error {
		_, err := c.ScrapeWithOptions(context.Background(), &search.ScrapeRequest{URL: "https://a.example/1"}, &ScrapeOptions{Subpages: &subpages, SubpageTarget: "pricing"})
		return err
	})
	if body["subpageTarget"] != "pricing" {
		t.Fatalf("subpageTarget = %v, want pricing", body["subpageTarget"])
	}
}

func TestScrapeWithOptionsSubpageTargetPaddedTrimmed(t *testing.T) {
	scrapeOK := `{"results":[{"url":"https://a.example/1","text":"# hello"}]}`
	subpages := 2
	body := captureBody(t, nil, scrapeOK, func(c *Client) error {
		_, err := c.ScrapeWithOptions(context.Background(), &search.ScrapeRequest{URL: "https://a.example/1"}, &ScrapeOptions{Subpages: &subpages, SubpageTarget: "  pricing  "})
		return err
	})
	if body["subpageTarget"] != "pricing" {
		t.Fatalf("subpageTarget = %v, want pricing", body["subpageTarget"])
	}
}

func TestScrapeWithOptionsTextTriState(t *testing.T) {
	scrapeOK := `{"results":[{"url":"https://a.example/1","text":"# hello"}]}`
	tests := []struct {
		name string
		opts *ScrapeOptions
		want any
	}{
		{name: "nil sends true", opts: nil, want: true},
		{name: "false sends false", opts: &ScrapeOptions{Text: boolPtr(false)}, want: false},
		{name: "true sends true", opts: &ScrapeOptions{Text: boolPtr(true)}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := captureBody(t, nil, scrapeOK, func(c *Client) error {
				_, err := c.ScrapeWithOptions(context.Background(), &search.ScrapeRequest{URL: "https://a.example/1"}, tt.opts)
				return err
			})
			if body["text"] != tt.want {
				t.Fatalf("text = %v, want %v", body["text"], tt.want)
			}
		})
	}
}
