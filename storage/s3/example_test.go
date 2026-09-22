package s3_test

import (
	"fmt"

	"go.uber.org/zap"

	"github.com/shepard-labs/go-clients/storage/s3"
)

// ExampleNewAWS constructs an AWS S3-backed client. Construction only builds
// the MinIO client value; no network I/O happens until the first operation.
func ExampleNewAWS() {
	logger := zap.NewNop()
	_, err := s3.NewAWS("AKIDEXAMPLE", "secretexample", "us-east-1", "my-bucket", "my-service", 0, logger)
	if err != nil {
		fmt.Println("error")
		return
	}
	fmt.Println("ok")
	// Output: ok
}

// ExampleNewMinIO constructs a client for a self-hosted MinIO server.
// Construction only builds the MinIO client value; no network I/O happens
// until the first operation.
func ExampleNewMinIO() {
	logger := zap.NewNop()
	_, err := s3.NewMinIO("localhost:9000", "minioadmin", "minioadmin", "my-bucket", "my-service", 0, false, logger)
	if err != nil {
		fmt.Println("error")
		return
	}
	fmt.Println("ok")
	// Output: ok
}

// ExampleNewGCSHMAC constructs a client for Google Cloud Storage via HMAC
// interoperability keys. Prefer the native storage/gcs package when
// service-account auth is available. Construction only builds the MinIO
// client value; no network I/O happens until the first operation.
func ExampleNewGCSHMAC() {
	logger := zap.NewNop()
	_, err := s3.NewGCSHMAC("GOOGACCESSKEY", "googsecret", "my-bucket", "my-service", 0, logger)
	if err != nil {
		fmt.Println("error")
		return
	}
	fmt.Println("ok")
	// Output: ok
}
