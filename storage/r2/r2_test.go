package r2

import (
	"testing"

	"go.uber.org/zap"

	clientstorage "github.com/shepard-labs/go-clients/storage"
)

// The operation-level behavior of the R2 client is owned by the shared s3
// core (storage/s3/s3_test.go covers it against fakes). These tests pin the
// wrapper's own responsibilities: endpoint derivation, maxDownload
// defaulting, and the public constructor contract.

func TestNewDerivesEndpoint(t *testing.T) {
	s, err := New("myacct", "key", "secret", "bucket", "svc", 0, zap.NewNop())
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	c, ok := s.(*Client)
	if !ok {
		t.Fatalf("expected *Client, got %T", s)
	}
	if c.endpoint != "myacct.r2.cloudflarestorage.com" {
		t.Fatalf("unexpected endpoint: %q", c.endpoint)
	}
}

func TestNewDefaultsMaxDownload(t *testing.T) {
	s, err := New("acct", "key", "secret", "bucket", "svc", 0, zap.NewNop())
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	c := s.(*Client)
	if c.maxDownload != clientstorage.DefaultMaxDownloadBytes {
		t.Fatalf("unexpected maxDownload: %d", c.maxDownload)
	}
}

func TestNewUsesExplicitMaxDownload(t *testing.T) {
	s, err := New("acct", "key", "secret", "", "svc", 123, zap.NewNop())
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	c := s.(*Client)
	if c.maxDownload != 123 {
		t.Fatalf("unexpected maxDownload: %d", c.maxDownload)
	}
}

func TestClientDelegatesToCore(t *testing.T) {
	s, err := New("acct", "key", "secret", "bucket", "svc", 0, zap.NewNop())
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	c := s.(*Client)
	if c.Storage == nil {
		t.Fatal("expected non-nil embedded core Storage")
	}
}
