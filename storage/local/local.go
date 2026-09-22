// Package local implements the storage.Storage interface backed by the local
// filesystem.
//
// Objects are stored as files under a root directory, with the slash-separated
// object name mapped to a relative path inside the root. Writes are atomic:
// content is staged to a temporary file in the target directory and renamed
// over the destination. No file contents are ever written to logs; only the
// root directory, object names, and sizes are logged.
package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	clientstorage "github.com/shepard-labs/go-clients/storage"
)

// Ensure *Client satisfies the shared interface.
var _ clientstorage.Storage = (*Client)(nil)

// Client implements storage.Storage over a local directory.
type Client struct {
	logger      *zap.Logger
	root        string
	serviceTag  string
	maxDownload int64
}

// New constructs a filesystem-backed storage.Storage rooted at rootDir.
//
// rootDir is created (including parents, 0755) if missing; an error is
// returned if it exists as a non-directory. maxDownloadBytes caps the size
// Download will buffer into memory; a value <= 0 selects
// storage.DefaultMaxDownloadBytes.
//
// serviceTag is accepted for cross-provider constructor symmetry but is NOT
// persisted anywhere: this is a bytes-only backend with no metadata sidecar,
// so the tag is recorded only in logs.
//
// Path containment is enforced lexically: after validation, the resolved
// absolute path must equal the absolute root or lie beneath it, otherwise an
// error is returned. Caveat: the check does not resolve symlinks, so a
// symlink planted inside rootDir pointing outside the root will be followed
// by the OS on read/write. Do not let untrusted actors create symlinks (or
// names) under a root shared with trusted data.
func New(rootDir, serviceTag string, maxDownloadBytes int64, logger *zap.Logger) (clientstorage.Storage, error) {
	if logger == nil {
		logger = zap.NewNop()
	}

	if err := os.MkdirAll(rootDir, 0755); err != nil {
		return nil, fmt.Errorf("local create root directory failed: %w", err)
	}
	fi, err := os.Stat(rootDir)
	if err != nil {
		return nil, fmt.Errorf("local stat root directory failed: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("local root is not a directory: %s", rootDir)
	}
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("local resolve root directory failed: %w", err)
	}

	logger.Info("local client initialized",
		zap.String("provider", "local"),
		zap.String("endpoint", absRoot),
		zap.String("service", serviceTag),
	)

	if maxDownloadBytes <= 0 {
		maxDownloadBytes = clientstorage.DefaultMaxDownloadBytes
	}

	return &Client{
		logger:      logger,
		root:        absRoot,
		serviceTag:  serviceTag,
		maxDownload: maxDownloadBytes,
	}, nil
}

// resolve validates name and maps it to an absolute path inside the root,
// rejecting paths that escape the root.
func (c *Client) resolve(name string) (string, error) {
	if err := clientstorage.ValidateObjectName(name); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(filepath.Join(c.root, filepath.FromSlash(name)))
	if err != nil {
		return "", fmt.Errorf("local resolve failed: %w", err)
	}
	if abs != c.root && !strings.HasPrefix(abs, c.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: name escapes root directory: %s", clientstorage.ErrInvalidObjectName, name)
	}
	return abs, nil
}

