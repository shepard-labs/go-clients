// Package firecrawl implements the search.Client interface backed by the
// Firecrawl API v2 (https://api.firecrawl.dev).
//
// Wire reconciliation (verified against firecrawl npm 4.38.0 and the
// firecrawl/firecrawl repository, apps/js-sdk/firecrawl/src/v2, over the
// network — no fallback shapes used):
//
//   - Auth: "Authorization: Bearer <key>" on every request.
//   - Search: POST /v2/search with {"query": q, "limit": n}. Success is
//     {"success": true, "data": {"web": [...], "news": [...], "images": [...]}};
//     web items carry {url, title, description, ...} and news items carry
//     {url, title, snippet, date, ...}. A legacy flat {"data": [...]} shape is
//     also accepted. Snippet maps from description (web) or snippet (news);
//     PublishedAt maps from date/publishedDate/publishedAt when present.
//   - Scrape: POST /v2/scrape with {"url": u, "formats": [...]}. Success is
//     {"success": true, "data": {"markdown", "html"/"rawHtml",
//     "metadata": {...}, "links": [...]}}. Metadata is flattened to
//     map[string]string.
//   - Crawl: POST /v2/crawl with {"url": u, "limit": n} (the v2 field is
//     "limit", not "maxPages"). Success is {"success": true, "id": jobID,
//     "url": u}. Status is GET /v2/crawl/{id} returning {"success": true,
//     "status": "scraping"|"completed"|"failed"|"cancelled", "data": [...]}
//     with scrape-shaped documents.
//   - Map: POST /v2/map with {"url": u} (plus "limit" when bounded). Success
//     is {"success": true, "links": [...]} where each link is a URL string or
//     a {url, ...} object.
//
// Crawl polls the server-side job until it completes; cancelling the context
// abandons polling but does NOT cancel the server-side job.
//
// *Client is immutable after New and safe for concurrent use.
package firecrawl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/shepard-labs/go-clients/search"
)

const (
	defaultBaseURL = "https://api.firecrawl.dev"
	defaultTimeout = 30 * time.Second
	maxAttempts    = 3

	// defaultPollInterval is the delay between Crawl status polls.
	defaultPollInterval = 2 * time.Second
	// defaultMaxPollWait bounds the total time Crawl spends polling.
	defaultMaxPollWait = 5 * time.Minute
)

// Ensure *Client satisfies the shared interface.
var _ search.Client = (*Client)(nil)

// Client implements search.Client backed by the Firecrawl API v2. The API key
// is bound at construction. It is immutable after New and safe for
// concurrent use.
type Client struct {
	httpClient *http.Client
	logger     *zap.Logger
	apiKey     string
	baseURL    string
	// pollInterval is the delay between Crawl status polls.
	pollInterval time.Duration
	// maxPollWait bounds the total time Crawl spends polling.
	maxPollWait time.Duration
	sleeper     func(context.Context, time.Duration) bool // defaults to sleepCtx; injectable in tests
}

// Option customizes a Client.
type Option func(*Client)

// WithHTTPClient sets the HTTP client. A timeout-less injected client gets a
// 30s default applied. A nil client is ignored.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc == nil {
			return
		}
		if hc.Timeout == 0 {
			cp := *hc
			cp.Timeout = defaultTimeout
			c.httpClient = &cp
			return
		}
		c.httpClient = hc
	}
}

// WithBaseURL overrides the API base URL. A trailing slash is trimmed; an
// empty value is ignored.
func WithBaseURL(base string) Option {
	return func(c *Client) {
		if strings.TrimSpace(base) == "" {
			return
		}
		c.baseURL = strings.TrimRight(base, "/")
	}
}

