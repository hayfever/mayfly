package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"
)

const (
	maxArchiveBytes     = 32 << 20
	maxSkillBytes       = 1 << 20
	maxExtractedBytes   = 16 << 20
	defaultSourceTimout = 30 * time.Second
)

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
	timeout := source.Timeout
	if timeout <= 0 {
		timeout = defaultSourceTimout
	}
	req, err := http.NewRequest(http.MethodGet, source.URL, nil)
	if err != nil {
		return fmt.Errorf("skill source %q: %w", source.Name, err)
	}
	for key, value := range source.Headers {
		req.Header.Set(key, value)
	}
	req.Header.Set("User-Agent", "mayflyd")
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("skill source %q: fetch: %w", source.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("skill source %q: status %d: %s", source.Name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	archive, err := io.ReadAll(io.LimitReader(resp.Body, maxArchiveBytes+1))
	if err != nil {
		return fmt.Errorf("skill source %q: read: %w", source.Name, err)
	}
	if len(archive) > maxArchiveBytes {
		return fmt.Errorf("skill source %q: archive exceeds %d bytes", source.Name, maxArchiveBytes)
	}
	sum := sha256.Sum256(archive)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), source.SHA256) {
		return fmt.Errorf("skill source %q: sha256 mismatch", source.Name)
	}

	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("skill source %q: gzip: %w", source.Name, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	label := source.Name
	if label == "" {
		label = source.URL
	}
	subdir := strings.Trim(path.Clean("/"+source.Subdir), "/")
	if source.Subdir == "" {
		subdir = ""
	}
	loaded := 0
	extracted := 0
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("skill source %q: tar: %w", source.Name, err)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
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
