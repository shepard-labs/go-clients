// Live conformance test for the S3 storage core.
//
// All tests in this file are env-gated and skip without credentials, so the
// default `go test ./...` stays hermetic and fast. They also skip under
// `-short`.
//
// Run against a local MinIO in docker:
//
//	docker run -d --name minio-smoke -p 9000:9000 -p 9001:9001 \
//	  -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
//	  minio/minio server /data --console-address ":9001"
//
// The bucket must pre-exist; create it via the console at
// http://localhost:9001 or with mc:
//
//	mc alias set local http://localhost:9000 minioadmin minioadmin && \
//	mc mb local/conformance-smoke
//
// Then run:
//
//	S3_SMOKE_MINIO_ENDPOINT=localhost:9000 \
//	S3_SMOKE_MINIO_KEY=minioadmin \
//	S3_SMOKE_MINIO_SECRET=minioadmin \
//	S3_SMOKE_MINIO_BUCKET=conformance-smoke \
//	go test ./s3/ -run TestConformanceMinIO -v
//
// Set S3_SMOKE_MINIO_SECURE=1 only for a TLS-served MinIO; plain local
// HTTP leaves it unset.
//
// Hosted providers (each skipped unless ALL of its vars are set):
//
//	S3_SMOKE_AWS_KEY / S3_SMOKE_AWS_SECRET / S3_SMOKE_AWS_REGION / S3_SMOKE_AWS_BUCKET
//	(optional S3_SMOKE_AWS_ENDPOINT override via WithEndpoint)
//	S3_SMOKE_B2_KEY / S3_SMOKE_B2_SECRET / S3_SMOKE_B2_REGION / S3_SMOKE_B2_BUCKET
//	(optional S3_SMOKE_B2_ENDPOINT override via WithEndpoint)
//	S3_SMOKE_WASABI_KEY / S3_SMOKE_WASABI_SECRET / S3_SMOKE_WASABI_REGION / S3_SMOKE_WASABI_BUCKET
//	(optional S3_SMOKE_WASABI_ENDPOINT override via WithEndpoint)
//	S3_SMOKE_GCSHMAC_KEY / S3_SMOKE_GCSHMAC_SECRET / S3_SMOKE_GCSHMAC_BUCKET
//	(optional S3_SMOKE_GCSHMAC_ENDPOINT override via WithEndpoint, e.g. the storage emulator)
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	clientstorage "github.com/shepard-labs/go-clients/storage"
)

// requireConformanceEnv returns the value of name, skipping the test when it
// is unset or empty.
func requireConformanceEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("skipping live conformance: %s not set", name)
	}
	return v
}

// skipIfShort skips live conformance tests under -short so the default suite
// stays hermetic and fast.
func skipIfShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping live conformance under -short")
	}
}

// runConformance exercises the full Storage lifecycle against s: Upload bytes
// -> Stat -> Exists -> Download -> UploadReader -> DownloadReader -> List ->
// Delete -> Exists false -> Download returns ErrObjectNotFound.
func runConformance(t *testing.T, s clientstorage.Storage, bucket string) {
	t.Helper()
	ctx := context.Background()

	// Unique prefix per run so parallel/coincident runs don't collide.
	safeName := strings.NewReplacer("/", "-", " ", "_").Replace(t.Name())
	prefix := fmt.Sprintf("conformance/%s-%d/", safeName, time.Now().UnixNano())

	content := []byte(fmt.Sprintf("s3 conformance payload @ %d", time.Now().UnixNano()))
	contentType := "text/plain; charset=utf-8"
	key := prefix + "bytes.txt"

	if err := s.Upload(ctx, key, content, contentType); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	info, err := s.Stat(ctx, key)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Name != key {
		t.Fatalf("Stat name = %q, want %q", info.Name, key)
	}
	if info.Size != int64(len(content)) {
		t.Fatalf("Stat size = %d, want %d", info.Size, len(content))
	}
	if info.ContentType != contentType {
		t.Fatalf("Stat content-type = %q, want %q", info.ContentType, contentType)
	}

	exists, err := s.Exists(ctx, key)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !exists {
		t.Fatalf("Exists = false after Upload, want true")
	}

	got, err := s.Download(ctx, key)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("Download = %q, want %q", got, content)
	}

	streamContent := []byte(fmt.Sprintf("s3 conformance stream @ %d", time.Now().UnixNano()))
	streamKey := prefix + "stream.bin"
	streamType := "application/octet-stream"
	if err := s.UploadReader(ctx, streamKey, bytes.NewReader(streamContent), streamType, int64(len(streamContent))); err != nil {
		t.Fatalf("UploadReader: %v", err)
	}

	rc, err := s.DownloadReader(ctx, streamKey)
	if err != nil {
		t.Fatalf("DownloadReader: %v", err)
	}
	streamed, err := io.ReadAll(rc)
	closeErr := rc.Close()
	if err != nil {
		t.Fatalf("DownloadReader read: %v", err)
	}
	if closeErr != nil {
		t.Fatalf("DownloadReader close: %v", closeErr)
	}
	if !bytes.Equal(streamed, streamContent) {
		t.Fatalf("DownloadReader = %q, want %q", streamed, streamContent)
	}

	listed, err := s.List(ctx, prefix)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := map[string]bool{}
	for _, oi := range listed {
		found[oi.Name] = true
	}
	if !found[key] || !found[streamKey] {
		t.Fatalf("List(%q) missing objects: found %v in bucket %q", prefix, found, bucket)
	}

	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(ctx, streamKey); err != nil {
		t.Fatalf("Delete stream object: %v", err)
	}

	exists, err = s.Exists(ctx, key)
	if err != nil {
		t.Fatalf("Exists after Delete: %v", err)
	}
	if exists {
		t.Fatalf("Exists = true after Delete, want false")
	}

	if _, err := s.Download(ctx, key); !errors.Is(err, clientstorage.ErrObjectNotFound) {
		t.Fatalf("Download after Delete = %v, want errors.Is ErrObjectNotFound", err)
	}
}

