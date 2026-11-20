package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// cacheEntryPattern is the only file name shape the cache reads, writes, or
// deletes. A digest is the whole identity of a source archive, so a hit needs
// no other metadata.
var cacheEntryPattern = regexp.MustCompile(`^[0-9a-f]{64}\.tar\.gz$`)

func cacheEntryName(digest string) string {
	return strings.ToLower(digest) + ".tar.gz"
}

// validDigest reports whether digest is a SHA-256 hex string. Config validation
// already enforces this shape; the check keeps a hand-built Source from
// reaching the file system.
func validDigest(digest string) bool {
	if len(digest) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

// readCachedArchive returns the verified archive for digest. A missing file
// returns an error that satisfies errors.Is(err, fs.ErrNotExist).
func readCachedArchive(dir, digest string) ([]byte, error) {
	file, err := os.Open(filepath.Join(dir, cacheEntryName(digest)))
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	archive, err := io.ReadAll(io.LimitReader(file, maxArchiveBytes+1))
	if err != nil {
		return nil, err
	}
	if len(archive) > maxArchiveBytes {
		return nil, fmt.Errorf("cached archive exceeds %d bytes", maxArchiveBytes)
	}
	sum := sha256.Sum256(archive)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), digest) {
		return nil, errors.New("cached archive sha256 mismatch")
	}
	return archive, nil
}

// writeCachedArchive stores archive under its digest. The write goes to a
// temporary file in the same directory, so a crash never leaves a truncated
// entry under a digest name.
func writeCachedArchive(dir, digest string, archive []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, "."+cacheEntryName(digest)+".tmp-")
	if err != nil {
		return err
	}
	tmpName := file.Name()
	discard := func() {
		_ = file.Close()
		_ = os.Remove(tmpName)
	}
	if err := file.Chmod(0o600); err != nil {
		discard()
		return err
	}
	if _, err := file.Write(archive); err != nil {
		discard()
		return err
	}
	if err := file.Sync(); err != nil {
		discard()
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, cacheEntryName(digest))); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// pruneCache deletes entries whose digest is not referenced. Other files in
// the directory are never touched, so an operator can keep unrelated data
// beside the cache.
func pruneCache(dir string, referenced map[string]bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !cacheEntryPattern.MatchString(name) {
			continue
		}
		if referenced[strings.TrimSuffix(name, ".tar.gz")] {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
