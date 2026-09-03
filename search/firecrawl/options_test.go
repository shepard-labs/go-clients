package firecrawl

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"go.uber.org/zap"

	search "github.com/shepard-labs/go-clients/search"
)

func optBool(b bool) *bool       { return &b }
func optInt(i int) *int          { return &i }
func optString(s string) *string { return &s }

func wireKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestNewReturnsConcreteClient(t *testing.T) {
	fc, ok := New("k", zap.NewNop()).(*Client)
	if !ok || fc == nil {
		t.Fatalf("expected New to return *Client, got %T", New("k", zap.NewNop()))
	}
}

func TestScrapeNilOptionsParity(t *testing.T) {
	for _, req := range []*search.ScrapeRequest{
		{URL: "https://example.com"},
		{URL: "https://example.com", Formats: []string{"markdown", "html"}},
	} {
		var bodies []map[string]any
		c, calls := stubClient(rtFunc(func(r *http.Request) (*http.Response, error) {
			bodies = append(bodies, decodeBody(t, r))
			return resp(http.StatusOK, `{"success":true,"data":{"url":"https://example.com","markdown":"m"}}`, nil), nil
		}))

		ctx := context.Background()
		if _, err := c.Scrape(ctx, req); err != nil {
			t.Fatalf("Scrape failed: %v", err)
		}
		if _, err := c.ScrapeWithOptions(ctx, req, nil); err != nil {
			t.Fatalf("ScrapeWithOptions(nil) failed: %v", err)
		}
		if calls.Load() != 2 {
			t.Fatalf("expected 2 HTTP calls, got %d", calls.Load())
		}
		if len(bodies) != 2 {
			t.Fatalf("expected 2 captured bodies, got %d", len(bodies))
		}
		if got, want := wireKeys(bodies[0]), wireKeys(bodies[1]); !reflect.DeepEqual(got, want) {
			t.Fatalf("key-set mismatch: Scrape=%v ScrapeWithOptions(nil)=%v", got, want)
		}
		if !reflect.DeepEqual(bodies[0], bodies[1]) {
			t.Fatalf("body mismatch: %#v vs %#v", bodies[0], bodies[1])
		}
	}
}

func TestScrapeWithOptionsWireKeys(t *testing.T) {
	var body map[string]any
	var gotHeaders http.Header
	c, _ := stubClient(rtFunc(func(r *http.Request) (*http.Response, error) {
		body = decodeBody(t, r)
		gotHeaders = r.Header
		return resp(http.StatusOK, `{"success":true,"data":{"url":"https://example.com","markdown":"m"}}`, nil), nil
	}))

	opts := &ScrapeOptions{
		OnlyMainContent:     optBool(true),
		Timeout:             optInt(5000),
		WaitFor:             optInt(500),
		Mobile:              optBool(true),
		SkipTLSVerification: optBool(true),
		RemoveBase64Images:  optBool(true),
		BlockAds:            optBool(true),
		Proxy:               optString("stealth"),
		StoreInCache:        optBool(true),
		MaxAge:              optInt(3600000),
		Headers:             map[string]string{"X-Custom": "v"},
		IncludeTags:         []string{"article"},
		ExcludeTags:         []string{"aside"},
		Location:            &ScrapeLocation{Country: "us", Languages: []string{"en"}},
		ZeroDataRetention:   optBool(true),
		RedactPII:           optBool(false),
	}
	req := &search.ScrapeRequest{URL: "https://example.com", Formats: []string{"markdown"}}
	if _, err := c.ScrapeWithOptions(context.Background(), req, opts); err != nil {
		t.Fatalf("ScrapeWithOptions failed: %v", err)
	}

	if body["onlyMainContent"] != true {
		t.Fatalf("onlyMainContent = %v, want true", body["onlyMainContent"])
	}
	if body["timeout"] != float64(5000) {
		t.Fatalf("timeout = %v, want 5000", body["timeout"])
	}
	if body["waitFor"] != float64(500) {
		t.Fatalf("waitFor = %v, want 500", body["waitFor"])
	}
	if body["mobile"] != true {
		t.Fatalf("mobile = %v, want true", body["mobile"])
	}
	if body["skipTlsVerification"] != true {
		t.Fatalf("skipTlsVerification = %v, want true", body["skipTlsVerification"])
	}
	if body["removeBase64Images"] != true {
		t.Fatalf("removeBase64Images = %v, want true", body["removeBase64Images"])
	}
	if body["blockAds"] != true {
		t.Fatalf("blockAds = %v, want true", body["blockAds"])
	}
	if body["proxy"] != "stealth" {
		t.Fatalf("proxy = %v, want stealth", body["proxy"])
	}
	if body["storeInCache"] != true {
		t.Fatalf("storeInCache = %v, want true", body["storeInCache"])
	}
	if body["maxAge"] != float64(3600000) {
		t.Fatalf("maxAge = %v, want 3600000", body["maxAge"])
	}
	hdrs, ok := body["headers"].(map[string]any)
	if !ok || hdrs["X-Custom"] != "v" {
		t.Fatalf("headers = %v, want {X-Custom:v}", body["headers"])
	}
	loc, ok := body["location"].(map[string]any)
	if !ok || loc["country"] != "US" {
		t.Fatalf("location = %v, want country US", body["location"])
	}
	langs, ok := loc["languages"].([]any)
	if !ok || len(langs) != 1 || langs[0] != "en" {
		t.Fatalf("location.languages = %v, want [en]", loc["languages"])
	}
	if body["zeroDataRetention"] != true {
		t.Fatalf("zeroDataRetention = %v, want true", body["zeroDataRetention"])
	}
	if body["redactPII"] != false {
		t.Fatalf("redactPII = %v, want false", body["redactPII"])
	}
	if body["url"] != "https://example.com" {
		t.Fatalf("url = %v, want https://example.com", body["url"])
	}

	// Headers travel as HTTP headers too, without overriding the credential.
	if gotHeaders.Get("X-Custom") != "v" {
		t.Fatalf("HTTP X-Custom = %q, want v", gotHeaders.Get("X-Custom"))
	}
	if gotHeaders.Get("Authorization") != "Bearer k" {
		t.Fatalf("HTTP Authorization = %q, want Bearer k", gotHeaders.Get("Authorization"))
	}
}

