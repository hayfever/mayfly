package apidoc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const regenerate = "go run ./hack/apidoc"

// TestRenderMatchesCommittedReference fails when api/openapi.yaml changed and
// docs/api-reference.md did not. It runs under go test ./..., so a stale
// reference cannot merge.
func TestRenderMatchesCommittedReference(t *testing.T) {
	spec := readFile(t, filepath.Join("..", "..", "api", "openapi.yaml"))
	want := readFile(t, filepath.Join("..", "..", "docs", "api-reference.md"))

	got, err := Render(spec)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if string(got) == string(want) {
		return
	}
	t.Fatalf("docs/api-reference.md is stale; regenerate it with:\n\n  %s\n\n%s",
		regenerate, UnifiedDiff("docs/api-reference.md", "docs/api-reference.md", want, got))
}

// TestRenderCoversEveryOperation guards the renderer against silently skipping
// a path or a method: every operation in the spec must appear as a table row.
func TestRenderCoversEveryOperation(t *testing.T) {
	spec := readFile(t, filepath.Join("..", "..", "api", "openapi.yaml"))
	rendered, err := Render(spec)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(spec, &doc); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	text := string(rendered)
	total := 0
	for path, item := range doc.Paths {
		for method := range item {
			if !isHTTPMethod(strings.ToLower(method)) {
				continue
			}
			total++
			row := "| `" + strings.ToUpper(method) + "` | `" + path + "` |"
			if !strings.Contains(text, row) {
				t.Errorf("the reference has no row for %s %s", strings.ToUpper(method), path)
			}
		}
	}
	if total == 0 {
		t.Fatal("the spec declares no operations")
	}
}

func TestUnifiedDiff(t *testing.T) {
	if diff := UnifiedDiff("a", "b", []byte("one\ntwo\n"), []byte("one\ntwo\n")); diff != "" {
		t.Fatalf("equal input produced a diff:\n%s", diff)
	}

	diff := UnifiedDiff("old", "new", []byte("one\ntwo\nthree\n"), []byte("one\n2\nthree\n"))
	for _, want := range []string{"--- old", "+++ new", "@@", "-two", "+2", " three"} {
		if !strings.Contains(diff, want) {
			t.Errorf("the diff does not contain %q:\n%s", want, diff)
		}
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
