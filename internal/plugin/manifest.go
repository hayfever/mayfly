// Package plugin defines the plugin manifest and the names Mayfly derives
// from it. A manifest is declarative: it holds no process, no client, and no
// cluster object. Loading, rendering, and calling are other packages' work.
//
// It imports neither Kubernetes nor the MCP client, so the loader, the CLI,
// and the reconciler share one set of types without pulling a dependency each.
package plugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hayfever/mayfly/internal/prompt"
)

// SchemaVersion is the manifest schema this package understands.
const SchemaVersion = 1

const (
	// ManifestFile is the manifest name inside plugins/<name>/.
	ManifestFile = "plugin.yaml"
	// FlatSuffix marks a manifest that sits directly in a bundle directory,
	// for ConfigMap bundles whose keys cannot contain "/".
	FlatSuffix = ".plugin.yaml"
	// SecretPrefix is the required prefix of a declared Kubernetes Secret.
	SecretPrefix = "mayfly-plugin-"
	// MaxToolName bounds a model-visible tool name.
	MaxToolName = 64
	// MaxTimeout bounds every configured call and init timeout.
	MaxTimeout = 15 * time.Minute
	// MaxOutputBytes caps one tool result, as agent.maxToolOutput does.
	MaxOutputBytes = 64 << 10
)

// Defaults. Validate writes them into the manifest, and the accessors use the
// same constants so a Manifest built in Go behaves like a parsed one.
const (
	defaultInitTimeout = 30 * time.Second
	defaultCallTimeout = 120 * time.Second
	defaultMaxRestarts = 2
	defaultStderrBytes = 1 << 20
	defaultRunAsUser   = 65532
	defaultReplicas    = 1
	defaultConcurrency = 8
	defaultPort        = 8080
	defaultPath        = "/mcp"
	defaultTransport   = "stdio"
	defaultWorkdir     = "plugin"
	defaultWorkspace   = "none"

	maxRestarts    = 10
	maxStderrBytes = 16 << 20
	maxReplicas    = 20
	maxConcurrency = 256

	// dnsMaxLength is the label limit for container and object names.
	dnsMaxLength   = 63
	nameHashLength = 8
)

var (
	// namePattern is the project bundle's name rule.
	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	// secretKeyPattern is the key rule for a Kubernetes Secret key.
	secretKeyPattern = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)
	// quantityPattern accepts the resource quantities the manifests use.
	quantityPattern = regexp.MustCompile(`^[+-]?[0-9]+(\.[0-9]+)?(m|k|M|G|T|P|E|Ki|Mi|Gi|Ti|Pi|Ei|e[+-]?[0-9]+)?$`)
)

// runtimeEnv lists the variables Mayfly sets for a plugin process.
var runtimeEnv = map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true}

// Placement is where a plugin runs.
type Placement string

const (
	Inline  Placement = "inline"
	Sidecar Placement = "sidecar"
	Service Placement = "service"
)

// Manifest is one plugin.yaml.
type Manifest struct {
	SchemaVersion int               `yaml:"schemaVersion"`
	Name          string            `yaml:"name"`
	Version       string            `yaml:"version"`
	Description   string            `yaml:"description"`
	Scope         string            `yaml:"scope"`
	Placement     Placement         `yaml:"placement"` // default inline
	Image         string            `yaml:"image"`
	Command       string            `yaml:"command"`
	Args          []string          `yaml:"args"`
	Local         *LocalRun         `yaml:"local"`
	Required      bool              `yaml:"required"`
	Env           map[string]string `yaml:"env"`
	Secrets       []Secret          `yaml:"secrets"`
	Tools         []Tool            `yaml:"tools"`
	Limits        Limits            `yaml:"limits"`
	Inline        *InlineSpec       `yaml:"inline"`
	Sidecar       *SidecarSpec      `yaml:"sidecar"`
	Service       *ServiceSpec      `yaml:"service"`

	Dir    string `yaml:"-"` // absolute directory of the manifest file
	Origin string `yaml:"-"` // manifest path, for errors
}

