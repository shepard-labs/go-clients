package postmark

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"

	"github.com/shepard-labs/go-clients/email"
)

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
