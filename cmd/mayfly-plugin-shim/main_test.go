package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShimInstallSelf(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "mayfly-plugin-shim")
	var stderr bytes.Buffer
	if code := run([]string{"install-self", dst}, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("installed copy differs from the running executable")
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("installed mode = %o, want 755", info.Mode().Perm())
	}
}

func TestShimCopyTree(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "bin", "tool"), "#!/bin/sh\necho hi\n", 0o755)
	writeTestFile(t, filepath.Join(src, "lib", "data.txt"), "payload", 0o644)
	writeTestFile(t, filepath.Join(src, "writable.txt"), "group write", 0o666)
	if err := os.MkdirAll(filepath.Join(src, "nested", "deep"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(src, "nested", "deep", "file.txt"), "nested", 0o640)
	if err := os.Symlink("tool", filepath.Join(src, "bin", "alias")); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "plugins", "demo")
	var stderr bytes.Buffer
	if code := run([]string{"copy", src, dst}, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}

	checks := []struct {
		path string
		mode os.FileMode
		body string
	}{
		{"bin/tool", 0o755, "#!/bin/sh\necho hi\n"},
		{"lib/data.txt", 0o644, "payload"},
		// Group and other write bits are dropped.
		{"writable.txt", 0o644, "group write"},
		{"nested/deep/file.txt", 0o640, "nested"},
	}
	for _, check := range checks {
		path := filepath.Join(dst, check.path)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", check.path, err)
		}
		if string(body) != check.body {
			t.Fatalf("%s = %q, want %q", check.path, body, check.body)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != check.mode {
			t.Fatalf("%s mode = %o, want %o", check.path, info.Mode().Perm(), check.mode)
		}
	}
	dir, err := os.Stat(filepath.Join(dst, "nested"))
	if err != nil {
		t.Fatal(err)
	}
	if dir.Mode().Perm() != 0o750 {
		t.Fatalf("directory mode = %o, want 750", dir.Mode().Perm())
	}
	link, err := os.Readlink(filepath.Join(dst, "bin", "alias"))
	if err != nil {
		t.Fatalf("symlink was not copied: %v", err)
	}
	if link != "tool" {
		t.Fatalf("symlink target = %q, want tool", link)
	}
}

func TestShimCopyRejectsEscapingSymlink(t *testing.T) {
	for _, target := range []string{"../outside", "/etc/passwd"} {
		t.Run(target, func(t *testing.T) {
			src := t.TempDir()
			writeTestFile(t, filepath.Join(src, "tool"), "x", 0o755)
			if err := os.Symlink(target, filepath.Join(src, "escape")); err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(t.TempDir(), "demo")
			var stderr bytes.Buffer
			if code := run([]string{"copy", src, dst}, &stderr); code == 0 {
				t.Fatal("copy accepted a symlink that leaves the plugin tree")
			}
			if !strings.Contains(stderr.String(), "symlink target") {
				t.Fatalf("stderr = %q", stderr.String())
			}
		})
	}
}

func TestShimCopyRejectsChainedSymlinkEscape(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "a", "tool"), "x", 0o755)
	// Each target looks local: ".." cleans to the plugin root, and the chain
	// through it cleans there too. Together they resolve one level above it.
	if err := os.Symlink("..", filepath.Join(src, "a", "s")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a/s/../..", filepath.Join(src, "t")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "demo")
	var stderr bytes.Buffer
	if code := run([]string{"copy", src, dst}, &stderr); code == 0 {
		t.Fatal("copy accepted a symlink chain that leaves the plugin tree")
	}
	if !strings.Contains(stderr.String(), "escapes the plugin directory") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestShimCopyAcceptsInternalSymlinkChain(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "bin", "tool"), "x", 0o755)
	if err := os.Symlink("tool", filepath.Join(src, "bin", "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "bin", "alias"), filepath.Join(src, "bin", "again")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "demo")
	var stderr bytes.Buffer
	if code := run([]string{"copy", src, dst}, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	body, err := os.ReadFile(filepath.Join(dst, "bin", "again"))
	if err != nil {
		t.Fatalf("chained link does not resolve: %v", err)
	}
	if string(body) != "x" {
		t.Fatalf("chained link reads %q", body)
	}
}

func TestShimCopyMissingDirectory(t *testing.T) {
	src := filepath.Join(t.TempDir(), "absent")
	var stderr bytes.Buffer
	if code := run([]string{"copy", src, filepath.Join(t.TempDir(), "demo")}, &stderr); code == 0 {
		t.Fatal("copy accepted a missing plugin directory")
	}
	if !strings.Contains(stderr.String(), "plugin image has no") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestShimSubcommandUsage(t *testing.T) {
	for _, sub := range []string{"serve", "probe"} {
		var stderr bytes.Buffer
		if code := run([]string{sub}, &stderr); code != 2 {
			t.Fatalf("%s exit = %d, want 2", sub, code)
		}
		if !strings.Contains(stderr.String(), "--listen") {
			t.Fatalf("%s stderr = %q", sub, stderr.String())
		}
	}
}

func writeTestFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	// The umask narrows the create mode; the test asserts the source mode.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