// LocalRun is how the local backend and mayfly plugin test start the plugin.
type LocalRun struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
}

// Secret is one environment variable read from a Kubernetes Secret.
type Secret struct {
	Env    string `yaml:"env"`
	Secret string `yaml:"secret"`
	Key    string `yaml:"key"`
}

// Tool is one declared tool. The manifest is also an allowlist: a server tool
// it does not declare is hidden from the model.
type Tool struct {
	Name       string `yaml:"name"`
	Idempotent bool   `yaml:"idempotent"`
	Timeout    string `yaml:"timeout"`
}

// Limits are the time and size bounds of one plugin.
type Limits struct {
	InitTimeout    string `yaml:"initTimeout"`    // default 30s
	CallTimeout    string `yaml:"callTimeout"`    // default 120s
	MaxOutputBytes int    `yaml:"maxOutputBytes"` // default 65536
	Rate           *Rate  `yaml:"rate"`
}

// Rate is a per-session call limit for one plugin.
type Rate struct {
	Requests int    `yaml:"requests"`
	Interval string `yaml:"interval"`
	Burst    int    `yaml:"burst"` // default Requests
}

// InlineSpec configures an inline plugin.
type InlineSpec struct {
	Workdir     string `yaml:"workdir"`     // plugin (default) or workspace
	MaxRestarts *int   `yaml:"maxRestarts"` // default 2
	StderrBytes int64  `yaml:"stderrBytes"` // default 1 MiB
}

// Resources are container resource requests and limits.
type Resources struct {
	Requests map[string]string `yaml:"requests"`
	Limits   map[string]string `yaml:"limits"`
}

// SidecarSpec configures a sidecar plugin.
type SidecarSpec struct {
	Resources Resources `yaml:"resources"`
	RunAsUser *int64    `yaml:"runAsUser"` // default 65532; never 0
	Workspace string    `yaml:"workspace"` // none (default), read, write
}

// Egress permits service traffic to one CIDR and port set.
type Egress struct {
	CIDR  string  `yaml:"cidr"`
	Ports []int32 `yaml:"ports"`
}

// ServiceSpec configures a service plugin.
type ServiceSpec struct {
	Replicas    *int32    `yaml:"replicas"`    // default 1
	Concurrency int       `yaml:"concurrency"` // default 8
	Resources   Resources `yaml:"resources"`
	Egress      []Egress  `yaml:"egress"`
	Transport   string    `yaml:"transport"` // stdio (default) or http
	Port        int32     `yaml:"port"`      // default 8080
	Path        string    `yaml:"path"`      // default /mcp
	MCPServer   string    `yaml:"mcpServer"`
}

// ValidateOptions carries the control plane's policy into validation.
type ValidateOptions struct {
	DeniedEnv    []string // config.SecretEnvNames()
	RequireImage bool     // kubernetes backend
}

// Parse decodes one manifest. Unknown fields are errors: a typo must not
// silently drop a limit or a secret.
func Parse(raw []byte, origin string) (Manifest, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", origin, err)
	}
	manifest.Origin = origin
	return manifest, nil
}

// Load reads and validates the manifest in dir.
func Load(dir string, opts ValidateOptions) (Manifest, error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return Manifest{}, fmt.Errorf("plugin directory %s: %w", dir, err)
	}
	origin := filepath.Join(absolute, ManifestFile)
	raw, err := os.ReadFile(origin)
	if err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", origin, err)
	}
	manifest, err := Parse(raw, origin)
	if err != nil {
		return Manifest{}, err
	}
	manifest.Dir = absolute
	if err := manifest.Validate(opts); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// Validate applies the load-time rules and fills in defaults. Scope is not
