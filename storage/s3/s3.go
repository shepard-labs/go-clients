// Package s3 implements the storage.Storage interface over any S3-compatible
// object store via the MinIO client.
//
// Why minio-go instead of aws-sdk-go-v2?
//
// The shared storage core performs a small, stable set of CRUD operations
// (put/get/stat/list/delete) against S3-compatible HTTP endpoints. minio-go
// provides a single wire path for AWS S3, MinIO, Backblaze B2, Wasabi, and
// GCS (via HMAC interoperability keys) with one client type, one auth
// mechanism (static access/secret key pairs), and one narrow interface to
// fake in tests. Adopting aws-sdk-go-v2 would add per-service endpoint and
// auth configuration plus a much larger API surface for features this package
// deliberately does not use.
//
// Revisit this choice if static keys stop being enough: IAM role assumption,
// session tokens, and server-side encryption with KMS (SSE-KMS) are
// first-class in aws-sdk-go-v2 and would justify migrating off minio-go.
package s3

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

// Config configures a generic S3-compatible storage.Client.
//
// Endpoint is the store's hostname, with or without an http(s):// scheme
// (for example "s3.us-east-1.amazonaws.com" or
// "https://s3.us-east-1.amazonaws.com"). Any scheme is stripped before
// dialing because minio.New expects a bare host. Use New or one of the
// provider constructors (NewAWS, NewMinIO, NewB2, NewWasabi, NewGCSHMAC)
// instead of hand-building this in most cases; reach for New directly only
// for an S3-compatible store with no dedicated constructor.
//
// Region is passed through to the MinIO client. An empty region lets the
// server resolve the bucket location (used by NewMinIO); hosted providers
// generally require an explicit region (for example "us-east-1").
//
// Secure selects TLS. It defaults to true for hosted providers; set it false
// only for a local MinIO (or other store) served over plain HTTP.
//
// BucketLookup selects virtual-host versus path-style bucket addressing.
// Hosted providers use minio.BucketLookupAuto; MinIO and GCS HMAC endpoints
// require minio.BucketLookupPath.
//
// MaxDownloadBytes caps the size Download will buffer into memory; a value
// <= 0 selects clientstorage.DefaultMaxDownloadBytes.
//
// Only static access/secret key pairs are supported; there is no session
// token, IAM, or KMS support (see the package rationale above).
type Config struct {
	Endpoint         string
	Region           string
	AccessKeyID      string
	SecretAccessKey  string
	Bucket           string
	ServiceTag       string
	MaxDownloadBytes int64
	Secure           bool
	BucketLookup     minio.BucketLookupType
}

// WithEndpoint overrides the Config.Endpoint derived by a provider
// constructor (NewAWS, NewB2, NewWasabi, NewGCSHMAC). It targets custom
// endpoints such as VPC endpoints, S3-compatible proxies, or LocalStack:
//
//	s, err := s3.NewAWS(key, secret, "us-east-1", bucket, tag, 0, logger,
//	    s3.WithEndpoint("https://vpce-1234-abcdef.s3.us-east-1.vpce.amazonaws.com"))
func WithEndpoint(endpoint string) func(*Config) {
	return func(c *Config) {
		c.Endpoint = endpoint
	}
}

// Client implements storage.Storage over an S3-compatible store, bound to a
// single bucket.
type Client struct {
	client      s3Client
	logger      *zap.Logger
	bucket      string
	endpoint    string
	region      string
	serviceTag  string
	maxDownload int64
	provider    string
}

// s3Object is a readable object handle returned by GetObject. It is satisfied
// by *minio.Object.
type s3Object interface {
	io.ReadCloser
	Stat() (minio.ObjectInfo, error)
}

