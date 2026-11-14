// Command mayfly-plugin-shim stages plugins into a session pod and adapts a
// stdio plugin to the transports Mayfly uses: it copies plugin trees into the
// pod, and it serves one stdio plugin over streamable HTTP for the shim's
// clients.
package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	// maxCopyBytes backstops one plugin tree. The pod volume limit stops a
	// pod that fills the disk; this stops a copy that would never fit.
	maxCopyBytes = 1 << 30
	// pluginImageDir is the directory every plugin image must contain.
	pluginImageDir = "/mayfly-plugin"
	// installMode is the mode of an installed executable.
	installMode fs.FileMode = 0o755
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run dispatches one subcommand and returns the process exit code.
func run(args []string, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "install-self":
		if len(args) != 2 {
			usage(stderr)
			return 2
		}
		if err := installSelf(args[1]); err != nil {
			fmt.Fprintf(stderr, "mayfly-plugin-shim: install-self: %v\n", err)
			return 1
		}
		return 0
	case "copy":
		if len(args) != 3 {
			usage(stderr)
			return 2
		}
		if err := copyTree(args[1], args[2]); err != nil {
			fmt.Fprintf(stderr, "mayfly-plugin-shim: copy: %v\n", err)
			return 1
		}
		return 0
	case "serve":
		return runServe(args[1:], stderr)
	case "probe":
		return runProbe(args[1:], stderr)
	default:
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, "usage: mayfly-plugin-shim install-self <dst>\n"+
		"       mayfly-plugin-shim copy <src> <dst>\n"+
		"       mayfly-plugin-shim serve --listen unix:PATH|tcp:ADDR --key-file FILE [--session ID] [--max-concurrency N] [--init-timeout D] -- COMMAND [ARG...]\n"+
		"       mayfly-plugin-shim probe --listen unix:PATH|tcp:ADDR [--timeout D]\n")
}

// installSelf copies the running executable to dst. The tools image is a
// normal program image and starts the shim directly; this puts a copy on the
// pod volume that the plugin copy containers and sidecars mount.
func installSelf(dst string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	srcInfo, err := os.Stat(exe)
	if err != nil {
		return err
	}
	if dstInfo, err := os.Stat(dst); err == nil && os.SameFile(srcInfo, dstInfo) {
		return fmt.Errorf("%s is the running executable", dst)
	}
	return copyFile(exe, dst, installMode)
}

// directoryMode records a source directory mode, applied after the walk so a
// read-only directory does not block its own children.
type directoryMode struct {
	path string
	mode fs.FileMode
}

// copyTree copies the plugin tree at src to dst. The plugin image's
// entrypoint never runs, so the tree is the whole contract between Mayfly and
// a plugin image.
func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("plugin image has no %s directory", src)
	}
	root, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, installMode); err != nil {
		return err
	}

	var copied int64
	var dirs []directoryMode
	var links []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		entryInfo, err := entry.Info()
		if err != nil {
			return err
		}
		mode := entryInfo.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(target, installMode); err != nil {
				return err
			}
			dirs = append(dirs, directoryMode{path: target, mode: copyMode(mode)})
			return nil
		case mode&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := checkLink(rel, link); err != nil {
				return err
			}
			// A retried init container finds the previous copy.
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
			links = append(links, target)
			return nil
		case mode.IsRegular():
			copied += entryInfo.Size()
			if copied > maxCopyBytes {
				return fmt.Errorf("%s: plugin image exceeds %d bytes", rel, int64(maxCopyBytes))
			}
			return copyFile(path, target, copyMode(mode))
		case mode&fs.ModeDevice != 0:
			return fmt.Errorf("%s: device files are not permitted", rel)
		default:
			return fmt.Errorf("%s: unsupported file type %s", rel, mode.Type())
		}
	})
	if err != nil {
		return err
	}
	// Reverse order visits children before their parents.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i].path, dirs[i].mode); err != nil {
			return err
		}
	}
	// checkLink already refused an absolute or lexically escaping target. The
	// copy is complete, so the links can be resolved for real: a chain through
	// another link can leave the tree although every target looks local.
	return checkResolvedLinks(dst, links)
}

// checkLink refuses a symlink that leaves the plugin tree. The copy is mounted
// at a different path, so an absolute link or one that climbs above the tree
// would point outside the plugin directory in the session pod.
func checkLink(rel, link string) error {
	if filepath.IsAbs(link) {
		return fmt.Errorf("%s: symlink target %q is absolute", rel, link)
	}
	joined := filepath.Clean(filepath.Join(filepath.Dir(rel), link))
	if joined == ".." || strings.HasPrefix(joined, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s: symlink target %q escapes the plugin directory", rel, link)
	}
	return nil
}

// checkResolvedLinks refuses a copied symlink whose target resolves outside
// dst. links holds the copied link paths, and the destination is resolved as a
// whole first, so the comparison is against the real directory the pod mounts.
//
// A lexical check alone accepts a/s -> .. and then t -> a/s/../..: both targets
// look local, but together they point above the plugin directory. EvalSymlinks
// follows the chain.
func checkResolvedLinks(dst string, links []string) error {
	if len(links) == 0 {
		return nil
	}
	root, err := filepath.EvalSymlinks(dst)
	if err != nil {
		return err
	}
	for _, link := range links {
		rel, err := filepath.Rel(dst, link)
		if err != nil {
			return err
		}
		resolved, err := filepath.EvalSymlinks(link)
		if err != nil {
			return fmt.Errorf("%s: symlink target: %w", rel, err)
		}
		inside, err := filepath.Rel(root, resolved)
		if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%s: symlink target %q escapes the plugin directory", rel, resolved)
		}
	}
	return nil
}

// copyMode keeps the source mode but drops group and other write. The files
// are read by one uid in the pod, so a wider write bit has no use.
func copyMode(mode fs.FileMode) fs.FileMode {
	return mode.Perm() &^ 0o022
}

// copyFile writes src to dst with mode, setting the mode explicitly because
// the process umask would otherwise narrow it.
func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Chmod(mode); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
