package search

import (
	"errors"
	"fmt"
	"testing"
)

func TestSearchQueryValidate(t *testing.T) {
	tests := []struct {
		name    string
		query   *SearchQuery
		wantErr bool
	}{
		{"valid", &SearchQuery{Query: "golang", NumResults: 10}, false},
		{"valid min results", &SearchQuery{Query: "golang", NumResults: 1}, false},
		{"valid max results", &SearchQuery{Query: "golang", NumResults: 100}, false},
		{"nil", nil, true},
		{"empty query", &SearchQuery{Query: "", NumResults: 10}, true},
		{"whitespace only", &SearchQuery{Query: "   ", NumResults: 10}, true},
		{"zero results", &SearchQuery{Query: "golang", NumResults: 0}, true},
		{"negative results", &SearchQuery{Query: "golang", NumResults: -1}, true},
		{"too many results", &SearchQuery{Query: "golang", NumResults: 101}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.query.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !errors.Is(err, ErrInvalidQuery) {
					t.Fatalf("expected ErrInvalidQuery, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestScrapeRequestValidate(t *testing.T) {
	tests := []struct {
		name    string
		req     *ScrapeRequest
		wantErr bool
	}{
		{"valid http", &ScrapeRequest{URL: "http://example.com"}, false},
		{"valid https", &ScrapeRequest{URL: "https://example.com/page"}, false},
		{"empty formats ok", &ScrapeRequest{URL: "https://example.com", Formats: []string{}}, false},
		{"nil formats ok", &ScrapeRequest{URL: "https://example.com", Formats: nil}, false},
		{"markdown", &ScrapeRequest{URL: "https://example.com", Formats: []string{"markdown"}}, false},
		{"html", &ScrapeRequest{URL: "https://example.com", Formats: []string{"html"}}, false},
		{"links", &ScrapeRequest{URL: "https://example.com", Formats: []string{"links"}}, false},
		{"screenshot", &ScrapeRequest{URL: "https://example.com", Formats: []string{"screenshot"}}, false},
		{"all known", &ScrapeRequest{URL: "https://example.com", Formats: []string{"markdown", "html", "links", "screenshot"}}, false},
		{"nil", nil, true},
		{"empty url", &ScrapeRequest{URL: ""}, true},
		{"relative path", &ScrapeRequest{URL: "/relative/path"}, true},
		{"ftp scheme", &ScrapeRequest{URL: "ftp://example.com/file"}, true},
		{"userinfo", &ScrapeRequest{URL: "https://user@example.com"}, true},
		{"unknown format", &ScrapeRequest{URL: "https://example.com", Formats: []string{"pdf"}}, true},
		{"mixed known unknown", &ScrapeRequest{URL: "https://example.com", Formats: []string{"markdown", "pdf"}}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("expected ErrInvalidRequest, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCrawlRequestValidate(t *testing.T) {
	tests := []struct {
		name    string
		req     *CrawlRequest
		wantErr bool
	}{
		{"valid", &CrawlRequest{StartURL: "https://example.com", MaxPages: 10}, false},
		{"min pages", &CrawlRequest{StartURL: "https://example.com", MaxPages: 1}, false},
		{"max pages", &CrawlRequest{StartURL: "https://example.com", MaxPages: 1000}, false},
		{"nil", nil, true},
		{"bad url", &CrawlRequest{StartURL: "not-a-url", MaxPages: 10}, true},
		{"empty url", &CrawlRequest{StartURL: "", MaxPages: 10}, true},
		{"zero pages", &CrawlRequest{StartURL: "https://example.com", MaxPages: 0}, true},
		{"too many pages", &CrawlRequest{StartURL: "https://example.com", MaxPages: 1001}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("expected ErrInvalidRequest, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestSentinels(t *testing.T) {
	sentinels := []struct {
		name string
		err  error
	}{
		{"not supported", ErrNotSupported},
		{"invalid query", ErrInvalidQuery},
		{"invalid request", ErrInvalidRequest},
		{"response too large", ErrResponseTooLarge},
	}

	for _, tt := range sentinels {
		t.Run(tt.name, func(t *testing.T) {
			if !errors.Is(tt.err, tt.err) {
				t.Fatalf("expected %v to match itself", tt.err)
			}
			wrapped := fmt.Errorf("op failed: %w", tt.err)
			if !errors.Is(wrapped, tt.err) {
				t.Fatalf("expected wrapped %v to match %v", wrapped, tt.err)
			}
		})
	}
}