type s3Client interface {
	PutObject(context.Context, string, string, io.Reader, int64, minio.PutObjectOptions) (minio.UploadInfo, error)
	GetObject(context.Context, string, string, minio.GetObjectOptions) (s3Object, error)
	StatObject(context.Context, string, string, minio.StatObjectOptions) (minio.ObjectInfo, error)
	ListObjects(context.Context, string, minio.ListObjectsOptions) <-chan minio.ObjectInfo
	RemoveObject(context.Context, string, string, minio.RemoveObjectOptions) error
}

type minioAdapter struct{ client *minio.Client }

func (a minioAdapter) PutObject(ctx context.Context, bucket, objectName string, r io.Reader, size int64, opts minio.PutObjectOptions) (minio.UploadInfo, error) {
	return a.client.PutObject(ctx, bucket, objectName, r, size, opts)
}

func (a minioAdapter) GetObject(ctx context.Context, bucket, objectName string, opts minio.GetObjectOptions) (s3Object, error) {
	return a.client.GetObject(ctx, bucket, objectName, opts)
}

func (a minioAdapter) StatObject(ctx context.Context, bucket, objectName string, opts minio.StatObjectOptions) (minio.ObjectInfo, error) {
	return a.client.StatObject(ctx, bucket, objectName, opts)
}

func (a minioAdapter) RemoveObject(ctx context.Context, bucket, objectName string, opts minio.RemoveObjectOptions) error {
	return a.client.RemoveObject(ctx, bucket, objectName, opts)
}

func (a minioAdapter) ListObjects(ctx context.Context, bucket string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo {
	return a.client.ListObjects(ctx, bucket, opts)
}

// New constructs a storage.Storage over an arbitrary S3-compatible endpoint
// from cfg.
//
// Endpoint accepts a bare host or an http(s):// URL; any scheme is stripped
// before dialing. Region is passed through to minio (empty lets the server
// resolve the bucket location). Secure selects TLS and BucketLookup selects
// the bucket addressing style; see Config for guidance. A MaxDownloadBytes
// <= 0 selects storage.DefaultMaxDownloadBytes. Static keys only.
//
// Prefer a provider constructor (NewAWS, NewMinIO, NewB2, NewWasabi,
// NewGCSHMAC) when one fits; use New for stores without a dedicated
// constructor.
func New(cfg Config, logger *zap.Logger) (clientstorage.Storage, error) {
	return newWithProvider(cfg, "s3", logger)
}

// NewWithProvider is New with an explicit provider tag. The tag prefixes all
// error strings ("%s upload failed", etc.) and log messages from the client,
// and is recorded in the "provider" zap field. It exists so packages wrapping
// the core (e.g. storage/r2, which passes "r2") preserve their historic
// observable strings; prefer New or a provider constructor otherwise.
func NewWithProvider(cfg Config, provider string, logger *zap.Logger) (clientstorage.Storage, error) {
	if provider == "" {
		provider = "s3"
	}
	return newWithProvider(cfg, provider, logger)
}

func newWithProvider(cfg Config, provider string, logger *zap.Logger) (clientstorage.Storage, error) {
	endpoint := cfg.Endpoint
	// Strip URL scheme if provided; minio.New expects host:port or hostname.
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
			endpoint = u.Host
		}
	}

	minioClient, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure:       cfg.Secure,
		Region:       cfg.Region,
		BucketLookup: cfg.BucketLookup,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create %s client: %w", provider, err)
	}

	if cfg.Bucket == "" || endpoint == "" {
		logger.Warn(provider+" client initialized with missing configuration",
			zap.String("provider", provider),
			zap.String("bucket", cfg.Bucket),
			zap.String("endpoint", endpoint),
			zap.String("region", cfg.Region),
		)
	}
	logger.Info(provider+" client initialized",
		zap.String("provider", provider),
		zap.String("bucket", cfg.Bucket),
		zap.String("endpoint", endpoint),
		zap.String("region", cfg.Region),
	)

	maxDownloadBytes := cfg.MaxDownloadBytes
	if maxDownloadBytes <= 0 {
		maxDownloadBytes = clientstorage.DefaultMaxDownloadBytes
	}

	return &Client{
		client:      minioAdapter{client: minioClient},
		logger:      logger,
		bucket:      cfg.Bucket,
		endpoint:    endpoint,
		region:      cfg.Region,
		serviceTag:  cfg.ServiceTag,
		maxDownload: maxDownloadBytes,
		provider:    provider,
	}, nil
}