// writeAtomic stages content to a temporary file in the target directory and
// renames it over path. The temporary file is removed on any failure.
func (c *Client) writeAtomic(path string, write func(*os.File) error, op string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("%s create parent directory failed: %w", op, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("%s create temp file failed: %w", op, err)
	}
	tmpName := tmp.Name()
	success := false
	defer func() {
		if !success {
			os.Remove(tmpName)
		}
	}()

	if err := write(tmp); err != nil {
		tmp.Close()
		return fmt.Errorf("%s write temp file failed: %w", op, err)
	}
	if err := tmp.Chmod(0644); err != nil {
		tmp.Close()
		return fmt.Errorf("%s chmod temp file failed: %w", op, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("%s close temp file failed: %w", op, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("%s rename temp file failed: %w", op, err)
	}
	success = true
	return nil
}

// Upload stores content under objectName. It implements storage.Storage.
func (c *Client) Upload(_ context.Context, objectName string, content []byte, contentType string) error {
	path, err := c.resolve(objectName)
	if err != nil {
		return err
	}

	c.logger.Info("uploading document to local",
		zap.String("provider", "local"),
		zap.String("endpoint", c.root),
		zap.String("object", objectName),
		zap.Int("size", len(content)),
		zap.String("content_type", contentType),
	)

	if err := c.writeAtomic(path, func(f *os.File) error {
		_, err := f.Write(content)
		return err
	}, "local upload"); err != nil {
		c.logger.Error("failed to upload to local", zap.String("provider", "local"), zap.Error(err))
		return err
	}
	return nil
}

// UploadReader streams size bytes from r and stores them under objectName. It
// implements storage.Storage. size is accepted for interface parity and
// otherwise unused: the stream is copied to the end.
func (c *Client) UploadReader(_ context.Context, objectName string, r io.Reader, contentType string, size int64) error {
	_ = size
	path, err := c.resolve(objectName)
	if err != nil {
		return err
	}

	c.logger.Info("uploading (reader) to local",
		zap.String("provider", "local"),
		zap.String("endpoint", c.root),
		zap.String("object", objectName),
		zap.String("content_type", contentType),
	)

	if err := c.writeAtomic(path, func(f *os.File) error {
		_, err := io.Copy(f, r)
		return err
	}, "local upload reader"); err != nil {
		c.logger.Error("failed to upload reader to local", zap.String("provider", "local"), zap.Error(err))
		return err
	}
	return nil
}

// Download returns the contents of objectName. It implements storage.Storage.
// Objects larger than the configured maximum return ErrObjectTooLarge.
func (c *Client) Download(_ context.Context, objectName string) ([]byte, error) {
	path, err := c.resolve(objectName)
	if err != nil {
		return nil, err
	}

	c.logger.Info("downloading document from local",
		zap.String("provider", "local"),
		zap.String("endpoint", c.root),
		zap.String("object", objectName),
	)

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", clientstorage.ErrObjectNotFound, objectName)
		}
		c.logger.Warn("failed to get object from local", zap.String("provider", "local"), zap.Error(err), zap.String("object", objectName))
		return nil, fmt.Errorf("local get object failed: %w", err)
	}
	defer f.Close()

	// Read up to maxDownload+1 so that exceeding the limit is detectable
	// rather than silently truncated.
	data, err := io.ReadAll(io.LimitReader(f, c.maxDownload+1))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", clientstorage.ErrObjectNotFound, objectName)
		}
		c.logger.Error("failed to read object from local", zap.String("provider", "local"), zap.Error(err), zap.String("object", objectName))
		return nil, fmt.Errorf("local read object failed: %w", err)
	}
	if int64(len(data)) > c.maxDownload {
		return nil, fmt.Errorf("%w: %s", clientstorage.ErrObjectTooLarge, objectName)
	}
	return data, nil
}

// Exists reports whether objectName exists via a metadata-only check. It
// implements storage.Storage.
func (c *Client) Exists(_ context.Context, objectName string) (bool, error) {
	path, err := c.resolve(objectName)
	if err != nil {
		return false, err
	}

	c.logger.Info("checking object existence in local",
		zap.String("provider", "local"),
		zap.String("endpoint", c.root),
		zap.String("object", objectName),
	)

	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		c.logger.Warn("failed to stat object in local", zap.String("provider", "local"), zap.Error(err), zap.String("object", objectName))
		return false, fmt.Errorf("local stat object failed: %w", err)
	}
	return true, nil
}

