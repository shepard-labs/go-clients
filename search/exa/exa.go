// Package exa implements the search.Client interface backed by the Exa API.
//
// Wire reconciliation (exa-js npm 2.19.0, exa-labs/exa-js src/index.ts,
// inspected live — no fallback shapes used):
//   - Base URL defaults to https://api.exa.ai; every request authenticates
//     with the `x-api-key: <key>` header.
//   - Search is POST /search with {"query":q,"numResults":n,
//     "contents":{"text":true}} and returns
//     {"results":[{"url","title","id","score","publishedDate","author",
//     "text","summary","highlights",...}]}. SearchResult.title is nullable
//     (null unmarshals to ""). Snippet prefers the per-result "text" field,
//     then "summary", then the joined "highlights".
//   - Scrape is POST /contents with {"urls":[url],"text":true} — note the key
//     is "urls", not "ids" — returning the same SearchResponse envelope; the
//     first result's "text" becomes Document.Markdown.
//   - Answer is POST /answer with {"query":q,"stream":false,"model":"exa"}
//     returning {"answer":...,"citations":[...]}; answer is a plain string
//     unless a structured output schema was used, in which case it is
//     returned as compact JSON.
//
// Crawl is not supported by Exa: Crawl returns search.ErrNotSupported
// immediately without validating its argument, so callers can probe for
// support.
package exa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/shepard-labs/go-clients/search"
)

const defaultBaseURL = "https://api.exa.ai"

// Ensure *Client satisfies the shared interface.
var _ search.Client = (*Client)(nil)

// Client implements search.Client backed by the Exa API. The API key is bound
// at construction. It is immutable after New and safe for concurrent use.
type Client struct {
	httpClient *http.Client
	logger     *zap.Logger
	apiKey     string
	baseURL    string
	sleeper    func(context.Context, time.Duration) bool // defaults to sleepCtx; injectable in tests
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Option customizes a Client built by New.
type Option func(*Client)

// WithHTTPClient sets the HTTP client. A client without a timeout gets a 30s
// default (applied to a copy, leaving the caller's client untouched).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h == nil {
			return
		}
		if h.Timeout == 0 {
			cp := *h
			cp.Timeout = 30 * time.Second
			c.httpClient = &cp
			return
		}
		c.httpClient = h
	}
}

// WithBaseURL overrides the API base URL. A trailing "/" is trimmed; a blank
// value is ignored.
func WithBaseURL(u string) Option {
	return func(c *Client) {
		if strings.TrimSpace(u) == "" {
			return
		}
		c.baseURL = strings.TrimRight(u, "/")
	}
}

// New constructs an Exa-backed search.Client bound to apiKey. A nil logger
// becomes zap.NewNop(). An empty apiKey is not rejected here; it fails
// descriptively at call time before any I/O is performed.
func New(apiKey string, logger *zap.Logger, opts ...Option) search.Client {
	if logger == nil {
		logger = zap.NewNop()
	}
	c := &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		logger:     logger,
		apiKey:     apiKey,
		baseURL:    defaultBaseURL,
		sleeper:    sleepCtx,
	}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt(c)
	}
	if c.httpClient == nil {
		c.httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if c.logger == nil {
		c.logger = zap.NewNop()
	}
	if c.baseURL == "" {
		c.baseURL = defaultBaseURL
	}
	if c.sleeper == nil {
		c.sleeper = sleepCtx
	}
	return c
}

// APIError is a non-2xx response from the Exa API. StatusCode always comes
// from the HTTP response, never from a body field.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Code
	}
	if msg != "" {
		return fmt.Sprintf("exa: request failed with status %d: %s", e.StatusCode, msg)
	}
	return fmt.Sprintf("exa: request failed with status %d", e.StatusCode)
}

// IsUnauthorized reports whether the API key is invalid (HTTP 401).
func (e APIError) IsUnauthorized() bool { return e.StatusCode == http.StatusUnauthorized }

// IsForbidden reports whether the request was forbidden (HTTP 403).
func (e APIError) IsForbidden() bool { return e.StatusCode == http.StatusForbidden }