// checked here: whether a private owner exists is the bundle's question.
func (m *Manifest) Validate(opts ValidateOptions) error {
	if err := m.validateIdentity(); err != nil {
		return err
	}
	if err := m.validatePlacement(); err != nil {
		return err
	}
	if m.remoteServer() {
		if err := m.validateRemoteServer(); err != nil {
			return err
		}
	} else {
		if m.Command == "" {
			return m.fail("command", "is required")
		}
		if opts.RequireImage && m.Image == "" {
			return m.fail("image", "is required on the kubernetes backend")
		}
	}
	m.applyDefaults()

	if err := m.validateInline(); err != nil {
		return err
	}
	if err := m.validateEnv(opts.DeniedEnv); err != nil {
		return err
	}
	if err := m.validateSecrets(); err != nil {
		return err
	}
	if err := m.validateTools(); err != nil {
		return err
	}
	if err := m.validateLimits(); err != nil {
		return err
	}
	if err := m.validateSidecar(); err != nil {
		return err
	}
	if m.remoteServer() {
		// An mcp.servers service has no workload of ours to size.
		return nil
	}
	return m.validateService()
}

// origin is the manifest path used in errors.
func (m *Manifest) origin() string {
	if m.Origin != "" {
		return m.Origin
	}
	return ManifestFile
}

// fail reports one rule violation, naming the manifest and the field.
func (m *Manifest) fail(field, format string, args ...any) error {
	return fmt.Errorf("%s: %s: %s", m.origin(), field, fmt.Sprintf(format, args...))
}

// remoteServer reports that the service is an existing mcp.servers entry.
func (m *Manifest) remoteServer() bool {
	return m.Service != nil && m.Service.MCPServer != ""
}

// validateIdentity applies rule 1.
func (m *Manifest) validateIdentity() error {
	if m.SchemaVersion != SchemaVersion {
		return m.fail("schemaVersion", "must be %d", SchemaVersion)
	}
	if !namePattern.MatchString(m.Name) {
		return m.fail("name", "invalid plugin name %q (use lowercase letters, digits, dot, underscore, hyphen)", m.Name)
	}
	return nil
}

// validatePlacement applies rule 3: one placement, one matching block.
func (m *Manifest) validatePlacement() error {
	switch m.Placement {
	case "":
		m.Placement = Inline
	case Inline, Sidecar, Service:
	default:
		return m.fail("placement", "must be inline, sidecar, or service, not %q", m.Placement)
	}
	for _, block := range []struct {
		field     string
		placement Placement
		set       bool
	}{
		{"inline", Inline, m.Inline != nil},
		{"sidecar", Sidecar, m.Sidecar != nil},
		{"service", Service, m.Service != nil},
	} {
		if block.set && block.placement != m.Placement {
			return m.fail(block.field, "is set but placement is %s", m.Placement)
		}
	}
	return nil
}

// validateRemoteServer applies the service.mcpServer half of rule 4: Mayfly
// creates nothing, so every field that describes a workload must be empty.
func (m *Manifest) validateRemoteServer() error {
	service := m.Service
	checks := []struct {
		field string
		set   bool
	}{
		{"image", m.Image != ""},
		{"command", m.Command != ""},
		{"args", len(m.Args) > 0},
		{"local", m.Local != nil},
		{"env", len(m.Env) > 0},
		{"secrets", len(m.Secrets) > 0},
		{"service.replicas", service.Replicas != nil},
		{"service.concurrency", service.Concurrency != 0},
		{"service.resources", len(service.Resources.Requests) > 0 || len(service.Resources.Limits) > 0},
		{"service.egress", len(service.Egress) > 0},
		{"service.transport", service.Transport != ""},
		{"service.port", service.Port != 0},
		{"service.path", service.Path != ""},
	}
	for _, check := range checks {
		if check.set {
			return m.fail(check.field, "must be empty when service.mcpServer is set")
		}
	}
	return nil
}