func TestScrapeWithOptionsOmitsUnset(t *testing.T) {
	var bodies []map[string]any
	c, _ := stubClient(rtFunc(func(r *http.Request) (*http.Response, error) {
		bodies = append(bodies, decodeBody(t, r))
		return resp(http.StatusOK, `{"success":true,"data":{"url":"https://example.com","markdown":"m"}}`, nil), nil
	}))

	ctx := context.Background()
	req := &search.ScrapeRequest{URL: "https://example.com"}
	for _, opts := range []*ScrapeOptions{nil, {}, {Location: &ScrapeLocation{}}} {
		if _, err := c.ScrapeWithOptions(ctx, req, opts); err != nil {
			t.Fatalf("ScrapeWithOptions(%+v) failed: %v", opts, err)
		}
	}
	optional := []string{
		"onlyMainContent", "timeout", "waitFor", "mobile", "skipTlsVerification",
		"removeBase64Images", "blockAds", "proxy", "storeInCache", "maxAge",
		"headers", "includeTags", "excludeTags", "location", "zeroDataRetention", "redactPII",
	}
	for i, body := range bodies {
		if got, want := wireKeys(body), []string{"url"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("call %d keys = %v, want %v", i, got, want)
		}
		for _, k := range optional {
			if _, present := body[k]; present {
				t.Fatalf("call %d: key %q should be omitted", i, k)
			}
		}
	}
}

func TestScrapeWithOptionsExplicitZeroWaitFor(t *testing.T) {
	var body map[string]any
	c, _ := stubClient(rtFunc(func(r *http.Request) (*http.Response, error) {
		body = decodeBody(t, r)
		return resp(http.StatusOK, `{"success":true,"data":{"url":"https://example.com","markdown":"m"}}`, nil), nil
	}))
	opts := &ScrapeOptions{WaitFor: optInt(0)}
	if _, err := c.ScrapeWithOptions(context.Background(), &search.ScrapeRequest{URL: "https://example.com"}, opts); err != nil {
		t.Fatalf("ScrapeWithOptions failed: %v", err)
	}
	v, present := body["waitFor"]
	if !present {
		t.Fatalf("waitFor should be sent for explicit zero, body=%v", body)
	}
	if v != float64(0) {
		t.Fatalf("waitFor = %v, want 0", v)
	}
}

