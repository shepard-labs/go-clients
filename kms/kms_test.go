package kms

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestEncryptRejectsOversizedPlaintext verifies the size guard fires before any
// RPC, so it is safe to call on a Client with a nil underlying client.
func TestEncryptRejectsOversizedPlaintext(t *testing.T) {
	c := &Client{} // client field nil on purpose; guard must trip first

	oversized := strings.Repeat("a", maxPlaintextBytes+1)
	_, err := c.EncryptCredentials(context.Background(), oversized)
	if err == nil {
		t.Fatal("expected error for oversized plaintext, got nil")
	}
	if !errors.Is(err, ErrPlaintextTooLarge) {
		t.Fatalf("expected ErrPlaintextTooLarge, got %v", err)
	}
}
