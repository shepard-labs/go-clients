// Package gcs implements the storage.Storage interface backed by Google Cloud
// Storage.
package gcs

import (
	"context"
	"encoding/base64"
	"errors"
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

var newStorageClient = func(ctx context.Context, opts ...option.ClientOption) (gcsClient, error) {
	client, err := storage.NewClient(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return storageClientAdapter{client: client}, nil
}

// Client implements storage.Storage backed by Google Cloud Storage, bound to
// a single bucket.
type Client struct {
	client      gcsClient
	logger      *zap.Logger
	bucket      string
	serviceTag  string
	maxDownload int64
}

type gcsClient interface {
	Bucket(string) gcsBucket
	Close() error
}

type gcsBucket interface {
	Object(string) gcsObject
}

type gcsObject interface {
	NewWriter(context.Context) gcsWriter
	NewReader(context.Context) (io.ReadCloser, error)
	Delete(context.Context) error
}

type gcsWriter interface {
	io.Writer
	Close() error
	setContentType(string)
	setMetadata(map[string]string)
}

type storageClientAdapter struct{ client *storage.Client }
type storageBucketAdapter struct{ bucket *storage.BucketHandle }
type storageObjectAdapter struct{ object *storage.ObjectHandle }
type storageWriterAdapter struct{ writer *storage.Writer }

func (a storageClientAdapter) Bucket(name string) gcsBucket {
	return storageBucketAdapter{bucket: a.client.Bucket(name)}
}
func (a storageClientAdapter) Close() error { return a.client.Close() }
func (a storageBucketAdapter) Object(name string) gcsObject {
	return storageObjectAdapter{object: a.bucket.Object(name)}
}
func (a storageObjectAdapter) NewWriter(ctx context.Context) gcsWriter {
	return storageWriterAdapter{writer: a.object.NewWriter(ctx)}
}
func (a storageObjectAdapter) NewReader(ctx context.Context) (io.ReadCloser, error) {
	return a.object.NewReader(ctx)
}
func (a storageObjectAdapter) Delete(ctx context.Context) error       { return a.object.Delete(ctx) }
func (a storageWriterAdapter) Write(p []byte) (int, error)            { return a.writer.Write(p) }
func (a storageWriterAdapter) Close() error                           { return a.writer.Close() }
func (a storageWriterAdapter) setContentType(contentType string)      { a.writer.ContentType = contentType }
func (a storageWriterAdapter) setMetadata(metadata map[string]string) { a.writer.Metadata = metadata }

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
		opts = append(opts, option.WithAuthCredentialsJSON(option.ServiceAccount, serviceAccountJSON))
	}

	client, err := newStorageClient(ctx, opts...)
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
	writer.setContentType(contentType)
	writer.setMetadata(c.metadata())

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
	writer.setContentType(contentType)
	writer.setMetadata(c.metadata())

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
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, fmt.Errorf("%w: %s", clientstorage.ErrObjectNotFound, objectName)
		}
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
		if errors.Is(err, storage.ErrObjectNotExist) {
			return fmt.Errorf("%w: %s", clientstorage.ErrObjectNotFound, objectName)
		}
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
