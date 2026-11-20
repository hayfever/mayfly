package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func archive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		header := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestLoadRemoteSource(t *testing.T) {
	payload := archive(t, map[string]string{
		"repo-abc/README.md":              "ignore me",
		"repo-abc/skills/triage.md":       "---\nname: remote-triage\ndescription: Remote\n---\nCheck evidence.",
		"repo-abc/skills/.hidden/skip.md": "---\nname: hidden\n---\nbody",
	})
	sum := sha256.Sum256(payload)
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	loaded, err := Load(nil, []Source{{
		Name:            "acme",
		URL:             server.URL + "/skills.tar.gz",
		SHA256:          hex.EncodeToString(sum[:]),
		Subdir:          "skills",
		StripComponents: 1,
		Headers:         map[string]string{"Authorization": "Bearer token"},
		Timeout:         5 * time.Second,
	}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer token" {
		t.Fatalf("authorization = %q", auth)
	}
	if len(loaded) != 1 || loaded[0].Name != "remote-triage" || loaded[0].Body != "Check evidence." {
		t.Fatalf("skills = %+v", loaded)
	}
}

func TestRemoteSourceRejectsBadDigestAndTraversal(t *testing.T) {
	payload := archive(t, map[string]string{"repo-abc/skills/triage.md": "---\nname: x\n---\nbody"})
	sum := sha256.Sum256(payload)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	if _, err := Load(nil, []Source{{Name: "bad", URL: server.URL, SHA256: strings.Repeat("0", 64)}}, Options{}); err == nil {
		t.Fatal("expected digest error")
	}
	if _, err := Load(nil, []Source{{
		Name:            "ok",
		URL:             server.URL,
		SHA256:          hex.EncodeToString(sum[:]),
		Subdir:          "skills",
		StripComponents: 1,
	}}, Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(nil, []Source{{
		Name:            "no-skills",
		URL:             server.URL,
		SHA256:          hex.EncodeToString(sum[:]),
		Subdir:          "missing",
		StripComponents: 1,
	}}, Options{}); err == nil {
		t.Fatal("expected no-skills error")
	}

	traversal := archive(t, map[string]string{"../escape.md": "---\nname: escape\n---\nbody"})
	tsum := sha256.Sum256(traversal)
	tserver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(traversal)
	}))
	defer tserver.Close()
	if _, err := Load(nil, []Source{{Name: "escape", URL: tserver.URL, SHA256: hex.EncodeToString(tsum[:])}}, Options{}); err == nil {
		t.Fatal("expected traversal error")
	}
}

func TestRemoteSourceDuplicatesLocal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "local.md"), []byte("---\nname: same\n---\nlocal"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := archive(t, map[string]string{"root/skills/remote.md": "---\nname: same\n---\nremote"})
	sum := sha256.Sum256(payload)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	if _, err := Load([]string{dir}, []Source{{
		Name:            "remote",
		URL:             server.URL,
		SHA256:          hex.EncodeToString(sum[:]),
		Subdir:          "skills",
		StripComponents: 1,
	}}, Options{}); err == nil {
		t.Fatal("expected duplicate skill error")
	}
}
