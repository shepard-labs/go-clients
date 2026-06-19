package kms

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/googleapis/gax-go/v2"
	"go.uber.org/zap"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type fakeKMSClient struct {
	encryptCalls int
	decryptCalls int
	closeErr     error
	encryptErrs  []error
	decryptErrs  []error
	encryptResp  *kmspb.EncryptResponse
	decryptResp  *kmspb.DecryptResponse
}

func (f *fakeKMSClient) Encrypt(_ context.Context, req *kmspb.EncryptRequest, _ ...gax.CallOption) (*kmspb.EncryptResponse, error) {
	f.encryptCalls++
	if len(f.encryptErrs) >= f.encryptCalls && f.encryptErrs[f.encryptCalls-1] != nil {
		return nil, f.encryptErrs[f.encryptCalls-1]
	}
	if f.encryptResp != nil {
		return f.encryptResp, nil
	}
	ciphertext := []byte("cipher:" + string(req.Plaintext))
	return &kmspb.EncryptResponse{
		Ciphertext:              ciphertext,
		CiphertextCrc32C:        wrapperspb.Int64(int64(crc32c(ciphertext))),
		VerifiedPlaintextCrc32C: true,
	}, nil
}

func (f *fakeKMSClient) Decrypt(_ context.Context, req *kmspb.DecryptRequest, _ ...gax.CallOption) (*kmspb.DecryptResponse, error) {
	f.decryptCalls++
	if len(f.decryptErrs) >= f.decryptCalls && f.decryptErrs[f.decryptCalls-1] != nil {
		return nil, f.decryptErrs[f.decryptCalls-1]
	}
	if f.decryptResp != nil {
		return f.decryptResp, nil
	}
	plaintext := strings.TrimPrefix(string(req.Ciphertext), "cipher:")
	return &kmspb.DecryptResponse{
		Plaintext:       []byte(plaintext),
		PlaintextCrc32C: wrapperspb.Int64(int64(crc32c([]byte(plaintext)))),
	}, nil
}

func (f *fakeKMSClient) Close() error { return f.closeErr }

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

func TestEncryptDecryptCredentials(t *testing.T) {
	fake := &fakeKMSClient{}
	c := &Client{client: fake, keyName: "projects/p/locations/l/keyRings/r/cryptoKeys/k", logger: zap.NewNop()}

	ciphertext, err := c.EncryptCredentials(context.Background(), "secret")
	if err != nil {
		t.Fatalf("EncryptCredentials failed: %v", err)
	}
	plaintext, err := c.DecryptCredentials(context.Background(), ciphertext)
	if err != nil {
		t.Fatalf("DecryptCredentials failed: %v", err)
	}
	if plaintext != "secret" {
		t.Fatalf("unexpected plaintext %q", plaintext)
	}
	if fake.encryptCalls != 1 || fake.decryptCalls != 1 {
		t.Fatalf("unexpected calls: encrypt=%d decrypt=%d", fake.encryptCalls, fake.decryptCalls)
	}
}

func TestEncryptRetriesAndValidatesChecksums(t *testing.T) {
	fake := &fakeKMSClient{encryptErrs: []error{errors.New("temporary")}}
	c := &Client{client: fake, logger: zap.NewNop()}

	if _, err := c.EncryptCredentials(context.Background(), "secret"); err != nil {
		t.Fatalf("EncryptCredentials failed after retry: %v", err)
	}
	if fake.encryptCalls != 2 {
		t.Fatalf("expected 2 encrypt calls, got %d", fake.encryptCalls)
	}

	c.client = &fakeKMSClient{encryptResp: &kmspb.EncryptResponse{VerifiedPlaintextCrc32C: false}}
	if _, err := c.EncryptCredentials(context.Background(), "secret"); err == nil || !strings.Contains(err.Error(), "request corrupted") {
		t.Fatalf("expected request corruption error, got %v", err)
	}

	c.client = &fakeKMSClient{encryptResp: &kmspb.EncryptResponse{Ciphertext: []byte("bad"), CiphertextCrc32C: wrapperspb.Int64(1), VerifiedPlaintextCrc32C: true}}
	if _, err := c.EncryptCredentials(context.Background(), "secret"); err == nil || !strings.Contains(err.Error(), "response corrupted") {
		t.Fatalf("expected response corruption error, got %v", err)
	}

	c.client = &fakeKMSClient{encryptErrs: []error{errors.New("one"), errors.New("two"), errors.New("three")}}
	if _, err := c.EncryptCredentials(context.Background(), "secret"); err == nil || !strings.Contains(err.Error(), "failed to encrypt") {
		t.Fatalf("expected final encrypt error, got %v", err)
	}
}

