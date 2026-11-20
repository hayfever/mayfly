package skills

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func sourceFrom(t *testing.T, name, archiveURL string, payload []byte) Source {
	t.Helper()
	sum := sha256.Sum256(payload)
	return Source{
		Name:            name,
		URL:             archiveURL,
		SHA256:          hex.EncodeToString(sum[:]),
		StripComponents: 1,
	}
}

// newCacheDir returns a directory below a temporary root so tests observe the
// mode the cache creates.
func newCacheDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "skill-cache")
}

func countingServer(t *testing.T, payload []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func TestCacheServesSecondLoadWithoutNetwork(t *testing.T) {
	payload := archive(t, map[string]string{"root/triage.md": "---\nname: triage\n---\nbody"})
	server, requests := countingServer(t, payload)
	source := sourceFrom(t, "acme", server.URL+"/skills.tar.gz", payload)
	cacheDir := newCacheDir(t)

	loaded, err := Load(nil, []Source{source}, Options{CacheDir: cacheDir})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].Name != "triage" {
		t.Fatalf("skills = %+v", loaded)
	}
	if requests.Load() != 1 {
		t.Fatalf("first load requests = %d", requests.Load())
	}

	server.Close()
	loaded, err = Load(nil, []Source{source}, Options{CacheDir: cacheDir})
	if err != nil {
		t.Fatalf("cached load: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("cached load made %d requests", requests.Load())
	}
	if len(loaded) != 1 || loaded[0].Name != "triage" {
		t.Fatalf("cached skills = %+v", loaded)
	}

	entry := filepath.Join(cacheDir, strings.ToLower(source.SHA256)+".tar.gz")
	info, err := os.Stat(entry)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("cache file mode = %o", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("cache dir mode = %o", dirInfo.Mode().Perm())
	}
}

func TestCacheCorruptedRefetchesAndRewrites(t *testing.T) {
	payload := archive(t, map[string]string{"root/triage.md": "---\nname: triage\n---\nbody"})
	server, requests := countingServer(t, payload)
	source := sourceFrom(t, "acme", server.URL, payload)
	cacheDir := newCacheDir(t)
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(cacheDir, strings.ToLower(source.SHA256)+".tar.gz")
	if err := os.WriteFile(entry, []byte("not the archive"), 0o600); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	if _, err := Load(nil, []Source{source}, Options{CacheDir: cacheDir, Logger: logger}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d", requests.Load())
	}
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("expected a warning, got %q", logs.String())
	}
	got, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("cache was not rewritten")
	}
}

func TestCacheDirNotWritableDoesNotFailLoad(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	payload := archive(t, map[string]string{"root/triage.md": "---\nname: triage\n---\nbody"})
	server, _ := countingServer(t, payload)
	source := sourceFrom(t, "acme", server.URL, payload)
	cacheDir := filepath.Join(t.TempDir(), "readonly")
	if err := os.MkdirAll(cacheDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cacheDir, 0o700) })

	if _, err := Load(nil, []Source{source}, Options{CacheDir: cacheDir}); err != nil {
		t.Fatal(err)
	}
}

func TestFetchFailureWithoutCacheNamesMissingCopy(t *testing.T) {
	payload := archive(t, map[string]string{"root/triage.md": "---\nname: triage\n---\nbody"})
	server, _ := countingServer(t, payload)
	source := sourceFrom(t, "acme", server.URL, payload)
	server.Close()

	_, err := Load(nil, []Source{source}, Options{CacheDir: newCacheDir(t)})
	if err == nil {
		t.Fatal("expected fetch error")
	}
	if !strings.Contains(err.Error(), "acme") || !strings.Contains(err.Error(), "no cached copy") {
		t.Fatalf("error = %v", err)
	}
}

func TestCachePruneRemovesUnreferencedEntries(t *testing.T) {
	payload := archive(t, map[string]string{"root/triage.md": "---\nname: triage\n---\nbody"})
	server, _ := countingServer(t, payload)
	source := sourceFrom(t, "acme", server.URL, payload)
	cacheDir := newCacheDir(t)
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	unreferenced := filepath.Join(cacheDir, strings.Repeat("b", 64)+".tar.gz")
	if err := os.WriteFile(unreferenced, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(cacheDir, "notes.txt")
	if err := os.WriteFile(notes, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(nil, []Source{source}, Options{CacheDir: cacheDir}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(unreferenced); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unreferenced entry still present: %v", err)
	}
	if _, err := os.Stat(notes); err != nil {
		t.Fatalf("notes.txt was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, strings.ToLower(source.SHA256)+".tar.gz")); err != nil {
		t.Fatalf("referenced entry missing: %v", err)
	}
}

func TestCacheSharesOneEntryPerDigest(t *testing.T) {
	payload := archive(t, map[string]string{
		"root/a/first.md":  "---\nname: first\n---\nbody",
		"root/b/second.md": "---\nname: second\n---\nbody",
	})
	server, requests := countingServer(t, payload)
	first := sourceFrom(t, "first", server.URL, payload)
	first.Subdir = "a"
	second := sourceFrom(t, "second", server.URL, payload)
	second.Subdir = "b"
	cacheDir := newCacheDir(t)

	loaded, err := Load(nil, []Source{first, second}, Options{CacheDir: cacheDir})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 {
		t.Fatalf("skills = %+v", loaded)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d", requests.Load())
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != strings.ToLower(first.SHA256)+".tar.gz" {
		t.Fatalf("cache entries = %+v", entries)
	}
}
