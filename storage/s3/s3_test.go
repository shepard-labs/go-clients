package s3

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	clientstorage "github.com/shepard-labs/go-clients/storage"
)

type fakeS3Client struct {
	putBucket string
	putObject string
	putBody   string
	putType   string
	putMeta   map[string]string
	putSize   int64
	putOpts   minio.PutObjectOptions
	putErr    error

	getBody    string
	getReader  io.ReadCloser
	getErr     error
	getStatErr error

	statInfo minio.ObjectInfo
	statErr  error

	removeBucket string
	removeObject string
	removeErr    error

	listBucket  string
	listObjects []minio.ObjectInfo
	listPrefix  string
}

func (f *fakeS3Client) PutObject(_ context.Context, bucket, objectName string, r io.Reader, size int64, opts minio.PutObjectOptions) (minio.UploadInfo, error) {
	f.putBucket = bucket
	f.putObject = objectName
	f.putType = opts.ContentType
	f.putMeta = opts.UserMetadata
	f.putSize = size
	f.putOpts = opts
	b, _ := io.ReadAll(r)
	f.putBody = string(b)
	if f.putErr != nil {
		return minio.UploadInfo{}, f.putErr
	}
	return minio.UploadInfo{Size: size}, nil
}

type fakeS3Object struct {
	io.ReadCloser
	statErr error
}

func (f fakeS3Object) Stat() (minio.ObjectInfo, error) { return minio.ObjectInfo{}, f.statErr }

func (f *fakeS3Client) GetObject(_ context.Context, bucket, objectName string, _ minio.GetObjectOptions) (s3Object, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.getReader != nil {
		return fakeS3Object{ReadCloser: f.getReader, statErr: f.getStatErr}, nil
	}
	return fakeS3Object{ReadCloser: io.NopCloser(strings.NewReader(f.getBody)), statErr: f.getStatErr}, nil
}

func (f *fakeS3Client) StatObject(context.Context, string, string, minio.StatObjectOptions) (minio.ObjectInfo, error) {
	return f.statInfo, f.statErr
}

type failingReadCloser struct{ err error }

func (f failingReadCloser) Read([]byte) (int, error) { return 0, f.err }
func (f failingReadCloser) Close() error             { return nil }

func (f *fakeS3Client) RemoveObject(_ context.Context, bucket, objectName string, _ minio.RemoveObjectOptions) error {
	f.removeBucket = bucket
	f.removeObject = objectName
	return f.removeErr
}

