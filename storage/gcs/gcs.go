// Package gcs implements the storage.Storage interface backed by Google Cloud
// Storage.
package gcs

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"time"

	"cloud.google.com/go/storage"
	"go.uber.org/zap"
	"google.golang.org/api/option"

	clientstorage "github.com/shepard-labs/go-clients/storage"
)

// Ensure *Client satisfies the shared interface.
var _ clientstorage.Storage = (*Client)(nil)

// Client implements storage.Storage backed by Google Cloud Storage, bound to
// a single bucket.
type Client struct {
	client      *storage.Client
	logger      *zap.Logger
	bucket      string
	serviceTag  string
	maxDownload int64
}

// New constructs a GCS-backed storage.Storage bound to bucket.
//
// serviceAccount, if non-empty, is a base64-encoded service account JSON key
// used for authentication; when empty, Application Default Credentials are
// used. serviceTag, if non-empty, is written as a "service" metadata value on
// uploaded objects. maxDownloadBytes caps the size Download will buffer into
// memory; pass 0 to use storage.DefaultMaxDownloadBytes.
func New(ctx context.Context, serviceAccount, bucket, serviceTag string, maxDownloadBytes int64, logger *zap.Logger) (clientstorage.Storage, error) {
	var opts []option.ClientOption

	if serviceAccount != "" {
		serviceAccountJSON, err := base64.StdEncoding.DecodeString(serviceAccount)
		if err != nil {
			return nil, fmt.Errorf("failed to decode service account JSON: %w", err)
		}
		opts = append(opts, option.WithCredentialsJSON(serviceAccountJSON))
	}

	client, err := storage.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage client: %w", err)
	}

	if maxDownloadBytes <= 0 {
		maxDownloadBytes = clientstorage.DefaultMaxDownloadBytes
	}

	return &Client{
		client:      client,
		logger:      logger,
		bucket:      bucket,
		serviceTag:  serviceTag,
		maxDownload: maxDownloadBytes,
	}, nil
}

// metadata builds the object metadata map, including the service tag only when set.
func (c *Client) metadata() map[string]string {
	m := map[string]string{
		"uploaded_at": time.Now().UTC().Format(time.RFC3339),
	}
	if c.serviceTag != "" {
		m["service"] = c.serviceTag
	}
	return m
}

// Upload stores content under objectName. It implements storage.Storage.
func (c *Client) Upload(ctx context.Context, objectName string, content []byte, contentType string) error {
	if err := clientstorage.ValidateObjectName(objectName); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	c.logger.Info("uploading document to GCS", zap.String("bucket", c.bucket), zap.String("object", objectName), zap.Int("size", len(content)))

	obj := c.client.Bucket(c.bucket).Object(objectName)
	writer := obj.NewWriter(ctx)
	writer.ContentType = contentType
	writer.Metadata = c.metadata()

	if _, err := writer.Write(content); err != nil {
		writer.Close()
		c.logger.Error("failed to write document", zap.Error(err))
		return fmt.Errorf("failed to write document: %w", err)
	}

	if err := writer.Close(); err != nil {
		c.logger.Error("failed to close writer", zap.Error(err))
		return fmt.Errorf("failed to close writer: %w", err)
	}

	c.logger.Info("document uploaded successfully", zap.String("bucket", c.bucket), zap.String("object", objectName))
	return nil
}

// UploadReader streams from r into objectName. It implements storage.Storage.
// GCS streams without a known content length, so size is accepted for
// interface parity but not used.
func (c *Client) UploadReader(ctx context.Context, objectName string, r io.Reader, contentType string, size int64) error {
	if err := clientstorage.ValidateObjectName(objectName); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	c.logger.Info("uploading (reader) to GCS", zap.String("bucket", c.bucket), zap.String("object", objectName), zap.Int64("size", size))

	obj := c.client.Bucket(c.bucket).Object(objectName)
	writer := obj.NewWriter(ctx)
	writer.ContentType = contentType
	writer.Metadata = c.metadata()

	if _, err := io.Copy(writer, r); err != nil {
		writer.Close()
		c.logger.Error("failed to stream document", zap.Error(err))
		return fmt.Errorf("failed to stream document: %w", err)
	}

	if err := writer.Close(); err != nil {
		c.logger.Error("failed to close writer", zap.Error(err))
		return fmt.Errorf("failed to close writer: %w", err)
	}

	c.logger.Info("document uploaded successfully", zap.String("bucket", c.bucket), zap.String("object", objectName))
	return nil
}

// Download returns the contents of objectName. It implements storage.Storage.
// Objects larger than the configured maximum return ErrObjectTooLarge.
func (c *Client) Download(ctx context.Context, objectName string) ([]byte, error) {
	if err := clientstorage.ValidateObjectName(objectName); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	c.logger.Info("downloading document from GCS", zap.String("bucket", c.bucket), zap.String("object", objectName))

	obj := c.client.Bucket(c.bucket).Object(objectName)
	reader, err := obj.NewReader(ctx)
	if err != nil {
		c.logger.Warn("failed to create reader", zap.Error(err), zap.String("object", objectName))
		return nil, fmt.Errorf("failed to open object: %w", err)
	}
	defer reader.Close()

	// Read up to maxDownload+1 so that exceeding the limit is detectable rather
	// than silently truncated.
	content, err := io.ReadAll(io.LimitReader(reader, c.maxDownload+1))
	if err != nil {
		c.logger.Error("failed to read content", zap.Error(err), zap.String("object", objectName))
		return nil, fmt.Errorf("failed to read content: %w", err)
	}
	if int64(len(content)) > c.maxDownload {
		return nil, fmt.Errorf("%w: %s", clientstorage.ErrObjectTooLarge, objectName)
	}

	return content, nil
}

// Delete removes objectName. It implements storage.Storage.
func (c *Client) Delete(ctx context.Context, objectName string) error {
	if err := clientstorage.ValidateObjectName(objectName); err != nil {
		return err
	}

	c.logger.Info("deleting document from GCS", zap.String("bucket", c.bucket), zap.String("object", objectName))

	obj := c.client.Bucket(c.bucket).Object(objectName)
	if err := obj.Delete(ctx); err != nil {
		c.logger.Error("failed to delete document", zap.Error(err))
		return fmt.Errorf("failed to delete document: %w", err)
	}

	c.logger.Info("document deleted successfully", zap.String("bucket", c.bucket), zap.String("object", objectName))
	return nil
}

// Close releases the underlying GCS client. It implements storage.Storage.
func (c *Client) Close() error {
	if err := c.client.Close(); err != nil {
		c.logger.Error("failed to close storage client", zap.Error(err))
		return err
	}
	c.logger.Info("GCS client closed")
	return nil
}