// IsRateLimited reports whether the request was rate-limited (HTTP 429).
func (e APIError) IsRateLimited() bool { return e.StatusCode == http.StatusTooManyRequests }

// parseAPIError builds an *APIError from a non-2xx response. StatusCode is
// always the HTTP status; on unmarshal failure (or an empty JSON error body)
// it falls back to the raw body snippet, capped at 512 chars.
func parseAPIError(statusCode int, body []byte) *APIError {
	var wire struct {
		Code    string `json:"code"`
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &wire); err == nil {
		msg := wire.Message
		if msg == "" {
			msg = wire.Error
		}
		if msg != "" || wire.Code != "" {
			return &APIError{StatusCode: statusCode, Code: wire.Code, Message: msg}
		}
	}
	return &APIError{StatusCode: statusCode, Message: truncateSnippet(body)}
}

// truncateSnippet collapses whitespace and caps a raw body at 512 chars.
func truncateSnippet(body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	if len(s) > 512 {
		s = s[:512]
	}
	return s
}

// retryAfterDelay honors the Retry-After header (delta-seconds or HTTP date),
// falling back to 500ms*attempt.
func retryAfterDelay(header string, attempt int) time.Duration {
	fallback := time.Duration(attempt) * 500 * time.Millisecond
	h := strings.TrimSpace(header)
	if h == "" {
		return fallback
	}
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return fallback
}

// readBounded drains rc (always closing it) while enforcing the shared
// search.MaxResponseBytes cap.
func readBounded(rc io.ReadCloser) ([]byte, error) {
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, search.MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("exa: read response: %w", err)
	}
	if int64(len(b)) > search.MaxResponseBytes {
		return nil, fmt.Errorf("exa: response body exceeds limit: %w", search.ErrResponseTooLarge)
	}
	return b, nil
}

// doRequest performs an HTTP request with retry logic against the Exa API.
// The body is marshaled once by the caller; a fresh reader is built per
// attempt so retries resend the full body. Transport errors back off
// 200ms*attempt, 5xx backs off 500ms*attempt, and 429 honors Retry-After
// (fallback 500ms*attempt). Other 4xx responses (including 402 billing)
// return an *APIError immediately and are never retried.
func (c *Client) doRequest(ctx context.Context, method, path string, bodyBytes []byte) ([]byte, error) {
	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var reqBody io.Reader
		if bodyBytes != nil {
			reqBody = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
		if err != nil {
			return nil, fmt.Errorf("exa: create request: %w", err)
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", c.apiKey)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("exa: request failed: %w", err)
			if attempt == maxAttempts {
				break
			}
			if !c.sleeper(ctx, time.Duration(attempt)*200*time.Millisecond) {
				return nil, ctx.Err()
			}
			continue
		}

		respBody, err := readBounded(resp.Body)
		if err != nil {
			if errors.Is(err, search.ErrResponseTooLarge) {
				c.logger.Error("exa: response too large", zap.String("endpoint", path))
				return nil, err
			}
			lastErr = err
			if attempt == maxAttempts {
				break
			}
			if !c.sleeper(ctx, time.Duration(attempt)*200*time.Millisecond) {
				return nil, ctx.Err()
			}
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			lastErr = parseAPIError(resp.StatusCode, respBody)
			if attempt == maxAttempts {
				break
			}
			if !c.sleeper(ctx, retryAfterDelay(resp.Header.Get("Retry-After"), attempt)) {
				return nil, ctx.Err()
			}
			continue
		}
		if resp.StatusCode >= 500 {
			lastErr = parseAPIError(resp.StatusCode, respBody)
			if attempt == maxAttempts {
				break
			}
			if !c.sleeper(ctx, time.Duration(attempt)*500*time.Millisecond) {
				return nil, ctx.Err()
			}
			continue
		}
		if resp.StatusCode >= 400 {
			apiErr := parseAPIError(resp.StatusCode, respBody)
			c.logger.Error("exa: request failed", zap.String("endpoint", path), zap.Int("status", resp.StatusCode))
			return nil, apiErr
		}
		if resp.StatusCode >= 300 {
			c.logger.Error("exa: unexpected status", zap.String("endpoint", path), zap.Int("status", resp.StatusCode))
			return nil, fmt.Errorf("exa: unexpected status %d", resp.StatusCode)
		}
		return respBody, nil
	}
	c.logger.Error("exa: request failed after retries", zap.String("endpoint", path))
	return nil, lastErr
}