// applyDefaults fills rules 5 and 6 and the documented defaults.
func (m *Manifest) applyDefaults() {
	if m.Local == nil {
		m.Local = &LocalRun{}
	}
	if m.Local.Command == "" {
		m.Local.Command = m.Command
	}
	if m.Local.Args == nil {
		m.Local.Args = m.Args
	}
	switch m.Placement {
	case Inline:
		if m.Inline == nil {
			m.Inline = &InlineSpec{}
		}
		if m.Inline.Workdir == "" {
			m.Inline.Workdir = defaultWorkdir
		}
		if m.Inline.MaxRestarts == nil {
			m.Inline.MaxRestarts = new(defaultMaxRestarts)
		}
		if m.Inline.StderrBytes == 0 {
			m.Inline.StderrBytes = defaultStderrBytes
		}
	case Sidecar:
		if m.Sidecar == nil {
			m.Sidecar = &SidecarSpec{}
		}
		if m.Sidecar.RunAsUser == nil {
			m.Sidecar.RunAsUser = new(int64(defaultRunAsUser))
		}
		if m.Sidecar.Workspace == "" {
			m.Sidecar.Workspace = defaultWorkspace
		}
	case Service:
		if m.Service == nil {
			m.Service = &ServiceSpec{}
		}
		if m.Service.Replicas == nil {
			m.Service.Replicas = new(int32(defaultReplicas))
		}
		if m.Service.Concurrency == 0 {
			m.Service.Concurrency = defaultConcurrency
		}
		if m.Service.Transport == "" {
			m.Service.Transport = defaultTransport
		}
		if m.Service.Port == 0 {
			m.Service.Port = defaultPort
		}
		if m.Service.Path == "" {
			m.Service.Path = defaultPath
		}
	}
	if m.Limits.InitTimeout == "" {
		m.Limits.InitTimeout = defaultInitTimeout.String()
	}
	if m.Limits.CallTimeout == "" {
		m.Limits.CallTimeout = defaultCallTimeout.String()
	}
	if m.Limits.MaxOutputBytes == 0 {
		m.Limits.MaxOutputBytes = MaxOutputBytes
	}
	if m.Limits.Rate != nil && m.Limits.Rate.Burst == 0 {
		m.Limits.Rate.Burst = m.Limits.Rate.Requests
	}
}

// validateInline applies rule 6.
func (m *Manifest) validateInline() error {
	if m.Placement != Inline {
		return nil
	}
	switch m.Inline.Workdir {
	case "plugin", "workspace":
	default:
		return m.fail("inline.workdir", "must be plugin or workspace, not %q", m.Inline.Workdir)
	}
	return nil
}

// validateEnv applies rule 7 to the literal environment.
func (m *Manifest) validateEnv(denied []string) error {
	policy := prompt.EnvPolicy{Deny: denied}
	names := make([]string, 0, len(m.Env))
	for name := range m.Env {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := checkEnvName(policy, name); err != nil {
			return m.fail("env", "%s: %v", name, err)
		}
		if strings.HasPrefix(m.Env[name], "env:") {
			return m.fail("env", "%s: value references a platform secret; declare the variable under secrets", name)
		}
	}
	declared := make(map[string]bool, len(m.Secrets))
	for i, secret := range m.Secrets {
		if err := checkEnvName(policy, secret.Env); err != nil {
			return m.fail(fmt.Sprintf("secrets[%d].env", i), "%v", err)
		}
		if _, ok := m.Env[secret.Env]; ok {
			return m.fail(fmt.Sprintf("secrets[%d].env", i), "%s is declared in both env and secrets", secret.Env)
		}
		if declared[secret.Env] {
			return m.fail(fmt.Sprintf("secrets[%d].env", i), "duplicate secret for %s", secret.Env)
		}
		declared[secret.Env] = true
	}
	return nil
}

// checkEnvName rejects a name the control plane owns or that Mayfly sets.
func checkEnvName(policy prompt.EnvPolicy, name string) error {
	if err := policy.Check(name); err != nil {
		return err
	}
	if runtimeEnv[name] {
		return fmt.Errorf("%s is set by Mayfly and cannot be declared", name)
	}
	return nil
}

// validateSecrets applies rule 8.
func (m *Manifest) validateSecrets() error {
	for i, secret := range m.Secrets {
		if !strings.HasPrefix(secret.Secret, SecretPrefix) {
			return m.fail(fmt.Sprintf("secrets[%d].secret", i), "must start with %s, not %q", SecretPrefix, secret.Secret)
		}
		if !isDNSSubdomain(secret.Secret) {
			return m.fail(fmt.Sprintf("secrets[%d].secret", i), "%q is not a DNS subdomain", secret.Secret)
		}
		if !secretKeyPattern.MatchString(secret.Key) {
			return m.fail(fmt.Sprintf("secrets[%d].key", i), "must match %s", secretKeyPattern)
		}
	}
	return nil
}