// NewAWS constructs an AWS S3-backed storage.Storage bound to bucket.
//
// Endpoint format: "s3.<region>.amazonaws.com" (overridable per call with
// WithEndpoint for VPC endpoints, proxies, or LocalStack). Region is
// required and is also used as the client region; an empty region returns an
// error. TLS is always on (Secure true) and bucket addressing is
// minio.BucketLookupAuto. serviceTag, if non-empty, is written as a
// "service" metadata value on uploaded objects. maxDownloadBytes caps the
// size Download will buffer into memory; pass 0 to use
// storage.DefaultMaxDownloadBytes. Static keys only.
func NewAWS(accessKeyID, secretKey, region, bucket, serviceTag string, maxDownloadBytes int64, logger *zap.Logger, opts ...func(*Config)) (clientstorage.Storage, error) {
	if region == "" {
		return nil, fmt.Errorf("aws region is required")
	}
	cfg := Config{
		Endpoint:         fmt.Sprintf("s3.%s.amazonaws.com", region),
		Region:           region,
		AccessKeyID:      accessKeyID,
		SecretAccessKey:  secretKey,
		Bucket:           bucket,
		ServiceTag:       serviceTag,
		MaxDownloadBytes: maxDownloadBytes,
		Secure:           true,
		BucketLookup:     minio.BucketLookupAuto,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return newWithProvider(cfg, "aws", logger)
}

// NewMinIO constructs a MinIO-backed storage.Storage bound to bucket.
//
// Endpoint is a bare host or http(s):// URL for a self-hosted MinIO server
// (for example "play.min.io" or "http://localhost:9000"); any scheme is
// stripped before dialing. Region is left empty so the server resolves the
// bucket location. The secure flag selects TLS — pass false only for local
// plain-HTTP servers — and bucket addressing is minio.BucketLookupPath.
// serviceTag, if non-empty, is written as a "service" metadata value on
// uploaded objects. maxDownloadBytes caps the size Download will buffer into
// memory; pass 0 to use storage.DefaultMaxDownloadBytes. Static keys only.
func NewMinIO(endpoint, accessKeyID, secretKey, bucket, serviceTag string, maxDownloadBytes int64, secure bool, logger *zap.Logger) (clientstorage.Storage, error) {
	cfg := Config{
		Endpoint:         endpoint,
		AccessKeyID:      accessKeyID,
		SecretAccessKey:  secretKey,
		Bucket:           bucket,
		ServiceTag:       serviceTag,
		MaxDownloadBytes: maxDownloadBytes,
		Secure:           secure,
		BucketLookup:     minio.BucketLookupPath,
	}
	return newWithProvider(cfg, "minio", logger)
}

// NewB2 constructs a Backblaze B2 (S3-compatible) storage.Storage bound to
// bucket.
//
// Endpoint format: "s3.<region>.backblazeb2.com" (overridable per call with
// WithEndpoint). Region is required; an empty region returns an error. TLS
// is always on (Secure true) and bucket addressing is
// minio.BucketLookupAuto. serviceTag, if non-empty, is written as a
// "service" metadata value on uploaded objects. maxDownloadBytes caps the
// size Download will buffer into memory; pass 0 to use
// storage.DefaultMaxDownloadBytes. Static application keys only.
func NewB2(accessKeyID, secretKey, region, bucket, serviceTag string, maxDownloadBytes int64, logger *zap.Logger, opts ...func(*Config)) (clientstorage.Storage, error) {
	if region == "" {
		return nil, fmt.Errorf("b2 region is required")
	}
	cfg := Config{
		Endpoint:         fmt.Sprintf("s3.%s.backblazeb2.com", region),
		Region:           region,
		AccessKeyID:      accessKeyID,
		SecretAccessKey:  secretKey,
		Bucket:           bucket,
		ServiceTag:       serviceTag,
		MaxDownloadBytes: maxDownloadBytes,
		Secure:           true,
		BucketLookup:     minio.BucketLookupAuto,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return newWithProvider(cfg, "b2", logger)
}

// NewWasabi constructs a Wasabi (S3-compatible) storage.Storage bound to
// bucket.
//
// Endpoint format: "s3.<region>.wasabisys.com" (overridable per call with
// WithEndpoint). Region is required; an empty region returns an error. TLS
// is always on (Secure true) and bucket addressing is
// minio.BucketLookupAuto. serviceTag, if non-empty, is written as a
// "service" metadata value on uploaded objects. maxDownloadBytes caps the
// size Download will buffer into memory; pass 0 to use
// storage.DefaultMaxDownloadBytes. Static keys only.
func NewWasabi(accessKeyID, secretKey, region, bucket, serviceTag string, maxDownloadBytes int64, logger *zap.Logger, opts ...func(*Config)) (clientstorage.Storage, error) {
	if region == "" {
		return nil, fmt.Errorf("wasabi region is required")
	}
	cfg := Config{
		Endpoint:         fmt.Sprintf("s3.%s.wasabisys.com", region),
		Region:           region,
		AccessKeyID:      accessKeyID,
		SecretAccessKey:  secretKey,
		Bucket:           bucket,
		ServiceTag:       serviceTag,
		MaxDownloadBytes: maxDownloadBytes,
		Secure:           true,
		BucketLookup:     minio.BucketLookupAuto,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return newWithProvider(cfg, "wasabi", logger)
}

// NewGCSHMAC constructs a Google Cloud Storage storage.Storage bound to
// bucket, authenticated with HMAC interoperability keys via the
// S3-compatible endpoint.
//
// Endpoint is "storage.googleapis.com" (overridable per call with
// WithEndpoint, e.g. for the storage emulator). Region is fixed to "auto"
// and bucket addressing is minio.BucketLookupPath; TLS is always on (Secure
// true). serviceTag, if non-empty, is written as a "service" metadata value
// on uploaded objects. maxDownloadBytes caps the size Download will buffer
// into memory; pass 0 to use storage.DefaultMaxDownloadBytes. Static HMAC
// keys only — prefer the native storage/gcs package when ADC/service-account
// auth is available.
func NewGCSHMAC(accessKeyID, secretKey, bucket, serviceTag string, maxDownloadBytes int64, logger *zap.Logger, opts ...func(*Config)) (clientstorage.Storage, error) {
	cfg := Config{
		Endpoint:         "storage.googleapis.com",
		Region:           "auto",
		AccessKeyID:      accessKeyID,
		SecretAccessKey:  secretKey,
		Bucket:           bucket,
		ServiceTag:       serviceTag,
		MaxDownloadBytes: maxDownloadBytes,
		Secure:           true,
		BucketLookup:     minio.BucketLookupPath,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return newWithProvider(cfg, "gcshmac", logger)
}

// isNotFound reports whether err is an S3 "object does not exist" error,
// covering both a missing key and a missing bucket.
func isNotFound(err error) bool {
	code := minio.ToErrorResponse(err).Code
	return code == "NoSuchKey" || code == "NoSuchBucket"
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

	c.logger.Info("uploading document to "+c.provider,
		zap.String("provider", c.provider),
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
		c.logger.Error("failed to upload to "+c.provider, zap.String("provider", c.provider), zap.Error(err))
		return fmt.Errorf("%s upload failed: %w", c.provider, err)
	}
	return nil
}

// UploadReader streams size bytes from r into objectName. It implements
// storage.Storage. size is passed through to the underlying PutObject call;
// pass -1 when the length is unknown to stream without a predeclared
// content length.
func (c *Client) UploadReader(ctx context.Context, objectName string, r io.Reader, contentType string, size int64) error {
	if err := clientstorage.ValidateObjectName(objectName); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	c.logger.Info("uploading (reader) to "+c.provider,
		zap.String("provider", c.provider),
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
		c.logger.Error("failed to upload reader to "+c.provider, zap.String("provider", c.provider), zap.Error(err))
		return fmt.Errorf("%s upload reader failed: %w", c.provider, err)
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

	c.logger.Info("downloading document from "+c.provider,
		zap.String("provider", c.provider),
		zap.String("bucket", c.bucket),
		zap.String("object", objectName),
	)

	obj, err := c.client.GetObject(ctx, c.bucket, objectName, minio.GetObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", clientstorage.ErrObjectNotFound, objectName)
		}
		c.logger.Warn("failed to get object from "+c.provider, zap.String("provider", c.provider), zap.Error(err), zap.String("object", objectName))
		return nil, fmt.Errorf("%s get object failed: %w", c.provider, err)
	}
	defer obj.Close()

	// Read up to maxDownload+1 so that exceeding the limit is detectable rather
	// than silently truncated.
	data, err := io.ReadAll(io.LimitReader(obj, c.maxDownload+1))
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", clientstorage.ErrObjectNotFound, objectName)
		}
		c.logger.Error("failed to read object from "+c.provider, zap.String("provider", c.provider), zap.Error(err), zap.String("object", objectName))
		return nil, fmt.Errorf("%s read object failed: %w", c.provider, err)
	}
	if int64(len(data)) > c.maxDownload {
		return nil, fmt.Errorf("%w: %s", clientstorage.ErrObjectTooLarge, objectName)
	}
	return data, nil
}

// Stat returns metadata for objectName without downloading its contents. It
// implements storage.Storage.
func (c *Client) Stat(ctx context.Context, objectName string) (clientstorage.ObjectInfo, error) {
	if err := clientstorage.ValidateObjectName(objectName); err != nil {
		return clientstorage.ObjectInfo{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	c.logger.Info("stat object in "+c.provider,
		zap.String("provider", c.provider),
		zap.String("bucket", c.bucket),
		zap.String("object", objectName),
	)

	oi, err := c.client.StatObject(ctx, c.bucket, objectName, minio.StatObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return clientstorage.ObjectInfo{}, fmt.Errorf("%w: %s", clientstorage.ErrObjectNotFound, objectName)
		}
		c.logger.Warn("failed to stat object in "+c.provider, zap.String("provider", c.provider), zap.Error(err), zap.String("object", objectName))
		return clientstorage.ObjectInfo{}, fmt.Errorf("%s stat object failed: %w", c.provider, err)
	}

	return clientstorage.ObjectInfo{
		Name:        oi.Key,
		Size:        oi.Size,
		ContentType: oi.ContentType,
		Updated:     oi.LastModified,
	}, nil
}

// Exists reports whether objectName exists via a metadata-only check. It
// implements storage.Storage.
func (c *Client) Exists(ctx context.Context, objectName string) (bool, error) {
	if err := clientstorage.ValidateObjectName(objectName); err != nil {
		return false, err
	}

	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	c.logger.Info("checking object existence in "+c.provider,
		zap.String("provider", c.provider),
		zap.String("bucket", c.bucket),
		zap.String("object", objectName),
	)

	_, err := c.client.StatObject(ctx, c.bucket, objectName, minio.StatObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		c.logger.Warn("failed to stat object in "+c.provider, zap.String("provider", c.provider), zap.Error(err), zap.String("object", objectName))
		return false, fmt.Errorf("%s stat object failed: %w", c.provider, err)
	}
	return true, nil
}

// DownloadReader streams the contents of objectName. The caller must Close the
// returned reader. It implements storage.Storage.
//
// Unlike Download, the stream is uncapped (no max-download limit applies),
// and no client-side timeout is set: the returned reader outlives this call,
// so a WithTimeout here would cancel the caller's reads. The caller's context
// governs the stream's lifetime — bounding it is the caller's responsibility
// (do not io.ReadAll an object of unknown size).
func (c *Client) DownloadReader(ctx context.Context, objectName string) (io.ReadCloser, error) {
	if err := clientstorage.ValidateObjectName(objectName); err != nil {
		return nil, err
	}

	// No WithTimeout here: the returned stream lives past this function's
	// return, so a client-side deadline would cancel the caller's reads.
	c.logger.Info("streaming download from "+c.provider,
		zap.String("provider", c.provider),
		zap.String("bucket", c.bucket),
		zap.String("object", objectName),
	)

	obj, err := c.client.GetObject(ctx, c.bucket, objectName, minio.GetObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", clientstorage.ErrObjectNotFound, objectName)
		}
		c.logger.Warn("failed to get object from "+c.provider, zap.String("provider", c.provider), zap.Error(err), zap.String("object", objectName))
		return nil, fmt.Errorf("%s get object failed: %w", c.provider, err)
	}

	// MinIO's GetObject is lazy: it does not issue the request until first
	// use. Probe Stat to surface NoSuchKey eagerly, for parity with GCS.
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", clientstorage.ErrObjectNotFound, objectName)
		}
		c.logger.Warn("failed to stat object from "+c.provider, zap.String("provider", c.provider), zap.Error(err), zap.String("object", objectName))
		return nil, fmt.Errorf("%s stat object failed: %w", c.provider, err)
	}

	return obj, nil
}

