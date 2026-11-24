// Package project loads a Mayfly project bundle: agents, connections, sources,
// schedules, and plugins. Scopes decide visibility, extends and overrides share
// one RFC 7386 merge, and every schedule is compiled into an executable plan at
// load time so a bad bundle fails at startup instead of at 07:00.
package project

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	// Bundle load resolves schedule time zones by name. The mayflyd image is
	// distroless and ships no zoneinfo, so the database is compiled in.
	_ "time/tzdata"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"

	"github.com/hayfever/mayfly/internal/agents"
	"github.com/hayfever/mayfly/internal/plugin"
	"github.com/hayfever/mayfly/internal/prompt"
	"github.com/hayfever/mayfly/internal/sources"
)

// SchemaVersion is the bundle schema this package understands.
const SchemaVersion = 1

const (
	scopeShared      = "shared"
	scopePrivateBase = "agent:"
)

const (
	// DefaultTTL is the schedule TTL when the bundle omits one.
	DefaultTTL = 30 * time.Minute
	// MaxTTL bounds a schedule's session lifetime.
	MaxTTL = 24 * time.Hour
	// DefaultTimezone is the canonical zone for a schedule without a
	// timezone field. "UTC" and "Etc/UTC" both resolve to it.
	DefaultTimezone = "Etc/UTC"
)

