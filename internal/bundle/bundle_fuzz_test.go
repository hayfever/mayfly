package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzAddBytes covers archive entry names and payloads. The writer strips the
// leading root from a name, so an absolute name never lands in the archive;
// it does not rewrite "..", and no caller passes one. The archive must stay
// readable and its manifest must list exactly what was written.
func FuzzAddBytes(f *testing.F) {
	f.Add("events.jsonl", []byte(`{"type":"x"}`))
	f.Add("session.json", []byte(`{}`))
	f.Add("plugins/notes.stderr.log", []byte("boom"))
	f.Add("/absolute/path", []byte("x"))
	f.Add("a/../b", []byte("x"))
	f.Add("../escape", []byte("x"))
	f.Add("", []byte(nil))
	f.Add("a\x00b", []byte("x"))

	f.Fuzz(func(t *testing.T, name string, data []byte) {
		var buf bytes.Buffer
		w := New(&buf, Manifest{Version: 1, SessionID: "s1", Status: "completed"}, 0)
		if err := w.AddBytes(name, data); err != nil {
			// A header the tar writer cannot encode is a rejected name.
			return
		}
		if err := w.Close(); err != nil {
			t.Fatalf("close bundle: %v", err)
		}

		entries := readFuzzBundle(t, &buf)
		want := strings.ToValidUTF8(path.Clean("/" + name)[1:], "\uFFFD")
		got, ok := entries[want]
		if !ok {
			t.Fatalf("archive has no entry %q; entries: %v", want, keysOf(entries))
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("entry %q payload = %q, want %q", want, got, data)
		}
		for entry := range entries {
			if strings.HasPrefix(entry, "/") {
				t.Fatalf("archive entry %q has an absolute name", entry)
			}
		}
		var manifest Manifest
		if err := json.Unmarshal(entries["manifest.json"], &manifest); err != nil {
			t.Fatalf("decode manifest: %v", err)
		}
		if len(manifest.Files) != 1 || manifest.Files[0] != want {
			t.Fatalf("manifest files = %v, want [%q]", manifest.Files, want)
		}
		if manifest.FileCount != 1 || manifest.Bytes != int64(len(data)) {
			t.Fatalf("manifest counts = %d files, %d bytes", manifest.FileCount, manifest.Bytes)
		}
	})
}

// FuzzAddDirPrefix covers the archive prefix of a workspace bundle. The prefix
// is cleaned the same way as an AddBytes name, and the relative path of every
// file is appended to it.
func FuzzAddDirPrefix(f *testing.F) {
	f.Add("files")
	f.Add("")
	f.Add("/root")
	f.Add("a/../b")
	f.Add("..")
	f.Add("a\x00b")

	dir := f.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "out.txt"), []byte("hello"), 0o644); err != nil {
		f.Fatal(err)
	}

	f.Fuzz(func(t *testing.T, prefix string) {
		var buf bytes.Buffer
		w := New(&buf, Manifest{Version: 1, SessionID: "s1", Status: "completed"}, 0)
		if err := w.AddDir(prefix, dir); err != nil {
			// A prefix the tar writer cannot encode is a rejected name.
			return
		}
		if err := w.Close(); err != nil {
			t.Fatalf("close bundle: %v", err)
		}
		entries := readFuzzBundle(t, &buf)
		want := strings.ToValidUTF8(path.Join(path.Clean("/" + prefix)[1:], "out.txt"), "\uFFFD")
		if got := entries[want]; string(got) != "hello" {
			t.Fatalf("archive has no entry %q; entries: %v", want, keysOf(entries))
		}
		for entry := range entries {
			if strings.HasPrefix(entry, "/") {
				t.Fatalf("archive entry %q has an absolute name", entry)
			}
		}
	})
}

func readFuzzBundle(t *testing.T, buf *bytes.Buffer) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(buf.Bytes()))
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
			t.Fatalf("read entry %q: %v", hdr.Name, err)
		}
		entries[hdr.Name] = data
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return entries
}

func keysOf(entries map[string][]byte) []string {
	out := make([]string, 0, len(entries))
	for name := range entries {
		out = append(out, name)
	}
	return out
}
