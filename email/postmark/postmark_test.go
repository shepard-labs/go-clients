package postmark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"

	"github.com/shepard-labs/go-clients/email"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (errorReader) Close() error             { return nil }

// newTestClient returns a *Client pointed at srvURL with a no-op logger.
func newTestClient(srvURL string) *Client {
	return &Client{
		httpClient:  http.DefaultClient,
		logger:      zap.NewNop(),
		serverToken: "test-token",
		baseURL:     srvURL,
	}
}

func validMsg() *email.Message {
	return &email.Message{
		From:     "from@example.com",
		To:       []string{"to@example.com"},
		Subject:  "hi",
		TextBody: "body",
	}
}

// TestSendRetryResendsBody is the regression test for the retry-body bug: a
// 5xx on the first attempt must not blank out the body on the retry.
func TestSendRetryResendsBody(t *testing.T) {
	var attempts int32
	bodies := make([]string, 0, 2)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(SendEmailResponse{MessageID: "abc-123"})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	res, err := c.Send(context.Background(), validMsg())
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if res.MessageID != "abc-123" {
		t.Fatalf("expected message id abc-123, got %q", res.MessageID)
	}
	if atomic.LoadInt32(&attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
	if len(bodies) != 2 {
		t.Fatalf("expected 2 captured bodies, got %d", len(bodies))
	}
	if bodies[0] == "" || bodies[1] == "" {
		t.Fatalf("a request body was empty: attempt1=%q attempt2=%q", bodies[0], bodies[1])
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("retry body differs from first: %q vs %q", bodies[0], bodies[1])
	}
}

func TestSendValidatesBeforeHTTP(t *testing.T) {
	var called int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	_, err := c.Send(context.Background(), &email.Message{}) // invalid
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if atomic.LoadInt32(&called) != 0 {
		t.Fatal("HTTP endpoint was called despite invalid message")
	}
}

func TestSendSetsServerTokenHeader(t *testing.T) {
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Postmark-Server-Token")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(SendEmailResponse{MessageID: "x"})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	if _, err := c.Send(context.Background(), validMsg()); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if gotToken != "test-token" {
		t.Fatalf("expected server token header, got %q", gotToken)
	}
}

func TestSendForwardsMessageStream(t *testing.T) {
	var body struct {
		MessageStream string `json:"MessageStream"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(SendEmailResponse{MessageID: "x"})
	}))
	defer srv.Close()

	msg := validMsg()
	msg.MessageStream = "broadcast"

	c := newTestClient(srv.URL)
	if _, err := c.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if body.MessageStream != "broadcast" {
		t.Fatalf("expected MessageStream %q, got %q", "broadcast", body.MessageStream)
	}
}

func TestPostmarkResourceMethods(t *testing.T) {
	paths := make([]string, 0, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusOK)
		switch r.URL.Path {
		case "/server":
			_ = json.NewEncoder(w).Encode(ServerInfo{Name: "srv", Color: "red"})
		default:
			_ = json.NewEncoder(w).Encode(Domain{ID: 42, Name: "example.com", DKIMVerified: true})
		}
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	if err := c.VerifyAuth(context.Background()); err != nil {
		t.Fatalf("VerifyAuth failed: %v", err)
	}
	server, err := c.GetServerInfo(context.Background())
	if err != nil {
		t.Fatalf("GetServerInfo failed: %v", err)
	}
	if server.Name != "srv" {
		t.Fatalf("unexpected server: %#v", server)
	}
	for name, call := range map[string]func() (*Domain, error){
		"get":        func() (*Domain, error) { return c.GetDomain(context.Background(), 42) },
		"dkim":       func() (*Domain, error) { return c.VerifyDKIM(context.Background(), 42) },
		"returnPath": func() (*Domain, error) { return c.VerifyReturnPath(context.Background(), 42) },
	} {
		domain, err := call()
		if err != nil {
			t.Fatalf("%s failed: %v", name, err)
		}
		if domain.ID != 42 || !domain.DKIMVerified {
			t.Fatalf("unexpected domain from %s: %#v", name, domain)
		}
	}
	if len(paths) != 5 {
		t.Fatalf("expected 5 requests, got %d: %v", len(paths), paths)
	}
}

func TestPostmarkErrors(t *testing.T) {
	pmErr := &PostmarkError{ErrorCode: 429, Message: "slow down"}
	if got := pmErr.Error(); got != "postmark error 429: slow down" {
		t.Fatalf("unexpected error string %q", got)
	}
	if !pmErr.IsRateLimitError() {
		t.Fatal("expected rate limit")
	}
	if !(&PostmarkError{ErrorCode: 300}).IsInvalidEmailError() || !(&PostmarkError{ErrorCode: 406}).IsInvalidEmailError() {
		t.Fatal("expected invalid email")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(PostmarkError{ErrorCode: 300, Message: "bad"})
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)
	_, err := c.Send(context.Background(), validMsg())
	var got *PostmarkError
	if !errors.As(err, &got) || !got.IsInvalidEmailError() {
		t.Fatalf("expected invalid PostmarkError, got %T %v", err, err)
	}
}

func TestPostmarkInvalidJSONResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not-json"))
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)
	if _, err := c.Send(context.Background(), validMsg()); err == nil {
		t.Fatal("expected send unmarshal error")
	}
	if _, err := c.GetServerInfo(context.Background()); err == nil {
		t.Fatal("expected server unmarshal error")
	}
	if _, err := c.GetDomain(context.Background(), 1); err == nil {
		t.Fatal("expected domain unmarshal error")
	}
}

func TestDoRequestErrorBranches(t *testing.T) {
	c := newTestClient("://bad-url")
	if _, err := c.doRequest(context.Background(), "GET", "/server", nil); err == nil {
		t.Fatal("expected request creation error")
	}

	c = newTestClient("http://postmark.test")
	c.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network")
	})}
	if _, err := c.doRequest(context.Background(), "GET", "/server", nil); err == nil || !strings.Contains(err.Error(), "request failed after retries") {
		t.Fatalf("expected network retry error, got %v", err)
	}

	c.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: errorReader{}}, nil
	})}
	if _, err := c.doRequest(context.Background(), "GET", "/server", nil); err == nil {
		t.Fatal("expected response read error")
	}

	c.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader("not-json"))}, nil
	})}
	if _, err := c.doRequest(context.Background(), "GET", "/server", nil); err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("expected HTTP 400 error, got %v", err)
	}

	attempts := 0
	c.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		return &http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	if _, err := c.doRequest(context.Background(), "GET", "/server", nil); err == nil || !strings.Contains(err.Error(), "request failed after retries") {
		t.Fatalf("expected retry exhaustion, got %v", err)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}

	badBody := func() {}
	if _, err := c.doRequest(context.Background(), "POST", "/email", badBody); err == nil || !strings.Contains(err.Error(), "marshal") {
		t.Fatalf("expected marshal error, got %v", err)
	}
}

func TestNewReturnsConfiguredSender(t *testing.T) {
	if New("token", zap.NewNop()) == nil {
		t.Fatal("expected sender")
	}
}

func TestDomainRequestReturnsRequestError(t *testing.T) {
	c := newTestClient("http://postmark.test")
	c.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("boom")
	})}
	if _, err := c.GetDomain(context.Background(), 1); err == nil {
		t.Fatal("expected domain request error")
	}
}
