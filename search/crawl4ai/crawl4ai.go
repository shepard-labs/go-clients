// Package crawl4ai implements the search.Client interface backed by a
// self-hosted Crawl4AI Docker server (default http://localhost:11235).
//
//   - Scrape and Crawl are supported via the async job API: submit
//     POST /crawl/job, then poll GET /crawl/job/{task_id} until the job
//     reaches a terminal status.
//   - Search is not supported and always returns search.ErrNotSupported.
//
// Wire reconciliation (read-only; nothing cloned into this repo, no
// dependencies added, no local server was running on port 11235):
//
//   - Reconciled against unclecode/crawl4ai:0.9.3 — the library reports
//     __version__ = "0.9.3" (crawl4ai/__version__.py), the latest GitHub
//     release tag is v0.9.3 (published 2026-08-31), and the Docker image is
//     therefore unclecode/crawl4ai:0.9.3. Checked 2026-09-03 via
//     docs.crawl4ai.com/core/self-hosting (v0.9.x) plus the raw files
//     deploy/docker/MIGRATION.md, server.py, job.py, api.py, and schemas.py
//     on main.
//   - Verified async contract: POST /crawl/job with
//     {"urls": [...], "browser_config": {}, "crawler_config": {}} returns
//     HTTP 202 {"task_id": "crawl_<hex8>"}; GET /crawl/job/{task_id} returns
//     {"task_id", "status": "processing"|"completed"|"failed", ...} with
//     "result" on completion (the handle_crawl_request payload, i.e.
//     {"success", "results": [...]}) or "error" on failure.
//   - Auth: 0.9.x is secure-by-default (MIGRATION.md). Every endpoint except
//     GET /health requires "Authorization: Bearer <token>"
//     (CRAWL4AI_API_TOKEN server-side). GET /health is public and returns
//     {"status": "ok", "timestamp", "version"}.
//   - Deviations from the pre-dispatch fallback sketch, which suggested
//     POST /crawl -> {"task_id"} and GET /job/{task_id}: /crawl is the
//     synchronous endpoint (returns {"success", "results"} directly), so this
//     client submits to the verified async endpoint POST /crawl/job and polls
//     the verified status endpoint GET /crawl/job/{task_id}. The self-hosting
//     docs page additionally documents a GET /job/{task_id} shape with
//     result{markdown, extracted_content, links}; completed-result parsing
//     below tolerates that shape too.
//   - Deep crawling: over the 0.9 network boundary, deep_crawl_strategy and
//     related crawler controls are rejected (HTTP 400); unknown fields are
//     dropped. Crawl therefore forwards MaxPages as a "max_pages" hint inside
//     crawler_config and aggregates whatever documents the completed job
//     returns. Multi-page expansion beyond that requires server-side config.
//
// Polling behaviour: Scrape/Crawl poll every 2s up to 5min. Cancelling the
// context aborts the wait and returns ctx.Err(), but the server-side job is
// NOT cancelled and keeps running. Only failures are logged, and logs never
// include the token, URLs, or page content. Because GET /health is public by
// default server config, VerifyAuth verifies reachability; an invalid token
// surfaces as an *APIError (401) on the next authenticated call.
package crawl4ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/shepard-labs/go-clients/search"
)

// defaultBaseURL is the Crawl4AI Docker server's default listen address.
const defaultBaseURL = "http://localhost:11235"

// pollInterval is the delay between job-status polls; maxPollWait bounds the
// total wait for a submitted job.
const (
	pollInterval = 2 * time.Second
	maxPollWait  = 5 * time.Minute
)

// Ensure *Client satisfies the shared interface.
var _ search.Client = (*Client)(nil)

// Client implements search.Client backed by a self-hosted Crawl4AI server.
// The API token is bound at construction. It is immutable after New (except
// for a timeout-less injected *http.Client, which gets the default timeout)
// and safe for concurrent use.
type Client struct {
	httpClient *http.Client
	logger     *zap.Logger
	baseURL    string
	token      string
	sleeper    func(context.Context, time.Duration) bool // defaults to sleepCtx; injectable in tests
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets the HTTP client. A nil client is ignored; a client
// without a timeout gets the 30s default.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.httpClient = h }
}

// WithToken sets the Crawl4AI API token sent as "Authorization: Bearer".
func WithToken(token string) Option {
	return func(c *Client) { c.token = token }
}

