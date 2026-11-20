package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updateSurface rewrites the golden file instead of comparing with it.
var updateSurface = flag.Bool("update", false, "rewrite cmd/mayfly/testdata/cli-surface.txt")

// surfaceGolden is the committed record of the CLI surface.
const surfaceGolden = "testdata/cli-surface.txt"

// surfaceText returns the surface in the file form.
func surfaceText() string {
	return strings.Join(surfaceLines(), "\n") + "\n"
}

// TestCLISurface fails when the code and the golden file disagree, so a flag
// change reaches a review as a golden diff.
func TestCLISurface(t *testing.T) {
	got := surfaceText()
	if *updateSurface {
		if err := os.MkdirAll(filepath.Dir(surfaceGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(surfaceGolden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(surfaceGolden)
	if err != nil {
		t.Fatalf("%v; regenerate with `go test ./cmd/mayfly -run TestCLISurface -update`", err)
	}
	if got != string(want) {
		t.Fatalf("the CLI surface differs from %s:\n%sregenerate with `go test ./cmd/mayfly -run TestCLISurface -update`",
			surfaceGolden, surfaceLineDiff(string(want), got))
	}
}

// surfaceLineDiff lists the lines that one side does not have, in file order.
func surfaceLineDiff(want, got string) string {
	wantLines := surfaceLinesOf(want)
	gotLines := surfaceLinesOf(got)
	inWant := map[string]bool{}
	for _, line := range wantLines {
		inWant[line] = true
	}
	inGot := map[string]bool{}
	for _, line := range gotLines {
		inGot[line] = true
	}
	var b strings.Builder
	for _, line := range wantLines {
		if !inGot[line] {
			fmt.Fprintf(&b, "- %s\n", line)
		}
	}
	for _, line := range gotLines {
		if !inWant[line] {
			fmt.Fprintf(&b, "+ %s\n", line)
		}
	}
	return b.String()
}

// surfaceLinesOf splits one surface file into its non-empty lines.
func surfaceLinesOf(text string) []string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestSurfaceCoversEveryCommand proves that the surface lists every command
// word main dispatches, and that every surface word has a handler.
func TestSurfaceCoversEveryCommand(t *testing.T) {
	names := map[string]bool{}
	aliases := map[string]bool{}
	parents := map[string][]string{}
	for _, cmd := range cliSurface {
		if names[cmd.name] {
			t.Fatalf("the surface lists %q twice", cmd.name)
		}
		names[cmd.name] = true
		for _, alias := range cmd.aliases {
			if aliases[alias] {
				t.Fatalf("the surface lists the alias %q twice", alias)
			}
			aliases[alias] = true
		}
		if len(cmd.subcommands) > 0 {
			parents[cmd.name] = cmd.subcommands
		}
	}

	for word := range commands {
		if !names[word] && !aliases[word] {
			t.Errorf("main dispatches %q, but the surface does not list it", word)
		}
	}
	for name := range names {
		if strings.Contains(name, " ") {
			continue // A subcommand word is dispatched by its parent.
		}
		if _, ok := commands[name]; !ok {
			t.Errorf("the surface lists %q, but main does not dispatch it", name)
		}
	}
	for alias := range aliases {
		if _, ok := commands[alias]; !ok {
			t.Errorf("the surface lists the alias %q, but main does not dispatch it", alias)
		}
	}

	for name := range names {
		fields := strings.Fields(name)
		if len(fields) == 1 {
			continue
		}
		if len(fields) != 2 {
			t.Fatalf("the surface name %q has more than two words", name)
		}
		if !names[fields[0]] {
			t.Errorf("the surface lists %q, but not its parent command %q", name, fields[0])
		}
		if !containsWord(parents[fields[0]], fields[1]) {
			t.Errorf("the surface lists %q, but %q does not declare it as a subcommand", name, fields[0])
		}
	}
	for parent, words := range parents {
		for _, word := range words {
			if !names[parent+" "+word] {
				t.Errorf("%q declares the subcommand %q, but the surface does not list it", parent, word)
			}
		}
	}
}

// TestSurfaceCoversPluginSubcommands proves that the surface lists every word
// cmdPlugin dispatches, and no other word.
func TestSurfaceCoversPluginSubcommands(t *testing.T) {
	surfaced := map[string]bool{}
	for _, cmd := range cliSurface {
		if cmd.name != "plugin" {
			continue
		}
		for _, word := range cmd.subcommands {
			surfaced[word] = true
		}
	}
	for word := range pluginCommands {
		if !surfaced[word] {
			t.Errorf("cmdPlugin dispatches %q, but the surface does not list it", word)
		}
	}
	for word := range surfaced {
		if _, ok := pluginCommands[word]; !ok {
			t.Errorf("the surface lists the plugin subcommand %q, but cmdPlugin does not dispatch it", word)
		}
	}
}

// TestSurfaceFlagsMatch proves that each surface command names the flag set
// its command parses, and that each declared alias names two flags of one
// type.
func TestSurfaceFlagsMatch(t *testing.T) {
	for _, cmd := range cliSurface {
		if cmd.newFlags == nil {
			continue
		}
		fs := cmd.newFlags()
		if fs.Name() != cmd.name {
			t.Errorf("the surface entry %q builds the flag set %q", cmd.name, fs.Name())
		}
		for _, pair := range cmd.flagAliases {
			short, long := fs.Lookup(pair[0]), fs.Lookup(pair[1])
			if short == nil || long == nil {
				t.Errorf("%q: the alias %v is not a pair of flags", cmd.name, pair)
				continue
			}
			if fmt.Sprintf("%T", short.Value) != fmt.Sprintf("%T", long.Value) {
				t.Errorf("%q: %q is %T, but %q is %T", cmd.name, pair[0], short.Value, pair[1], long.Value)
			}
			if short.DefValue != long.DefValue {
				t.Errorf("%q: %q defaults to %q, but %q defaults to %q", cmd.name, pair[0], short.DefValue, pair[1], long.DefValue)
			}
		}
	}
}

// containsWord reports whether words holds want.
func containsWord(words []string, want string) bool {
	for _, word := range words {
		if word == want {
			return true
		}
	}
	return false
}
