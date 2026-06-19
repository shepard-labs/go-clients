package gcs

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"go.uber.org/zap"
	"google.golang.org/api/option"

	clientstorage "github.com/shepard-labs/go-clients/storage"
)

type fakeGCSClient struct {
	bucket   *fakeGCSBucket
	closeErr error
}

func (f *fakeGCSClient) Bucket(string) gcsBucket { return f.bucket }
func (f *fakeGCSClient) Close() error            { return f.closeErr }

type fakeGCSBucket struct{ object *fakeGCSObject }

func (f *fakeGCSBucket) Object(name string) gcsObject {
	f.object.name = name
	return f.object
}

type fakeGCSObject struct {
	name      string
	body      string
	readerErr error
	deleteErr error
	writer    *fakeGCSWriter
}

func (f *fakeGCSObject) NewWriter(context.Context) gcsWriter {
	if f.writer == nil {
		f.writer = &fakeGCSWriter{}
	}
	return f.writer
}

func (f *fakeGCSObject) NewReader(context.Context) (io.ReadCloser, error) {
	if f.readerErr != nil {
		return nil, f.readerErr
	}
	return io.NopCloser(strings.NewReader(f.body)), nil
}

func (f *fakeGCSObject) Delete(context.Context) error { return f.deleteErr }

type fakeGCSWriter struct {
	body        strings.Builder
	contentType string
	metadata    map[string]string
	writeErr    error
	closeErr    error
	closed      bool
}

func (f *fakeGCSWriter) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.body.Write(p)
}

func (f *fakeGCSWriter) Close() error {
	f.closed = true
	return f.closeErr
}

func (f *fakeGCSWriter) setContentType(contentType string)      { f.contentType = contentType }
func (f *fakeGCSWriter) setMetadata(metadata map[string]string) { f.metadata = metadata }

func newFakeClient(obj *fakeGCSObject, maxDownload int64) *Client {
	return &Client{
		client:      &fakeGCSClient{bucket: &fakeGCSBucket{object: obj}},
		logger:      zap.NewNop(),
		bucket:      "bucket",
		serviceTag:  "svc",
		maxDownload: maxDownload,
	}
}

func TestUploadAndUploadReader(t *testing.T) {
	obj := &fakeGCSObject{}
	c := newFakeClient(obj, 10)
	if err := c.Upload(context.Background(), "object", []byte("data"), "text/plain"); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	if obj.name != "object" || obj.writer.body.String() != "data" || obj.writer.contentType != "text/plain" {
		t.Fatalf("unexpected writer state: %#v", obj.writer)
	}
	if obj.writer.metadata["service"] != "svc" || obj.writer.metadata["uploaded_at"] == "" {
		t.Fatalf("metadata not set: %#v", obj.writer.metadata)
	}

	obj.writer = &fakeGCSWriter{}
	if err := c.UploadReader(context.Background(), "reader", strings.NewReader("stream"), "application/octet-stream", 6); err != nil {
		t.Fatalf("UploadReader failed: %v", err)
	}
	if obj.writer.body.String() != "stream" || obj.writer.contentType != "application/octet-stream" {
		t.Fatalf("unexpected reader upload: %#v", obj.writer)
	}
}

