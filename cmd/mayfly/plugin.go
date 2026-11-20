package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/hayfever/mayfly/internal/plugin"
	"github.com/hayfever/mayfly/internal/pluginhost"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

// pluginTemplateFS holds the scaffolds mayfly plugin init writes. The all:
// prefix keeps .dockerignore, which a bare embed pattern would skip.
//
//go:embed all:plugintemplates
var pluginTemplateFS embed.FS

// pluginLangs lists the language scaffolds. The name is the template
// directory and the --lang value.
var pluginLangs = map[string]bool{"go": true, "python": true, "ts": true}

// localInitTimeout is the floor for a local start. A local command can
// compile before it serves, and go run builds from an empty cache in tens of
// seconds; a pod only starts a process, so the manifest's initTimeout is the
// right bound there and this one is the right bound here.
const localInitTimeout = 3 * time.Minute

// scaffoldData fills one plugin template.
type scaffoldData struct {
	Name    string
	Version string
	Image   string
}

// localRun is one plugin started the way mayfly plugin check and test start
// it: as an inline plugin, on this machine, with this process's environment.
type localRun struct {
	host      *pluginhost.Host
	artifacts string
	secrets   string
	workspace string
	owned     bool
}

// close stops the plugin and removes the temporary directories of the run.
// The workspace is removed only when this run created it.
func (r *localRun) close() {
	if r.host != nil {
		r.host.Close()
		r.host = nil
	}
	for _, dir := range []string{r.artifacts, r.secrets} {
		if dir != "" {
			_ = os.RemoveAll(dir)
		}
	}
	r.artifacts, r.secrets = "", ""
	if r.owned {
		_ = os.RemoveAll(r.workspace)
		r.owned = false
	}
}

// pluginCommands maps each mayfly plugin subcommand word to its handler.
// surface.go lists the same words, and the surface test fails when the two
// disagree.
var pluginCommands = map[string]func([]string) error{
	"init":  cmdPluginInit,
	"check": cmdPluginCheck,
	"test":  cmdPluginTest,
}

// cmdPlugin runs the offline plugin commands. None of them reads MAYFLY_URL or
// a token: placement does not change what the plugin process sees, so a
// plugin that passes here behaves the same in every placement.
func cmdPlugin(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: mayfly plugin init|check|test ...")
	}
	switch args[0] {
	case "help", "-h", "--help":
		pluginUsage()
		return nil
	}
	run, ok := pluginCommands[args[0]]
	if !ok {
		return fmt.Errorf("unknown plugin command %q", args[0])
	}
	return run(args[1:])
}

// pluginUsage prints the subcommands of mayfly plugin.
func pluginUsage() {
	fmt.Fprint(os.Stderr, `mayfly plugin - offline plugin tools

Commands:
  init --lang go|python|ts DIR   write a plugin scaffold into DIR
  check DIR                      validate the manifest against the server
  test DIR TOOL JSON [--workspace DIR] [--key KEY] [--timeout 2m]
                                 check, then call one tool
`)
}

// pluginInitFlags holds the flags of mayfly plugin init.
type pluginInitFlags struct {
	*flag.FlagSet
	lang *string
}

// newPluginInitFlags builds the flag set that mayfly plugin init parses.
func newPluginInitFlags() *pluginInitFlags {
	flags := flag.NewFlagSet("plugin init", flag.ContinueOnError)
	return &pluginInitFlags{FlagSet: flags, lang: flags.String("lang", "go", "go, python, or ts")}
}