func TestDecryptErrors(t *testing.T) {
	c := &Client{client: &fakeKMSClient{}, logger: zap.NewNop()}
	if _, err := c.DecryptCredentials(context.Background(), "not-base64"); err == nil {
		t.Fatal("expected base64 decode error")
	}

	ciphertext, err := (&Client{client: &fakeKMSClient{}, logger: zap.NewNop()}).EncryptCredentials(context.Background(), "secret")
	if err != nil {
		t.Fatalf("EncryptCredentials failed: %v", err)
	}
	c.client = &fakeKMSClient{decryptResp: &kmspb.DecryptResponse{Plaintext: []byte("secret"), PlaintextCrc32C: wrapperspb.Int64(1)}}
	if _, err := c.DecryptCredentials(context.Background(), ciphertext); err == nil || !strings.Contains(err.Error(), "response corrupted") {
		t.Fatalf("expected decrypt corruption error, got %v", err)
	}

	fake := &fakeKMSClient{decryptErrs: []error{errors.New("one"), errors.New("two"), errors.New("three")}}
	c.client = fake
	if _, err := c.DecryptCredentials(context.Background(), ciphertext); err == nil || !strings.Contains(err.Error(), "failed to decrypt") {
		t.Fatalf("expected final decrypt error, got %v", err)
	}
	if fake.decryptCalls != 3 {
		t.Fatalf("expected 3 decrypt calls, got %d", fake.decryptCalls)
	}
}

func TestSleepCtxCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Hour) {
		t.Fatal("expected sleepCtx to report cancellation")
	}
}

func TestNewRejectsInvalidServiceAccount(t *testing.T) {
	_, err := New(context.Background(), "not-base64", "key", zap.NewNop())
	if err == nil {
		t.Fatal("expected invalid service account error")
	}
}

func TestNewUsesClientFactory(t *testing.T) {
	original := newKMSClient
	defer func() { newKMSClient = original }()
	fake := &fakeKMSClient{}
	newKMSClient = func(context.Context, ...option.ClientOption) (kmsClient, error) { return fake, nil }

	enc, err := New(context.Background(), base64.StdEncoding.EncodeToString([]byte(`{}`)), "key", zap.NewNop())
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	c := enc.(*Client)
	if c.client != fake || c.keyName != "key" {
		t.Fatalf("unexpected client: %#v", c)
	}

	want := errors.New("factory")
	newKMSClient = func(context.Context, ...option.ClientOption) (kmsClient, error) { return nil, want }
	if _, err := New(context.Background(), "", "key", zap.NewNop()); !errors.Is(err, want) {
		t.Fatalf("expected factory error, got %v", err)
	}
}

func TestCloseReturnsClientError(t *testing.T) {
	want := errors.New("close failed")
	c := &Client{client: &fakeKMSClient{closeErr: want}, logger: zap.NewNop()}
	if err := c.Close(); !errors.Is(err, want) {
		t.Fatalf("expected close error, got %v", err)
	}
	c = &Client{client: &fakeKMSClient{}, logger: zap.NewNop()}
	if err := c.Close(); err != nil {
		t.Fatalf("expected nil close error, got %v", err)
	}
}
