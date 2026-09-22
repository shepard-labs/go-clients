package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	clientstorage "github.com/shepard-labs/go-clients/storage"
)

func newTestStorage(t *testing.T, maxDownload int64) (clientstorage.Storage, string) {
	t.Helper()
	root := t.TempDir()
	s, err := New(root, "test", maxDownload, zap.NewNop())
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	return s, root
}

func TestUploadDownloadRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStorage(t, 0)

	content := "hello nested world"
	if err := s.Upload(ctx, "a/b/c.txt", []byte(content), "text/plain"); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	got, err := s.Download(ctx, "a/b/c.txt")
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if string(got) != content {
		t.Fatalf("Download mismatch: got %q, want %q", got, content)
	}
}

func TestUploadReaderStream(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStorage(t, 0)

	content := "streamed content here"
	if err := s.UploadReader(ctx, "stream.txt", strings.NewReader(content), "text/plain", int64(len(content))); err != nil {
		t.Fatalf("UploadReader failed: %v", err)
	}
	got, err := s.Download(ctx, "stream.txt")
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if string(got) != content {
		t.Fatalf("Download mismatch: got %q, want %q", got, content)
	}
}

func TestOverwrite(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStorage(t, 0)

	if err := s.Upload(ctx, "over.txt", []byte("first version, longer"), "text/plain"); err != nil {
		t.Fatalf("first Upload failed: %v", err)
	}
	second := "second"
	if err := s.Upload(ctx, "over.txt", []byte(second), "text/plain"); err != nil {
		t.Fatalf("second Upload failed: %v", err)
	}
	got, err := s.Download(ctx, "over.txt")
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if string(got) != second {
		t.Fatalf("Overwrite mismatch: got %q, want %q", got, second)
	}
}

func TestDownloadTooLarge(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStorage(t, 4)

	if err := s.Upload(ctx, "big.txt", []byte("way too big"), "text/plain"); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	if _, err := s.Download(ctx, "big.txt"); !errors.Is(err, clientstorage.ErrObjectTooLarge) {
		t.Fatalf("expected ErrObjectTooLarge, got %v", err)
	}
}

func TestNotFound(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStorage(t, 0)

	if _, err := s.Download(ctx, "missing.txt"); !errors.Is(err, clientstorage.ErrObjectNotFound) {
		t.Fatalf("Download: expected ErrObjectNotFound, got %v", err)
	}
	if _, err := s.Stat(ctx, "missing.txt"); !errors.Is(err, clientstorage.ErrObjectNotFound) {
		t.Fatalf("Stat: expected ErrObjectNotFound, got %v", err)
	}
	if _, err := s.DownloadReader(ctx, "missing.txt"); !errors.Is(err, clientstorage.ErrObjectNotFound) {
		t.Fatalf("DownloadReader: expected ErrObjectNotFound, got %v", err)
	}
	if err := s.Delete(ctx, "missing.txt"); !errors.Is(err, clientstorage.ErrObjectNotFound) {
		t.Fatalf("Delete: expected ErrObjectNotFound, got %v", err)
	}
	exists, err := s.Exists(ctx, "missing.txt")
	if err != nil {
		t.Fatalf("Exists failed: %v", err)
	}
	if exists {
		t.Fatalf("Exists = true for missing key, want false")
	}
}

func TestStatFields(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStorage(t, 0)

	content := "stat me"
	name := "stat.txt"
	if err := s.Upload(ctx, name, []byte(content), "text/plain"); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	info, err := s.Stat(ctx, name)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if info.Name != name {
		t.Fatalf("Stat Name = %q, want %q", info.Name, name)
	}
	if info.Size != int64(len(content)) {
		t.Fatalf("Stat Size = %d, want %d", info.Size, len(content))
	}
	if info.Updated.IsZero() {
		t.Fatalf("Stat Updated is zero")
	}
	if info.ContentType != "" {
		t.Fatalf("Stat ContentType = %q, want empty", info.ContentType)
	}
}

func TestExistsTrue(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStorage(t, 0)

	if err := s.Upload(ctx, "here.txt", []byte("x"), "text/plain"); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	exists, err := s.Exists(ctx, "here.txt")
	if err != nil {
		t.Fatalf("Exists failed: %v", err)
	}
	if !exists {
		t.Fatalf("Exists = false for uploaded key, want true")
	}
}