// New constructs a Firecrawl-backed search.Client bound to apiKey. An empty
// apiKey is NOT rejected here; each method reports it before any I/O instead.
// A nil logger becomes zap.NewNop().
func New(apiKey string, logger *zap.Logger, opts ...Option) search.Client {
	if logger == nil {
		logger = zap.NewNop()
	}
	c := &Client{
		httpClient:   &http.Client{Timeout: defaultTimeout},
		logger:       logger,
		apiKey:       apiKey,
		baseURL:      defaultBaseURL,
		pollInterval: defaultPollInterval,
		maxPollWait:  defaultMaxPollWait,
		sleeper:      sleepCtx,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	if c.httpClient == nil {
		c.httpClient = &http.Client{Timeout: defaultTimeout}
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

// sleepCtx waits for d or until ctx is cancelled, whichever comes first. It
// reports false if the context was cancelled (so callers stop retrying).
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

// sleep waits for d via the injectable sleeper.
func (c *Client) sleep(ctx context.Context, d time.Duration) bool {
	if c.sleeper != nil {
		return c.sleeper(ctx, d)
	}
	return sleepCtx(ctx, d)
}

// APIError is a non-2xx response from the Firecrawl API, a success:false
// envelope on a 2xx response, or a terminally failed server-side crawl job.
// StatusCode always comes from the HTTP response (http.StatusOK when the
// transport succeeded but the job itself failed server-side), never from a
// body field.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("firecrawl: request failed with status %d: %s", e.StatusCode, e.Message)
}

// IsUnauthorized reports whether the API key is invalid or expired (HTTP 401).
func (e *APIError) IsUnauthorized() bool { return e.StatusCode == http.StatusUnauthorized }

// IsForbidden reports whether the request was forbidden (HTTP 403).
func (e *APIError) IsForbidden() bool { return e.StatusCode == http.StatusForbidden }

// IsRateLimited reports whether the request was rate-limited (HTTP 429).
func (e *APIError) IsRateLimited() bool { return e.StatusCode == http.StatusTooManyRequests }

// parseAPIError builds an *APIError from a non-2xx response. StatusCode is
// always the HTTP status; the message comes from the body's error/message
// field when parseable, else the HTTP status text, else a raw snippet capped
// at 512 chars.
func parseAPIError(statusCode int, body []byte) *APIError {
	var env struct {
		Error   string `json:"error"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	msg, code := "", ""
	if json.Unmarshal(body, &env) == nil {
		switch {
		case env.Error != "":
			msg, code = env.Error, env.Code
		case env.Message != "":
			msg, code = env.Message, env.Code
		}
	}
	if msg == "" {
		if snippet := strings.TrimSpace(string(body)); snippet != "" {
			if len(snippet) > 512 {
				snippet = snippet[:512]
			}
			msg = snippet
		} else {
			msg = http.StatusText(statusCode)
		}
	}
	return &APIError{StatusCode: statusCode, Code: code, Message: msg}
}

// checkKey reports whether the client has an API key. Empty keys are rejected
// per method, before any I/O.
func (c *Client) checkKey() error {
	if c.apiKey == "" {
		return errors.New("firecrawl: api key is empty")
	}
	return nil
}

// readCapped drains rc (always closing it) through a limit of
// search.MaxResponseBytes+1, reporting search.ErrResponseTooLarge when the cap
// is exceeded.
func readCapped(rc io.ReadCloser) ([]byte, error) {
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, search.MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("firecrawl: failed to read response: %w", err)
	}
	if int64(len(b)) > search.MaxResponseBytes {
		return nil, fmt.Errorf("firecrawl: response exceeds %d bytes: %w", search.MaxResponseBytes, search.ErrResponseTooLarge)
	}
	return b, nil
}

// retryAfterDelay resolves the Retry-After header (seconds or HTTP date) with
// fallback when absent or unparseable.
func retryAfterDelay(header string, fallback time.Duration) time.Duration {
	if h := strings.TrimSpace(header); h != "" {
		if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
		if t, err := http.ParseTime(h); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
			return fallback
		}
	}
	return fallback
}

// doRequest performs an HTTP request with retry logic against the Firecrawl
// API. It marshals payload once so a fresh reader is built per attempt;
// retries therefore resend the full body. Transport errors back off
// 200ms*attempt, 5xx backs off 500ms*attempt, and 429 honors Retry-After
// (fallback 500ms*attempt). Other 4xx (including 402 billing) return an
// *APIError immediately and are never retried. A 2xx response carrying
// success:false is likewise an *APIError.
func (c *Client) doRequest(ctx context.Context, method, path string, payload any) ([]byte, error) {
	var bodyBytes []byte
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("firecrawl: failed to marshal request body: %w", err)
		}
		bodyBytes = b
	}
	endpoint := c.baseURL + path

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var reqBody io.Reader
		if bodyBytes != nil {
			reqBody = bytes.NewReader(bodyBytes)
		}

		req, err := http.NewRequestWithContext(ctx, method, endpoint, reqBody)
		if err != nil {
			return nil, fmt.Errorf("firecrawl: failed to create request: %w", err)
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		if bodyBytes != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			c.logger.Error("firecrawl: request transport error; retrying")
			if !c.sleep(ctx, time.Duration(attempt+1)*200*time.Millisecond) {
				return nil, ctx.Err()
			}
			continue
		}

		respBody, err := readCapped(resp.Body)
		if err != nil {
			if errors.Is(err, search.ErrResponseTooLarge) {
				c.logger.Error("firecrawl: response too large", zap.Int("status", resp.StatusCode))
				return nil, err
			}
			lastErr = err
			if !c.sleep(ctx, time.Duration(attempt+1)*200*time.Millisecond) {
				return nil, ctx.Err()
			}
			continue
		}

		switch {
		case resp.StatusCode >= 500:
			lastErr = parseAPIError(resp.StatusCode, respBody)
			c.logger.Error("firecrawl: server error; retrying", zap.Int("status", resp.StatusCode))
			if !c.sleep(ctx, time.Duration(attempt+1)*500*time.Millisecond) {
				return nil, ctx.Err()
			}
			continue
		case resp.StatusCode == http.StatusTooManyRequests:
			lastErr = parseAPIError(resp.StatusCode, respBody)
			c.logger.Error("firecrawl: rate limited; retrying", zap.Int("status", resp.StatusCode))
			delay := retryAfterDelay(resp.Header.Get("Retry-After"), time.Duration(attempt+1)*500*time.Millisecond)
			if !c.sleep(ctx, delay) {
				return nil, ctx.Err()
			}
			continue
		case resp.StatusCode >= 400:
			apiErr := parseAPIError(resp.StatusCode, respBody)
			c.logger.Error("firecrawl: request failed", zap.Int("status", resp.StatusCode))
			return nil, apiErr
		case resp.StatusCode >= 300:
			c.logger.Error("firecrawl: unexpected status", zap.Int("status", resp.StatusCode))
			return nil, fmt.Errorf("firecrawl: unexpected status %d", resp.StatusCode)
		}

		if failed := envelopeError(resp.StatusCode, respBody); failed != nil {
			c.logger.Error("firecrawl: request failed", zap.Int("status", resp.StatusCode))
			return nil, failed
		}
		return respBody, nil
	}

	return nil, fmt.Errorf("firecrawl: request failed after retries: %w", lastErr)
}

// envelopeError reports a 2xx response carrying success:false as an *APIError,
// or nil when the envelope shows no failure.
func envelopeError(statusCode int, body []byte) *APIError {
	var env struct {
		Success *bool  `json:"success"`
		Error   string `json:"error"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &env) != nil || env.Success == nil || *env.Success {
		return nil
	}
	msg, code := env.Error, env.Code
	if msg == "" {
		msg = env.Message
	}
	if msg == "" {
		msg = http.StatusText(statusCode)
	}
	return &APIError{StatusCode: statusCode, Code: code, Message: msg}
}

// searchHit is a single web or news result in a /v2/search response.
type searchHit struct {
	URL           string  `json:"url"`
	Title         string  `json:"title"`
	Description   string  `json:"description"`
	Snippet       string  `json:"snippet"`
	Score         float64 `json:"score"`
	PublishedAt   string  `json:"publishedAt"`
	PublishedDate string  `json:"publishedDate"`
	Date          string  `json:"date"`
}

func (h searchHit) toResult() search.SearchResult {
	snippet := h.Description
	if snippet == "" {
		snippet = h.Snippet
	}
	published := h.PublishedAt
	if published == "" {
		published = h.PublishedDate
	}
	if published == "" {
		published = h.Date
	}
	return search.SearchResult{
		URL:         h.URL,
		Title:       h.Title,
		Snippet:     snippet,
		Score:       h.Score,
		PublishedAt: published,
	}
}

// Search runs a web search query, mapping the v2 web/news/image groups onto a
// flat page of results.
func (c *Client) Search(ctx context.Context, q *search.SearchQuery) (*search.SearchPage, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	if err := c.checkKey(); err != nil {
		return nil, err
	}

	respBody, err := c.doRequest(ctx, http.MethodPost, "/v2/search", map[string]any{
		"query": q.Query,
		"limit": q.NumResults,
	})
	if err != nil {
		return nil, err
	}

	var env struct {
		Data  json.RawMessage `json:"data"`
		Error string          `json:"error"`
	}
	if err := json.Unmarshal(respBody, &env); err != nil {
		c.logger.Error("firecrawl: failed to decode search response")
		return nil, fmt.Errorf("firecrawl: failed to decode search response: %w", err)
	}
	if len(env.Data) == 0 {
		c.logger.Error("firecrawl: search response missing data")
		return nil, fmt.Errorf("firecrawl: search response missing data: %s", env.Error)
	}

	var hits []searchHit
	if err := json.Unmarshal(env.Data, &hits); err != nil {
		var grouped struct {
			Web    []searchHit `json:"web"`
			News   []searchHit `json:"news"`
			Images []searchHit `json:"images"`
		}
		if err := json.Unmarshal(env.Data, &grouped); err != nil {
			c.logger.Error("firecrawl: failed to decode search response")
			return nil, fmt.Errorf("firecrawl: failed to decode search response: %w", err)
		}
		hits = append(append(grouped.Web, grouped.News...), grouped.Images...)
	}

	page := &search.SearchPage{}
	for _, h := range hits {
		page.Results = append(page.Results, h.toResult())
	}
	return page, nil
}

// scrapeData is a single scraped document in a v2 scrape/crawl response.
type scrapeData struct {
	URL      string         `json:"url"`
	Markdown string         `json:"markdown"`
	HTML     string         `json:"html"`
	RawHTML  string         `json:"rawHtml"`
	Metadata map[string]any `json:"metadata"`
	Links    []string       `json:"links"`
}

// toDocument maps wire data onto a search.Document, falling back to
// fallbackURL when the payload carries no URL of its own.
func (d *scrapeData) toDocument(fallbackURL string) search.Document {
	doc := search.Document{
		URL:      fallbackURL,
		Markdown: d.Markdown,
		HTML:     d.HTML,
		Metadata: flattenMetadata(d.Metadata),
		Links:    d.Links,
	}
	if doc.HTML == "" {
		doc.HTML = d.RawHTML
	}
	if d.URL != "" {
		doc.URL = d.URL
	} else if doc.Metadata["url"] != "" {
		doc.URL = doc.Metadata["url"]
	}
	return doc
}

// flattenMetadata stringifies a scrape metadata object. Slice values (e.g.
// keywords) are comma-joined; other non-strings use their default formatting.
// A nil input yields nil.
func flattenMetadata(in map[string]any) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		switch t := v.(type) {
		case nil:
			continue
		case string:
			out[k] = t
		case []any:
			parts := make([]string, 0, len(t))
			for _, p := range t {
				parts = append(parts, fmt.Sprint(p))
			}
			out[k] = strings.Join(parts, ", ")
		default:
			out[k] = fmt.Sprint(t)
		}
	}
	return out
}