// validateTools applies rule 9.
func (m *Manifest) validateTools() error {
	if len(m.Tools) == 0 {
		return m.fail("tools", "at least one tool is required")
	}
	declared := make(map[string]bool, len(m.Tools))
	exposed := make(map[string]bool, len(m.Tools))
	for i, tool := range m.Tools {
		field := fmt.Sprintf("tools[%d].name", i)
		if strings.TrimSpace(tool.Name) == "" {
			return m.fail(field, "must not be empty")
		}
		if declared[tool.Name] {
			return m.fail(field, "duplicate tool %q", tool.Name)
		}
		declared[tool.Name] = true
		name := ToolName(m.Name, tool.Name)
		if len(name) > MaxToolName {
			return m.fail(field, "%s is longer than %d characters", name, MaxToolName)
		}
		if exposed[name] {
			return m.fail(field, "%s collides with another tool of this plugin", name)
		}
		exposed[name] = true
	}
	return nil
}

// validateLimits applies rule 10.
func (m *Manifest) validateLimits() error {
	for _, duration := range []struct {
		field string
		raw   string
	}{
		{"limits.initTimeout", m.Limits.InitTimeout},
		{"limits.callTimeout", m.Limits.CallTimeout},
	} {
		if err := checkTimeout(duration.raw); err != nil {
			return m.fail(duration.field, "%v", err)
		}
	}
	for i, tool := range m.Tools {
		if err := checkTimeout(tool.Timeout); err != nil {
			return m.fail(fmt.Sprintf("tools[%d].timeout", i), "%v", err)
		}
	}
	if m.Inline != nil {
		if restarts := *m.Inline.MaxRestarts; restarts < 0 || restarts > maxRestarts {
			return m.fail("inline.maxRestarts", "must be in 0..%d", maxRestarts)
		}
		if m.Inline.StderrBytes < 0 || m.Inline.StderrBytes > maxStderrBytes {
			return m.fail("inline.stderrBytes", "must be in 0..%d", maxStderrBytes)
		}
	}
	if m.Limits.MaxOutputBytes < 1 || m.Limits.MaxOutputBytes > MaxOutputBytes {
		return m.fail("limits.maxOutputBytes", "must be in 1..%d", MaxOutputBytes)
	}
	if rate := m.Limits.Rate; rate != nil {
		if rate.Requests < 1 {
			return m.fail("limits.rate.requests", "must be at least 1")
		}
		interval, err := time.ParseDuration(rate.Interval)
		if err != nil {
			return m.fail("limits.rate.interval", "%v", err)
		}
		if interval < time.Second {
			return m.fail("limits.rate.interval", "must be at least 1s")
		}
		if rate.Burst < 0 {
			return m.fail("limits.rate.burst", "must not be negative")
		}
	}
	return nil
}

// validateSidecar applies rule 11.
func (m *Manifest) validateSidecar() error {
	if m.Placement != Sidecar {
		return nil
	}
	if *m.Sidecar.RunAsUser == 0 {
		return m.fail("sidecar.runAsUser", "must not be 0; the plugin runs as non-root")
	}
	switch m.Sidecar.Workspace {
	case "none", "read", "write":
	default:
		return m.fail("sidecar.workspace", "must be none, read, or write, not %q", m.Sidecar.Workspace)
	}
	if field, err := checkResources(m.Sidecar.Resources); err != nil {
		return m.fail("sidecar.resources."+field, "%v", err)
	}
	return nil
}

