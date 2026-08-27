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
	"google.golang.org/api/iterator"
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
	Objects(context.Context, *storage.Query) gcsObjectIterator
}

type gcsObjectIterator interface {
	Next() (*storage.ObjectAttrs, error)
}

type gcsObject interface {
	NewWriter(context.Context) gcsWriter
	NewReader(context.Context) (io.ReadCloser, error)
	Attrs(context.Context) (*storage.ObjectAttrs, error)
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
type storageIteratorAdapter struct{ it *storage.ObjectIterator }

func (a storageClientAdapter) Bucket(name string) gcsBucket {
	return storageBucketAdapter{bucket: a.client.Bucket(name)}
}
func (a storageClientAdapter) Close() error { return a.client.Close() }
func (a storageBucketAdapter) Object(name string) gcsObject {
	return storageObjectAdapter{object: a.bucket.Object(name)}
}
func (a storageBucketAdapter) Objects(ctx context.Context, q *storage.Query) gcsObjectIterator {
	return storageIteratorAdapter{it: a.bucket.Objects(ctx, q)}
}
func (a storageIteratorAdapter) Next() (*storage.ObjectAttrs, error) { return a.it.Next() }
func (a storageObjectAdapter) NewWriter(ctx context.Context) gcsWriter {
	return storageWriterAdapter{writer: a.object.NewWriter(ctx)}
}
func (a storageObjectAdapter) NewReader(ctx context.Context) (io.ReadCloser, error) {
	return a.object.NewReader(ctx)
}
func (a storageObjectAdapter) Attrs(ctx context.Context) (*storage.ObjectAttrs, error) {
	return a.object.Attrs(ctx)
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

// Stat returns metadata for objectName without downloading its contents. It
// implements storage.Storage.
func (c *Client) Stat(ctx context.Context, objectName string) (clientstorage.ObjectInfo, error) {
	if err := clientstorage.ValidateObjectName(objectName); err != nil {
		return clientstorage.ObjectInfo{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	c.logger.Info("statting object in GCS", zap.String("bucket", c.bucket), zap.String("object", objectName))

	obj := c.client.Bucket(c.bucket).Object(objectName)
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return clientstorage.ObjectInfo{}, fmt.Errorf("%w: %s", clientstorage.ErrObjectNotFound, objectName)
		}
		c.logger.Warn("failed to stat object", zap.Error(err), zap.String("object", objectName))
		return clientstorage.ObjectInfo{}, fmt.Errorf("failed to stat object: %w", err)
	}

	return clientstorage.ObjectInfo{
		Name:        attrs.Name,
		Size:        attrs.Size,
		ContentType: attrs.ContentType,
		Updated:     attrs.Updated,
	}, nil
}

// Exists reports whether objectName exists, via a metadata-only check. It
// implements storage.Storage.
func (c *Client) Exists(ctx context.Context, objectName string) (bool, error) {
	if err := clientstorage.ValidateObjectName(objectName); err != nil {
		return false, err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	c.logger.Info("checking object existence in GCS", zap.String("bucket", c.bucket), zap.String("object", objectName))

	obj := c.client.Bucket(c.bucket).Object(objectName)
	if _, err := obj.Attrs(ctx); err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return false, nil
		}
		c.logger.Warn("failed to check object existence", zap.Error(err), zap.String("object", objectName))
		return false, fmt.Errorf("failed to check object existence: %w", err)
	}

	return true, nil
}

// DownloadReader streams the contents of objectName. It implements
// storage.Storage. The caller must Close the returned reader. Unlike Download
// the stream is uncapped: no maxDownload limit is applied, so bounding the
// read is the caller's responsibility.
func (c *Client) DownloadReader(ctx context.Context, objectName string) (io.ReadCloser, error) {
	if err := clientstorage.ValidateObjectName(objectName); err != nil {
		return nil, err
	}

	// Deliberately no context.WithTimeout here: the returned reader is a live
	// stream, and a deferred cancel would kill it as soon as this method
	// returns. The caller's context governs the stream's lifetime.
	c.logger.Info("opening download stream from GCS", zap.String("bucket", c.bucket), zap.String("object", objectName))

	obj := c.client.Bucket(c.bucket).Object(objectName)
	reader, err := obj.NewReader(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, fmt.Errorf("%w: %s", clientstorage.ErrObjectNotFound, objectName)
		}
		c.logger.Warn("failed to open object", zap.Error(err), zap.String("object", objectName))
		return nil, fmt.Errorf("failed to open object: %w", err)
	}

	return reader, nil
}

// List returns objects whose names begin with prefix. An empty prefix lists
// the whole bucket. It implements storage.Storage.
func (c *Client) List(ctx context.Context, prefix string) ([]clientstorage.ObjectInfo, error) {
	if prefix != "" {
		if err := clientstorage.ValidateObjectName(prefix); err != nil {
			return nil, err
		}
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	c.logger.Info("listing objects in GCS", zap.String("bucket", c.bucket), zap.String("prefix", prefix))

	it := c.client.Bucket(c.bucket).Objects(ctx, &storage.Query{Prefix: prefix})
	var objects []clientstorage.ObjectInfo
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			c.logger.Error("failed to list objects", zap.Error(err), zap.String("prefix", prefix))
			return nil, fmt.Errorf("failed to list objects: %w", err)
		}
		objects = append(objects, clientstorage.ObjectInfo{
			Name:        attrs.Name,
			Size:        attrs.Size,
			ContentType: attrs.ContentType,
			Updated:     attrs.Updated,
		})
	}

	c.logger.Info("objects listed successfully", zap.String("bucket", c.bucket), zap.String("prefix", prefix), zap.Int("count", len(objects)))
	return objects, nil
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
