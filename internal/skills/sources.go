package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxArchiveBytes     = 32 << 20
	maxSkillBytes       = 1 << 20
	maxExtractedBytes   = 16 << 20
	defaultSourceTimout = 30 * time.Second
)

func (s Source) label() string {
	if s.Name != "" {
		return s.Name
	}
	return s.URL
}

// logLabel names a source in log lines. A source without a name falls back to
// its URL without user info, query, or fragment, which can carry credentials.
func (s Source) logLabel() string {
	if s.Name != "" {
		return s.Name
	}
	u, err := url.Parse(s.URL)
	if err != nil {
		return "<unparsable url>"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func (l *skillLoader) loadSource(source Source) error {
	if strings.TrimSpace(source.URL) == "" {
		return fmt.Errorf("skill source %q: url is required", source.Name)
	}
	if strings.TrimSpace(source.SHA256) == "" {
		return fmt.Errorf("skill source %q: sha256 is required", source.Name)
	}
	if source.StripComponents < 0 {
		return fmt.Errorf("skill source %q: strip_components must not be negative", source.Name)
	}

	cacheable := l.cacheDir != "" && validDigest(source.SHA256)
	if cacheable {
		archive, err := readCachedArchive(l.cacheDir, source.SHA256)
		switch {
		case err == nil:
			l.logInfo("skill source cache hit", "source", source.logLabel(), "sha256", strings.ToLower(source.SHA256))
			return l.extractArchive(source, archive)
		case errors.Is(err, fs.ErrNotExist):
			// No entry yet. Fetch as usual.
		default:
			l.logWarn("skill source cache read failed; refetching",
				"source", source.logLabel(), "sha256", strings.ToLower(source.SHA256), "error", err)
			if err := os.Remove(filepath.Join(l.cacheDir, cacheEntryName(source.SHA256))); err != nil && !errors.Is(err, fs.ErrNotExist) {
				l.logWarn("skill source cache remove failed", "source", source.logLabel(), "error", err)
			}
		}
	}

	archive, err := l.fetchSource(source)
	if err != nil {
		if cacheable {
			return fmt.Errorf("%w (no cached copy of sha256 %s exists)", err, strings.ToLower(source.SHA256))
		}
		return err
	}
	l.logInfo("skill source fetched", "source", source.logLabel(), "sha256", strings.ToLower(source.SHA256))

	if err := l.extractArchive(source, archive); err != nil {
		return err
	}
	if cacheable {
		if err := writeCachedArchive(l.cacheDir, source.SHA256, archive); err != nil {
			l.logWarn("skill source cache write failed", "source", source.logLabel(), "error", err)
		}
	}
	return nil
}

// fetchSource downloads and verifies one archive. Request headers may hold
// credentials; they are never logged or written to disk.
func (l *skillLoader) fetchSource(source Source) ([]byte, error) {
	timeout := source.Timeout
	if timeout <= 0 {
		timeout = defaultSourceTimout
	}
	req, err := http.NewRequest(http.MethodGet, source.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("skill source %q: %w", source.Name, err)
	}
	for key, value := range source.Headers {
		req.Header.Set(key, value)
	}
	req.Header.Set("User-Agent", "mayflyd")
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("skill source %q: fetch: %w", source.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("skill source %q: status %d: %s", source.Name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	archive, err := io.ReadAll(io.LimitReader(resp.Body, maxArchiveBytes+1))
	if err != nil {
		return nil, fmt.Errorf("skill source %q: read: %w", source.Name, err)
	}
	if len(archive) > maxArchiveBytes {
		return nil, fmt.Errorf("skill source %q: archive exceeds %d bytes", source.Name, maxArchiveBytes)
	}
	sum := sha256.Sum256(archive)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), source.SHA256) {
		return nil, fmt.Errorf("skill source %q: sha256 mismatch", source.Name)
	}
	return archive, nil
}

// extractArchive loads the Markdown skills from a verified archive. Both the
// cached and the fetched path use it.
func (l *skillLoader) extractArchive(source Source, archive []byte) error {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("skill source %q: gzip: %w", source.Name, err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	label := source.label()
	subdir := strings.Trim(path.Clean("/"+source.Subdir), "/")
	if source.Subdir == "" {
		subdir = ""
	}
	loaded := 0
	extracted := 0
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("skill source %q: tar: %w", source.Name, err)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA { //nolint:staticcheck // old tar writers use the deprecated regular-file typeflag.
			continue
		}
		name := path.Clean(strings.TrimPrefix(header.Name, "./"))
		if name == "." || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("skill source %q: unsafe archive path %q", source.Name, header.Name)
		}
		parts := strings.Split(name, "/")
		if len(parts) <= source.StripComponents {
			continue
		}
		rel := strings.Join(parts[source.StripComponents:], "/")
		if hasHiddenPath(rel) {
			continue
		}
		if subdir != "" && rel != subdir && !strings.HasPrefix(rel, subdir+"/") {
			continue
		}
		ext := strings.ToLower(path.Ext(rel))
		if ext != ".md" && ext != ".markdown" {
			continue
		}
		if header.Size < 0 || header.Size > maxSkillBytes {
			return fmt.Errorf("skill source %q: skill %s is too large", source.Name, rel)
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxSkillBytes+1))
		if err != nil {
			return fmt.Errorf("skill source %q: read %s: %w", source.Name, rel, err)
		}
		if len(data) > maxSkillBytes {
			return fmt.Errorf("skill source %q: skill %s is too large", source.Name, rel)
		}
		extracted += len(data)
		if extracted > maxExtractedBytes {
			return fmt.Errorf("skill source %q: extracted skills exceed %d bytes", source.Name, maxExtractedBytes)
		}
		origin := label + ":" + rel
		skill, err := parseSkill(data, baseName(rel), origin)
		if err != nil {
			return err
		}
		if err := l.add(skill, origin); err != nil {
			return err
		}
		loaded++
	}
	if loaded == 0 {
		return fmt.Errorf("skill source %q: no skills found", source.Name)
	}
	return nil
}

func hasHiddenPath(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}