func TestScrapeWithOptionsValidation(t *testing.T) {
	big := strings.Repeat("v", 8*1024)
	tests := []struct {
		name string
		opts *ScrapeOptions
	}{
		{"bad proxy", &ScrapeOptions{Proxy: optString("turbo")}},
		{"empty proxy", &ScrapeOptions{Proxy: optString("")}},
		{"maxAge without cache", &ScrapeOptions{MaxAge: optInt(100)}},
		{"maxAge with cache false", &ScrapeOptions{StoreInCache: optBool(false), MaxAge: optInt(100)}},
		{"negative maxAge", &ScrapeOptions{StoreInCache: optBool(true), MaxAge: optInt(-1)}},
		{"denied authorization header", &ScrapeOptions{Headers: map[string]string{"Authorization": "x"}}},
		{"denied header lowercase", &ScrapeOptions{Headers: map[string]string{"authorization": "x"}}},
		{"oversize headers", &ScrapeOptions{Headers: map[string]string{"X-Big": big}}},
		{"bad country", &ScrapeOptions{Location: &ScrapeLocation{Country: "USA"}}},
		{"languages without country", &ScrapeOptions{Location: &ScrapeLocation{Languages: []string{"en"}}}},
		{"blank include tag", &ScrapeOptions{IncludeTags: []string{""}}},
		{"blank exclude tag", &ScrapeOptions{ExcludeTags: []string{"  "}}},
		{"timeout zero", &ScrapeOptions{Timeout: optInt(0)}},
		{"timeout too large", &ScrapeOptions{Timeout: optInt(600001)}},
		{"waitFor negative", &ScrapeOptions{WaitFor: optInt(-1)}},
		{"waitFor too large", &ScrapeOptions{WaitFor: optInt(30001)}},
		{"waitFor exceeds timeout", &ScrapeOptions{Timeout: optInt(100), WaitFor: optInt(5000)}},
		{"waitFor exceeds timeout by one", &ScrapeOptions{Timeout: optInt(1000), WaitFor: optInt(1001)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, calls := stubClient(rtFunc(func(*http.Request) (*http.Response, error) {
				return resp(http.StatusOK, `{}`, nil), nil
			}))
			_, err := c.ScrapeWithOptions(context.Background(), &search.ScrapeRequest{URL: "https://example.com"}, tt.opts)
			if !errors.Is(err, search.ErrInvalidRequest) {
				t.Fatalf("expected ErrInvalidRequest, got %v", err)
			}
			if calls.Load() != 0 {
				t.Fatalf("expected 0 HTTP calls, got %d", calls.Load())
			}
		})
	}
}

func TestScrapeWithOptionsWaitForTimeoutAllowed(t *testing.T) {
	tests := []struct {
		name string
		opts *ScrapeOptions
	}{
		{"waitFor equals timeout", &ScrapeOptions{Timeout: optInt(5000), WaitFor: optInt(5000)}},
		{"waitFor without timeout", &ScrapeOptions{WaitFor: optInt(5000)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, calls := stubClient(rtFunc(func(*http.Request) (*http.Response, error) {
				return resp(http.StatusOK, `{"success":true,"data":{"url":"https://example.com","markdown":"m"}}`, nil), nil
			}))
			if _, err := c.ScrapeWithOptions(context.Background(), &search.ScrapeRequest{URL: "https://example.com"}, tt.opts); err != nil {
				t.Fatalf("ScrapeWithOptions failed: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("expected 1 HTTP call, got %d", calls.Load())
			}
		})
	}
}

func TestScrapeWithOptionsSuccessMapsDocument(t *testing.T) {
	c, calls := stubClient(rtFunc(func(r *http.Request) (*http.Response, error) {
		body := decodeBody(t, r)
		if body["timeout"] != float64(1000) || body["proxy"] != "auto" {
			return nil, errors.New("unexpected options payload")
		}
		return resp(http.StatusOK, `{"success":true,"data":{`+
			`"url":"https://example.com","markdown":"# hi","html":"<h1>hi</h1>",`+
			`"metadata":{"title":"T","keywords":["a","b"]},`+
			`"links":["https://example.com/x"]}}`, nil), nil
	}))

	opts := &ScrapeOptions{Timeout: optInt(1000), Proxy: optString("auto")}
	doc, err := c.ScrapeWithOptions(context.Background(), &search.ScrapeRequest{URL: "https://example.com"}, opts)
	if err != nil {
		t.Fatalf("ScrapeWithOptions failed: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 HTTP call, got %d", calls.Load())
	}
	if doc.URL != "https://example.com" || doc.Markdown != "# hi" || doc.HTML != "<h1>hi</h1>" {
		t.Fatalf("unexpected document: %#v", doc)
	}
	if doc.Metadata["title"] != "T" || doc.Metadata["keywords"] != "a, b" {
		t.Fatalf("unexpected metadata: %#v", doc.Metadata)
	}
	if len(doc.Links) != 1 || doc.Links[0] != "https://example.com/x" {
		t.Fatalf("unexpected links: %#v", doc.Links)
	}
}