// checkAPIKey fails descriptively when no key was bound, before any I/O.
func (c *Client) checkAPIKey() error {
	if c.apiKey == "" {
		return fmt.Errorf("exa: api key is empty")
	}
	return nil
}

// exaResult is one entry of the SearchResponse "results" array shared by
// /search and /contents. Title is nullable on the wire; null unmarshals to "".
type exaResult struct {
	URL           string   `json:"url"`
	Title         string   `json:"title"`
	ID            string   `json:"id"`
	Score         float64  `json:"score"`
	PublishedDate string   `json:"publishedDate"`
	Author        string   `json:"author"`
	Text          string   `json:"text"`
	Summary       string   `json:"summary"`
	Highlights    []string `json:"highlights"`
}

type searchResponse struct {
	Results []exaResult `json:"results"`
}

// Search runs a web search query. Snippet prefers the per-result "text"
// field, then "summary", then the joined "highlights".
func (c *Client) Search(ctx context.Context, q *search.SearchQuery) (*search.SearchPage, error) {
	return c.SearchWithOptions(ctx, q, nil)
}

// SearchWithOptions runs a web search query with optional parameters. A nil
// ctx fails on the search-path sentinel before any validation or I/O; a nil
// opts sends the default body.
func (c *Client) SearchWithOptions(ctx context.Context, q *search.SearchQuery, opts *SearchOptions) (*search.SearchPage, error) {
	if ctx == nil {
		return nil, fmt.Errorf("exa: search: %w: nil context", search.ErrInvalidQuery)
	}
	if err := q.Validate(); err != nil {
		return nil, err
	}
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if err := c.checkAPIKey(); err != nil {
		return nil, err
	}

	body := map[string]any{
		"query":      q.Query,
		"numResults": q.NumResults,
		"contents":   map[string]any{"text": true},
	}
	if opts != nil {
		if opts.Type != "" {
			body["type"] = opts.Type
		}
		if opts.Category != "" {
			body["category"] = opts.Category
		}
		if include, err := normalizeDomains(opts.IncludeDomains); err == nil && len(include) > 0 {
			body["includeDomains"] = include
		}
		if exclude, err := normalizeDomains(opts.ExcludeDomains); err == nil && len(exclude) > 0 {
			body["excludeDomains"] = exclude
		}
		if opts.StartPublishedDate != "" {
			body["startPublishedDate"] = opts.StartPublishedDate
		}
		if opts.EndPublishedDate != "" {
			body["endPublishedDate"] = opts.EndPublishedDate
		}
		if opts.Livecrawl != "" {
			body["livecrawl"] = opts.Livecrawl
		}
		if opts.LivecrawlTimeout != nil {
			body["livecrawlTimeout"] = *opts.LivecrawlTimeout
		}
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("exa: marshal search request: %w", err)
	}

	respBody, err := c.doRequest(ctx, http.MethodPost, "/search", bodyBytes)
	if err != nil {
		return nil, err
	}

	var resp searchResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		c.logger.Error("exa: decode search response failed")
		return nil, fmt.Errorf("exa: decode search response: %w", err)
	}

	page := &search.SearchPage{}
	for _, r := range resp.Results {
		snippet := r.Text
		if snippet == "" {
			snippet = r.Summary
		}
		if snippet == "" && len(r.Highlights) > 0 {
			snippet = strings.Join(r.Highlights, "\n")
		}
		page.Results = append(page.Results, search.SearchResult{
			URL:         r.URL,
			Title:       r.Title,
			Snippet:     snippet,
			Score:       r.Score,
			PublishedAt: r.PublishedDate,
		})
	}
	return page, nil
}

// Scrape scrapes a single page via the contents endpoint. The first result's
// "text" becomes Document.Markdown.
func (c *Client) Scrape(ctx context.Context, r *search.ScrapeRequest) (*search.Document, error) {
	return c.ScrapeWithOptions(ctx, r, nil)
}