// cmdPluginInit writes a scaffold. The plugin name defaults to the directory
// name, and the manifest is validated before anything reaches the disk.
func cmdPluginInit(args []string) error {
	f := newPluginInitFlags()
	positional, err := parseArgs(f.FlagSet, args, "lang")
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("usage: mayfly plugin init --lang go|python|ts DIR")
	}
	if !pluginLangs[*f.lang] {
		return fmt.Errorf("unknown language %q (use go, python, or ts)", *f.lang)
	}
	dir, err := filepath.Abs(positional[0])
	if err != nil {
		return err
	}
	name := filepath.Base(dir)
	if name == "." || name == string(filepath.Separator) || name == "" {
		return fmt.Errorf("%s: cannot derive a plugin name from this directory", positional[0])
	}
	data := scaffoldData{
		Name:    name,
		Version: "0.1.0",
		Image:   "ghcr.io/example/mayfly-plugin-" + name + ":0.1.0",
	}
	files, err := renderScaffold(*f.lang, data)
	if err != nil {
		return err
	}
	if err := checkManifest(files[plugin.ManifestFile], filepath.Join(dir, plugin.ManifestFile)); err != nil {
		return err
	}
	written, err := writeScaffold(dir, files)
	if err != nil {
		return err
	}
	for _, file := range written {
		fmt.Println("wrote", filepath.Join(dir, file))
	}
	fmt.Println("next: mayfly plugin check", positional[0])
	return nil
}

// renderScaffold executes every template of one language. The destination
// path is the template path without the .tmpl suffix.
func renderScaffold(lang string, data scaffoldData) (map[string][]byte, error) {
	root := path.Join("plugintemplates", lang)
	files := make(map[string][]byte)
	err := fs.WalkDir(pluginTemplateFS, root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		raw, err := pluginTemplateFS.ReadFile(name)
		if err != nil {
			return err
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(name, root+"/"), ".tmpl")
		tmpl, err := template.New(rel).Option("missingkey=error").Parse(string(raw))
		if err != nil {
			return fmt.Errorf("template %s: %w", name, err)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, data); err != nil {
			return fmt.Errorf("template %s: %w", name, err)
		}
		files[rel] = buf.Bytes()
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no scaffold for language %q", lang)
	}
	return files, nil
}

// checkManifest validates a rendered manifest before init writes anything, so
// a directory name that is not a plugin name fails without a partial tree.
func checkManifest(raw []byte, origin string) error {
	manifest, err := plugin.Parse(raw, origin)
	if err != nil {
		return err
	}
	return manifest.Validate(plugin.ValidateOptions{RequireImage: true})
}

// writeScaffold creates the directory and every file. An existing non-empty
// directory is refused rather than overwritten.
func writeScaffold(dir string, files map[string][]byte) ([]string, error) {
	if err := checkScaffoldDir(dir); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		target := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(target, files[name], 0o644); err != nil {
			return nil, err
		}
	}
	return names, nil
}

// checkScaffoldDir accepts a missing directory or an empty one.
func checkScaffoldDir(dir string) error {
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case !info.IsDir():
		return fmt.Errorf("%s is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s is not empty (found %s)", dir, entries[0].Name())
	}
	return nil
}

// pluginCheckFlags holds the flags of mayfly plugin check. The command reads
// one directory, so the flag set is empty.
type pluginCheckFlags struct {
	*flag.FlagSet
}

// newPluginCheckFlags builds the flag set that mayfly plugin check parses.
func newPluginCheckFlags() *pluginCheckFlags {
	return &pluginCheckFlags{FlagSet: flag.NewFlagSet("plugin check", flag.ContinueOnError)}
}

// cmdPluginCheck validates the manifest, starts the plugin with its local
// command, and compares the reported tools with the declared ones.
func cmdPluginCheck(args []string) error {
	f := newPluginCheckFlags()
	positional, err := parseArgs(f.FlagSet, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("usage: mayfly plugin check DIR")
	}
	manifest, err := loadLocalManifest(positional[0])
	if err != nil {
		return err
	}
	run, err := startLocal(manifest, "")
	if err != nil {
		return err
	}
	defer run.close()
	names := toolNames(manifest)
	fmt.Printf("%s placement=%s tools=%d: %s\n", manifest.Name, manifest.Placement, len(names), strings.Join(names, ", "))
	return nil
}

