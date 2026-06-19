// Package r2 implements the storage.Storage interface backed by Cloudflare R2
// via the S3-compatible MinIO client.
package r2

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"go.uber.org/zap"

	clientstorage "github.com/shepard-labs/go-clients/storage"
)

const defaultTimeout = 5 * time.Minute

// Ensure *Client satisfies the shared interface.
var _ clientstorage.Storage = (*Client)(nil)

// Client implements storage.Storage backed by Cloudflare R2, bound to a single
// bucket.
type Client struct {
	client      r2Client
	logger      *zap.Logger
	bucket      string
	endpoint    string
	serviceTag  string
	maxDownload int64
}

type r2Client interface {
	PutObject(context.Context, string, string, io.Reader, int64, minio.PutObjectOptions) (minio.UploadInfo, error)
	GetObject(context.Context, string, string, minio.GetObjectOptions) (io.ReadCloser, error)
	RemoveObject(context.Context, string, string, minio.RemoveObjectOptions) error
}

type minioAdapter struct{ client *minio.Client }

func (a minioAdapter) PutObject(ctx context.Context, bucket, objectName string, r io.Reader, size int64, opts minio.PutObjectOptions) (minio.UploadInfo, error) {
	return a.client.PutObject(ctx, bucket, objectName, r, size, opts)
}

func (a minioAdapter) GetObject(ctx context.Context, bucket, objectName string, opts minio.GetObjectOptions) (io.ReadCloser, error) {
	return a.client.GetObject(ctx, bucket, objectName, opts)
}

func (a minioAdapter) RemoveObject(ctx context.Context, bucket, objectName string, opts minio.RemoveObjectOptions) error {
	return a.client.RemoveObject(ctx, bucket, objectName, opts)
}

// New constructs an R2-backed storage.Storage bound to bucket.
//
// The endpoint is derived from accountID (<accountID>.r2.cloudflarestorage.com).
// serviceTag, if non-empty, is written as a "service" metadata value on
// uploaded objects. maxDownloadBytes caps the size Download will buffer into
// memory; pass 0 to use storage.DefaultMaxDownloadBytes.
func New(accountID, accessKeyID, secretKey, bucket, serviceTag string, maxDownloadBytes int64, logger *zap.Logger) (clientstorage.Storage, error) {
	endpoint := fmt.Sprintf("%s.r2.cloudflarestorage.com", accountID)
	// Strip URL scheme if provided; minio.New expects host:port or hostname.
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
			endpoint = u.Host
		}
	}

	minioClient, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKeyID, secretKey, ""),
		Secure: true,
		// R2 requires path-style bucket access.
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create R2 client: %w", err)
	}

	if bucket == "" || endpoint == "" {
		logger.Warn("R2 client initialized with missing configuration", zap.String("bucket", bucket), zap.String("endpoint", endpoint))
	}
	logger.Info("Cloudflare R2 client initialized", zap.String("bucket", bucket), zap.String("endpoint", endpoint))

	if maxDownloadBytes <= 0 {
		maxDownloadBytes = clientstorage.DefaultMaxDownloadBytes
	}

	return &Client{
		client:      minioAdapter{client: minioClient},
		logger:      logger,
		bucket:      bucket,
		endpoint:    endpoint,
		serviceTag:  serviceTag,
		maxDownload: maxDownloadBytes,
	}, nil
}

// metadata builds the user-metadata map, including the service tag only when set.
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

	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	c.logger.Info("uploading document to R2",
		zap.String("bucket", c.bucket),
		zap.String("object", objectName),
		zap.Int("size", len(content)),
		zap.String("content_type", contentType),
	)

	_, err := c.client.PutObject(ctx, c.bucket, objectName, bytes.NewReader(content), int64(len(content)), minio.PutObjectOptions{
		ContentType:  contentType,
		UserMetadata: c.metadata(),
	})
	if err != nil {
		c.logger.Error("failed to upload to R2", zap.Error(err))
		return fmt.Errorf("r2 upload failed: %w", err)
	}
	return nil
}

// UploadReader streams size bytes from r into objectName. It implements
// storage.Storage.
func (c *Client) UploadReader(ctx context.Context, objectName string, r io.Reader, contentType string, size int64) error {
	if err := clientstorage.ValidateObjectName(objectName); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	c.logger.Info("uploading (reader) to R2",
		zap.String("bucket", c.bucket),
		zap.String("object", objectName),
		zap.Int64("size", size),
		zap.String("content_type", contentType),
	)

	_, err := c.client.PutObject(ctx, c.bucket, objectName, r, size, minio.PutObjectOptions{
		ContentType:  contentType,
		UserMetadata: c.metadata(),
	})
	if err != nil {
		c.logger.Error("failed to upload reader to R2", zap.Error(err))
		return fmt.Errorf("r2 upload reader failed: %w", err)
	}
	return nil
}

// Download returns the contents of objectName. It implements storage.Storage.
// Objects larger than the configured maximum return ErrObjectTooLarge.
func (c *Client) Download(ctx context.Context, objectName string) ([]byte, error) {
	if err := clientstorage.ValidateObjectName(objectName); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	c.logger.Info("downloading document from R2",
		zap.String("bucket", c.bucket),
		zap.String("object", objectName),
	)

	obj, err := c.client.GetObject(ctx, c.bucket, objectName, minio.GetObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, fmt.Errorf("%w: %s", clientstorage.ErrObjectNotFound, objectName)
		}
		c.logger.Warn("failed to get object from R2", zap.Error(err), zap.String("object", objectName))
		return nil, fmt.Errorf("r2 get object failed: %w", err)
	}
	defer obj.Close()

	// Read up to maxDownload+1 so that exceeding the limit is detectable rather
	// than silently truncated.
	data, err := io.ReadAll(io.LimitReader(obj, c.maxDownload+1))
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, fmt.Errorf("%w: %s", clientstorage.ErrObjectNotFound, objectName)
		}
		c.logger.Error("failed to read object from R2", zap.Error(err), zap.String("object", objectName))
		return nil, fmt.Errorf("r2 read object failed: %w", err)
	}
	if int64(len(data)) > c.maxDownload {
		return nil, fmt.Errorf("%w: %s", clientstorage.ErrObjectTooLarge, objectName)
	}
	return data, nil
}

// Delete removes objectName. It implements storage.Storage.
func (c *Client) Delete(ctx context.Context, objectName string) error {
	if err := clientstorage.ValidateObjectName(objectName); err != nil {
		return err
	}

	c.logger.Info("deleting document from R2",
		zap.String("bucket", c.bucket),
		zap.String("object", objectName),
	)

	if err := c.client.RemoveObject(ctx, c.bucket, objectName, minio.RemoveObjectOptions{}); err != nil {
		c.logger.Error("failed to delete from R2", zap.Error(err))
		return fmt.Errorf("r2 delete failed: %w", err)
	}
	return nil
}

// Close is a no-op for the MinIO-backed R2 client. It implements storage.Storage.
func (c *Client) Close() error {
	c.logger.Info("R2 client closed")
	return nil
}
