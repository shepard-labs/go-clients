// Package r2 implements the storage.Storage interface backed by Cloudflare R2.
//
// R2 is S3-compatible, so this package is a thin delegating wrapper over the
// shared S3 core in storage/s3: New derives the R2 endpoint from the
// Cloudflare account ID and delegates to s3.New with TLS on and path-style
// bucket addressing (which R2 requires).
package r2

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"go.uber.org/zap"

	clientstorage "github.com/shepard-labs/go-clients/storage"
	"github.com/shepard-labs/go-clients/storage/s3"
)

// Ensure *Client satisfies the shared interface (via the embedded core).
var _ clientstorage.Storage = (*Client)(nil)

// Client is an R2-bound storage.Storage. All operations are served by the
// embedded shared S3-core Storage; endpoint and maxDownload are retained for
// observability and tests.
type Client struct {
	clientstorage.Storage
	endpoint    string
	maxDownload int64
}

// New constructs an R2-backed storage.Storage bound to bucket.
//
// The endpoint is derived from accountID (<accountID>.r2.cloudflarestorage.com).
// serviceTag, if non-empty, is written as a "service" metadata value on
// uploaded objects. maxDownloadBytes caps the size Download will buffer into
// memory; pass 0 to use storage.DefaultMaxDownloadBytes.
//
// NOTE: errors are emitted by the shared s3 core with the "r2" provider tag
// (e.g. "r2 upload failed"), preserving this package's historic error strings.
// Log message text matches modulo "R2"→"r2" casing and carries a
// provider="r2" zap field. The core also maps NoSuchBucket (in addition to
// NoSuchKey) to ErrObjectNotFound.
func New(accountID, accessKeyID, secretKey, bucket, serviceTag string, maxDownloadBytes int64, logger *zap.Logger) (clientstorage.Storage, error) {
	endpoint := fmt.Sprintf("%s.r2.cloudflarestorage.com", accountID)
	// Strip URL scheme if provided, mirroring the core; minio.New expects
	// host:port or hostname. Kept here so the retained endpoint field matches
	// what the core dials.
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
			endpoint = u.Host
		}
	}

	inner, err := s3.NewWithProvider(s3.Config{
		Endpoint:         endpoint,
		AccessKeyID:      accessKeyID,
		SecretAccessKey:  secretKey,
		Bucket:           bucket,
		ServiceTag:       serviceTag,
		MaxDownloadBytes: maxDownloadBytes,
		Secure:           true,
		// R2 requires path-style bucket access.
		BucketLookup: minio.BucketLookupPath,
	}, "r2", logger)
	if err != nil {
		return nil, err
	}

	if maxDownloadBytes <= 0 {
		maxDownloadBytes = clientstorage.DefaultMaxDownloadBytes
	}

	return &Client{
		Storage:     inner,
		endpoint:    endpoint,
		maxDownload: maxDownloadBytes,
	}, nil
}