// pluginTestFlags holds the flags of mayfly plugin test.
type pluginTestFlags struct {
	*flag.FlagSet
	workspace *string
	key       *string
	timeout   *time.Duration
}

// newPluginTestFlags builds the flag set that mayfly plugin test parses.
func newPluginTestFlags() *pluginTestFlags {
	flags := flag.NewFlagSet("plugin test", flag.ContinueOnError)
	f := &pluginTestFlags{FlagSet: flags}
	f.workspace = flags.String("workspace", "", "workspace directory (default: a temporary directory)")
	f.key = flags.String("key", "", "idempotency key passed to the tool")
	f.timeout = flags.Duration("timeout", 2*time.Minute, "call timeout")
	return f
}

// cmdPluginTest checks the plugin like check does, then calls one tool and
// prints its result. An error result exits non-zero.
func cmdPluginTest(args []string) error {
	f := newPluginTestFlags()
	positional, err := parseArgs(f.FlagSet, args, "workspace", "key", "timeout")
	if err != nil {
		return err
	}
	if len(positional) != 3 {
		return fmt.Errorf("usage: mayfly plugin test DIR TOOL JSON [--workspace DIR] [--key KEY] [--timeout 2m]")
	}
	manifest, err := loadLocalManifest(positional[0])
	if err != nil {
		return err
	}
	tool, ok := declaredTool(manifest, positional[1])
	if !ok {
		return fmt.Errorf("plugin %q declares no tool %q (declared: %s)", manifest.Name, positional[1], strings.Join(toolNames(manifest), ", "))
	}
	var arguments map[string]any
	if err := json.Unmarshal([]byte(positional[2]), &arguments); err != nil {
		return fmt.Errorf("arguments must be a JSON object: %w", err)
	}
	run, err := startLocal(manifest, *f.workspace)
	if err != nil {
		return err
	}
	defer run.close()
	ctx, cancel := context.WithTimeout(context.Background(), *f.timeout)
	defer cancel()
	name := plugin.ToolName(manifest.Name, tool.Name)
	content, isErr, err := run.host.Call(ctx, name, []byte(positional[2]), *f.key, "")
	if err != nil {
		return err
	}
	fmt.Println(content)
	if isErr {
		return fmt.Errorf("tool %s returned an error", name)
	}
	return nil
}

// loadLocalManifest reads and validates one manifest. The image is optional:
// the CLI never pulls one, so the kubernetes requirement does not apply.
func loadLocalManifest(dir string) (plugin.Manifest, error) {
	return plugin.Load(dir, plugin.ValidateOptions{})
}

// declaredTool finds a declared tool by its manifest name.
func declaredTool(manifest plugin.Manifest, name string) (plugin.Tool, bool) {
	for _, tool := range manifest.Tools {
		if tool.Name == name {
			return tool, true
		}
	}
	return plugin.Tool{}, false
}

// toolNames lists the declared tool names, in manifest order.
func toolNames(manifest plugin.Manifest) []string {
	names := make([]string, 0, len(manifest.Tools))
	for _, tool := range manifest.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// startLocal runs the plugin as an inline process on this machine. A failed
// start carries the plugin's stderr, which is the only place a crashing
// interpreter explains itself.
func startLocal(manifest plugin.Manifest, workspace string) (*localRun, error) {
	run := &localRun{}
	if workspace == "" {
		dir, err := os.MkdirTemp("", "mayfly-plugin-workspace-")
		if err != nil {
			run.close()
			return nil, err
		}
		workspace, run.owned = dir, true
	}
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		run.close()
		return nil, err
	}
	run.workspace = absolute
	if run.secrets, err = writeSecrets(manifest); err != nil {
		run.close()
		return nil, err
	}
	if run.artifacts, err = os.MkdirTemp("", "mayfly-plugin-artifacts-"); err != nil {
		run.close()
		return nil, err
	}
	def := localDef(manifest, run.secrets)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	host, errs := pluginhost.Start(context.Background(), []sessioncfg.Plugin{def}, pluginhost.Options{
		SessionID:    "local",
		Agent:        "mayfly",
		WorkspaceDir: run.workspace,
		ArtifactsDir: run.artifacts,
		Logger:       logger,
	})
	run.host = host
	if len(errs) > 0 {
		err := errors.Join(errs...)
		if tail := stderrTail(run.artifacts, def.Name); tail != "" {
			run.close()
			return nil, fmt.Errorf("%w\nplugin stderr:\n%s", err, tail)
		}
		run.close()
		return nil, err
	}
	if hidden := host.Hidden()[def.Name]; len(hidden) > 0 {
		run.close()
		return nil, fmt.Errorf("plugin %q reports tools that plugin.yaml does not declare: %s; declare them, or stop the server reporting them",
			def.Name, strings.Join(hidden, ", "))
	}
	return run, nil
}