// Scrape scrapes a single page, mapping the v2 document onto a
// search.Document.
func (c *Client) Scrape(ctx context.Context, r *search.ScrapeRequest) (*search.Document, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if err := c.checkKey(); err != nil {
		return nil, err
	}

	payload := map[string]any{"url": r.URL}
	if len(r.Formats) > 0 {
		payload["formats"] = r.Formats
	}
	respBody, err := c.doRequest(ctx, http.MethodPost, "/v2/scrape", payload)
	if err != nil {
		return nil, err
	}

	var env struct {
		Data  *scrapeData `json:"data"`
		Error string      `json:"error"`
	}
	if err := json.Unmarshal(respBody, &env); err != nil {
		c.logger.Error("firecrawl: failed to decode scrape response")
		return nil, fmt.Errorf("firecrawl: failed to decode scrape response: %w", err)
	}
	if env.Data == nil {
		c.logger.Error("firecrawl: scrape response missing data")
		return nil, fmt.Errorf("firecrawl: scrape response missing data: %s", env.Error)
	}
	doc := env.Data.toDocument(r.URL)
	return &doc, nil
}

// StartCrawl starts a server-side crawl job and returns its job ID.
func (c *Client) StartCrawl(ctx context.Context, r *search.CrawlRequest) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	if err := c.checkKey(); err != nil {
		return "", err
	}

	respBody, err := c.doRequest(ctx, http.MethodPost, "/v2/crawl", map[string]any{
		"url":   r.StartURL,
		"limit": r.MaxPages,
	})
	if err != nil {
		return "", err
	}

	var env struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(respBody, &env); err != nil {
		c.logger.Error("firecrawl: failed to decode start-crawl response")
		return "", fmt.Errorf("firecrawl: failed to decode start-crawl response: %w", err)
	}
	if env.ID == "" {
		c.logger.Error("firecrawl: crawl start response missing job id")
		return "", fmt.Errorf("firecrawl: crawl start response missing job id: %s", env.Error)
	}
	return env.ID, nil
}

