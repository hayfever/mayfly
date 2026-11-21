package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"testing"
)

// TestBundleNameIsValidUTF8 pins the name the writer stores. manifest.json is
// JSON, which cannot carry invalid UTF-8, so an entry name with such bytes
// must reach the archive in its sanitised form. A manifest that lists a
// different name than the archive carries makes the bundle unreliable to read.
func TestBundleNameIsValidUTF8(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf, Manifest{Version: 1, SessionID: "s1", Status: "completed"}, 0)
	if err := w.AddBytes("\x96", []byte("data")); err != nil {
		t.Fatalf("add bytes: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatalf("open gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	entries := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %q: %v", hdr.Name, err)
		}
		entries[hdr.Name] = data
	}

	const want = "\uFFFD"
	if got := entries[want]; string(got) != "data" {
		t.Fatalf("archive has no entry %q; entries: %v", want, keysOf(entries))
	}
	var manifest Manifest
	if err := json.Unmarshal(entries["manifest.json"], &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if len(manifest.Files) != 1 || manifest.Files[0] != want {
		t.Fatalf("manifest files = %q, want [%q]", manifest.Files, want)
	}
}
