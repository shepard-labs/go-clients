// Package twitter implements a hand-written client for a curated slice of the
// X (Twitter) API v2: publishing posts, chunked media upload, and reading back
// an account's own content and profile.
//
// The client is constructed with a user-context OAuth2 access token (the token
// a user grants when they link their X account); refreshing that token is the
// caller's responsibility. On a 401 (APIError.IsUnauthorized) the caller should
// refresh the token and build a new client. *Client is immutable after New and
// safe for concurrent use.
package twitter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

const baseURL = "https://api.x.com"

// ErrInvalidRequest indicates a client-side validation failure detected before
// any HTTP request is made.
var ErrInvalidRequest = errors.New("twitter: invalid request")

// Client talks to the X API v2 using a bound OAuth2 user-context access token.
// It is immutable after New and safe for concurrent use.
type Client struct {
	httpClient  *http.Client
	logger      *zap.Logger
	accessToken string
	baseURL     string
	sleeper     func(context.Context, time.Duration) bool // defaults to sleepCtx; injectable in tests
}

// New constructs a Client bound to a user-context OAuth2 access token.
func New(accessToken string, logger *zap.Logger) *Client {
	return &Client{
		httpClient:  &http.Client{Timeout: 30 * time.Second},
		logger:      logger,
		accessToken: accessToken,
		baseURL:     baseURL,
		sleeper:     sleepCtx,
	}
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

// Problem is a single RFC-7807 problem object from an X API "errors" array.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	Status int    `json:"status,omitempty"`
}

// APIError is a non-2xx response from the X API. StatusCode always comes from
// the HTTP response, never from a body field.
type APIError struct {
	StatusCode int
	Title      string
	Detail     string
	Type       string
	Problems   []Problem // populated from the "errors" array when present
}

func (e *APIError) Error() string {
	switch {
	case e.Detail != "":
		return fmt.Sprintf("twitter: HTTP %d: %s: %s", e.StatusCode, e.Title, e.Detail)
	case e.Title != "":
		return fmt.Sprintf("twitter: HTTP %d: %s", e.StatusCode, e.Title)
	default:
		return fmt.Sprintf("twitter: HTTP %d", e.StatusCode)
	}
}

// IsRateLimit reports whether the request was rate-limited (HTTP 429).
func (e *APIError) IsRateLimit() bool { return e.StatusCode == http.StatusTooManyRequests }

// IsUnauthorized reports whether the token is invalid or expired (HTTP 401).
// The caller should refresh the OAuth2 token and build a new client.
func (e *APIError) IsUnauthorized() bool { return e.StatusCode == http.StatusUnauthorized }

// IsForbidden reports whether the request was forbidden (HTTP 403), typically a
// missing OAuth2 scope or access-tier issue.
func (e *APIError) IsForbidden() bool { return e.StatusCode == http.StatusForbidden }

// problemBody carries both RFC-7807 shapes X uses: a top-level problem and an
// enveloped "errors" array.
type problemBody struct {
	Title  string    `json:"title"`
	Detail string    `json:"detail"`
	Type   string    `json:"type"`
	Errors []Problem `json:"errors"`
}

// parseAPIError builds an *APIError from a non-2xx response. StatusCode is
// always the HTTP status; on unmarshal failure it falls back to the raw body.
func parseAPIError(statusCode int, body []byte) error {
	var pb problemBody
	if err := json.Unmarshal(body, &pb); err != nil {
		return fmt.Errorf("HTTP %d: %s", statusCode, string(body))
	}
	apiErr := &APIError{
		StatusCode: statusCode,
		Title:      pb.Title,
		Detail:     pb.Detail,
		Type:       pb.Type,
		Problems:   pb.Errors,
	}
	if apiErr.Title == "" && apiErr.Detail == "" && apiErr.Type == "" && len(pb.Errors) > 0 {
		apiErr.Title = pb.Errors[0].Title
		apiErr.Detail = pb.Errors[0].Detail
		apiErr.Type = pb.Errors[0].Type
	}
	return apiErr
}

// doRequest performs an HTTP request with retry logic against the X API. It owns
// the body bytes so a fresh reader is built per attempt; retries therefore
// resend the full body for JSON and multipart alike. The retry sleep uses plain
// time.Sleep (Postmark parity), not sleepCtx.
func (c *Client) doRequest(ctx context.Context, method, path string, body []byte, contentType string) ([]byte, error) {
	url := c.baseURL + path

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		var reqBody io.Reader
		if body != nil {
			reqBody = bytes.NewReader(body)
		}

		req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}

		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.accessToken)
		if body != nil {
			req.Header.Set("Content-Type", contentType)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
			continue
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("failed to read response: %w", err)
			continue
		}

		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("server error: %d", resp.StatusCode)
			time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
			continue
		}

		if resp.StatusCode >= 400 {
			return nil, parseAPIError(resp.StatusCode, respBody)
		}

		return respBody, nil
	}

	return nil, fmt.Errorf("request failed after retries: %w", lastErr)
}

// doJSON marshals reqBody to JSON (when non-nil), performs the request, and
// unmarshals the response into respOut (when non-nil). It is used by every
// endpoint except the multipart media-append helper.
func (c *Client) doJSON(ctx context.Context, method, path string, reqBody, respOut any) error {
	var body []byte
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("failed to marshal request body: %w", err)
		}
		body = b
	}

	respBody, err := c.doRequest(ctx, method, path, body, "application/json")
	if err != nil {
		return err
	}

	if respOut != nil {
		if err := json.Unmarshal(respBody, respOut); err != nil {
			return fmt.Errorf("failed to unmarshal response: %w", err)
		}
	}
	return nil
}

// joinIDs validates and comma-joins an ids slice for a single query value.
func joinIDs(ids []string) (string, error) {
	if len(ids) == 0 {
		return "", fmt.Errorf("%w: at least one id required", ErrInvalidRequest)
	}
	return strings.Join(ids, ","), nil
}