// resolveTimezone validates an optional IANA zone name. An empty name is the
// default zone. The returned name is what the bundle declared, and the
// location is never nil; callers use it for cron evaluation.
func resolveTimezone(name string) (string, *time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", time.UTC, nil
	}
	if strings.HasPrefix(name, "/") || strings.Contains(name, "..") || strings.Contains(name, `\`) {
		return "", nil, fmt.Errorf("timezone %q is not an IANA name", name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return "", nil, fmt.Errorf("timezone %q is not an IANA name: %w", name, err)
	}
	return name, loc, nil
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Options carries source runner limits into plan construction.
type Options struct {
	Concurrency int
	Timeout     time.Duration
	// DeniedEnv lists control-plane secret variables (see
	// config.SecretEnvNames). No bundle may declare them or read them from a
	// template, and MAYFLY_* is always denied.
	DeniedEnv []string
	// RequirePluginImage makes plugin.image mandatory. The local backend runs
	// local.command instead, so it leaves this false.
	RequirePluginImage bool
}

// Schedule is one validated schedule bound to its agent and plan. Location is
// the zone the cron fields are read in; it is UTC when Timezone is empty, and
// never nil.
type Schedule struct {
	Name      string
	Agent     string
	Cron      string
	Timezone  string
	Location  *time.Location
	Source    string
	TTL       time.Duration
	TriggerOn []string
	Context   *prompt.Template
	Title     *prompt.Template
	Prompt    *prompt.Template
}

// Bundle is a fully resolved project.
type Bundle struct {
	Agents      []agents.Agent
	Default     string
	Schedules   []Schedule
	Sources     map[string]sources.Definition
	Connections map[string]sources.Connection
	Plans       map[string]*sources.Plan
	Plugins     map[string]plugin.Manifest

	agentsByName    map[string]agents.Agent
	schedulesByName map[string]Schedule
}

// Agent returns the named agent.
func (b *Bundle) Agent(name string) (agents.Agent, bool) {
	agent, ok := b.agentsByName[name]
	return agent, ok
}

// Schedule returns the named schedule.
func (b *Bundle) Schedule(name string) (Schedule, bool) {
	schedule, ok := b.schedulesByName[name]
	return schedule, ok
}

// Describe prints the resolved bundle in a stable order, one line per
// definition. It is the output of mayflyd -check.
func (b *Bundle) Describe() string {
	var out strings.Builder
	fmt.Fprintf(&out, "agents: %d schedules: %d sources: %d connections: %d plugins: %d\n",
		len(b.Agents), len(b.Schedules), len(b.Sources), len(b.Connections), len(b.Plugins))
	for _, agent := range b.Agents {
		fmt.Fprintf(&out, "agent %s", agent.Name)
		if agent.Default {
			out.WriteString(" default")
		}
		if names := agent.PluginNames(); len(names) > 0 {
			fmt.Fprintf(&out, " plugins=%s", strings.Join(names, ","))
		}
		out.WriteString("\n")
	}
	for _, name := range sortedKeys(b.Plugins) {
		manifest := b.Plugins[name]
		placement := manifest.Placement
		if placement == "" {
			placement = plugin.Inline
		}
		effects := strings.Join(manifest.Effects(), ",")
		if effects == "" {
			effects = "-"
		}
		image := manifest.Image
		if image == "" {
			image = "-"
		}
		fmt.Fprintf(&out, "plugin %s placement=%s scope=%s tools=%d effects=%s image=%s",
			name, placement, manifest.Scope, len(manifest.Tools), effects, image)
		if manifest.Service != nil && manifest.Service.MCPServer != "" {
			fmt.Fprintf(&out, " mcpServer=%s", manifest.Service.MCPServer)
		}
		out.WriteString("\n")
	}
	for _, schedule := range b.Schedules {
		fmt.Fprintf(&out, "schedule %s agent=%s cron=%q", schedule.Name, schedule.Agent, schedule.Cron)
		if schedule.Timezone != "" {
			fmt.Fprintf(&out, " timezone=%s", schedule.Timezone)
		}
		fmt.Fprintf(&out, " source=%s ttl=%s trigger=%s\n",
			schedule.Source, schedule.TTL, strings.Join(schedule.TriggerOn, ","))
	}
	for _, schedule := range b.Schedules {
		plan := b.Plans[schedule.Name]
		if plan == nil {
			continue
		}
		names := make([]string, 0, len(plan.Steps))
		for _, step := range plan.Steps {
			names = append(names, step.Name())
		}
		fmt.Fprintf(&out, "plan %s steps %d terminal %s: %s\n",
			schedule.Name, len(plan.Steps), plan.Terminal, strings.Join(names, " -> "))
	}
	return out.String()
}

// rawDefinition is one definition before extends resolution.
type rawDefinition struct {
	raw    map[string]any
	scope  string
	origin string
}

// fileManifest is the YAML envelope of one project file.
type fileManifest struct {
	SchemaVersion int              `yaml:"schemaVersion"`
	Name          string           `yaml:"name"`
	Version       string           `yaml:"version"`
	Agents        []map[string]any `yaml:"agents"`
	Connections   []map[string]any `yaml:"connections"`
	Sources       []map[string]any `yaml:"sources"`
}

// Load reads every bundle file under dirs and resolves it.
func Load(dirs []string, opts Options) (*Bundle, error) {
	agentRaw := make([]rawDefinition, 0, 4)
	pluginRaw := make([]plugin.Manifest, 0, 4)
	sourceRaw := map[string]rawDefinition{}
	connRaw := map[string]rawDefinition{}
	seenAgents := map[string]string{}
	seenPlugins := map[string]string{}

	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		info, err := os.Stat(dir)
		if err != nil {
			return nil, fmt.Errorf("project directory %s: %w", dir, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("project path %s is not a directory", dir)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			return nil, fmt.Errorf("project directory %s: %w", dir, err)
		}
		err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if path != dir && strings.HasPrefix(entry.Name(), ".") {
					return fs.SkipDir
				}
				return nil
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			name, isManifest, skip := plugin.IsManifestPath(filepath.ToSlash(rel))
			if skip {
				return nil
			}
			if isManifest {
				raw, err := root.ReadFile(rel)
				if err != nil {
					return fmt.Errorf("%s: %w", path, err)
				}
				manifest, err := plugin.Parse(raw, path)
				if err != nil {
					return err
				}
				if manifest.Name != name {
					return fmt.Errorf("%s: plugin name %q does not match %q from the path", path, manifest.Name, name)
				}
				if prev, ok := seenPlugins[name]; ok {
					return fmt.Errorf("duplicate plugin %q in %s and %s", name, prev, path)
				}
				manifest.Dir, err = filepath.Abs(filepath.Dir(path))
				if err != nil {
					return fmt.Errorf("%s: %w", path, err)
				}
				seenPlugins[name] = path
				pluginRaw = append(pluginRaw, manifest)
				return nil
			}
			switch strings.ToLower(filepath.Ext(path)) {
			case ".yaml", ".yml", ".json":
			default:
				return nil
			}
			raw, err := root.ReadFile(rel)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			manifest, err := decodeManifest(raw, path)
			if err != nil {
				return err
			}
			for _, rawAgent := range manifest.Agents {
				name, ok := rawName(rawAgent)
				if !ok || name == "" {
					return fmt.Errorf("%s: every agent needs a name", path)
				}
				if prev, ok := seenAgents[name]; ok {
					return fmt.Errorf("duplicate agent %q in %s and %s", name, prev, path)
				}
				seenAgents[name] = path
				if err := resolvePromptFile(root, rel, rawAgent, path); err != nil {
					return err
				}
				agentRaw = append(agentRaw, rawDefinition{raw: rawAgent, origin: path})
			}
			for _, rawSource := range manifest.Sources {
				if err := addDefinition("source", rawSource, path, sourceRaw); err != nil {
					return err
				}
			}
			for _, rawConn := range manifest.Connections {
				if err := addDefinition("connection", rawConn, path, connRaw); err != nil {
					return err
				}
			}
			return nil
		})
		_ = root.Close()
		if err != nil {
			return nil, err
		}
	}
	return buildBundle(agentRaw, pluginRaw, sourceRaw, connRaw, seenAgents, opts)
}

func decodeManifest(raw []byte, path string) (fileManifest, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var manifest fileManifest
	if err := decoder.Decode(&manifest); err != nil {
		return fileManifest{}, fmt.Errorf("%s: %w", path, err)
	}
	if manifest.SchemaVersion != SchemaVersion {
		return fileManifest{}, fmt.Errorf("%s: unsupported schemaVersion %d", path, manifest.SchemaVersion)
	}
	if strings.TrimSpace(manifest.Name) == "" {
		return fileManifest{}, fmt.Errorf("%s: bundle name is required", path)
	}
	return manifest, nil
}

// resolvePromptFile replaces systemPromptFile with its contents so a mounted
// ConfigMap can keep long prompts in a separate key.
func resolvePromptFile(root *os.Root, manifestRel string, raw map[string]any, path string) error {
	file := rawString(raw, "systemPromptFile")
	if file == "" {
		return nil
	}
	if rawString(raw, "systemPrompt") != "" {
		return fmt.Errorf("%s: an agent sets both systemPrompt and systemPromptFile", path)
	}
	clean := filepath.Clean(file)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s: systemPromptFile must stay inside the bundle directory", path)
	}
	rel := filepath.Join(filepath.Dir(manifestRel), clean)
	data, err := root.ReadFile(rel)
	if err != nil {
		return fmt.Errorf("%s: systemPromptFile: %w", path, err)
	}
	raw["systemPrompt"] = strings.TrimSpace(string(data))
	delete(raw, "systemPromptFile")
	return nil
}

func addDefinition(kind string, raw map[string]any, origin string, into map[string]rawDefinition) error {
	name, ok := rawName(raw)
	if !ok || name == "" {
		return fmt.Errorf("%s: every %s needs a name", origin, kind)
	}
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%s: invalid %s name %q", origin, kind, name)
	}
	scope, err := parseScope(rawString(raw, "scope"))
	if err != nil {
		return fmt.Errorf("%s: %s %q: %w", origin, kind, name, err)
	}
	if prev, ok := into[name]; ok {
		return fmt.Errorf("duplicate %s %q in %s and %s; a private %s must use extends and a new name", kind, name, prev.origin, origin, kind)
	}
	into[name] = rawDefinition{raw: raw, scope: scope, origin: origin}
	return nil
}

func parseScope(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "", raw == scopeShared:
		return scopeShared, nil
	case strings.HasPrefix(raw, scopePrivateBase) && len(raw) > len(scopePrivateBase):
		return raw, nil
	default:
		return "", fmt.Errorf("invalid scope %q (use shared or agent:<name>)", raw)
	}
}

func scopeOwner(scope string) string {
	return strings.TrimPrefix(scope, scopePrivateBase)
}

func visible(scope, agent string) bool {
	return scope == scopeShared || scopeOwner(scope) == agent
}

func buildBundle(agentRaw []rawDefinition, pluginRaw []plugin.Manifest, sourceRaw, connRaw map[string]rawDefinition, seenAgents map[string]string, opts Options) (*Bundle, error) {
	bundle := &Bundle{
		Sources:         map[string]sources.Definition{},
		Connections:     map[string]sources.Connection{},
		Plans:           map[string]*sources.Plan{},
		Plugins:         map[string]plugin.Manifest{},
		agentsByName:    map[string]agents.Agent{},
		schedulesByName: map[string]Schedule{},
	}
	validateOpts := plugin.ValidateOptions{DeniedEnv: opts.DeniedEnv, RequireImage: opts.RequirePluginImage}
	for _, manifest := range pluginRaw {
		if err := manifest.Validate(validateOpts); err != nil {
			return nil, err
		}
		scope, err := parseScope(manifest.Scope)
		if err != nil {
			return nil, fmt.Errorf("%s: plugin %q: %w", manifest.Origin, manifest.Name, err)
		}
		manifest.Scope = scope
		bundle.Plugins[manifest.Name] = manifest
	}
	for _, item := range agentRaw {
		var def agents.Definition
		if err := decodeStrict(item.raw, &def); err != nil {
			return nil, fmt.Errorf("%s: %w", item.origin, err)
		}
		agent, err := agents.Build(def, item.origin)
		if err != nil {
			return nil, err
		}
		bundle.Agents = append(bundle.Agents, agent)
		if agent.Default {
			if bundle.Default != "" {
				return nil, fmt.Errorf("more than one agent is marked as default; second is %q", agent.Name)
			}
			bundle.Default = agent.Name
		}
	}
	sort.Slice(bundle.Agents, func(i, j int) bool { return bundle.Agents[i].Name < bundle.Agents[j].Name })
	for _, agent := range bundle.Agents {
		bundle.agentsByName[agent.Name] = agent
	}

	for _, name := range sortedKeys(connRaw) {
		merged, err := resolveExtends("connection", connRaw, name, nil)
		if err != nil {
			return nil, err
		}
		var conn sources.Connection
		if err := decodeStrict(merged, &conn); err != nil {
			return nil, fmt.Errorf("%s: %w", connRaw[name].origin, err)
		}
		conn.Name = name
		conn.Scope = connRaw[name].scope
		if conn.BaseURL == "" {
			return nil, fmt.Errorf("%s: connection %q needs baseURL", connRaw[name].origin, name)
		}
		if err := checkDeclaredEnv(conn.RequiredEnv, conn.OptionalEnv, opts.DeniedEnv); err != nil {
			return nil, fmt.Errorf("%s: connection %q: %w", connRaw[name].origin, name, err)
		}
		conn.ResolvedHeaders, err = resolveHeaders(conn.Headers, opts.DeniedEnv)
		if err != nil {
			return nil, fmt.Errorf("%s: connection %q: %w", connRaw[name].origin, name, err)
		}
		conn.ResolvedTimeout, err = parseTimeout(conn.Timeout)
		if err != nil {
			return nil, fmt.Errorf("%s: connection %q: %w", connRaw[name].origin, name, err)
		}
		bundle.Connections[name] = conn
	}

	for _, name := range sortedKeys(sourceRaw) {
		merged, err := resolveExtends("source", sourceRaw, name, nil)
		if err != nil {
			return nil, err
		}
		var def sources.Definition
		if err := decodeStrict(merged, &def); err != nil {
			return nil, fmt.Errorf("%s: %w", sourceRaw[name].origin, err)
		}
		def.Name = name
		def.Scope = sourceRaw[name].scope
		if def.Type == "" {
			return nil, fmt.Errorf("%s: source %q needs a type", sourceRaw[name].origin, name)
		}
		if err := checkDeclaredEnv(def.RequiredEnv, def.OptionalEnv, opts.DeniedEnv); err != nil {
			return nil, fmt.Errorf("%s: source %q: %w", sourceRaw[name].origin, name, err)
		}
		bundle.Sources[name] = def
	}

	for _, scope := range []string{"source", "connection", "plugin"} {
		if err := checkScopeOwners(bundle, scope, seenAgents); err != nil {
			return nil, err
		}
	}
	for _, name := range sortedKeys(bundle.Sources) {
		if err := checkSourceVisibility(bundle, name); err != nil {
			return nil, err
		}
	}
	if err := checkSourceCycles(bundle.Sources); err != nil {
		return nil, err
	}

	for _, agent := range bundle.Agents {
		for _, rawSchedule := range agent.Schedules {
			schedule, err := buildSchedule(bundle, agent.Name, rawSchedule, opts.DeniedEnv)
			if err != nil {
				return nil, err
			}
			if prev, ok := bundle.schedulesByName[schedule.Name]; ok {
				return nil, fmt.Errorf("duplicate schedule %q in %s and %s", schedule.Name, prev.Agent, agent.Name)
			}
			plan, err := bundle.buildPlan(agent.Name, schedule, opts)
			if err != nil {
				return nil, err
			}
			bundle.Plans[schedule.Name] = plan
			bundle.schedulesByName[schedule.Name] = schedule
			bundle.Schedules = append(bundle.Schedules, schedule)
		}
	}
	sort.Slice(bundle.Schedules, func(i, j int) bool { return bundle.Schedules[i].Name < bundle.Schedules[j].Name })
	return bundle, nil
}

func decodeStrict(raw map[string]any, out any) error {
	data, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	return decoder.Decode(out)
}

// checkDeclaredEnv validates the variables a definition's templates may read.
// requiredEnv stops startup when a template credential is absent: an unset
// value would otherwise reach the provider as an empty header and surface only
// as an item error. optionalEnv may be unset and then renders empty. Neither
// may name a control-plane secret.
func checkDeclaredEnv(required, optional, denied []string) error {
	policy := prompt.EnvPolicy{Deny: denied}
	for _, list := range []struct {
		key   string
		names []string
	}{{"requiredEnv", required}, {"optionalEnv", optional}} {
		for _, name := range list.names {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("%s has an empty name", list.key)
			}
			if err := policy.Check(name); err != nil {
				return fmt.Errorf("%s: %w", list.key, err)
			}
		}
	}
	for _, name := range required {
		if os.Getenv(name) == "" {
			return fmt.Errorf("environment variable %s is not set", name)
		}
	}
	return nil
}

func resolveHeaders(headers map[string]string, denied []string) (map[string]string, error) {
	if len(headers) == 0 {
		return nil, nil
	}
	policy := prompt.EnvPolicy{Deny: denied}
	out := make(map[string]string, len(headers))
	for key, value := range headers {
		if name, ok := strings.CutPrefix(value, "env:"); ok {
			if err := policy.Check(name); err != nil {
				return nil, fmt.Errorf("header %s: %w", key, err)
			}
			value = os.Getenv(name)
			if value == "" {
				return nil, fmt.Errorf("header %s references unset environment variable %s", key, name)
			}
		}
		out[key] = value
	}
	return out, nil
}

func parseTimeout(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 60 * time.Second, nil
	}
	timeout, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("timeout: %w", err)
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("timeout must be positive")
	}
	return timeout, nil
}

func checkScopeOwners(bundle *Bundle, kind string, seenAgents map[string]string) error {
	check := func(name, scope string) error {
		if scope == scopeShared {
			return nil
		}
		owner := scopeOwner(scope)
		if _, ok := seenAgents[owner]; !ok {
			return fmt.Errorf("%s %q is private to unknown agent %q", kind, name, owner)
		}
		return nil
	}
	if kind == "connection" {
		for name, record := range bundle.Connections {
			if err := check(name, record.Scope); err != nil {
				return err
			}
		}
		return nil
	}
	if kind == "plugin" {
		for name, manifest := range bundle.Plugins {
			if err := check(name, manifest.Scope); err != nil {
				return err
			}
		}
		return nil
	}
	for name, record := range bundle.Sources {
		if err := check(name, record.Scope); err != nil {
			return err
		}
	}
	return nil
}

// checkSourceCycles rejects a needs graph that loops, even when no schedule
// reaches the loop. A schedule-only check would accept dead configuration.
func checkSourceCycles(defs map[string]sources.Definition) error {
	const (
		unvisited = iota
		visiting
		done
	)
	state := make(map[string]int, len(defs))
	var visit func(string, []string) error
	visit = func(name string, stack []string) error {
		switch state[name] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("source %q has a needs cycle: %s", name, strings.Join(append(stack, name), " -> "))
		}
		state[name] = visiting
		for _, need := range defs[name].Needs {
			if err := visit(need, append(stack, name)); err != nil {
				return err
			}
		}
		state[name] = done
		return nil
	}
	for _, name := range sortedKeys(defs) {
		if err := visit(name, nil); err != nil {
			return err
		}
	}
	return nil
}

func checkSourceVisibility(bundle *Bundle, name string) error {
	def := bundle.Sources[name]
	conn, ok := bundle.Connections[def.Connection]
	if !ok {
		return fmt.Errorf("source %q uses undefined connection %q", name, def.Connection)
	}
	if def.Scope == scopeShared && conn.Scope != scopeShared {
		return fmt.Errorf("shared source %q cannot use private connection %q", name, conn.Name)
	}
	if def.Scope != scopeShared && !visible(conn.Scope, scopeOwner(def.Scope)) {
		return fmt.Errorf("source %q cannot use connection %q from another agent", name, conn.Name)
	}
	checkNeed := func(ref string) error {
		if ref == name {
			return fmt.Errorf("source %q depends on itself", name)
		}
		dependency, ok := bundle.Sources[ref]
		if !ok {
			return fmt.Errorf("source %q needs undefined source %q", name, ref)
		}
		if def.Scope == scopeShared && dependency.Scope != scopeShared {
			return fmt.Errorf("shared source %q cannot need private source %q", name, ref)
		}
		if def.Scope != scopeShared && !visible(dependency.Scope, scopeOwner(def.Scope)) {
			return fmt.Errorf("source %q cannot need source %q from another agent", name, ref)
		}
		return nil
	}
	seen := map[string]bool{}
	for _, need := range def.Needs {
		if seen[need] {
			return fmt.Errorf("source %q lists need %q twice", name, need)
		}
		seen[need] = true
		if err := checkNeed(need); err != nil {
			return err
		}
	}
	if def.Fanout != nil {
		if err := checkNeed(def.Fanout.From); err != nil {
			return fmt.Errorf("source %q fanout: %w", name, err)
		}
		parent := bundle.Sources[def.Fanout.From]
		for _, field := range def.Fanout.URLs {
			if _, ok := parent.Extract.Fields[field]; !ok {
				return fmt.Errorf("source %q fanout field %q is not an extract field of %q", name, field, parent.Name)
			}
		}
	}
	return nil
}

func buildSchedule(bundle *Bundle, agentName string, raw agents.Schedule, denied []string) (Schedule, error) {
	schedule := Schedule{
		Name:   strings.TrimSpace(raw.Name),
		Agent:  agentName,
		Cron:   strings.TrimSpace(raw.Cron),
		Source: strings.TrimSpace(raw.Source),
		TTL:    DefaultTTL,
	}
	if !namePattern.MatchString(schedule.Name) {
		return Schedule{}, fmt.Errorf("%s: invalid schedule name %q", agentName, schedule.Name)
	}
	if schedule.Source == "" {
		return Schedule{}, fmt.Errorf("schedule %q: source is required", schedule.Name)
	}
	if _, ok := bundle.Sources[schedule.Source]; !ok {
		return Schedule{}, fmt.Errorf("schedule %q: source %q is not defined", schedule.Name, schedule.Source)
	}
	if !visible(bundle.Sources[schedule.Source].Scope, agentName) {
		return Schedule{}, fmt.Errorf("schedule %q: source %q is not visible to %s", schedule.Name, schedule.Source, agentName)
	}
	for _, field := range strings.Fields(schedule.Cron) {
		if strings.HasPrefix(field, "CRON_TZ=") || strings.HasPrefix(field, "TZ=") {
			return Schedule{}, fmt.Errorf("schedule %q: cron must not embed CRON_TZ= or TZ=; set the timezone field", schedule.Name)
		}
	}
	if schedule.Cron == "" || strings.HasPrefix(schedule.Cron, "@") || len(strings.Fields(schedule.Cron)) != 5 {
		return Schedule{}, fmt.Errorf("schedule %q: cron must be a five-field expression", schedule.Name)
	}
	if _, err := cron.ParseStandard(schedule.Cron); err != nil {
		return Schedule{}, fmt.Errorf("schedule %q: %w", schedule.Name, err)
	}
	timezone, location, err := resolveTimezone(raw.Timezone)
	if err != nil {
		return Schedule{}, fmt.Errorf("schedule %q: %w", schedule.Name, err)
	}
	schedule.Timezone = timezone
	schedule.Location = location
	if raw.TTL != "" {
		ttl, err := time.ParseDuration(raw.TTL)
		if err != nil {
			return Schedule{}, fmt.Errorf("schedule %q: ttl: %w", schedule.Name, err)
		}
		if ttl <= 0 || ttl > MaxTTL {
			return Schedule{}, fmt.Errorf("schedule %q: ttl must be between 0 and %s", schedule.Name, MaxTTL)
		}
		schedule.TTL = ttl
	}
	schedule.TriggerOn = []string{"changed"}
	if len(raw.TriggerOn) > 0 {
		schedule.TriggerOn = nil
		for _, trigger := range raw.TriggerOn {
			switch trigger {
			case "changed", "new":
			default:
				return Schedule{}, fmt.Errorf("schedule %q: triggerOn %q is not supported (use changed or new)", schedule.Name, trigger)
			}
			schedule.TriggerOn = append(schedule.TriggerOn, trigger)
		}
	}
	if err := checkDeclaredEnv(raw.RequiredEnv, raw.OptionalEnv, denied); err != nil {
		return Schedule{}, fmt.Errorf("schedule %q: %w", schedule.Name, err)
	}
	env := prompt.EnvPolicy{
		Allow: append(append([]string(nil), raw.RequiredEnv...), raw.OptionalEnv...),
		Deny:  denied,
	}
	if strings.TrimSpace(raw.Context) != "" {
		context, err := prompt.Parse("schedule:"+schedule.Name+":context", raw.Context, env)
		if err != nil {
			return Schedule{}, fmt.Errorf("schedule %q: %w", schedule.Name, err)
		}
		schedule.Context = context
	}
	title, err := prompt.Parse("schedule:"+schedule.Name+":title", raw.Title, env)
	if err != nil {
		return Schedule{}, fmt.Errorf("schedule %q: %w", schedule.Name, err)
	}
	schedule.Title = title
	body, err := prompt.Parse("schedule:"+schedule.Name+":prompt", raw.Prompt, env)
	if err != nil {
		return Schedule{}, fmt.Errorf("schedule %q: %w", schedule.Name, err)
	}
	schedule.Prompt = body
	return schedule, nil
}

func (b *Bundle) buildPlan(agentName string, schedule Schedule, opts Options) (*sources.Plan, error) {
	order := make([]string, 0, 4)
	state := map[string]int{}
	var visit func(string, []string) error
	visit = func(name string, stack []string) error {
		switch state[name] {
		case 2:
			return nil
		case 1:
			return fmt.Errorf("schedule %q: source cycle: %s", schedule.Name, strings.Join(append(stack, name), " -> "))
		}
		def, ok := b.Sources[name]
		if !ok {
			return fmt.Errorf("schedule %q: source %q is not defined", schedule.Name, name)
		}
		if !visible(def.Scope, agentName) {
			return fmt.Errorf("schedule %q: source %q is not visible to %s", schedule.Name, name, agentName)
		}
		state[name] = 1
		for _, need := range def.Needs {
			if err := visit(need, append(stack, name)); err != nil {
				return err
			}
		}
		state[name] = 2
		order = append(order, name)
		return nil
	}
	if err := visit(schedule.Source, nil); err != nil {
		return nil, err
	}
	steps := make([]sources.Step, 0, len(order))
	for _, name := range order {
		def := b.Sources[name]
		conn := b.Connections[def.Connection]
		if !visible(conn.Scope, agentName) {
			return nil, fmt.Errorf("schedule %q: connection %q is not visible to %s", schedule.Name, conn.Name, agentName)
		}
		source, err := sources.Build(def, conn, sources.Options{Concurrency: opts.Concurrency, Timeout: opts.Timeout, DeniedEnv: opts.DeniedEnv})
		if err != nil {
			return nil, fmt.Errorf("schedule %q: %w", schedule.Name, err)
		}
		steps = append(steps, sources.Step{Source: source})
	}
	return &sources.Plan{Schedule: schedule.Name, Steps: steps, Terminal: schedule.Source}, nil
}
