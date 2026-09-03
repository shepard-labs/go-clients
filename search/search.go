// Package search defines provider-agnostic interfaces and request types for
// web search, page scrape, and site crawl operations.
//
// Provider implementations live in subpackages (firecrawl, exa, crawl4ai).
// Credentials are bound at construction time by each provider client.
//
// Provider coverage matrix:
//   - Firecrawl: Search, Scrape, and Crawl are all supported.
//   - Exa: Search and Scrape are supported; Crawl is not supported.
//   - Crawl4AI: Scrape and Crawl are supported; Search is not supported.
//
// Unsupported operations return ErrNotSupported. This package has no third-party
// SDK dependencies.
package search

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// MaxResponseBytes caps the size of a single provider HTTP response body.
const MaxResponseBytes int64 = 10 << 20

var (
	ErrNotSupported     = errors.New("search: operation not supported by provider")
	ErrInvalidQuery     = errors.New("search: invalid query")
	ErrInvalidRequest   = errors.New("search: invalid request")
	ErrResponseTooLarge = errors.New("search: response too large")
)

// SearchQuery is a web search request.
type SearchQuery struct {
	Query      string
	NumResults int
}

// Validate reports whether the query is well-formed.
func (q *SearchQuery) Validate() error {
	switch {
	case q == nil:
		return fmt.Errorf("%w: query is nil", ErrInvalidQuery)
	case strings.TrimSpace(q.Query) == "":
		return fmt.Errorf("%w: Query is required", ErrInvalidQuery)
	case q.NumResults < 1 || q.NumResults > 100:
		return fmt.Errorf("%w: NumResults must be 1..100", ErrInvalidQuery)
	default:
		return nil
	}
}

// SearchResult is a single search hit.
type SearchResult struct {
	URL         string
	Title       string
	Snippet     string
	Score       float64
	PublishedAt string
}

// SearchPage is a page of search results.
type SearchPage struct {
	Results []SearchResult
}

// ScrapeRequest is a single-page scrape request.
type ScrapeRequest struct {
	URL     string
	Formats []string
}

// Validate reports whether the scrape request is well-formed.
func (r *ScrapeRequest) Validate() error {
	switch {
	case r == nil:
		return fmt.Errorf("%w: request is nil", ErrInvalidRequest)
	case !validHTTPURL(r.URL):
		return fmt.Errorf("%w: invalid URL", ErrInvalidRequest)
	default:
		for _, f := range r.Formats {
			switch f {
			case "markdown", "html", "links", "screenshot":
			default:
				return fmt.Errorf("%w: unsupported format %q", ErrInvalidRequest, f)
			}
		}
		return nil
	}
}

// Document is a scraped page.
type Document struct {
	URL      string
	Markdown string
	HTML     string
	Metadata map[string]string
	Links    []string
}

// CrawlRequest is a site crawl request.
type CrawlRequest struct {
	StartURL string
	MaxPages int
}

// Validate reports whether the crawl request is well-formed.
func (r *CrawlRequest) Validate() error {
	switch {
	case r == nil:
		return fmt.Errorf("%w: request is nil", ErrInvalidRequest)
	case !validHTTPURL(r.StartURL):
		return fmt.Errorf("%w: invalid StartURL", ErrInvalidRequest)
	case r.MaxPages < 1 || r.MaxPages > 1000:
		return fmt.Errorf("%w: MaxPages must be 1..1000", ErrInvalidRequest)
	default:
		return nil
	}
}

// CrawlPage is a set of crawled documents.
type CrawlPage struct {
	Documents []Document
}

// Client is a provider-agnostic search/scrape/crawl client.
type Client interface {
	// Search runs a web search query.
	Search(ctx context.Context, q *SearchQuery) (*SearchPage, error)
	// Scrape scrapes a single page.
	Scrape(ctx context.Context, r *ScrapeRequest) (*Document, error)
	// Crawl crawls a site starting from StartURL.
	Crawl(ctx context.Context, r *CrawlRequest) (*CrawlPage, error)
	// Close releases any resources held by the client.
	Close() error
}

// validHTTPURL reports whether raw is an absolute http(s) URL with a host and no userinfo.
func validHTTPURL(raw string) bool {
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.Host == "" {
		return false
	}
	if u.User != nil {
		return false
	}
	return true
}
