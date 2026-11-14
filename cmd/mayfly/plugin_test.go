package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hayfever/mayfly/internal/plugin"
)

// wordCountExample is the inline TypeScript example the CLI tests drive.
const wordCountExample = "../../examples/plugins/word-count"

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = write
	result := make(chan []byte, 1)
	go func() {
		raw, _ := io.ReadAll(read)
		result <- raw
	}()
	defer func() {
		write.Close()
		os.Stdout = old
	}()
	runErr := fn()
	write.Close()
	os.Stdout = old
	return strings.TrimSpace(string(<-result)), runErr
}

// scaffold runs mayfly plugin init for one language.
func scaffold(t *testing.T, lang, dir string) string {
	t.Helper()
	out, err := captureStdout(t, func() error {
		return cmdPlugin([]string{"init", "--lang", lang, dir})
	})
	if err != nil {
		t.Fatalf("init --lang %s: %v", lang, err)
	}
	return out
}

// requireTool skips a test when the interpreter a scaffold needs is absent.
func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s is not installed", name)
	}
}

func TestInitWritesValidManifest(t *testing.T) {
	cases := []struct {
		lang  string
		files []string
	}{
		{"go", []string{"plugin.yaml", "main.go", "go.mod", "Dockerfile", ".dockerignore"}},
		{"python", []string{"plugin.yaml", "server.py", "requirements.txt", "Dockerfile", ".dockerignore"}},
		{"ts", []string{"plugin.yaml", "src/index.ts", "package.json", "tsconfig.json", "Dockerfile", ".dockerignore"}},
	}
	for _, tc := range cases {
		t.Run(tc.lang, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "word-stats")
			out := scaffold(t, tc.lang, dir)
			for _, name := range tc.files {
				path := filepath.Join(dir, filepath.FromSlash(name))
				if info, err := os.Stat(path); err != nil || info.IsDir() {
					t.Fatalf("%s: %v", path, err)
				}
				if !strings.Contains(out, path) {
					t.Errorf("init does not report writing %s: %q", path, out)
				}
			}
			manifest, err := plugin.Load(dir, plugin.ValidateOptions{RequireImage: true})
			if err != nil {
				t.Fatalf("generated manifest: %v", err)
			}
			if manifest.Name != "word-stats" {
				t.Errorf("name = %q, want word-stats", manifest.Name)
			}
			if manifest.Placement != plugin.Inline {
				t.Errorf("placement = %q, want inline", manifest.Placement)
			}
			if len(manifest.Tools) != 1 || manifest.Tools[0].Name != "send" {
				t.Fatalf("tools = %+v, want one tool named send", manifest.Tools)
			}
			if manifest.Tools[0].Idempotent {
				t.Error("the sample tool must stay at idempotent: false")
			}
			source := readScaffoldSource(t, dir, tc.lang)
			for _, want := range []string{"mayfly.io/idempotency-key", "Idempotency-Key"} {
				if !strings.Contains(source, want) {
					t.Errorf("the sample tool does not mention %q", want)
				}
			}
		})
	}
}

// readScaffoldSource is the executable part of one scaffold, as one string.
func readScaffoldSource(t *testing.T, dir, lang string) string {
	t.Helper()
	names := map[string][]string{
		"go":     {"main.go"},
		"python": {"server.py"},
		"ts":     {filepath.Join("src", "index.ts")},
	}[lang]
	var b strings.Builder
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
	}
	return b.String()
}

func TestInitRefusesNonEmptyDir(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(keep, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := captureStdout(t, func() error {
		return cmdPlugin([]string{"init", dir})
	})
	if err == nil {
		t.Fatal("init wrote into a non-empty directory")
	}
	if !strings.Contains(err.Error(), "not empty") || !strings.Contains(err.Error(), "keep.txt") {
		t.Fatalf("error = %v, want the directory and the first entry", err)
	}
	raw, readErr := os.ReadFile(keep)
	if readErr != nil || string(raw) != "mine" {
		t.Fatalf("init changed an existing file: %q, %v", raw, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "plugin.yaml")); statErr == nil {
		t.Fatal("init wrote a manifest into a non-empty directory")
	}
}

func TestCheckGoTemplate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scaffold-check")
	scaffold(t, "go", dir)
	out, err := captureStdout(t, func() error {
		return cmdPlugin([]string{"check", dir})
	})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	const want = "scaffold-check placement=inline tools=1: send"
	if out != want {
		t.Fatalf("check printed %q, want %q", out, want)
	}
}