// validateService applies rule 12.
func (m *Manifest) validateService() error {
	if m.Placement != Service {
		return nil
	}
	service := m.Service
	if replicas := *service.Replicas; replicas < 0 || replicas > maxReplicas {
		return m.fail("service.replicas", "must be in 0..%d", maxReplicas)
	}
	if service.Concurrency < 1 || service.Concurrency > maxConcurrency {
		return m.fail("service.concurrency", "must be in 1..%d", maxConcurrency)
	}
	switch service.Transport {
	case "stdio", "http":
	default:
		return m.fail("service.transport", "must be stdio or http, not %q", service.Transport)
	}
	if service.Port < 1 || service.Port > 65535 {
		return m.fail("service.port", "must be in 1..65535")
	}
	if !strings.HasPrefix(service.Path, "/") {
		return m.fail("service.path", "must start with /, not %q", service.Path)
	}
	for i, egress := range service.Egress {
		if _, _, err := net.ParseCIDR(egress.CIDR); err != nil {
			return m.fail(fmt.Sprintf("service.egress[%d].cidr", i), "%v", err)
		}
		for j, port := range egress.Ports {
			if port < 1 || port > 65535 {
				return m.fail(fmt.Sprintf("service.egress[%d].ports[%d]", i, j), "must be in 1..65535")
			}
		}
	}
	if field, err := checkResources(service.Resources); err != nil {
		return m.fail("service.resources."+field, "%v", err)
	}
	return nil
}

// checkTimeout accepts an unset timeout (it inherits) or a positive one that
// does not exceed MaxTimeout.
func checkTimeout(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	timeout, err := time.ParseDuration(raw)
	if err != nil {
		return err
	}
	if timeout <= 0 {
		return fmt.Errorf("must be positive")
	}
	if timeout > MaxTimeout {
		return fmt.Errorf("must be at most %s", MaxTimeout)
	}
	return nil
}

// checkResources rejects a quantity Kubernetes would reject, reporting the
// offending field suffix.
func checkResources(resources Resources) (string, error) {
	for _, set := range []struct {
		name  string
		items map[string]string
	}{
		{"requests", resources.Requests},
		{"limits", resources.Limits},
	} {
		names := make([]string, 0, len(set.items))
		for name := range set.items {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if !quantityPattern.MatchString(set.items[name]) {
				return set.name + "." + name, fmt.Errorf("invalid quantity %q", set.items[name])
			}
		}
	}
	return "", nil
}

// isDNSSubdomain reports whether name is a lowercase RFC 1123 subdomain.
func isDNSSubdomain(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > dnsMaxLength {
			return false
		}
		for i, r := range label {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			case r == '-' && i > 0 && i < len(label)-1:
			default:
				return false
			}
		}
	}
	return true
}

// InitTimeout bounds start, initialize, and tools/list.
func (m Manifest) InitTimeout() time.Duration {
	return duration(m.Limits.InitTimeout, defaultInitTimeout)
}

// CallTimeout bounds one tool call that declares no timeout of its own.
func (m Manifest) CallTimeout() time.Duration {
	return duration(m.Limits.CallTimeout, defaultCallTimeout)
}

// ToolTimeout is the timeout of one declared tool by its manifest name.
func (m Manifest) ToolTimeout(name string) time.Duration {
	for _, tool := range m.Tools {
		if tool.Name == name {
			return duration(tool.Timeout, m.CallTimeout())
		}
	}
	return m.CallTimeout()
}

// Effects lists the tools that are not idempotent, sorted.
func (m Manifest) Effects() []string {
	var out []string
	for _, tool := range m.Tools {
		if !tool.Idempotent {
			out = append(out, tool.Name)
		}
	}
	sort.Strings(out)
	return out
}

// SpecHash identifies a manifest, so a manifest change updates the service
// objects that were rendered from it. Paths are not part of it. The service
// reconciler mixes its RenderConfig and the gateway key into the annotation
// it stores.
func (m Manifest) SpecHash() string {
	spec := m
	spec.Dir, spec.Origin = "", ""
	// The manifest holds only strings, numbers, slices, and maps, so encoding
	// cannot fail.
	raw, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// duration parses raw, or returns def when raw is unset or unusable.
func duration(raw string, def time.Duration) time.Duration {
	parsed, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || parsed <= 0 {
		return def
	}
	return parsed
}
