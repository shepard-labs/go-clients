// Package kms implements credential encryption and decryption backed by
// Google Cloud KMS.
//
// The crypto key resource name and credentials are bound at construction.
package kms

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"hash/crc32"
	"time"

	kmsapi "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"go.uber.org/zap"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// maxPlaintextBytes is the largest plaintext Cloud KMS will encrypt in a single
// Encrypt call. Inputs larger than this are rejected before the RPC.
const maxPlaintextBytes = 64 * 1024

// ErrPlaintextTooLarge indicates the plaintext exceeds the KMS per-call limit.
var ErrPlaintextTooLarge = errors.New("plaintext exceeds maximum size")

// Encryptor encrypts and decrypts short credential strings using a bound KMS
// key. Implementations are safe for concurrent use.
type Encryptor interface {
	// EncryptCredentials encrypts plaintext and returns base64-encoded ciphertext.
	EncryptCredentials(ctx context.Context, plaintext string) (string, error)

	// DecryptCredentials decrypts base64-encoded ciphertext and returns the plaintext.
	DecryptCredentials(ctx context.Context, ciphertext string) (string, error)

	// Close releases the underlying KMS client.
	Close() error
}

// Client implements Encryptor backed by Google Cloud KMS.
type Client struct {
	client  *kmsapi.KeyManagementClient
	keyName string
	logger  *zap.Logger
}

// Ensure *Client satisfies the Encryptor interface.
var _ Encryptor = (*Client)(nil)

// New constructs a KMS-backed Encryptor bound to keyName.
//
// keyName is the full KMS crypto key resource name, e.g.
// "projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>".
// serviceAccount, if non-empty, is a base64-encoded service account JSON key;
// when empty, Application Default Credentials are used.
func New(ctx context.Context, serviceAccount, keyName string, logger *zap.Logger) (Encryptor, error) {
	var opts []option.ClientOption

	if serviceAccount != "" {
		serviceAccountJSON, err := base64.StdEncoding.DecodeString(serviceAccount)
		if err != nil {
			return nil, fmt.Errorf("failed to decode service account JSON: %w", err)
		}
		opts = append(opts, option.WithCredentialsJSON(serviceAccountJSON))
	}

	client, err := kmsapi.NewKeyManagementClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create KMS client: %w", err)
	}

	return &Client{
		client:  client,
		keyName: keyName,
		logger:  logger,
	}, nil
}

func crc32c(data []byte) uint32 {
	return crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))
}

// EncryptCredentials encrypts plaintext credentials using KMS and returns
// base64-encoded ciphertext.
func (c *Client) EncryptCredentials(ctx context.Context, plaintext string) (string, error) {
	if len(plaintext) > maxPlaintextBytes {
		return "", fmt.Errorf("%w: %d bytes (max %d)", ErrPlaintextTooLarge, len(plaintext), maxPlaintextBytes)
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	plaintextCRC32C := crc32c([]byte(plaintext))

	req := &kmspb.EncryptRequest{
		Name:            c.keyName,
		Plaintext:       []byte(plaintext),
		PlaintextCrc32C: wrapperspb.Int64(int64(plaintextCRC32C)),
	}

	var result *kmspb.EncryptResponse
	var err error

	for attempt := 0; attempt < 3; attempt++ {
		result, err = c.client.Encrypt(ctx, req)
		if err == nil {
			break
		}
		c.logger.Warn("KMS encrypt attempt failed, retrying",
			zap.Int("attempt", attempt+1),
			zap.Error(err))
		time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
	}

	if err != nil {
		c.logger.Error("failed to encrypt credentials after retries", zap.Error(err))
		return "", fmt.Errorf("failed to encrypt credentials: %w", err)
	}

	if !result.VerifiedPlaintextCrc32C {
		return "", fmt.Errorf("encrypt: request corrupted in-transit")
	}
	if int64(crc32c(result.Ciphertext)) != result.CiphertextCrc32C.Value {
		return "", fmt.Errorf("encrypt: response corrupted in-transit")
	}

	return base64.StdEncoding.EncodeToString(result.Ciphertext), nil
}

// DecryptCredentials decrypts base64-encoded ciphertext using KMS and returns
// the plaintext credentials.
func (c *Client) DecryptCredentials(ctx context.Context, ciphertext string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	decoded, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("failed to decode ciphertext: %w", err)
	}

	ciphertextCRC32C := crc32c(decoded)

	req := &kmspb.DecryptRequest{
		Name:             c.keyName,
		Ciphertext:       decoded,
		CiphertextCrc32C: wrapperspb.Int64(int64(ciphertextCRC32C)),
	}

	var result *kmspb.DecryptResponse

	for attempt := 0; attempt < 3; attempt++ {
		result, err = c.client.Decrypt(ctx, req)
		if err == nil {
			break
		}
		c.logger.Warn("KMS decrypt attempt failed, retrying",
			zap.Int("attempt", attempt+1),
			zap.Error(err))
		time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
	}

	if err != nil {
		c.logger.Error("failed to decrypt credentials after retries", zap.Error(err))
		return "", fmt.Errorf("failed to decrypt credentials: %w", err)
	}

	if int64(crc32c(result.Plaintext)) != result.PlaintextCrc32C.Value {
		return "", fmt.Errorf("decrypt: response corrupted in-transit")
	}

	return string(result.Plaintext), nil
}

// Close closes the underlying KMS client. It implements Encryptor.
func (c *Client) Close() error {
	if err := c.client.Close(); err != nil {
		c.logger.Error("failed to close KMS client", zap.Error(err))
		return err
	}
	c.logger.Info("KMS client closed")
	return nil
}
