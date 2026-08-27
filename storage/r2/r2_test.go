package r2

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"go.uber.org/zap"

	clientstorage "github.com/shepard-labs/go-clients/storage"
)

type fakeR2Client struct {
	putBucket  string
	putObject  string
	putBody    string
	putType    string
	putMeta    map[string]string
	putErr     error
	getBody    string
	getReader  io.ReadCloser
	getErr     error
	getStatErr error
	statInfo   minio.ObjectInfo
	statErr    error
	removeErr  error

	listObjects []minio.ObjectInfo
	listPrefix  string
}

func (f *fakeR2Client) PutObject(_ context.Context, bucket, objectName string, r io.Reader, size int64, opts minio.PutObjectOptions) (minio.UploadInfo, error) {
	f.putBucket = bucket
	f.putObject = objectName
	f.putType = opts.ContentType
	f.putMeta = opts.UserMetadata
	b, _ := io.ReadAll(r)
	f.putBody = string(b)
	if f.putErr != nil {
		return minio.UploadInfo{}, f.putErr
	}
	return minio.UploadInfo{Size: size}, nil
}

type fakeR2Object struct {
	io.ReadCloser
	statErr error
}

func (f fakeR2Object) Stat() (minio.ObjectInfo, error) { return minio.ObjectInfo{}, f.statErr }

func (f *fakeR2Client) GetObject(context.Context, string, string, minio.GetObjectOptions) (r2Object, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.getReader != nil {
		return fakeR2Object{ReadCloser: f.getReader, statErr: f.getStatErr}, nil
	}
	return fakeR2Object{ReadCloser: io.NopCloser(strings.NewReader(f.getBody)), statErr: f.getStatErr}, nil
}

func (f *fakeR2Client) StatObject(context.Context, string, string, minio.StatObjectOptions) (minio.ObjectInfo, error) {
	return f.statInfo, f.statErr
}

type failingReadCloser struct{ err error }

func (f failingReadCloser) Read([]byte) (int, error) { return 0, f.err }
func (f failingReadCloser) Close() error             { return nil }

func (f *fakeR2Client) RemoveObject(context.Context, string, string, minio.RemoveObjectOptions) error {
	return f.removeErr
}

