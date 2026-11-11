package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDirsSkipsHiddenDirectories(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("incident.md", "---\nname: incident\n---\nbody")
	write(".20260101/incident.md", "---\nname: incident\n---\nbody")
	loaded, err := LoadDirs([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].Name != "incident" {
		t.Fatalf("skills = %+v", loaded)
	}
}

func TestLoadDirsRejectsDuplicateNames(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.md", "b.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("---\nname: same\n---\nbody"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := LoadDirs([]string{dir}); err == nil {
		t.Fatal("expected duplicate name error")
	}
}