func TestListPrefixAndAll(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStorage(t, 0)

	names := []string{"pre/one.txt", "pre/two.txt", "pre/nested/three.txt", "other/four.txt"}
	for _, n := range names {
		if err := s.Upload(ctx, n, []byte("data"), "text/plain"); err != nil {
			t.Fatalf("Upload %q failed: %v", n, err)
		}
	}

	prefixed, err := s.List(ctx, "pre")
	if err != nil {
		t.Fatalf("List(prefix) failed: %v", err)
	}
	wantPrefixed := map[string]bool{"pre/one.txt": true, "pre/two.txt": true, "pre/nested/three.txt": true}
	if len(prefixed) != len(wantPrefixed) {
		t.Fatalf("List(prefix) returned %d objects, want %d", len(prefixed), len(wantPrefixed))
	}
	for _, o := range prefixed {
		if !wantPrefixed[o.Name] {
			t.Fatalf("List(prefix) unexpected object %q", o.Name)
		}
	}

	all, err := s.List(ctx, "")
	if err != nil {
		t.Fatalf("List(all) failed: %v", err)
	}
	wantAll := map[string]bool{"pre/one.txt": true, "pre/two.txt": true, "pre/nested/three.txt": true, "other/four.txt": true}
	if len(all) != len(wantAll) {
		t.Fatalf("List(all) returned %d objects, want %d", len(all), len(wantAll))
	}
	for _, o := range all {
		if !wantAll[o.Name] {
			t.Fatalf("List(all) unexpected object %q", o.Name)
		}
	}
}

func TestValidation(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStorage(t, 0)

	bad := []string{"../x", "/abs", ""}
	for _, name := range bad {
		if err := s.Upload(ctx, name, []byte("x"), "text/plain"); !errors.Is(err, clientstorage.ErrInvalidObjectName) {
			t.Fatalf("Upload(%q): expected ErrInvalidObjectName, got %v", name, err)
		}
		if _, err := s.Download(ctx, name); !errors.Is(err, clientstorage.ErrInvalidObjectName) {
			t.Fatalf("Download(%q): expected ErrInvalidObjectName, got %v", name, err)
		}
		if _, err := s.Stat(ctx, name); !errors.Is(err, clientstorage.ErrInvalidObjectName) {
			t.Fatalf("Stat(%q): expected ErrInvalidObjectName, got %v", name, err)
		}
		if _, err := s.Exists(ctx, name); !errors.Is(err, clientstorage.ErrInvalidObjectName) {
			t.Fatalf("Exists(%q): expected ErrInvalidObjectName, got %v", name, err)
		}
		if err := s.Delete(ctx, name); !errors.Is(err, clientstorage.ErrInvalidObjectName) {
			t.Fatalf("Delete(%q): expected ErrInvalidObjectName, got %v", name, err)
		}
		if _, err := s.DownloadReader(ctx, name); !errors.Is(err, clientstorage.ErrInvalidObjectName) {
			t.Fatalf("DownloadReader(%q): expected ErrInvalidObjectName, got %v", name, err)
		}
		if err := s.UploadReader(ctx, name, strings.NewReader("x"), "text/plain", 1); !errors.Is(err, clientstorage.ErrInvalidObjectName) {
			t.Fatalf("UploadReader(%q): expected ErrInvalidObjectName, got %v", name, err)
		}
	}
	if _, err := s.List(ctx, "../bad"); !errors.Is(err, clientstorage.ErrInvalidObjectName) {
		t.Fatalf("List(../bad): expected ErrInvalidObjectName, got %v", err)
	}
}

func TestNewCreatesRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "a", "b", "c")
	s, err := New(root, "test", 0, zap.NewNop())
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer s.Close()
	fi, err := os.Stat(root)
	if err != nil {
		t.Fatalf("root was not created: %v", err)
	}
	if !fi.IsDir() {
		t.Fatalf("root is not a directory")
	}
}

func TestNewRejectsFileAsRoot(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "afile")
	if err := os.WriteFile(filePath, []byte("x"), 0644); err != nil {
		t.Fatalf("setup WriteFile failed: %v", err)
	}
	if _, err := New(filePath, "test", 0, zap.NewNop()); err == nil {
		t.Fatalf("expected error for file-as-root, got nil")
	}
}

func TestClose(t *testing.T) {
	s, _ := newTestStorage(t, 0)
	if err := s.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

func TestFilesAreRegular(t *testing.T) {
	ctx := context.Background()
	s, root := newTestStorage(t, 0)

	if err := s.Upload(ctx, "a/b/c.txt", []byte("regular"), "text/plain"); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(root, "a", "b", "c.txt"))
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("on-disk file is not regular: %v", fi.Mode())
	}
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), ".tmp-") {
			t.Errorf("temp leftover found: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkDir failed: %v", err)
	}
}