func TestCheckPythonAndTSTemplates(t *testing.T) {
	cases := []struct{ lang, tool string }{
		{"python", "python3"},
		{"ts", "node"},
	}
	for _, tc := range cases {
		t.Run(tc.lang, func(t *testing.T) {
			requireTool(t, tc.tool)
			dir := filepath.Join(t.TempDir(), "scaffold-check")
			scaffold(t, tc.lang, dir)
			if _, err := captureStdout(t, func() error {
				return cmdPlugin([]string{"check", dir})
			}); err != nil {
				t.Fatalf("check: %v", err)
			}
		})
	}
}

// TestScaffoldForwardsIdempotencyKey proves the sample tool of every scaffold
// forwards _meta["mayfly.io/idempotency-key"] as an Idempotency-Key header.
func TestScaffoldForwardsIdempotencyKey(t *testing.T) {
	type received struct {
		key  string
		body string
	}
	for _, tc := range []struct{ lang, tool string }{
		{"go", "go"},
		{"python", "python3"},
		{"ts", "node"},
	} {
		t.Run(tc.lang, func(t *testing.T) {
			requireTool(t, tc.tool)
			calls := make(chan received, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				calls <- received{key: r.Header.Get("Idempotency-Key"), body: string(body)}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			dir := filepath.Join(t.TempDir(), "key-forward")
			scaffold(t, tc.lang, dir)
			origin := filepath.Join(dir, "plugin.yaml")
			raw, err := os.ReadFile(origin)
			if err != nil {
				t.Fatal(err)
			}
			env := "\nenv:\n  SAMPLE_API_URL: " + server.URL + "\n"
			if err := os.WriteFile(origin, append(raw, []byte(env)...), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := captureStdout(t, func() error {
				return cmdPlugin([]string{"test", dir, "send", `{"text":"hello"}`, "--key", "s1/2/3"})
			})
			if err != nil {
				t.Fatalf("test: %v", err)
			}
			if !strings.Contains(out, "status 200") {
				t.Fatalf("output = %q, want the HTTP status", out)
			}
			call := <-calls
			if call.key != "s1/2/3" {
				t.Errorf("Idempotency-Key = %q, want the gateway key s1/2/3", call.key)
			}
			if !strings.Contains(call.body, `"hello"`) {
				t.Errorf("body = %q, want the tool argument", call.body)
			}
		})
	}
}

func TestPluginTestWordCount(t *testing.T) {
	requireTool(t, "node")
	workspace := t.TempDir()
	notes := filepath.Join(workspace, "notes.txt")
	if err := os.WriteFile(notes, []byte("one two three\nfour five\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return cmdPlugin([]string{"test", wordCountExample, "count_words", `{"path":"notes.txt"}`, "--workspace", workspace, "--key", "s1/1/1"})
	})
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	if out != "5" {
		t.Fatalf("test printed %q, want the word count 5", out)
	}
}

func TestPluginTestErrors(t *testing.T) {
	t.Run("unknown tool", func(t *testing.T) {
		_, err := captureStdout(t, func() error {
			return cmdPlugin([]string{"test", wordCountExample, "count_lines", "{}"})
		})
		if err == nil || !strings.Contains(err.Error(), `declares no tool "count_lines"`) {
			t.Fatalf("error = %v, want an unknown-tool message", err)
		}
		if !strings.Contains(err.Error(), "count_words") {
			t.Fatalf("error = %v, want the declared tools", err)
		}
	})
	t.Run("invalid JSON", func(t *testing.T) {
		_, err := captureStdout(t, func() error {
			return cmdPlugin([]string{"test", wordCountExample, "count_words", `{"path":`})
		})
		if err == nil || !strings.Contains(err.Error(), "JSON object") {
			t.Fatalf("error = %v, want an invalid-JSON message", err)
		}
	})
	t.Run("missing secret", func(t *testing.T) {
		const name = "CLI_TEST_MISSING_SECRET"
		t.Setenv(name, "")
		dir := filepath.Join(t.TempDir(), "needs-secret")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		manifest := `schemaVersion: 1
name: needs-secret
placement: inline
command: /bin/true
tools:
  - name: ping
secrets:
  - env: ` + name + `
    secret: mayfly-plugin-needs-secret
    key: token
`
		if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := captureStdout(t, func() error {
			return cmdPlugin([]string{"check", dir})
		})
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("error = %v, want a missing-secret message naming %s", err, name)
		}
	})
	t.Run("error result", func(t *testing.T) {
		requireTool(t, "node")
		workspace := t.TempDir()
		out, err := captureStdout(t, func() error {
			return cmdPlugin([]string{"test", wordCountExample, "count_words", `{"path":"absent.txt"}`, "--workspace", workspace})
		})
		if err == nil || !strings.Contains(err.Error(), "returned an error") {
			t.Fatalf("error = %v, want an error-result message", err)
		}
		if !strings.Contains(out, "absent.txt") {
			t.Fatalf("output = %q, want the tool's error text", out)
		}
	})
}