// New constructs a Crawl4AI-backed search.Client. An empty baseURL selects
// the default (http://localhost:11235); a nil logger selects zap.NewNop.
// An empty token logs a Warn (server 0.9 requires auth by default) but
// construction still succeeds; calls then surface the server's 401 as an
// *APIError.
func New(baseURL string, logger *zap.Logger, opts ...Option) search.Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	c := &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		logger:     logger,
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		sleeper:    sleepCtx,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	if c.httpClient == nil {
		c.httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if c.httpClient.Timeout == 0 {
		c.httpClient.Timeout = 30 * time.Second
	}
	if c.sleeper == nil {
		c.sleeper = sleepCtx
	}
	if c.logger == nil {
		c.logger = zap.NewNop()
	}
	if c.token == "" {
		c.logger.Warn("crawl4ai: no API token configured; server 0.9 requires auth and requests will fail with 401")
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

// APIError is a non-2xx response from the Crawl4AI server. StatusCode always
// comes from the HTTP response, never from a body field. Code carries a
// server-provided error code when the body includes one.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("crawl4ai: request failed with status %d: %s", e.StatusCode, e.Message)
}

// IsUnauthorized reports whether the token is missing or invalid (HTTP 401).
func (e *APIError) IsUnauthorized() bool { return e.StatusCode == http.StatusUnauthorized }

// IsForbidden reports whether the request was forbidden (HTTP 403).
func (e *APIError) IsForbidden() bool { return e.StatusCode == http.StatusForbidden }

// IsRateLimited reports whether the request was rate-limited (HTTP 429).
func (e *APIError) IsRateLimited() bool { return e.StatusCode == http.StatusTooManyRequests }

// apiErrorBody is the best-effort decode of a non-2xx JSON body. FastAPI
// HTTPException renders as {"detail": "..."}.
type apiErrorBody struct {
	Detail  string `json:"detail"`
	Message string `json:"message"`
	Error   string `json:"error"`
	Code    string `json:"code"`
}

// parseAPIError builds an *APIError from a non-2xx response. StatusCode is
// always the HTTP status; on unmarshal failure it falls back to the raw body.
func parseAPIError(statusCode int, body []byte) error {
	var eb apiErrorBody
	if err := json.Unmarshal(body, &eb); err != nil {
		return &APIError{StatusCode: statusCode, Message: string(body)}
	}
	msg := eb.Detail
	if msg == "" {
		msg = eb.Message
	}
	if msg == "" {
		msg = eb.Error
	}
	if msg == "" {
		msg = string(body)
	}
	return &APIError{StatusCode: statusCode, Code: eb.Code, Message: msg}
}

// readCapped reads and closes rc, capping the body at
// search.MaxResponseBytes. It reports tooLarge when the body exceeds the cap.
func readCapped(rc io.ReadCloser) (body []byte, tooLarge bool, err error) {
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, search.MaxResponseBytes+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(raw)) > search.MaxResponseBytes {
		return nil, true, nil
	}
	return raw, false, nil
}

// retryAfterDelay honors the Retry-After response header (seconds), falling
// back to 500ms*attempt when it is absent or unparsable.
func retryAfterDelay(header http.Header, attempt int) time.Duration {
	fallback := time.Duration(attempt+1) * 500 * time.Millisecond
	v := strings.TrimSpace(header.Get("Retry-After"))
	if v == "" {
		return fallback
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	return fallback
}

// doRequest performs an HTTP request with retry logic against the Crawl4AI
// server. The body bytes are owned by doRequest so a fresh reader is built
// per attempt; retries therefore resend the full body. Transport errors retry
// after 200ms*attempt, 5xx after 500ms*attempt, and 429 after Retry-After
// (fallback 500ms*attempt); other 4xx return an *APIError immediately and are
// never retried. Over-cap bodies return search.ErrResponseTooLarge.
func (c *Client) doRequest(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	url := c.baseURL + path

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		var reqBody io.Reader
		if body != nil {
			reqBody = bytes.NewReader(body)
		}

		req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
		if err != nil {
			return nil, fmt.Errorf("crawl4ai: failed to create request: %w", err)
		}

		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			if attempt < 2 {
				if !c.sleeper(ctx, time.Duration(attempt+1)*200*time.Millisecond) {
					return nil, ctx.Err()
				}
			}
			continue
		}

		raw, tooLarge, readErr := readCapped(resp.Body)
		status := resp.StatusCode
		header := resp.Header.Clone()
		if readErr != nil {
			lastErr = fmt.Errorf("crawl4ai: failed to read response: %w", readErr)
			if attempt < 2 {
				if !c.sleeper(ctx, time.Duration(attempt+1)*200*time.Millisecond) {
					return nil, ctx.Err()
				}
			}
			continue
		}
		if tooLarge {
			return nil, fmt.Errorf("crawl4ai: response body exceeded limit: %w", search.ErrResponseTooLarge)
		}

		switch {
		case status == http.StatusTooManyRequests:
			apiErr := parseAPIError(status, raw)
			if attempt < 2 {
				lastErr = apiErr
				if !c.sleeper(ctx, retryAfterDelay(header, attempt)) {
					return nil, ctx.Err()
				}
				continue
			}
			return nil, apiErr
		case status >= 500:
			lastErr = parseAPIError(status, raw)
			if attempt < 2 {
				if !c.sleeper(ctx, time.Duration(attempt+1)*500*time.Millisecond) {
					return nil, ctx.Err()
				}
			}
			continue
		case status >= 400:
			return nil, parseAPIError(status, raw)
		case status >= 200 && status < 300:
			return raw, nil
		default:
			return nil, parseAPIError(status, raw)
		}
	}

	if lastErr == nil {
		return nil, fmt.Errorf("crawl4ai: request failed after retries")
	}
	return nil, fmt.Errorf("crawl4ai: request failed after retries: %w", lastErr)
}