// CrawlStatus fetches a crawl job once. The returned bool reports whether the
// job reached a terminal state (completed, failed, or cancelled). A completed
// job yields its documents; failed/cancelled jobs yield an *APIError (done is
// still true); an in-progress job yields its documents so far with done=false.
func (c *Client) CrawlStatus(ctx context.Context, jobID string) (*search.CrawlPage, bool, error) {
	if strings.TrimSpace(jobID) == "" {
		return nil, false, fmt.Errorf("firecrawl: %w: job id is required", search.ErrInvalidRequest)
	}
	if err := c.checkKey(); err != nil {
		return nil, false, err
	}

	respBody, err := c.doRequest(ctx, http.MethodGet, "/v2/crawl/"+url.PathEscape(jobID), nil)
	if err != nil {
		return nil, false, err
	}

	var env struct {
		Status string       `json:"status"`
		Data   []scrapeData `json:"data"`
		Error  string       `json:"error"`
	}
	if err := json.Unmarshal(respBody, &env); err != nil {
		c.logger.Error("firecrawl: failed to decode crawl status response")
		return nil, false, fmt.Errorf("firecrawl: failed to decode crawl status response: %w", err)
	}

	page := &search.CrawlPage{}
	for i := range env.Data {
		page.Documents = append(page.Documents, env.Data[i].toDocument(""))
	}

	switch env.Status {
	case "completed":
		return page, true, nil
	case "failed", "cancelled":
		c.logger.Error("firecrawl: crawl job terminated unsuccessfully")
		msg := env.Error
		if msg == "" {
			msg = "crawl job " + env.Status
		}
		return nil, true, &APIError{StatusCode: http.StatusOK, Code: "crawl_" + env.Status, Message: msg}
	default:
		return page, false, nil
	}
}