// Stat returns metadata for objectName without reading its contents. It
// implements storage.Storage. ContentType is always empty: the backend stores
// bytes only, with no metadata sidecar.
func (c *Client) Stat(_ context.Context, objectName string) (clientstorage.ObjectInfo, error) {
	path, err := c.resolve(objectName)
	if err != nil {
		return clientstorage.ObjectInfo{}, err
	}

	c.logger.Info("stat object in local",
		zap.String("provider", "local"),
		zap.String("endpoint", c.root),
		zap.String("object", objectName),
	)

	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return clientstorage.ObjectInfo{}, fmt.Errorf("%w: %s", clientstorage.ErrObjectNotFound, objectName)
		}
		c.logger.Warn("failed to stat object in local", zap.String("provider", "local"), zap.Error(err), zap.String("object", objectName))
		return clientstorage.ObjectInfo{}, fmt.Errorf("local stat object failed: %w", err)
	}

	return clientstorage.ObjectInfo{
		Name:    objectName,
		Size:    fi.Size(),
		Updated: fi.ModTime(),
	}, nil
}

// DownloadReader streams the contents of objectName. The caller must Close the
// returned reader. It implements storage.Storage.
//
// Unlike Download, the stream is uncapped (no max-download limit applies):
// bounding the stream is the caller's responsibility (do not io.ReadAll an
// object of unknown size).
func (c *Client) DownloadReader(_ context.Context, objectName string) (io.ReadCloser, error) {
	path, err := c.resolve(objectName)
	if err != nil {
		return nil, err
	}

	c.logger.Info("streaming download from local",
		zap.String("provider", "local"),
		zap.String("endpoint", c.root),
		zap.String("object", objectName),
	)

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", clientstorage.ErrObjectNotFound, objectName)
		}
		c.logger.Warn("failed to get object from local", zap.String("provider", "local"), zap.Error(err), zap.String("object", objectName))
		return nil, fmt.Errorf("local get object failed: %w", err)
	}
	return f, nil
}

// List returns objects whose names begin with prefix. An empty prefix lists
// the whole root. It implements storage.Storage.
func (c *Client) List(_ context.Context, prefix string) ([]clientstorage.ObjectInfo, error) {
	if prefix != "" {
		if err := clientstorage.ValidateObjectName(prefix); err != nil {
			return nil, err
		}
	}

	c.logger.Info("listing objects in local",
		zap.String("provider", "local"),
		zap.String("endpoint", c.root),
		zap.String("prefix", prefix),
	)

	var objects []clientstorage.ObjectInfo
	err := filepath.WalkDir(c.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(c.root, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if !strings.HasPrefix(name, prefix) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		objects = append(objects, clientstorage.ObjectInfo{
			Name:    name,
			Size:    info.Size(),
			Updated: info.ModTime(),
		})
		return nil
	})
	if err != nil {
		c.logger.Error("failed to list objects in local", zap.String("provider", "local"), zap.Error(err), zap.String("prefix", prefix))
		return nil, fmt.Errorf("local list objects failed: %w", err)
	}

	c.logger.Info("objects listed successfully",
		zap.String("provider", "local"),
		zap.String("endpoint", c.root),
		zap.String("prefix", prefix),
		zap.Int("count", len(objects)),
	)
	return objects, nil
}

// Delete removes objectName. It implements storage.Storage.
//
// Only files can be deleted: a name resolving to a directory returns an
// error. Empty parent directories are left in place.
func (c *Client) Delete(_ context.Context, objectName string) error {
	path, err := c.resolve(objectName)
	if err != nil {
		return err
	}

	c.logger.Info("deleting document from local",
		zap.String("provider", "local"),
		zap.String("endpoint", c.root),
		zap.String("object", objectName),
	)

	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s", clientstorage.ErrObjectNotFound, objectName)
		}
		c.logger.Error("failed to delete from local", zap.String("provider", "local"), zap.Error(err))
		return fmt.Errorf("local delete failed: %w", err)
	}
	if fi.IsDir() {
		return fmt.Errorf("local delete failed: %s is a directory", objectName)
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s", clientstorage.ErrObjectNotFound, objectName)
		}
		c.logger.Error("failed to delete from local", zap.String("provider", "local"), zap.Error(err))
		return fmt.Errorf("local delete failed: %w", err)
	}
	return nil
}

// Close releases any underlying client resources. It is a no-op for the
// filesystem-backed client. It implements storage.Storage.
func (c *Client) Close() error {
	c.logger.Info("local client closed", zap.String("provider", "local"))
	return nil
}
