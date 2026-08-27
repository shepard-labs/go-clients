// Package storage defines a provider-agnostic interface for blob/object
// storage, plus its subpackage implementations (gcs, r2).
//
// Each implementation binds its destination bucket and credentials at
// construction time. The Storage interface covers the operations common to
// all providers: uploading bytes, streaming uploads from a reader,
// downloading, streaming downloads, existence and metadata checks, listing,
// and deleting objects by name.
//
// Application-specific path conventions (e.g. building object names from
// business identifiers) are intentionally out of scope; build those on top of
// a Storage value in the consuming application.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// DefaultMaxDownloadBytes caps the size of an object that Download will buffer
// into memory when a provider is constructed without an explicit limit. It
// guards against unbounded memory growth from hostile or unexpectedly large
// objects.
const DefaultMaxDownloadBytes int64 = 100 << 20 // 100 MiB

var (
	// ErrInvalidObjectName indicates an object name failed validation.
	ErrInvalidObjectName = errors.New("invalid object name")

	// ErrObjectTooLarge indicates a downloaded object exceeded the configured
	// size limit.
	ErrObjectTooLarge = errors.New("object exceeds maximum download size")

	// ErrObjectNotFound indicates the requested object does not exist.
	ErrObjectNotFound = errors.New("object not found")
)

// ValidateObjectName rejects names that are empty, absolute, or contain a ".."
// path segment. This blocks traversal-style keys from reaching the provider.
// It does not constrain the character set otherwise, since object stores
// permit a wide range of key characters.
func ValidateObjectName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: name is empty", ErrInvalidObjectName)
	}
	if strings.HasPrefix(name, "/") {
		return fmt.Errorf("%w: name must not be absolute", ErrInvalidObjectName)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return fmt.Errorf("%w: name must not contain %q segments", ErrInvalidObjectName, "..")
		}
	}
	return nil
}

// ObjectInfo describes a single object returned by List.
type ObjectInfo struct {
	Name        string
	Size        int64
	ContentType string
	Updated     time.Time
}

// Storage is a provider-agnostic object store bound to a single bucket.
//
// Implementations are safe for concurrent use. Object names are
// provider-relative keys within the bound bucket.
type Storage interface {
	// Upload stores content under objectName with the given content type.
	Upload(ctx context.Context, objectName string, content []byte, contentType string) error

	// UploadReader streams size bytes from r and stores them under objectName.
	// For providers that stream without a known length, size may be ignored.
	UploadReader(ctx context.Context, objectName string, r io.Reader, contentType string, size int64) error

	// Download returns the full contents of objectName.
	Download(ctx context.Context, objectName string) ([]byte, error)

	// Exists reports whether objectName exists, via a metadata-only check (no
	// object contents are transferred).
	Exists(ctx context.Context, objectName string) (bool, error)

	// Stat returns metadata for objectName without downloading its contents.
	// Returns ErrObjectNotFound if the object does not exist.
	Stat(ctx context.Context, objectName string) (ObjectInfo, error)

	// DownloadReader streams the contents of objectName. The caller must Close
	// the returned reader; its lifetime is governed by the caller's context, not
	// a client-side timeout. Unlike Download it does NOT buffer the object or
	// enforce the configured max-download cap — bounding the stream is the
	// caller's responsibility (do not io.ReadAll an object of unknown size).
	// Returns ErrObjectNotFound if the object does not exist.
	DownloadReader(ctx context.Context, objectName string) (io.ReadCloser, error)

	// List returns objects whose names begin with prefix. An empty prefix
	// lists the whole bucket. Results are unordered.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)

	// Delete removes objectName.
	Delete(ctx context.Context, objectName string) error

	// Close releases any underlying client resources.
	Close() error
}