// localDef is the runtime description of one manifest, as the local backend
// builds it: always inline, with the local command that runs on this machine.
func localDef(manifest plugin.Manifest, secretsDir string) sessioncfg.Plugin {
	def := sessioncfg.Plugin{
		Name:               manifest.Name,
		Placement:          string(plugin.Inline),
		Dir:                manifest.Dir,
		Command:            manifest.Local.Command,
		Args:               append([]string(nil), manifest.Local.Args...),
		Workdir:            "plugin",
		Env:                manifest.Env,
		SecretsDir:         secretsDir,
		InitTimeoutSeconds: localInitSeconds(manifest),
		MaxOutputBytes:     manifest.Limits.MaxOutputBytes,
	}
	if manifest.Inline != nil {
		def.Workdir = manifest.Inline.Workdir
		def.StderrBytes = manifest.Inline.StderrBytes
	}
	for _, secret := range manifest.Secrets {
		def.Secrets = append(def.Secrets, sessioncfg.PluginSecret{Env: secret.Env})
	}
	for _, tool := range manifest.Tools {
		def.Tools = append(def.Tools, sessioncfg.PluginTool{
			Name:           tool.Name,
			Idempotent:     tool.Idempotent,
			TimeoutSeconds: int(manifest.ToolTimeout(tool.Name).Seconds()),
		})
	}
	return def
}

// localInitSeconds is the manifest's init timeout, raised to the local floor.
func localInitSeconds(manifest plugin.Manifest) int {
	seconds := int(manifest.InitTimeout().Seconds())
	if floor := int(localInitTimeout.Seconds()); seconds < floor {
		seconds = floor
	}
	return seconds
}

// writeSecrets copies the declared secrets from this process's environment
// into a private directory, one file per variable, which is how the plugin
// host hands a secret to a plugin process. A secret the environment does not
// set fails the run: a plugin started without its credential fails later and
// less clearly.
func writeSecrets(manifest plugin.Manifest) (string, error) {
	if len(manifest.Secrets) == 0 {
		return "", nil
	}
	dir, err := os.MkdirTemp("", "mayfly-plugin-secrets-")
	if err != nil {
		return "", err
	}
	for _, secret := range manifest.Secrets {
		value := os.Getenv(secret.Env)
		if value == "" {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("secret %s: set %s in the environment", secret.Env, secret.Env)
		}
		if err := os.WriteFile(filepath.Join(dir, secret.Env), []byte(value), 0o600); err != nil {
			_ = os.RemoveAll(dir)
			return "", err
		}
	}
	return dir, nil
}

// stderrTail returns the end of one plugin's stderr log, if it wrote one.
func stderrTail(artifactsDir, name string) string {
	raw, err := os.ReadFile(filepath.Join(artifactsDir, "plugins", name+".stderr.log"))
	if err != nil {
		return ""
	}
	const max = 4096
	if len(raw) > max {
		raw = raw[len(raw)-max:]
	}
	return strings.TrimRight(string(raw), "\n")
}
