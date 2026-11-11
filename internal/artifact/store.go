// Package artifact stores session bundles in a configured object store.
package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/hayden/mayfly/internal/config"
)

// ErrNotFound is returned when an artifact key does not exist.
var ErrNotFound = errors.New("artifact not found")

// Store persists session bundles.
type Store interface {
	// Put writes size bytes from r under key. sha256Hex is the lowercase hex
	// digest of the payload and is used for S3 request signing.
	Put(ctx context.Context, key string, r io.Reader, size int64, sha256Hex, contentType string) (string, error)
	// Get opens a stored artifact.
	Get(ctx context.Context, key string) (io.ReadCloser, int64, error)
}

// New builds the configured store.
func New(cfg config.ArtifactsConfig) (Store, error) {
	switch cfg.Store {
	case "local":
		return NewLocal(cfg.Local.Dir)
	case "s3":
		return NewS3(cfg.S3)
	default:
		return nil, fmt.Errorf("unsupported artifact store %q", cfg.Store)
	}
}

// Key returns the object key for a session bundle.
func Key(sessionID string, at time.Time) string {
	return path.Join("sessions", sessionID, fmt.Sprintf("%d-%s.tar.gz", at.UTC().Unix(), sessionID))
}

// NewLocal returns a store rooted at dir.
func NewLocal(dir string) (Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("artifact dir: %w", err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("create artifact dir: %w", err)
	}
	return &localStore{dir: abs}, nil
}

type localStore struct {
	dir string
}

func (s *localStore) Put(ctx context.Context, key string, r io.Reader, size int64, _ string, _ string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	name, err := s.resolve(key)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(path.Dir(name), 0o750); err != nil {
		return "", fmt.Errorf("create artifact path: %w", err)
	}
	tmp, err := os.CreateTemp(path.Dir(name), ".upload-*")
	if err != nil {
		return "", fmt.Errorf("create artifact temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	n, err := io.Copy(tmp, io.LimitReader(r, size+1))
	if err != nil {
		return "", fmt.Errorf("write artifact: %w", err)
	}
	if n != size {
		return "", fmt.Errorf("artifact size mismatch: got %d, want %d", n, size)
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("sync artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close artifact: %w", err)
	}
	if err := os.Rename(tmpName, name); err != nil {
		return "", fmt.Errorf("publish artifact: %w", err)
	}
	return "file://" + name, nil
}

func (s *localStore) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	name, err := s.resolve(key)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

func (s *localStore) resolve(key string) (string, error) {
	if key == "" {
		return "", errors.New("empty artifact key")
	}
	clean := path.Clean("/" + key)
	name := filepath.Join(s.dir, filepath.FromSlash(clean))
	rel, err := filepath.Rel(s.dir, name)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid artifact key %q", key)
	}
	return name, nil
}