func TestDownloadDeleteAndClose(t *testing.T) {
	obj := &fakeGCSObject{body: "content"}
	c := newFakeClient(obj, 20)
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

func TestDownloadAndDeleteErrors(t *testing.T) {
	if _, err := newFakeClient(&fakeGCSObject{body: "toolong"}, 3).Download(context.Background(), "object"); !errors.Is(err, clientstorage.ErrObjectTooLarge) {
		t.Fatalf("expected too large, got %v", err)
	}
	if _, err := newFakeClient(&fakeGCSObject{readerErr: storage.ErrObjectNotExist}, 10).Download(context.Background(), "object"); !errors.Is(err, clientstorage.ErrObjectNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
	if _, err := newFakeClient(&fakeGCSObject{readerErr: errors.New("read")}, 10).Download(context.Background(), "object"); err == nil {
		t.Fatal("expected reader error")
	}
	if err := newFakeClient(&fakeGCSObject{deleteErr: storage.ErrObjectNotExist}, 10).Delete(context.Background(), "object"); !errors.Is(err, clientstorage.ErrObjectNotFound) {
		t.Fatalf("expected delete not found, got %v", err)
	}
	if err := newFakeClient(&fakeGCSObject{deleteErr: errors.New("delete")}, 10).Delete(context.Background(), "object"); err == nil {
		t.Fatal("expected delete error")
	}
}

func TestUploadErrorsAndValidation(t *testing.T) {
	c := newFakeClient(&fakeGCSObject{}, 10)
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

	obj := &fakeGCSObject{writer: &fakeGCSWriter{writeErr: errors.New("write")}}
	if err := newFakeClient(obj, 10).Upload(context.Background(), "object", []byte("x"), "text/plain"); err == nil {
		t.Fatal("expected write error")
	}
	obj = &fakeGCSObject{writer: &fakeGCSWriter{closeErr: errors.New("close")}}
	if err := newFakeClient(obj, 10).UploadReader(context.Background(), "object", strings.NewReader("x"), "text/plain", 1); err == nil {
		t.Fatal("expected close error")
	}
	obj = &fakeGCSObject{writer: &fakeGCSWriter{writeErr: errors.New("copy")}}
	if err := newFakeClient(obj, 10).UploadReader(context.Background(), "object", strings.NewReader("x"), "text/plain", 1); err == nil {
		t.Fatal("expected copy error")
	}
	obj = &fakeGCSObject{writer: &fakeGCSWriter{closeErr: errors.New("close")}}
	if err := newFakeClient(obj, 10).Upload(context.Background(), "object", []byte("x"), "text/plain"); err == nil {
		t.Fatal("expected upload close error")
	}
}

func TestNewRejectsInvalidServiceAccountAndCloseError(t *testing.T) {
	if _, err := New(context.Background(), "not-base64", "bucket", "svc", 0, zap.NewNop()); err == nil {
		t.Fatal("expected invalid service account error")
	}
	original := newStorageClient
	defer func() { newStorageClient = original }()
	fake := &fakeGCSClient{bucket: &fakeGCSBucket{object: &fakeGCSObject{}}}
	newStorageClient = func(context.Context, ...option.ClientOption) (gcsClient, error) { return fake, nil }
	s, err := New(context.Background(), base64.StdEncoding.EncodeToString([]byte(`{}`)), "bucket", "svc", 0, zap.NewNop())
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	c := s.(*Client)
	if c.client != fake || c.maxDownload != clientstorage.DefaultMaxDownloadBytes {
		t.Fatalf("unexpected client: %#v", c)
	}
	wantFactory := errors.New("factory")
	newStorageClient = func(context.Context, ...option.ClientOption) (gcsClient, error) { return nil, wantFactory }
	if _, err := New(context.Background(), "", "bucket", "svc", 1, zap.NewNop()); !errors.Is(err, wantFactory) {
		t.Fatalf("expected factory error, got %v", err)
	}

	want := errors.New("close")
	c = &Client{client: &fakeGCSClient{closeErr: want}, logger: zap.NewNop()}
	if err := c.Close(); !errors.Is(err, want) {
		t.Fatalf("expected close error, got %v", err)
	}
}

func TestMetadataOmitsEmptyServiceTag(t *testing.T) {
	c := &Client{}
	m := c.metadata()
	if _, ok := m["service"]; ok {
		t.Fatalf("unexpected service metadata: %#v", m)
	}
}

func TestStorageWriterAdapter(t *testing.T) {
	w := storageWriterAdapter{writer: &storage.Writer{}}
	w.setContentType("text/plain")
	w.setMetadata(map[string]string{"k": "v"})
	if w.writer.ContentType != "text/plain" || w.writer.Metadata["k"] != "v" {
		t.Fatalf("adapter did not set writer fields: %#v", w.writer)
	}
}