// doJSON marshals reqBody to JSON (when non-nil), performs the request, and
// unmarshals the response into respOut (when non-nil).
func (c *Client) doJSON(ctx context.Context, method, path string, reqBody, respOut any) error {
	var body []byte
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		body = b
	}

	respBody, err := c.doRequest(ctx, method, path, body)
	if err != nil {
		return err
	}

	if respOut != nil {
		if err := json.Unmarshal(respBody, respOut); err != nil {
			return fmt.Errorf("unmarshal response: %w", err)
		}
	}
	return nil
}

// submitRequest is the POST /crawl/job payload.
type submitRequest struct {
	URLs          []string       `json:"urls"`
	BrowserConfig map[string]any `json:"browser_config,omitempty"`
	CrawlerConfig map[string]any `json:"crawler_config,omitempty"`
}

// submitResponse is the POST /crawl/job acknowledgement.
type submitResponse struct {
	TaskID  string `json:"task_id"`
	Message string `json:"message"`
}

// statusResponse is the GET /crawl/job/{task_id} payload.
type statusResponse struct {
	TaskID string          `json:"task_id"`
	Status string          `json:"status"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

// submitJob enqueues an async crawl job for urls and returns its task id.
func (c *Client) submitJob(ctx context.Context, urls []string, crawlerConfig map[string]any) (string, error) {
	req := submitRequest{URLs: urls, CrawlerConfig: crawlerConfig}
	var resp submitResponse
	if err := c.doJSON(ctx, http.MethodPost, "/crawl/job", req, &resp); err != nil {
		return "", fmt.Errorf("submit crawl job: %w", err)
	}
	if strings.TrimSpace(resp.TaskID) == "" {
		return "", fmt.Errorf("submit crawl job: empty task_id in response")
	}
	return resp.TaskID, nil
}

// fetchJob returns the current documents (when completed) and status string
// for taskID. A failed/cancelled job returns an *APIError carrying the
// server's message (StatusCode is the poll HTTP response code).
func (c *Client) fetchJob(ctx context.Context, taskID string) (*search.CrawlPage, string, error) {
	if strings.TrimSpace(taskID) == "" {
		return nil, "", fmt.Errorf("fetch job: %w: task id is required", search.ErrInvalidRequest)
	}
	var resp statusResponse
	if err := c.doJSON(ctx, http.MethodGet, "/crawl/job/"+taskID, nil, &resp); err != nil {
		return nil, "", fmt.Errorf("fetch job: %w", err)
	}
	switch status := strings.ToLower(strings.TrimSpace(resp.Status)); status {
	case "completed":
		docs, err := documentsFromResult(resp.Result, "")
		if err != nil {
			return nil, resp.Status, fmt.Errorf("fetch job: %w", err)
		}
		return &search.CrawlPage{Documents: docs}, resp.Status, nil
	case "failed", "cancelled":
		msg := strings.TrimSpace(resp.Error)
		if msg == "" {
			msg = "unknown error"
		}
		return nil, resp.Status, &APIError{StatusCode: http.StatusOK, Code: "crawl_" + status, Message: msg}
	case "":
		return nil, "", fmt.Errorf("fetch job: missing status in response")
	default:
		return nil, resp.Status, nil
	}
}

// pollJob waits for taskID to reach a terminal status and returns the
// aggregated documents. Cancelling ctx aborts the wait with ctx.Err(); the
// server-side job keeps running.
func (c *Client) pollJob(ctx context.Context, taskID string) (*search.CrawlPage, error) {
	deadline := time.Now().Add(maxPollWait)
	for {
		page, status, err := c.fetchJob(ctx, taskID)
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(status, "completed") {
			return page, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("poll job: timed out after %s: %w", maxPollWait, context.DeadlineExceeded)
		}
		wait := pollInterval
		if wait > remaining {
			wait = remaining
		}
		if !c.sleeper(ctx, wait) {
			return nil, ctx.Err()
		}
	}
}

// Search is not supported by Crawl4AI (it crawls given URLs; it does not run
// web searches). It always returns search.ErrNotSupported without validating
// the query.
func (c *Client) Search(ctx context.Context, q *search.SearchQuery) (*search.SearchPage, error) {
	return nil, fmt.Errorf("crawl4ai: search: %w", search.ErrNotSupported)
}

// Scrape submits a single-URL crawl job and polls until it completes,
// mapping the result to a Document. Formats are validated but otherwise not
// forwarded: the server always returns the full result (markdown, html,
// links) and screenshot output has no field on search.Document.
func (c *Client) Scrape(ctx context.Context, r *search.ScrapeRequest) (*search.Document, error) {
	if err := r.Validate(); err != nil {
		return nil, fmt.Errorf("crawl4ai: scrape: %w", err)
	}
	taskID, err := c.submitJob(ctx, []string{r.URL}, nil)
	if err != nil {
		c.logger.Error("crawl4ai: scrape submit failed")
		return nil, fmt.Errorf("crawl4ai: scrape: %w", err)
	}
	page, err := c.pollJob(ctx, taskID)
	if err != nil {
		c.logger.Error("crawl4ai: scrape poll failed", zap.String("task_id", taskID))
		return nil, fmt.Errorf("crawl4ai: scrape: %w", err)
	}
	if len(page.Documents) == 0 {
		c.logger.Error("crawl4ai: scrape returned no documents", zap.String("task_id", taskID))
		return nil, fmt.Errorf("crawl4ai: scrape: no documents in completed job")
	}
	doc := page.Documents[0]
	if doc.URL == "" {
		doc.URL = r.URL
	}
	return &doc, nil
}

// Crawl submits a crawl job for StartURL and polls until it completes,
// aggregating every returned result into a CrawlPage. MaxPages is forwarded
// as a "max_pages" hint inside crawler_config; the 0.9 network boundary
// rejects declarative deep-crawl strategies, so wider expansion requires
// server-side configuration and the hint may be dropped by the server.
func (c *Client) Crawl(ctx context.Context, r *search.CrawlRequest) (*search.CrawlPage, error) {
	if err := r.Validate(); err != nil {
		return nil, fmt.Errorf("crawl4ai: crawl: %w", err)
	}
	taskID, err := c.StartCrawl(ctx, r)
	if err != nil {
		c.logger.Error("crawl4ai: crawl submit failed")
		return nil, fmt.Errorf("crawl4ai: crawl: %w", err)
	}
	page, err := c.pollJob(ctx, taskID)
	if err != nil {
		c.logger.Error("crawl4ai: crawl poll failed", zap.String("task_id", taskID))
		return nil, fmt.Errorf("crawl4ai: crawl: %w", err)
	}
	return page, nil
}

// StartCrawl validates r, submits the crawl job, and returns its task id for
// polling via JobStatus.
func (c *Client) StartCrawl(ctx context.Context, r *search.CrawlRequest) (string, error) {
	if err := r.Validate(); err != nil {
		return "", fmt.Errorf("crawl4ai: start crawl: %w", err)
	}
	taskID, err := c.submitJob(ctx, []string{r.StartURL}, map[string]any{"max_pages": r.MaxPages})
	if err != nil {
		return "", fmt.Errorf("crawl4ai: start crawl: %w", err)
	}
	return taskID, nil
}

// JobStatus returns the current documents (page-or-nil) and status string
// for a previously submitted task id. While the job is still processing it
// returns (nil, status, nil); a failed job returns the server's error.
func (c *Client) JobStatus(ctx context.Context, taskID string) (*search.CrawlPage, string, error) {
	page, status, err := c.fetchJob(ctx, taskID)
	if err != nil {
		return nil, status, fmt.Errorf("crawl4ai: job status: %w", err)
	}
	return page, status, nil
}

// VerifyAuth pings the server's public health endpoint; non-2xx responses
// map to an *APIError. Note the endpoint is unauthenticated by default server
// config, so this verifies reachability; token problems surface as *APIError
// (401) on the next authenticated call.
func (c *Client) VerifyAuth(ctx context.Context) error {
	if err := c.doJSON(ctx, http.MethodGet, "/health", nil, nil); err != nil {
		return fmt.Errorf("crawl4ai: verify auth: %w", err)
	}
	return nil
}

// Close releases any resources held by the client. There are none.
func (c *Client) Close() error { return nil }

// documentsFromResult maps a completed job's "result" payload to documents.
// It accepts the server's {"results": [...]} envelope, a bare array of
// results, and the docs-page's single-result {markdown, extracted_content,
// links} shape. fallbackURL fills in items that carry no URL.
func documentsFromResult(raw json.RawMessage, fallbackURL string) ([]search.Document, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, fmt.Errorf("empty result in completed job")
	}
	var v any
	if err := json.Unmarshal(trimmed, &v); err != nil {
		return nil, fmt.Errorf("decode job result: %w", err)
	}
	switch t := v.(type) {
	case map[string]any:
		if results, ok := t["results"]; ok {
			return documentsFromItems(results, fallbackURL)
		}
		if looksLikeDocument(t) {
			return []search.Document{documentFromItem(t, fallbackURL)}, nil
		}
		return nil, fmt.Errorf("unexpected job result shape")
	case []any:
		return documentsFromItems(t, fallbackURL)
	default:
		return nil, fmt.Errorf("unexpected job result shape")
	}
}

// documentsFromItems maps result items to documents, skipping entries that
// are not objects or report success == false.
func documentsFromItems(v any, fallbackURL string) ([]search.Document, error) {
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("unexpected job result shape")
	}
	var docs []search.Document
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if success, ok := m["success"].(bool); ok && !success {
			continue
		}
		docs = append(docs, documentFromItem(m, fallbackURL))
	}
	return docs, nil
}

// looksLikeDocument reports whether m carries single-document fields.
func looksLikeDocument(m map[string]any) bool {
	for _, k := range []string{"markdown", "html", "cleaned_html", "links", "url", "extracted_content"} {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

// documentFromItem maps one crawl result object to a search.Document.
func documentFromItem(m map[string]any, fallbackURL string) search.Document {
	doc := search.Document{
		URL:      stringField(m, "url"),
		Markdown: extractMarkdown(m),
		HTML:     stringField(m, "html", "cleaned_html"),
		Links:    parseLinks(m["links"]),
		Metadata: parseMetadata(m["metadata"]),
	}
	if doc.URL == "" {
		doc.URL = fallbackURL
	}
	return doc
}

// stringField returns the first non-empty string value under keys.
func stringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// extractMarkdown pulls the markdown from a result object, which may carry
// it as a string or as an object with raw_markdown/fit_markdown, falling
// back to a string extracted_content.
func extractMarkdown(m map[string]any) string {
	if s := stringField(m, "markdown"); s != "" {
		return s
	}
	if obj, ok := m["markdown"].(map[string]any); ok {
		if s := stringField(obj, "raw_markdown", "fit_markdown", "markdown", "text"); s != "" {
			return s
		}
	}
	if s, ok := m["extracted_content"].(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	return ""
}

// parseLinks flattens the links value: the server renders
// {"internal": [...], "external": [...]} with string or {"href": ...}
// entries, but a bare array or string is tolerated too.
func parseLinks(v any) []string {
	var out []string
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if t != "" {
			out = append(out, t)
		}
	case []any:
		for _, e := range t {
			out = appendLinkValue(out, e)
		}
	case map[string]any:
		for _, k := range []string{"internal", "external", "links"} {
			if items, ok := t[k].([]any); ok {
				for _, e := range items {
					out = appendLinkValue(out, e)
				}
			}
		}
	}
	return out
}

// appendLinkValue appends one link entry in string or {"href"/"url"} shape.
func appendLinkValue(out []string, e any) []string {
	switch t := e.(type) {
	case string:
		if t != "" {
			out = append(out, t)
		}
	case map[string]any:
		if s := stringField(t, "href", "url"); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// parseMetadata flattens a result metadata object to string values; nested
// values are JSON-encoded, nils are skipped.
func parseMetadata(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		switch t := val.(type) {
		case nil:
		case string:
			out[k] = t
		case bool, float64, int, int64:
			out[k] = fmt.Sprintf("%v", t)
		default:
			if b, err := json.Marshal(t); err == nil {
				out[k] = string(b)
			} else {
				out[k] = fmt.Sprintf("%v", t)
			}
		}
	}
	return out
}