func (f *fakeR2Client) ListObjects(_ context.Context, _ string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo {
	f.listPrefix = opts.Prefix
	ch := make(chan minio.ObjectInfo, len(f.listObjects))
	for _, obj := range f.listObjects {
		ch <- obj
	}
	close(ch)
	return ch
}

func testClient(fake *fakeR2Client, maxDownload int64) *Client {
	return &Client{client: fake, logger: zap.NewNop(), bucket: "bucket", endpoint: "endpoint", serviceTag: "svc", maxDownload: maxDownload}
}

func TestUploadAndUploadReader(t *testing.T) {
	fake := &fakeR2Client{}
	c := testClient(fake, 10)
	if err := c.Upload(context.Background(), "path/object", []byte("data"), "text/plain"); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	if fake.putBucket != "bucket" || fake.putObject != "path/object" || fake.putBody != "data" || fake.putType != "text/plain" {
		t.Fatalf("unexpected put state: %#v", fake)
	}
	if fake.putMeta["service"] != "svc" || fake.putMeta["uploaded_at"] == "" {
		t.Fatalf("metadata not set: %#v", fake.putMeta)
	}

	if err := c.UploadReader(context.Background(), "reader", strings.NewReader("stream"), "application/octet-stream", 6); err != nil {
		t.Fatalf("UploadReader failed: %v", err)
	}
	if fake.putBody != "stream" {
		t.Fatalf("unexpected reader body %q", fake.putBody)
	}
}

func TestDownloadAndDelete(t *testing.T) {
	fake := &fakeR2Client{getBody: "content"}
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
	if err := c.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

func TestDownloadErrors(t *testing.T) {
	c := testClient(&fakeR2Client{getBody: "toolong"}, 3)
	if _, err := c.Download(context.Background(), "object"); !errors.Is(err, clientstorage.ErrObjectTooLarge) {
		t.Fatalf("expected too large, got %v", err)
	}

	missing := minio.ErrorResponse{Code: "NoSuchKey"}
	c = testClient(&fakeR2Client{getErr: missing}, 10)
	if _, err := c.Download(context.Background(), "object"); !errors.Is(err, clientstorage.ErrObjectNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}

	c = testClient(&fakeR2Client{getErr: errors.New("boom")}, 10)
	if _, err := c.Download(context.Background(), "object"); err == nil {
		t.Fatal("expected get error")
	}

	c = testClient(&fakeR2Client{getReader: failingReadCloser{err: missing}}, 10)
	if _, err := c.Download(context.Background(), "object"); !errors.Is(err, clientstorage.ErrObjectNotFound) {
		t.Fatalf("expected read not found, got %v", err)
	}
	c = testClient(&fakeR2Client{getReader: failingReadCloser{err: errors.New("read")}}, 10)
	if _, err := c.Download(context.Background(), "object"); err == nil {
		t.Fatal("expected read error")
	}
}

func TestStat(t *testing.T) {
	updated := time.Now()
	fake := &fakeR2Client{statInfo: minio.ObjectInfo{Key: "obj", Size: 5, ContentType: "text/plain", LastModified: updated}}
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
	missing := minio.ErrorResponse{Code: "NoSuchKey"}

	c := testClient(&fakeR2Client{statErr: missing}, 10)
	if _, err := c.Stat(context.Background(), "obj"); !errors.Is(err, clientstorage.ErrObjectNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}

	c = testClient(&fakeR2Client{statErr: errors.New("boom")}, 10)
	if _, err := c.Stat(context.Background(), "obj"); err == nil {
		t.Fatal("expected stat error")
	}

	c = testClient(&fakeR2Client{statErr: missing}, 10)
	exists, err := c.Exists(context.Background(), "obj")
	if err != nil || exists {
		t.Fatalf("expected (false, nil), got (%v, %v)", exists, err)
	}

	c = testClient(&fakeR2Client{statErr: errors.New("boom")}, 10)
	exists, err = c.Exists(context.Background(), "obj")
	if err == nil || exists {
		t.Fatalf("expected (false, err), got (%v, %v)", exists, err)
	}

	c = testClient(&fakeR2Client{statInfo: minio.ObjectInfo{Key: "obj", Size: 5}}, 10)
	exists, err = c.Exists(context.Background(), "obj")
	if err != nil || !exists {
		t.Fatalf("expected (true, nil), got (%v, %v)", exists, err)
	}
}

func TestDownloadReader(t *testing.T) {
	missing := minio.ErrorResponse{Code: "NoSuchKey"}

	c := testClient(&fakeR2Client{getBody: "content"}, 10)
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

	c = testClient(&fakeR2Client{getErr: missing}, 10)
	if _, err := c.DownloadReader(context.Background(), "obj"); !errors.Is(err, clientstorage.ErrObjectNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}

	// Eager Stat probe surfaces lazy-GetObject NoSuchKey.
	c = testClient(&fakeR2Client{getBody: "content", getStatErr: missing}, 10)
	if _, err := c.DownloadReader(context.Background(), "obj"); !errors.Is(err, clientstorage.ErrObjectNotFound) {
		t.Fatalf("expected not found from stat probe, got %v", err)
	}

	c = testClient(&fakeR2Client{}, 10)
	if _, err := c.DownloadReader(context.Background(), "/bad"); err == nil {
		t.Fatal("expected validation error")
	}
	if _, err := c.DownloadReader(context.Background(), ""); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestList(t *testing.T) {
	updated := time.Now()
	fake := &fakeR2Client{listObjects: []minio.ObjectInfo{
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
	if len(objects) != 2 {
		t.Fatalf("unexpected object count: %d", len(objects))
	}
	if objects[0].Name != "pre/a" || objects[0].Size != 1 || objects[0].ContentType != "text/plain" || !objects[0].Updated.Equal(updated) {
		t.Fatalf("unexpected object info: %#v", objects[0])
	}
}

func TestListErrors(t *testing.T) {
	c := testClient(&fakeR2Client{}, 10)
	if _, err := c.List(context.Background(), "../bad"); err == nil {
		t.Fatal("expected prefix validation error")
	}

	c = testClient(&fakeR2Client{listObjects: []minio.ObjectInfo{{Err: errors.New("list")}}}, 10)
	if _, err := c.List(context.Background(), ""); err == nil {
		t.Fatal("expected list error")
	}
}

func TestValidationAndOperationErrors(t *testing.T) {
	c := testClient(&fakeR2Client{}, 10)
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

	if err := testClient(&fakeR2Client{putErr: errors.New("put")}, 10).Upload(context.Background(), "object", []byte("x"), "text/plain"); err == nil {
		t.Fatal("expected put error")
	}
	if err := testClient(&fakeR2Client{putErr: errors.New("put reader")}, 10).UploadReader(context.Background(), "object", strings.NewReader("x"), "text/plain", 1); err == nil {
		t.Fatal("expected put reader error")
	}
	if err := testClient(&fakeR2Client{removeErr: errors.New("remove")}, 10).Delete(context.Background(), "object"); err == nil {
		t.Fatal("expected remove error")
	}
}

func TestNewDefaultsMaxDownload(t *testing.T) {
	s, err := New("acct", "key", "secret", "bucket", "svc", 0, zap.NewNop())
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	c := s.(*Client)
	if c.endpoint != "acct.r2.cloudflarestorage.com" || c.maxDownload != clientstorage.DefaultMaxDownloadBytes {
		t.Fatalf("unexpected client: %#v", c)
	}
}

func TestNewUsesExplicitMaxDownload(t *testing.T) {
	s, err := New("acct", "key", "secret", "", "svc", 123, zap.NewNop())
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	c := s.(*Client)
	if c.maxDownload != 123 {
		t.Fatalf("unexpected client: %#v", c)
	}
}

func TestMetadataOmitsEmptyServiceTag(t *testing.T) {
	c := &Client{}
	m := c.metadata()
	if _, ok := m["service"]; ok {
		t.Fatalf("unexpected service metadata: %#v", m)
	}
}
