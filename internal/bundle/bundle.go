// Package bundle writes session artifact tarballs. The bundle format is
// stable and self-describing: manifest.json is written last and lists every
// entry in the archive.
package bundle

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Manifest describes one exported session.
type Manifest struct {
	Version      int       `json:"version"`
	SessionID    string    `json:"session_id"`
	Title        string    `json:"title,omitempty"`
	Agent        string    `json:"agent,omitempty"`
	Harness      string    `json:"harness,omitempty"`
	Model        string    `json:"model,omitempty"`
	Status       string    `json:"status"`
	Reason       string    `json:"reason,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	FinishedAt   time.Time `json:"finished_at"`
	AgentVersion string    `json:"agent_version,omitempty"`
	Skills       []string  `json:"skills,omitempty"`
	MCPServers   []string  `json:"mcp_servers,omitempty"`
	Plugins      []string  `json:"plugins,omitempty"`
	MessageCount int       `json:"message_count"`
	ToolCalls    int       `json:"tool_calls"`
	FileCount    int       `json:"file_count"`
	Bytes        int64     `json:"bytes"`
	Partial      bool      `json:"partial"`
	Truncated    bool      `json:"truncated"`
	Note         string    `json:"note,omitempty"`
	Files        []string  `json:"files"`
}

// Writer streams a gzip-compressed tar archive.
type Writer struct {
	gz       *gzip.Writer
	tw       *tar.Writer
	manifest Manifest
	files    []string
	limit    int64
	bytes    int64
	skipped  bool
	closed   bool
}

// New creates a bundle writer. fileLimit caps the total bytes of files added
// through AddDir; core JSONL entries added with AddBytes are always included.
func New(w io.Writer, m Manifest, fileLimit int64) *Writer {
	gz := gzip.NewWriter(w)
	return &Writer{
		gz:       gz,
		tw:       tar.NewWriter(gz),
		manifest: m,
		limit:    fileLimit,
		files:    []string{},
	}
}

// cleanName normalizes an archive entry name: it drops the leading root and
// replaces bytes that the JSON manifest cannot represent, so the manifest
// lists the name the archive carries.
func cleanName(name string) string {
	return strings.ToValidUTF8(path.Clean("/" + name)[1:], "\uFFFD")
}

// AddBytes adds an in-memory entry.
func (b *Writer) AddBytes(name string, data []byte) error {
	if b.closed {
		return fmt.Errorf("bundle already closed")
	}
	name = cleanName(name)
	if err := b.tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(data)),
		ModTime:  time.Now().UTC(),
		Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	if _, err := b.tw.Write(data); err != nil {
		return err
	}
	b.files = append(b.files, name)
	b.bytes += int64(len(data))
	return nil
}

// AddDir adds every regular file under dir with the given archive prefix.
// Symlinks are skipped. Files that would exceed the configured limit are
// skipped and Truncated is set in the manifest.
func (b *Writer) AddDir(prefix, dir string) error {
	if b.closed {
		return fmt.Errorf("bundle already closed")
	}
	prefix = cleanName(prefix)
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped, not fatal
		}
		if d.Type()&fs.ModeSymlink != 0 {
			b.skipped = true
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if b.limit > 0 && b.bytes+info.Size() > b.limit {
			b.skipped = true
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			_ = f.Close()
			continue
		}
		name := strings.ToValidUTF8(path.Join(prefix, filepath.ToSlash(rel)), "\uFFFD")
		header := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     info.Size(),
			ModTime:  info.ModTime().UTC(),
			Typeflag: tar.TypeReg,
		}
		if err := b.tw.WriteHeader(header); err != nil {
			_ = f.Close()
			return err
		}
		if _, err := io.Copy(b.tw, f); err != nil {
			_ = f.Close()
			return err
		}
		_ = f.Close()
		b.files = append(b.files, name)
		b.bytes += info.Size()
	}
	return nil
}

// Close writes manifest.json and finishes the archive. It is safe to call once.
func (b *Writer) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	b.manifest.Files = append([]string(nil), b.files...)
	b.manifest.FileCount = len(b.files)
	b.manifest.Bytes = b.bytes
	b.manifest.Truncated = b.manifest.Truncated || b.skipped
	data, err := json.MarshalIndent(b.manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := b.tw.WriteHeader(&tar.Header{
		Name:     "manifest.json",
		Mode:     0o644,
		Size:     int64(len(data)),
		ModTime:  time.Now().UTC(),
		Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	if _, err := b.tw.Write(data); err != nil {
		return err
	}
	if err := b.tw.Close(); err != nil {
		return err
	}
	return b.gz.Close()
}