// List returns objects whose names begin with prefix. An empty prefix lists
// the whole bucket. It implements storage.Storage.
func (c *Client) List(ctx context.Context, prefix string) ([]clientstorage.ObjectInfo, error) {
	if prefix != "" {
		if err := clientstorage.ValidateObjectName(prefix); err != nil {
			return nil, err
		}
	}

	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	c.logger.Info("listing objects in "+c.provider,
		zap.String("provider", c.provider),
		zap.String("bucket", c.bucket),
		zap.String("prefix", prefix),
	)

	var objects []clientstorage.ObjectInfo
	for obj := range c.client.ListObjects(ctx, c.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			c.logger.Error("failed to list objects in "+c.provider, zap.String("provider", c.provider), zap.Error(obj.Err), zap.String("prefix", prefix))
			return nil, fmt.Errorf("%s list objects failed: %w", c.provider, obj.Err)
		}
		objects = append(objects, clientstorage.ObjectInfo{
			Name:        obj.Key,
			Size:        obj.Size,
			ContentType: obj.ContentType,
			Updated:     obj.LastModified,
		})
	}

	c.logger.Info("objects listed successfully",
		zap.String("provider", c.provider),
		zap.String("bucket", c.bucket),
		zap.String("prefix", prefix),
		zap.Int("count", len(objects)),
	)
	return objects, nil
}

// Delete removes objectName. It implements storage.Storage.
func (c *Client) Delete(ctx context.Context, objectName string) error {
	if err := clientstorage.ValidateObjectName(objectName); err != nil {
		return err
	}

	c.logger.Info("deleting document from "+c.provider,
		zap.String("provider", c.provider),
		zap.String("bucket", c.bucket),
		zap.String("object", objectName),
	)

	if err := c.client.RemoveObject(ctx, c.bucket, objectName, minio.RemoveObjectOptions{}); err != nil {
		c.logger.Error("failed to delete from "+c.provider, zap.String("provider", c.provider), zap.Error(err))
		return fmt.Errorf("%s delete failed: %w", c.provider, err)
	}
	return nil
}

// Close releases any underlying client resources. It is a no-op for the
// MinIO-backed client. It implements storage.Storage.
func (c *Client) Close() error {
	c.logger.Info(c.provider+" client closed", zap.String("provider", c.provider))
	return nil
}