func TestConformanceMinIO(t *testing.T) {
	skipIfShort(t)
	endpoint := requireConformanceEnv(t, "S3_SMOKE_MINIO_ENDPOINT")
	key := requireConformanceEnv(t, "S3_SMOKE_MINIO_KEY")
	secret := requireConformanceEnv(t, "S3_SMOKE_MINIO_SECRET")
	bucket := requireConformanceEnv(t, "S3_SMOKE_MINIO_BUCKET")
	secure := os.Getenv("S3_SMOKE_MINIO_SECURE") == "1"

	s, err := NewMinIO(endpoint, key, secret, bucket, "conformance", 0, secure, zap.NewNop())
	if err != nil {
		t.Fatalf("NewMinIO: %v", err)
	}
	defer s.Close()
	runConformance(t, s, bucket)
}

func TestConformanceAWS(t *testing.T) {
	skipIfShort(t)
	key := requireConformanceEnv(t, "S3_SMOKE_AWS_KEY")
	secret := requireConformanceEnv(t, "S3_SMOKE_AWS_SECRET")
	region := requireConformanceEnv(t, "S3_SMOKE_AWS_REGION")
	bucket := requireConformanceEnv(t, "S3_SMOKE_AWS_BUCKET")

	var opts []func(*Config)
	if ep := os.Getenv("S3_SMOKE_AWS_ENDPOINT"); ep != "" {
		opts = append(opts, WithEndpoint(ep))
	}
	s, err := NewAWS(key, secret, region, bucket, "conformance", 0, zap.NewNop(), opts...)
	if err != nil {
		t.Fatalf("NewAWS: %v", err)
	}
	defer s.Close()
	runConformance(t, s, bucket)
}

func TestConformanceB2(t *testing.T) {
	skipIfShort(t)
	key := requireConformanceEnv(t, "S3_SMOKE_B2_KEY")
	secret := requireConformanceEnv(t, "S3_SMOKE_B2_SECRET")
	region := requireConformanceEnv(t, "S3_SMOKE_B2_REGION")
	bucket := requireConformanceEnv(t, "S3_SMOKE_B2_BUCKET")

	var opts []func(*Config)
	if ep := os.Getenv("S3_SMOKE_B2_ENDPOINT"); ep != "" {
		opts = append(opts, WithEndpoint(ep))
	}
	s, err := NewB2(key, secret, region, bucket, "conformance", 0, zap.NewNop(), opts...)
	if err != nil {
		t.Fatalf("NewB2: %v", err)
	}
	defer s.Close()
	runConformance(t, s, bucket)
}

func TestConformanceWasabi(t *testing.T) {
	skipIfShort(t)
	key := requireConformanceEnv(t, "S3_SMOKE_WASABI_KEY")
	secret := requireConformanceEnv(t, "S3_SMOKE_WASABI_SECRET")
	region := requireConformanceEnv(t, "S3_SMOKE_WASABI_REGION")
	bucket := requireConformanceEnv(t, "S3_SMOKE_WASABI_BUCKET")

	var opts []func(*Config)
	if ep := os.Getenv("S3_SMOKE_WASABI_ENDPOINT"); ep != "" {
		opts = append(opts, WithEndpoint(ep))
	}
	s, err := NewWasabi(key, secret, region, bucket, "conformance", 0, zap.NewNop(), opts...)
	if err != nil {
		t.Fatalf("NewWasabi: %v", err)
	}
	defer s.Close()
	runConformance(t, s, bucket)
}

func TestConformanceGCSHMAC(t *testing.T) {
	skipIfShort(t)
	key := requireConformanceEnv(t, "S3_SMOKE_GCSHMAC_KEY")
	secret := requireConformanceEnv(t, "S3_SMOKE_GCSHMAC_SECRET")
	bucket := requireConformanceEnv(t, "S3_SMOKE_GCSHMAC_BUCKET")

	var opts []func(*Config)
	if ep := os.Getenv("S3_SMOKE_GCSHMAC_ENDPOINT"); ep != "" {
		opts = append(opts, WithEndpoint(ep))
	}
	s, err := NewGCSHMAC(key, secret, bucket, "conformance", 0, zap.NewNop(), opts...)
	if err != nil {
		t.Fatalf("NewGCSHMAC: %v", err)
	}
	defer s.Close()
	runConformance(t, s, bucket)
}