func (f *fakeS3Client) ListObjects(_ context.Context, bucket string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo {
	f.listBucket = bucket
	f.listPrefix = opts.Prefix
	ch := make(chan minio.ObjectInfo, len(f.listObjects))
	for _, obj := range f.listObjects {
		ch <- obj
	}
	close(ch)
	return ch
}

func testClient(fake *fakeS3Client, maxDownload int64) *Client {
	return &Client{client: fake, logger: zap.NewNop(), bucket: "bucket", endpoint: "endpoint", serviceTag: "svc", maxDownload: maxDownload, provider: "s3"}
}

func TestUploadAndUploadReader(t *testing.T) {
	fake := &fakeS3Client{}
	c := testClient(fake, 10)
	if err := c.Upload(context.Background(), "path/object", []byte("data"), "text/plain"); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	if fake.putBucket != "bucket" || fake.putObject != "path/object" || fake.putBody != "data" || fake.putType != "text/plain" {
		t.Fatalf("unexpected put state: bucket=%q object=%q body=%q type=%q", fake.putBucket, fake.putObject, fake.putBody, fake.putType)
	}
	if fake.putMeta["service"] != "svc" || fake.putMeta["uploaded_at"] == "" {
		t.Fatalf("metadata not set: %#v", fake.putMeta)
	}
	if fake.putSize != int64(len("data")) {
		t.Fatalf("unexpected put size: %d", fake.putSize)
	}

	if err := c.UploadReader(context.Background(), "reader", strings.NewReader("stream"), "application/octet-stream", 6); err != nil {
		t.Fatalf("UploadReader failed: %v", err)
	}
	if fake.putBody != "stream" {
		t.Fatalf("unexpected reader body %q", fake.putBody)
	}
	if fake.putSize != 6 {
		t.Fatalf("upload size option not passed through: %d", fake.putSize)
	}
	if fake.putOpts.ContentType != "application/octet-stream" {
		t.Fatalf("upload content-type option not passed through: %q", fake.putOpts.ContentType)
	}
}

func TestDownloadAndDelete(t *testing.T) {
	fake := &fakeS3Client{getBody: "content"}
	c := testClient(fake, 20)
	data, err := c.Download(context.Background(), "object")
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if string(data) != "content" {
		t.Fatalf("unexpected data %q", data)
	}
	if err := c.Delete(context.Background(), "object"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if fake.removeBucket != "bucket" || fake.removeObject != "object" {
		t.Fatalf("unexpected remove state: bucket=%q object=%q", fake.removeBucket, fake.removeObject)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

func TestDownloadErrors(t *testing.T) {
	c := testClient(&fakeS3Client{getBody: "toolong"}, 3)
	if _, err := c.Download(context.Background(), "object"); !errors.Is(err, clientstorage.ErrObjectTooLarge) {
		t.Fatalf("expected too large, got %v", err)
	}

	for _, code := range []string{"NoSuchKey", "NoSuchBucket"} {
		missing := minio.ErrorResponse{Code: code}
		c = testClient(&fakeS3Client{getErr: missing}, 10)
		if _, err := c.Download(context.Background(), "object"); !errors.Is(err, clientstorage.ErrObjectNotFound) {
			t.Fatalf("code %s: expected not found, got %v", code, err)
		}
	}

	c = testClient(&fakeS3Client{getErr: errors.New("boom")}, 10)
	if _, err := c.Download(context.Background(), "object"); err == nil {
		t.Fatal("expected get error")
	}

	for _, code := range []string{"NoSuchKey", "NoSuchBucket"} {
		missing := minio.ErrorResponse{Code: code}
		c = testClient(&fakeS3Client{getReader: failingReadCloser{err: missing}}, 10)
		if _, err := c.Download(context.Background(), "object"); !errors.Is(err, clientstorage.ErrObjectNotFound) {
			t.Fatalf("code %s: expected read not found, got %v", code, err)
		}
	}
	c = testClient(&fakeS3Client{getReader: failingReadCloser{err: errors.New("read")}}, 10)
	if _, err := c.Download(context.Background(), "object"); err == nil {
		t.Fatal("expected read error")
	}
}

func TestStat(t *testing.T) {
	updated := time.Now()
	fake := &fakeS3Client{statInfo: minio.ObjectInfo{Key: "obj", Size: 5, ContentType: "text/plain", LastModified: updated}}
	c := testClient(fake, 10)

	info, err := c.Stat(context.Background(), "obj")
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if info.Name != "obj" || info.Size != 5 || info.ContentType != "text/plain" || !info.Updated.Equal(updated) {
		t.Fatalf("unexpected object info: %#v", info)
	}
}

func TestStatExistsErrors(t *testing.T) {
	for _, code := range []string{"NoSuchKey", "NoSuchBucket"} {
		missing := minio.ErrorResponse{Code: code}

		c := testClient(&fakeS3Client{statErr: missing}, 10)
		if _, err := c.Stat(context.Background(), "obj"); !errors.Is(err, clientstorage.ErrObjectNotFound) {
			t.Fatalf("code %s: expected not found, got %v", code, err)
		}

		c = testClient(&fakeS3Client{statErr: missing}, 10)
		exists, err := c.Exists(context.Background(), "obj")
		if err != nil || exists {
			t.Fatalf("code %s: expected (false, nil), got (%v, %v)", code, exists, err)
		}
	}

	c := testClient(&fakeS3Client{statErr: errors.New("boom")}, 10)
	if _, err := c.Stat(context.Background(), "obj"); err == nil {
		t.Fatal("expected stat error")
	}

	c = testClient(&fakeS3Client{statErr: errors.New("boom")}, 10)
	exists, err := c.Exists(context.Background(), "obj")
	if err == nil || exists {
		t.Fatalf("expected (false, err), got (%v, %v)", exists, err)
	}

	c = testClient(&fakeS3Client{statInfo: minio.ObjectInfo{Key: "obj", Size: 5}}, 10)
	exists, err = c.Exists(context.Background(), "obj")
	if err != nil || !exists {
		t.Fatalf("expected (true, nil), got (%v, %v)", exists, err)
	}
}

func TestDownloadReader(t *testing.T) {
	c := testClient(&fakeS3Client{getBody: "content"}, 10)
	r, err := c.DownloadReader(context.Background(), "obj")
	if err != nil {
		t.Fatalf("DownloadReader failed: %v", err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if string(data) != "content" {
		t.Fatalf("unexpected data %q", data)
	}

	for _, code := range []string{"NoSuchKey", "NoSuchBucket"} {
		missing := minio.ErrorResponse{Code: code}

		c = testClient(&fakeS3Client{getErr: missing}, 10)
		if _, err := c.DownloadReader(context.Background(), "obj"); !errors.Is(err, clientstorage.ErrObjectNotFound) {
			t.Fatalf("code %s: expected not found, got %v", code, err)
		}

		// Eager Stat probe surfaces lazy-GetObject NoSuchKey.
		c = testClient(&fakeS3Client{getBody: "content", getStatErr: missing}, 10)
		if _, err := c.DownloadReader(context.Background(), "obj"); !errors.Is(err, clientstorage.ErrObjectNotFound) {
			t.Fatalf("code %s: expected not found from stat probe, got %v", code, err)
		}
	}

	c = testClient(&fakeS3Client{getErr: errors.New("boom")}, 10)
	if _, err := c.DownloadReader(context.Background(), "obj"); err == nil {
		t.Fatal("expected get error")
	}

	c = testClient(&fakeS3Client{getBody: "content", getStatErr: errors.New("stat")}, 10)
	if _, err := c.DownloadReader(context.Background(), "obj"); err == nil {
		t.Fatal("expected stat probe error")
	}

	c = testClient(&fakeS3Client{}, 10)
	if _, err := c.DownloadReader(context.Background(), "/bad"); err == nil {
		t.Fatal("expected validation error")
	}
	if _, err := c.DownloadReader(context.Background(), ""); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestList(t *testing.T) {
	updated := time.Now()
	fake := &fakeS3Client{listObjects: []minio.ObjectInfo{
		{Key: "pre/a", Size: 1, ContentType: "text/plain", LastModified: updated},
		{Key: "pre/b", Size: 2},
	}}
	c := testClient(fake, 10)

	objects, err := c.List(context.Background(), "pre")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if fake.listPrefix != "pre" {
		t.Fatalf("prefix not passed through: %q", fake.listPrefix)
	}
	if fake.listBucket != "bucket" {
		t.Fatalf("bucket not passed through: %q", fake.listBucket)
	}
	if len(objects) != 2 {
		t.Fatalf("unexpected object count: %d", len(objects))
	}
	if objects[0].Name != "pre/a" || objects[0].Size != 1 || objects[0].ContentType != "text/plain" || !objects[0].Updated.Equal(updated) {
		t.Fatalf("unexpected object info: %#v", objects[0])
	}
}

func TestListErrors(t *testing.T) {
	c := testClient(&fakeS3Client{}, 10)
	if _, err := c.List(context.Background(), "../bad"); err == nil {
		t.Fatal("expected prefix validation error")
	}

	c = testClient(&fakeS3Client{listObjects: []minio.ObjectInfo{{Err: errors.New("list")}}}, 10)
	if _, err := c.List(context.Background(), ""); err == nil {
		t.Fatal("expected list error")
	}
}

func TestValidationAndOperationErrors(t *testing.T) {
	c := testClient(&fakeS3Client{}, 10)
	if err := c.Upload(context.Background(), "../bad", nil, ""); err == nil {
		t.Fatal("expected upload validation error")
	}
	if err := c.UploadReader(context.Background(), "", strings.NewReader(""), "", 0); err == nil {
		t.Fatal("expected upload reader validation error")
	}
	if _, err := c.Download(context.Background(), "/bad"); err == nil {
		t.Fatal("expected download validation error")
	}
	if err := c.Delete(context.Background(), "../bad"); err == nil {
		t.Fatal("expected delete validation error")
	}

	if err := testClient(&fakeS3Client{putErr: errors.New("put")}, 10).Upload(context.Background(), "object", []byte("x"), "text/plain"); err == nil {
		t.Fatal("expected put error")
	}
	if err := testClient(&fakeS3Client{putErr: errors.New("put reader")}, 10).UploadReader(context.Background(), "object", strings.NewReader("x"), "text/plain", 1); err == nil {
		t.Fatal("expected put reader error")
	}
	if err := testClient(&fakeS3Client{removeErr: errors.New("remove")}, 10).Delete(context.Background(), "object"); err == nil {
		t.Fatal("expected remove error")
	}
}

func TestNewDefaultsMaxDownload(t *testing.T) {
	s, err := New(Config{
		Endpoint:        "example.com",
		AccessKeyID:     "key",
		SecretAccessKey: "secret",
		Bucket:          "bucket",
		ServiceTag:      "svc",
		Secure:          true,
	}, zap.NewNop())
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	c := s.(*Client)
	if c.maxDownload != clientstorage.DefaultMaxDownloadBytes {
		t.Fatalf("unexpected maxDownload: %d", c.maxDownload)
	}
	if c.provider != "s3" {
		t.Fatalf("unexpected provider: %q", c.provider)
	}
}

func TestNewUsesExplicitMaxDownload(t *testing.T) {
	s, err := New(Config{
		Endpoint:         "example.com",
		AccessKeyID:      "key",
		SecretAccessKey:  "secret",
		ServiceTag:       "svc",
		MaxDownloadBytes: 123,
		Secure:           true,
	}, zap.NewNop())
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	c := s.(*Client)
	if c.maxDownload != 123 {
		t.Fatalf("unexpected maxDownload: %d", c.maxDownload)
	}
}

func TestNewAWSEndpoint(t *testing.T) {
	s, err := NewAWS("key", "secret", "us-east-1", "bucket", "svc", 0, zap.NewNop())
	if err != nil {
		t.Fatalf("NewAWS failed: %v", err)
	}
	c := s.(*Client)
	if c.endpoint != "s3.us-east-1.amazonaws.com" {
		t.Fatalf("unexpected endpoint: %q", c.endpoint)
	}
	if c.region != "us-east-1" {
		t.Fatalf("unexpected region: %q", c.region)
	}
	if c.provider != "aws" {
		t.Fatalf("unexpected provider: %q", c.provider)
	}
	if c.maxDownload != clientstorage.DefaultMaxDownloadBytes {
		t.Fatalf("unexpected maxDownload: %d", c.maxDownload)
	}

	if _, err := NewAWS("key", "secret", "", "bucket", "svc", 0, zap.NewNop()); err == nil {
		t.Fatal("expected empty-region error")
	}
}

func TestNewMinIOSchemeStripAndSecure(t *testing.T) {
	s, err := NewMinIO("https://host:9000", "key", "secret", "bucket", "svc", 0, true, zap.NewNop())
	if err != nil {
		t.Fatalf("NewMinIO failed: %v", err)
	}
	c := s.(*Client)
	if c.endpoint != "host:9000" {
		t.Fatalf("scheme not stripped: %q", c.endpoint)
	}
	if c.provider != "minio" {
		t.Fatalf("unexpected provider: %q", c.provider)
	}

	// Secure is consumed by minio.New and is not retained on Client;
	// both variants must at least construct successfully.
	for _, secure := range []bool{true, false} {
		if _, err := NewMinIO("http://localhost:9000", "key", "secret", "bucket", "svc", 0, secure, zap.NewNop()); err != nil {
			t.Fatalf("NewMinIO secure=%v failed: %v", secure, err)
		}
	}

	s, err = NewMinIO("play.min.io", "key", "secret", "bucket", "svc", 0, true, zap.NewNop())
	if err != nil {
		t.Fatalf("NewMinIO failed: %v", err)
	}
	if c := s.(*Client); c.endpoint != "play.min.io" {
		t.Fatalf("bare host must pass through unchanged: %q", c.endpoint)
	}
}

func TestNewB2Endpoint(t *testing.T) {
	s, err := NewB2("key", "secret", "us-west-004", "bucket", "svc", 0, zap.NewNop())
	if err != nil {
		t.Fatalf("NewB2 failed: %v", err)
	}
	c := s.(*Client)
	if c.endpoint != "s3.us-west-004.backblazeb2.com" {
		t.Fatalf("unexpected endpoint: %q", c.endpoint)
	}
	if c.provider != "b2" {
		t.Fatalf("unexpected provider: %q", c.provider)
	}

	if _, err := NewB2("key", "secret", "", "bucket", "svc", 0, zap.NewNop()); err == nil {
		t.Fatal("expected empty-region error")
	}
}

func TestNewWasabiEndpoint(t *testing.T) {
	s, err := NewWasabi("key", "secret", "us-east-1", "bucket", "svc", 0, zap.NewNop())
	if err != nil {
		t.Fatalf("NewWasabi failed: %v", err)
	}
	c := s.(*Client)
	if c.endpoint != "s3.us-east-1.wasabisys.com" {
		t.Fatalf("unexpected endpoint: %q", c.endpoint)
	}
	if c.provider != "wasabi" {
		t.Fatalf("unexpected provider: %q", c.provider)
	}

	if _, err := NewWasabi("key", "secret", "", "bucket", "svc", 0, zap.NewNop()); err == nil {
		t.Fatal("expected empty-region error")
	}
}

func TestNewGCSHMACEndpoint(t *testing.T) {
	s, err := NewGCSHMAC("key", "secret", "bucket", "svc", 0, zap.NewNop())
	if err != nil {
		t.Fatalf("NewGCSHMAC failed: %v", err)
	}
	c := s.(*Client)
	if c.endpoint != "storage.googleapis.com" {
		t.Fatalf("unexpected endpoint: %q", c.endpoint)
	}
	if c.region != "auto" {
		t.Fatalf("unexpected region: %q", c.region)
	}
	if c.provider != "gcshmac" {
		t.Fatalf("unexpected provider: %q", c.provider)
	}
}

func TestWithEndpointOverride(t *testing.T) {
	s, err := NewAWS("key", "secret", "us-east-1", "bucket", "svc", 0, zap.NewNop(),
		WithEndpoint("https://vpce-1234-abcdef.s3.us-east-1.vpce.amazonaws.com"))
	if err != nil {
		t.Fatalf("NewAWS failed: %v", err)
	}
	if c := s.(*Client); c.endpoint != "vpce-1234-abcdef.s3.us-east-1.vpce.amazonaws.com" {
		t.Fatalf("WithEndpoint not applied: %q", c.endpoint)
	}

	s, err = NewB2("key", "secret", "us-west-004", "bucket", "svc", 0, zap.NewNop(),
		WithEndpoint("https://proxy.example.com:8443"))
	if err != nil {
		t.Fatalf("NewB2 failed: %v", err)
	}
	if c := s.(*Client); c.endpoint != "proxy.example.com:8443" {
		t.Fatalf("WithEndpoint not applied: %q", c.endpoint)
	}
}

func TestProviderTagInErrors(t *testing.T) {
	// The core tags each provider ("aws", "minio", "b2", "wasabi",
	// "gcshmac"); the generic New hardcodes "s3".
	for _, provider := range []string{"aws", "gcshmac", "s3"} {
		fake := &fakeS3Client{putErr: errors.New("boom")}
		c := testClient(fake, 10)
		c.provider = provider
		err := c.Upload(context.Background(), "object", []byte("x"), "text/plain")
		if err == nil {
			t.Fatalf("provider %s: expected upload error", provider)
		}
		if !strings.Contains(err.Error(), provider) {
			t.Fatalf("provider %s: error %q missing provider tag", provider, err)
		}
	}

	if _, err := NewAWS("key", "secret", "", "bucket", "svc", 0, zap.NewNop()); err == nil || !strings.Contains(err.Error(), "aws") {
		t.Fatalf("expected aws-tagged region error, got %v", err)
	}
}

func TestSecretHygiene(t *testing.T) {
	const accessKey = "AKIAEXAMPLEKEY123"
	const secret = "supersecretvalue456"

	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core)

	// Constructor path: init logs must not carry credentials.
	s, err := NewAWS(accessKey, secret, "us-east-1", "bucket", "svc", 0, logger)
	if err != nil {
		t.Fatalf("NewAWS failed: %v", err)
	}
	c := s.(*Client)
	// Failing-path operation: error logs must not carry credentials either.
	c.client = &fakeS3Client{putErr: errors.New("boom")}
	c.logger = logger
	if err := c.Upload(context.Background(), "object", []byte("x"), "text/plain"); err == nil {
		t.Fatal("expected upload error")
	}

	for _, entry := range logs.All() {
		if strings.Contains(entry.Message, accessKey) || strings.Contains(entry.Message, secret) {
			t.Fatalf("log message leaks credential: %q", entry.Message)
		}
		for k, v := range entry.ContextMap() {
			vs, ok := v.(string)
			if !ok {
				continue
			}
			if strings.Contains(vs, accessKey) || strings.Contains(vs, secret) {
				t.Fatalf("log field %q leaks credential: %q", k, vs)
			}
		}
	}
}

func TestMetadataOmitsEmptyServiceTag(t *testing.T) {
	c := &Client{}
	m := c.metadata()
	if _, ok := m["service"]; ok {
		t.Fatalf("unexpected service metadata: %#v", m)
	}
	if m["uploaded_at"] == "" {
		t.Fatalf("uploaded_at must always be set: %#v", m)
	}
}
