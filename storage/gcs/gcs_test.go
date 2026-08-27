package gcs

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"go.uber.org/zap"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	clientstorage "github.com/shepard-labs/go-clients/storage"
)

type fakeGCSClient struct {
	bucket   *fakeGCSBucket
	closeErr error
}

func (f *fakeGCSClient) Bucket(string) gcsBucket { return f.bucket }
func (f *fakeGCSClient) Close() error            { return f.closeErr }

type fakeGCSBucket struct {
	object     *fakeGCSObject
	list       *fakeGCSIterator
	listPrefix string
}

func (f *fakeGCSBucket) Object(name string) gcsObject {
	f.object.name = name
	return f.object
}

func (f *fakeGCSBucket) Objects(_ context.Context, q *storage.Query) gcsObjectIterator {
	f.listPrefix = q.Prefix
	if f.list == nil {
		f.list = &fakeGCSIterator{}
	}
	return f.list
}

type fakeGCSIterator struct {
	attrs []*storage.ObjectAttrs
	err   error
	idx   int
}

func (f *fakeGCSIterator) Next() (*storage.ObjectAttrs, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.idx >= len(f.attrs) {
		return nil, iterator.Done
	}
	f.idx++
	return f.attrs[f.idx-1], nil
}

type fakeGCSObject struct {
	name      string
	body      string
	readerErr error
	deleteErr error
	attrs     *storage.ObjectAttrs
	attrsErr  error
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

func (f *fakeGCSObject) Attrs(context.Context) (*storage.ObjectAttrs, error) {
	if f.attrsErr != nil {
		return nil, f.attrsErr
	}
	if f.attrs != nil {
		return f.attrs, nil
	}
	return &storage.ObjectAttrs{Name: f.name}, nil
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

func TestList(t *testing.T) {
	updated := time.Now()
	bucket := &fakeGCSBucket{
		object: &fakeGCSObject{},
		list: &fakeGCSIterator{attrs: []*storage.ObjectAttrs{
			{Name: "pre/a", Size: 1, ContentType: "text/plain", Updated: updated},
			{Name: "pre/b", Size: 2},
		}},
	}
	c := &Client{client: &fakeGCSClient{bucket: bucket}, logger: zap.NewNop(), bucket: "bucket"}

	objects, err := c.List(context.Background(), "pre")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if bucket.listPrefix != "pre" {
		t.Fatalf("prefix not passed through: %q", bucket.listPrefix)
	}
	if len(objects) != 2 {
		t.Fatalf("unexpected object count: %d", len(objects))
	}
	if objects[0].Name != "pre/a" || objects[0].Size != 1 || objects[0].ContentType != "text/plain" || !objects[0].Updated.Equal(updated) {
		t.Fatalf("unexpected object info: %#v", objects[0])
	}
}

func TestListErrors(t *testing.T) {
	bucket := &fakeGCSBucket{object: &fakeGCSObject{}}
	c := &Client{client: &fakeGCSClient{bucket: bucket}, logger: zap.NewNop(), bucket: "bucket"}

	if _, err := c.List(context.Background(), "../bad"); err == nil {
		t.Fatal("expected prefix validation error")
	}

	bucket.list = &fakeGCSIterator{err: errors.New("iterate")}
	if _, err := c.List(context.Background(), ""); err == nil {
		t.Fatal("expected iterator error")
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

func TestStat(t *testing.T) {
	updated := time.Now()
	obj := &fakeGCSObject{attrs: &storage.ObjectAttrs{Name: "obj", Size: 5, ContentType: "text/plain", Updated: updated}}
	c := newFakeClient(obj, 10)

	info, err := c.Stat(context.Background(), "obj")
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if info.Name != "obj" || info.Size != 5 || info.ContentType != "text/plain" || !info.Updated.Equal(updated) {
		t.Fatalf("unexpected object info: %#v", info)
	}
}

func TestExistsStatErrors(t *testing.T) {
	if _, err := newFakeClient(&fakeGCSObject{attrsErr: storage.ErrObjectNotExist}, 10).Stat(context.Background(), "object"); !errors.Is(err, clientstorage.ErrObjectNotFound) {
		t.Fatalf("expected stat not found, got %v", err)
	}
	if _, err := newFakeClient(&fakeGCSObject{attrsErr: errors.New("attrs")}, 10).Stat(context.Background(), "object"); err == nil {
		t.Fatal("expected stat error")
	}

	exists, err := newFakeClient(&fakeGCSObject{attrsErr: storage.ErrObjectNotExist}, 10).Exists(context.Background(), "object")
	if err != nil || exists {
		t.Fatalf("expected (false, nil), got (%v, %v)", exists, err)
	}
	exists, err = newFakeClient(&fakeGCSObject{attrsErr: errors.New("attrs")}, 10).Exists(context.Background(), "object")
	if err == nil || exists {
		t.Fatalf("expected (false, err), got (%v, %v)", exists, err)
	}
	exists, err = newFakeClient(&fakeGCSObject{}, 10).Exists(context.Background(), "object")
	if err != nil || !exists {
		t.Fatalf("expected (true, nil), got (%v, %v)", exists, err)
	}

	if _, err := newFakeClient(&fakeGCSObject{}, 10).Stat(context.Background(), "/bad"); err == nil {
		t.Fatal("expected stat validation error")
	}
	if _, err := newFakeClient(&fakeGCSObject{}, 10).Exists(context.Background(), "../bad"); err == nil {
		t.Fatal("expected exists validation error")
	}
}

func TestDownloadReader(t *testing.T) {
	obj := &fakeGCSObject{body: "content"}
	c := newFakeClient(obj, 10)

	reader, err := c.DownloadReader(context.Background(), "object")
	if err != nil {
		t.Fatalf("DownloadReader failed: %v", err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if string(data) != "content" {
		t.Fatalf("unexpected data %q", data)
	}

	if _, err := newFakeClient(&fakeGCSObject{readerErr: storage.ErrObjectNotExist}, 10).DownloadReader(context.Background(), "object"); !errors.Is(err, clientstorage.ErrObjectNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
	if _, err := newFakeClient(&fakeGCSObject{readerErr: errors.New("read")}, 10).DownloadReader(context.Background(), "object"); err == nil {
		t.Fatal("expected reader error")
	}
	if _, err := newFakeClient(&fakeGCSObject{}, 10).DownloadReader(context.Background(), "/bad"); err == nil {
		t.Fatal("expected download reader validation error")
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