// Crawl crawls a site starting from StartURL, polling the server-side job
// every pollInterval until it completes or maxPollWait elapses. Cancelling the
// context abandons polling and returns ctx.Err(); the server-side job is NOT
// cancelled.
func (c *Client) Crawl(ctx context.Context, r *search.CrawlRequest) (*search.CrawlPage, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	jobID, err := c.StartCrawl(ctx, r)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(c.maxPollWait)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, done, err := c.CrawlStatus(ctx, jobID)
		if err != nil {
			return nil, err
		}
		if done {
			if page == nil {
				page = &search.CrawlPage{}
			}
			return page, nil
		}
		if time.Now().After(deadline) {
			c.logger.Error("firecrawl: crawl polling timed out")
			return nil, fmt.Errorf("firecrawl: crawl polling timed out after %s: %w", c.maxPollWait, context.DeadlineExceeded)
		}
		if !c.sleep(ctx, c.pollInterval) {
			return nil, ctx.Err()
		}
	}
}

// isHTTPURL reports whether raw is an absolute http(s) URL with a host and no
// userinfo (mirrors the root package's validation for Map input).
func isHTTPURL(raw string) bool {
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

// Map lists URLs discovered from startURL. A positive maxPages bounds the
// discovery via the v2 "limit" field; non-positive values leave it unbounded.
func (c *Client) Map(ctx context.Context, startURL string, maxPages int) ([]string, error) {
	if !isHTTPURL(startURL) {
		return nil, fmt.Errorf("firecrawl: %w: invalid StartURL", search.ErrInvalidRequest)
	}
	if err := c.checkKey(); err != nil {
		return nil, err
	}

	payload := map[string]any{"url": startURL}
	if maxPages > 0 {
		payload["limit"] = maxPages
	}
	respBody, err := c.doRequest(ctx, http.MethodPost, "/v2/map", payload)
	if err != nil {
		return nil, err
	}

	var env struct {
		Links []json.RawMessage `json:"links"`
		Error string            `json:"error"`
	}
	if err := json.Unmarshal(respBody, &env); err != nil {
		c.logger.Error("firecrawl: failed to decode map response")
		return nil, fmt.Errorf("firecrawl: failed to decode map response: %w", err)
	}

	var out []string
	for _, raw := range env.Links {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if s != "" {
				out = append(out, s)
			}
			continue
		}
		var obj struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(raw, &obj); err == nil && obj.URL != "" {
			out = append(out, obj.URL)
		}
	}
	return out, nil
}

// Close releases any resources held by the client. It holds none.
func (c *Client) Close() error { return nil }
