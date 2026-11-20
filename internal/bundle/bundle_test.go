package bundle

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBundleContents(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "sub", "out.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "bundle.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{
		Version:    1,
		SessionID:  "s1",
		Status:     "completed",
		CreatedAt:  time.Unix(100, 0),
		FinishedAt: time.Unix(200, 0),
	}
	bw := New(f, manifest, 1024)
	if err := bw.AddBytes("events.jsonl", []byte(`{"type":"x"}`+"\n")); err != nil {
		t.Fatal(err)
	}
	if err := bw.AddDir("files", workspace); err != nil {
		t.Fatal(err)
	}
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	entries := readTar(t, path)
	if string(entries["files/sub/out.txt"]) != "hello" {
		t.Fatalf("workspace entry missing: %v", keys(entries))
	}
	if string(entries["events.jsonl"]) == "" {
		t.Fatal("events entry missing")
	}
	var got Manifest
	if err := json.Unmarshal(entries["manifest.json"], &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "completed" || got.FileCount != 2 || got.Truncated {
		t.Fatalf("manifest = %+v", got)
	}
}

func TestBundleTruncatesLargeFiles(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "big.bin"), make([]byte, 64), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "bundle.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	bw := New(f, Manifest{Version: 1, SessionID: "s1"}, 10)
	if err := bw.AddDir("files", workspace); err != nil {
		t.Fatal(err)
	}
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	entries := readTar(t, path)
	var got Manifest
	if err := json.Unmarshal(entries["manifest.json"], &got); err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || got.FileCount != 0 {
		t.Fatalf("expected truncated manifest, got %+v", got)
	}
}

func readTar(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[header.Name] = data
	}
	return out
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