// ScrapeWithOptions scrapes a single page via the contents endpoint with
// optional parameters. A nil ctx fails on the scrape-path sentinel before
// any validation or I/O; a nil opts sends the default body (text requested).
func (c *Client) ScrapeWithOptions(ctx context.Context, r *search.ScrapeRequest, opts *ScrapeOptions) (*search.Document, error) {
	if ctx == nil {
		return nil, fmt.Errorf("exa: scrape: %w: nil context", search.ErrInvalidRequest)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if err := c.checkAPIKey(); err != nil {
		return nil, err
	}

	text := true
	if opts != nil && opts.Text != nil {
		text = *opts.Text
	}
	body := map[string]any{
		"urls": []string{r.URL},
		"text": text,
	}
	if opts != nil {
		if opts.Livecrawl != "" {
			body["livecrawl"] = opts.Livecrawl
		}
		if opts.LivecrawlTimeout != nil {
			body["livecrawlTimeout"] = *opts.LivecrawlTimeout
		}
		if opts.Subpages != nil {
			body["subpages"] = *opts.Subpages
		}
		if opts.SubpageTarget != "" {
			body["subpageTarget"] = opts.SubpageTarget
		}
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("exa: marshal scrape request: %w", err)
	}

	respBody, err := c.doRequest(ctx, http.MethodPost, "/contents", bodyBytes)
	if err != nil {
		return nil, err
	}

	var resp searchResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		c.logger.Error("exa: decode scrape response failed")
		return nil, fmt.Errorf("exa: decode scrape response: %w", err)
	}
	if len(resp.Results) == 0 {
		c.logger.Error("exa: scrape returned no results")
		return nil, fmt.Errorf("exa: scrape: no results")
	}

	res := resp.Results[0]
	doc := &search.Document{
		URL:      res.URL,
		Markdown: res.Text,
	}
	if doc.URL == "" {
		doc.URL = r.URL
	}
	if res.Author != "" || res.PublishedDate != "" {
		doc.Metadata = map[string]string{}
		if res.Author != "" {
			doc.Metadata["author"] = res.Author
		}
		if res.PublishedDate != "" {
			doc.Metadata["publishedDate"] = res.PublishedDate
		}
	}
	return doc, nil
}

// Crawl is not supported by Exa. It returns search.ErrNotSupported
// immediately without validating its argument, so callers can probe for
// support.
func (c *Client) Crawl(ctx context.Context, r *search.CrawlRequest) (*search.CrawlPage, error) {
	return nil, fmt.Errorf("exa: crawl: %w", search.ErrNotSupported)
}

// Answer generates an answer to a query via the answer endpoint. A blank
// query fails before any I/O. A structured (non-string) answer is returned
// as compact JSON.
func (c *Client) Answer(ctx context.Context, query string) (string, error) {
	if strings.TrimSpace(query) == "" {
		return "", fmt.Errorf("exa: answer: %w: query is required", search.ErrInvalidQuery)
	}
	if err := c.checkAPIKey(); err != nil {
		return "", err
	}

	bodyBytes, err := json.Marshal(map[string]any{
		"query":  query,
		"stream": false,
		"model":  "exa",
	})
	if err != nil {
		return "", fmt.Errorf("exa: marshal answer request: %w", err)
	}

	respBody, err := c.doRequest(ctx, http.MethodPost, "/answer", bodyBytes)
	if err != nil {
		return "", err
	}

	var resp struct {
		Answer json.RawMessage `json:"answer"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		c.logger.Error("exa: decode answer response failed")
		return "", fmt.Errorf("exa: decode answer response: %w", err)
	}
	if len(resp.Answer) == 0 || string(resp.Answer) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(resp.Answer, &s); err == nil {
		return s, nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, resp.Answer); err != nil {
		c.logger.Error("exa: decode answer response failed")
		return "", fmt.Errorf("exa: decode answer response: %w", err)
	}
	return buf.String(), nil
}

// Close releases any resources held by the client.
func (c *Client) Close() error { return nil }
