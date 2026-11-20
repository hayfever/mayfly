package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeBundle(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func load(t *testing.T, dir string) (*Bundle, error) {
	t.Helper()
	return Load([]string{dir}, Options{Concurrency: 1, Timeout: 5 * time.Second})
}

const baseConnections = `
connections:
  - name: api
    baseURL: https://api.test
`

func TestExtendsMergesFields(t *testing.T) {
	dir := writeBundle(t, map[string]string{
		"a.yaml": `schemaVersion: 1
name: base
agents:
  - name: watcher
    systemPrompt: watch
` + baseConnections + `
sources:
  - name: parent
    type: http.json
    connection: api
    request:
      method: GET
      path: /items
      headers:
        X-Keep: "yes"
        X-Drop: "gone"
    extract:
      list: results
      fields:
        name:
          path: results.name
    limits:
      maxItems: 10
`,
		"b.yaml": `schemaVersion: 1
name: child
sources:
  - name: child
    extends: parent
    request:
      headers:
        X-Drop: null
        X-Add: "new"
    extract:
      fields:
        url:
          path: results.url
    limits:
      maxItems: 5
`,
	})
	bundle, err := load(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	def := bundle.Sources["child"]
	if def.Request.Path != "/items" || def.Request.Method != "GET" {
		t.Fatalf("request not inherited: %+v", def.Request)
	}
	if def.Request.Headers["X-Keep"] != "yes" || def.Request.Headers["X-Add"] != "new" {
		t.Fatalf("headers = %+v", def.Request.Headers)
	}
	if _, ok := def.Request.Headers["X-Drop"]; ok {
		t.Fatalf("null did not delete X-Drop: %+v", def.Request.Headers)
	}
	if _, ok := def.Extract.Fields["name"]; !ok {
		t.Fatalf("child lost the inherited field: %+v", def.Extract.Fields)
	}
	if _, ok := def.Extract.Fields["url"]; !ok {
		t.Fatalf("child field missing: %+v", def.Extract.Fields)
	}
	if def.Limits.MaxItems != 5 {
		t.Fatalf("maxItems = %d, want 5", def.Limits.MaxItems)
	}
	if def.Name != "child" || def.Type != "http.json" {
		t.Fatalf("identity = %q/%q", def.Name, def.Type)
	}
}

func TestScopeRules(t *testing.T) {
	agent := "agents:\n  - name: watcher\n    systemPrompt: watch\n"
	sources := func(extra string) string {
		return `schemaVersion: 1
name: bundle
` + agent + `
connections:
  - name: private-conn
    baseURL: https://api.test
    scope: agent:watcher
  - name: shared-conn
    baseURL: https://api.test
sources:
` + extra
	}
	cases := []struct {
		name  string
		extra string
		want  string
	}{
		{
			name: "shared source on private connection",
			extra: `  - name: shared-source
    type: http.json
    connection: private-conn
    request:
      path: /x
`,
			want: "cannot use private connection",
		},
		{
			name: "private owner unknown",
			extra: `  - name: ghost
    type: http.json
    connection: shared-conn
    request:
      path: /x
    scope: agent:nobody
`,
			want: "private to unknown agent",
		},
		{
			name: "invalid scope",
			extra: `  - name: bad
    type: http.json
    connection: shared-conn
    request:
      path: /x
    scope: team
`,
			want: "invalid scope",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeBundle(t, map[string]string{"bundle.yaml": sources(tc.extra)})
			_, err := load(t, dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestDuplicateDefinitionIsRejected(t *testing.T) {
	dir := writeBundle(t, map[string]string{
		"a.yaml": `schemaVersion: 1
name: a
` + baseConnections + `
sources:
  - name: shared
    type: http.json
    connection: api
    request:
      path: /x
`,
		"b.yaml": `schemaVersion: 1
name: b
sources:
  - name: shared
    type: http.json
    connection: api
    request:
      path: /y
    scope: agent:watcher
`,
	})
	_, err := load(t, dir)
	if err == nil || !strings.Contains(err.Error(), "duplicate source") {
		t.Fatalf("err = %v", err)
	}
}

func TestSharedCannotExtendPrivate(t *testing.T) {
	dir := writeBundle(t, map[string]string{
		"bundle.yaml": `schemaVersion: 1
name: bundle
agents:
  - name: watcher
    systemPrompt: watch
` + baseConnections + `
sources:
  - name: private
    type: http.json
    connection: api
    request:
      path: /x
    scope: agent:watcher
  - name: shared
    extends: private
`,
	})
	_, err := load(t, dir)
	if err == nil || !strings.Contains(err.Error(), "cannot extend private") {
		t.Fatalf("err = %v", err)
	}
}

func TestScheduleValidation(t *testing.T) {
	cases := []struct {
		name   string
		cron   string
		source string
		extra  string
		want   string
	}{
		{"six fields", "0 0 7 * * *", "nightly", "", "five-field"},
		{"macro", "@daily", "nightly", "", "five-field"},
		{"missing source", "0 7 * * *", "missing", "", "is not defined"},
		{"large ttl", "0 7 * * *", "nightly", "\n        ttl: 48h", "ttl must be"},
		{"bad trigger", "0 7 * * *", "nightly", "\n        triggerOn: [same]", "triggerOn"},
		{"timezone prefix", "CRON_TZ=Europe/Berlin 0 7 * * *", "nightly", "", "five-field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeBundle(t, map[string]string{
				"bundle.yaml": `schemaVersion: 1
name: bundle
agents:
  - name: watcher
    systemPrompt: watch
    schedules:
      - name: nightly
        cron: "` + tc.cron + `"
        source: ` + tc.source + `
        prompt: run` + tc.extra + `
` + baseConnections + `
sources:
  - name: nightly
    type: http.json
    connection: api
    request:
      path: /x
`,
			})
			_, err := load(t, dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestPlanOrderAndTerminal(t *testing.T) {
	dir := writeBundle(t, map[string]string{
		"bundle.yaml": `schemaVersion: 1
name: bundle
agents:
  - name: watcher
    systemPrompt: watch
    schedules:
      - name: nightly
        cron: "0 7 * * *"
        source: third
        prompt: run
` + baseConnections + `
sources:
  - name: third
    type: http.json
    connection: api
    needs: [second]
    request:
      path: /x
  - name: first
    type: http.json
    connection: api
    request:
      path: /x
  - name: second
    type: http.json
    connection: api
    needs: [first]
    request:
      path: /x
`,
	})
	bundle, err := load(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	plan := bundle.Plans["nightly"]
	var order []string
	for _, step := range plan.Steps {
		order = append(order, step.Name())
	}
	if got := strings.Join(order, ","); got != "first,second,third" {
		t.Fatalf("order = %s", got)
	}
	if plan.Terminal != "third" || plan.Schedule != "nightly" {
		t.Fatalf("plan = %+v", plan)
	}
	schedule, ok := bundle.Schedule("nightly")
	if !ok {
		t.Fatal("schedule not found")
	}
	if schedule.TTL != DefaultTTL || len(schedule.TriggerOn) != 1 || schedule.TriggerOn[0] != "changed" {
		t.Fatalf("defaults = %+v", schedule)
	}
}

func TestConnectionHeadersFromEnv(t *testing.T) {
	t.Setenv("PROVIDER_TOKEN", "secret")
	t.Setenv("EMPTY_TOKEN", "")
	dir := writeBundle(t, map[string]string{
		"bundle.yaml": `schemaVersion: 1
name: bundle
agents:
  - name: watcher
    systemPrompt: watch
connections:
  - name: good
    baseURL: https://api.test
    headers:
      Authorization: env:PROVIDER_TOKEN
      X-Template: 'Bearer {{ env "PROVIDER_TOKEN" }}'
sources:
  - name: one
    type: http.json
    connection: good
    request:
      path: /x
`,
	})
	bundle, err := load(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	headers := bundle.Connections["good"].ResolvedHeaders
	if headers["Authorization"] != "secret" {
		t.Fatalf("Authorization = %q", headers["Authorization"])
	}
	if headers["X-Template"] != `Bearer {{ env "PROVIDER_TOKEN" }}` {
		t.Fatalf("template header was resolved early: %q", headers["X-Template"])
	}

	dir = writeBundle(t, map[string]string{
		"bundle.yaml": `schemaVersion: 1
name: bundle
agents:
  - name: watcher
    systemPrompt: watch
connections:
  - name: bad
    baseURL: https://api.test
    headers:
      Authorization: env:EMPTY_TOKEN
sources: []
`,
	})
	if _, err := load(t, dir); err == nil || !strings.Contains(err.Error(), "unset environment variable") {
		t.Fatalf("err = %v", err)
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	dir := writeBundle(t, map[string]string{
		"bundle.yaml": `schemaVersion: 1
name: bundle
agents: []
schedules: []
`,
	})
	if _, err := load(t, dir); err == nil || !strings.Contains(err.Error(), "field schedules not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestDescribe(t *testing.T) {
	dir := writeBundle(t, map[string]string{
		"bundle.yaml": `schemaVersion: 1
name: bundle
agents:
  - name: watcher
    systemPrompt: watch
    default: true
    schedules:
      - name: nightly
        cron: "0 7 * * *"
        source: one
        prompt: run
` + baseConnections + `
sources:
  - name: one
    type: http.json
    connection: api
    request:
      path: /x
`,
	})
	bundle, err := load(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := `agents: 1 schedules: 1 sources: 1 connections: 1 plugins: 0
agent watcher default
schedule nightly agent=watcher cron="0 7 * * *" source=one ttl=30m0s trigger=changed
plan nightly steps 1 terminal one: one
`
	if got := bundle.Describe(); got != want {
		t.Fatalf("Describe() =\n%s\nwant\n%s", got, want)
	}
}

func TestRequiredEnvFailsStartup(t *testing.T) {
	t.Setenv("WATCH_TOKEN", "")
	dir := writeBundle(t, map[string]string{
		"bundle.yaml": `schemaVersion: 1
name: bundle
agents:
  - name: watcher
    systemPrompt: watch
connections:
  - name: firecrawl
    baseURL: https://api.test
    requiredEnv: [WATCH_TOKEN]
sources:
  - name: pages
    type: http.json
    connection: firecrawl
    request:
      path: /scrape
`,
	})
	_, err := load(t, dir)
	if err == nil {
		t.Fatal("load succeeded with an unset requiredEnv variable")
	}
	if !strings.Contains(err.Error(), `connection "firecrawl"`) || !strings.Contains(err.Error(), "WATCH_TOKEN") {
		t.Fatalf("err = %v", err)
	}

	t.Setenv("WATCH_TOKEN", "token")
	bundle, err := load(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Connections["firecrawl"].RequiredEnv) != 1 {
		t.Fatalf("requiredEnv = %v", bundle.Connections["firecrawl"].RequiredEnv)
	}

	t.Setenv("PAGE_ID", "")
	dir = writeBundle(t, map[string]string{
		"bundle.yaml": `schemaVersion: 1
name: bundle
agents:
  - name: watcher
    systemPrompt: watch
connections:
  - name: api
    baseURL: https://api.test
sources:
  - name: pages
    type: http.json
    connection: api
    requiredEnv: [PAGE_ID]
    request:
      path: /x
`,
	})
	_, err = load(t, dir)
	if err == nil || !strings.Contains(err.Error(), `source "pages"`) || !strings.Contains(err.Error(), "PAGE_ID") {
		t.Fatalf("err = %v", err)
	}
}

func TestBundleRejectsBrokenGraph(t *testing.T) {
	cases := []struct {
		name   string
		files  map[string]string
		expect string
	}{
		{
			name: "unknown connection",
			files: map[string]string{"a.yaml": `schemaVersion: 1
name: bundle
agents:
  - name: watcher
    systemPrompt: watch
sources:
  - name: pages
    type: http.json
    connection: missing
    request:
      path: /x
`},
			expect: `source "pages" uses undefined connection "missing"`,
		},
		{
			name: "source cycle",
			files: map[string]string{"a.yaml": `schemaVersion: 1
name: bundle
agents:
  - name: watcher
    systemPrompt: watch
connections:
  - name: api
    baseURL: https://api.test
sources:
  - name: one
    type: http.json
    connection: api
    needs: [two]
    request:
      path: /one
  - name: two
    type: http.json
    connection: api
    needs: [one]
    request:
      path: /two
`},
			expect: "has a needs cycle: one -> two -> one",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, writeBundle(t, tc.files))
			if err == nil || !strings.Contains(err.Error(), tc.expect) {
				t.Fatalf("err = %v, want %q", err, tc.expect)
			}
		})
	}
}

func TestPrivateCannotExtendAnotherAgentsPrivate(t *testing.T) {
	dir := writeBundle(t, map[string]string{
		"bundle.yaml": `schemaVersion: 1
name: bundle
agents:
  - name: a
    systemPrompt: a
  - name: b
    systemPrompt: b
connections:
  - name: b-api
    baseURL: https://b.test
    scope: agent:b
  - name: b-own
    extends: b-api
    scope: agent:b
  - name: a-copy
    extends: b-api
    scope: agent:a
`,
	})
	_, err := load(t, dir)
	if err == nil || !strings.Contains(err.Error(), `connection "a-copy"`) || !strings.Contains(err.Error(), "private to b") {
		t.Fatalf("err = %v", err)
	}
}

func scheduleBundle(scheduleEnv, context string) string {
	return `schemaVersion: 1
name: bundle
agents:
  - name: watcher
    systemPrompt: watch
    schedules:
      - name: nightly
        cron: "0 7 * * *"
        source: pages
` + scheduleEnv + `
        context: '` + context + `'
        title: t
        prompt: p
` + baseConnections + `
sources:
  - name: pages
    type: http.json
    connection: api
    request:
      path: /x
`
}

func TestScheduleEnvAllowlist(t *testing.T) {
	t.Setenv("WATCH_CONTEXT", "")
	t.Setenv("OTHER", "x")
	// An optional variable may be unset and renders empty.
	dir := writeBundle(t, map[string]string{"bundle.yaml": scheduleBundle(`        optionalEnv: [WATCH_CONTEXT]`, `{{ env "WATCH_CONTEXT" }}`)})
	bundle, err := load(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	schedule, _ := bundle.Schedule("nightly")
	if got, err := schedule.Context.Render(nil); err != nil || got != "" {
		t.Fatalf("unset optional env = %q, %v", got, err)
	}
	t.Setenv("WATCH_CONTEXT", "we are cheaper")
	if got, err := schedule.Context.Render(nil); err != nil || got != "we are cheaper" {
		t.Fatalf("optional env = %q, %v", got, err)
	}

	for _, tc := range []struct {
		env, context, want string
		opts               Options
	}{
		{``, `{{ env "WATCH_CONTEXT" }}`, "not declared", Options{}},
		{`        optionalEnv: [WATCH_CONTEXT]`, `{{ env "OTHER" }}`, "not declared", Options{}},
		{`        optionalEnv: [MAYFLY_API_TOKEN]`, `{{ env "MAYFLY_API_TOKEN" }}`, "reserved", Options{}},
		{`        optionalEnv: [OPENAI_API_KEY]`, `x`, "control-plane secret", Options{DeniedEnv: []string{"OPENAI_API_KEY"}}},
		{`        requiredEnv: [MISSING_REQUIRED]`, `x`, "MISSING_REQUIRED is not set", Options{}},
	} {
		dir := writeBundle(t, map[string]string{"bundle.yaml": scheduleBundle(tc.env, tc.context)})
		tc.opts.Concurrency, tc.opts.Timeout = 1, 5*time.Second
		if _, err := Load([]string{dir}, tc.opts); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("env %q context %q: err = %v, want %q", tc.env, tc.context, err, tc.want)
		}
	}
}

func TestDeniedEnvCannotBeDeclaredOrSentAsHeader(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "llm")
	for _, conn := range []string{
		"    requiredEnv: [OPENAI_API_KEY]\n",
		"    optionalEnv: [MAYFLY_WEBHOOK_SECRET]\n",
		"    headers:\n      Authorization: env:OPENAI_API_KEY\n",
		"    headers:\n      Authorization: env:MAYFLY_API_TOKEN\n",
	} {
		dir := writeBundle(t, map[string]string{"bundle.yaml": `schemaVersion: 1
name: bundle
agents:
  - name: watcher
    systemPrompt: watch
connections:
  - name: api
    baseURL: https://api.test
` + conn})
		_, err := Load([]string{dir}, Options{Concurrency: 1, Timeout: time.Second, DeniedEnv: []string{"OPENAI_API_KEY"}})
		if err == nil || (!strings.Contains(err.Error(), "control-plane secret") && !strings.Contains(err.Error(), "reserved")) {
			t.Fatalf("%q: err = %v", conn, err)
		}
	}
}

const pluginManifest = `schemaVersion: 1
name: notify
version: 0.1.0
placement: sidecar
image: ghcr.io/acme/notify:0.1.0
command: bin/notify
secrets:
  - env: NOTIFY_WEBHOOK_URL
    secret: mayfly-plugin-notify
    key: webhook-url
tools:
  - name: post
  - name: recent
    idempotent: true
`

func TestLoadPluginManifests(t *testing.T) {
	dir := writeBundle(t, map[string]string{
		"bundle.yaml": `schemaVersion: 1
name: bundle
agents:
  - name: writer
    systemPrompt: write
    plugins: [notify]
`,
		"plugins/notify/plugin.yaml": pluginManifest,
	})
	bundle, err := load(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	manifest, ok := bundle.Plugins["notify"]
	if !ok {
		t.Fatalf("plugins = %v", bundle.Plugins)
	}
	if manifest.Placement != "sidecar" || manifest.Scope != "shared" || len(manifest.Tools) != 2 {
		t.Fatalf("manifest = %+v", manifest)
	}
	if manifest.Dir != filepath.Join(dir, "plugins", "notify") {
		t.Fatalf("dir = %q", manifest.Dir)
	}
	if effects := manifest.Effects(); len(effects) != 1 || effects[0] != "post" {
		t.Fatalf("effects = %v", effects)
	}

	if _, err := Load([]string{dir}, Options{Concurrency: 1, Timeout: time.Second, RequirePluginImage: true}); err != nil {
		t.Fatalf("image is present: %v", err)
	}
	noImage := writeBundle(t, map[string]string{
		"plugins/notify/plugin.yaml": strings.Replace(pluginManifest, "image: ghcr.io/acme/notify:0.1.0\n", "", 1),
	})
	if _, err := Load([]string{noImage}, Options{Concurrency: 1, Timeout: time.Second, RequirePluginImage: true}); err == nil || !strings.Contains(err.Error(), "image") {
		t.Fatalf("err = %v, want a missing image error", err)
	}
}

func TestLoadSkipsPluginSource(t *testing.T) {
	dir := writeBundle(t, map[string]string{
		"plugins/notify/plugin.yaml":  pluginManifest,
		"plugins/notify/package.json": `{not json at all`,
		"plugins/notify/tsconfig.json": `{
  "compilerOptions": {"strict": true}
}`,
		"plugins/notify/extra.yaml": "notABundleKey: true\n",
	})
	bundle, err := load(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bundle.Plugins["notify"]; !ok {
		t.Fatalf("plugins = %v", bundle.Plugins)
	}
}

func TestFlatPluginManifest(t *testing.T) {
	dir := writeBundle(t, map[string]string{
		"notify.plugin.yaml": pluginManifest,
		"nested/other.plugin.yaml": `schemaVersion: 1
name: other
command: bin/other
tools:
  - name: run
`,
	})
	bundle, err := load(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bundle.Plugins["notify"]; !ok {
		t.Fatalf("plugins = %v", bundle.Plugins)
	}
	if bundle.Plugins["other"].Dir != filepath.Join(dir, "nested") {
		t.Fatalf("dir = %q", bundle.Plugins["other"].Dir)
	}

	mismatch := writeBundle(t, map[string]string{
		"plugins/x/plugin.yaml": strings.Replace(pluginManifest, "name: notify", "name: y", 1),
	})
	if _, err := load(t, mismatch); err == nil || !strings.Contains(err.Error(), `"y"`) || !strings.Contains(err.Error(), `"x"`) {
		t.Fatalf("err = %v, want a name mismatch naming both names", err)
	}
}

func TestDuplicatePlugin(t *testing.T) {
	dir := writeBundle(t, map[string]string{
		"plugins/notify/plugin.yaml": pluginManifest,
		"flat/notify.plugin.yaml":    pluginManifest,
	})
	_, err := load(t, dir)
	if err == nil || !strings.Contains(err.Error(), `duplicate plugin "notify"`) {
		t.Fatalf("err = %v", err)
	}
	for _, path := range []string{
		filepath.Join(dir, "plugins", "notify", "plugin.yaml"),
		filepath.Join(dir, "flat", "notify.plugin.yaml"),
	} {
		if !strings.Contains(err.Error(), path) {
			t.Fatalf("err = %v, want %s", err, path)
		}
	}
}

func TestPrivatePluginScope(t *testing.T) {
	private := strings.Replace(pluginManifest, "placement: sidecar", "placement: sidecar\nscope: agent:ghost", 1)
	dir := writeBundle(t, map[string]string{"plugins/notify/plugin.yaml": private})
	if _, err := load(t, dir); err == nil || !strings.Contains(err.Error(), `private to unknown agent "ghost"`) {
		t.Fatalf("err = %v", err)
	}

	dir = writeBundle(t, map[string]string{
		"bundle.yaml": `schemaVersion: 1
name: bundle
agents:
  - name: ghost
    systemPrompt: boo
`,
		"plugins/notify/plugin.yaml": private,
	})
	bundle, err := load(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Plugins["notify"].Scope != "agent:ghost" {
		t.Fatalf("scope = %q", bundle.Plugins["notify"].Scope)
	}
}

func TestDescribePlugins(t *testing.T) {
	dir := writeBundle(t, map[string]string{
		"bundle.yaml": `schemaVersion: 1
name: bundle
agents:
  - name: ghost
    systemPrompt: boo
  - name: writer
    systemPrompt: write
    default: true
    plugins: [notify, crm]
`,
		"plugins/notify/plugin.yaml": pluginManifest,
		"plugins/word-stats/plugin.yaml": `schemaVersion: 1
name: word-stats
command: server/server
tools:
  - name: count_words
    idempotent: true
`,
		"plugins/crm/plugin.yaml": `schemaVersion: 1
name: crm
placement: service
service:
  mcpServer: crm
tools:
  - name: find_account
    idempotent: true
`,
	})
	bundle, err := load(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := `agents: 2 schedules: 0 sources: 0 connections: 0 plugins: 3
agent ghost
agent writer default plugins=notify,crm
plugin crm placement=service scope=shared tools=1 effects=- image=- mcpServer=crm
plugin notify placement=sidecar scope=shared tools=2 effects=post image=ghcr.io/acme/notify:0.1.0
plugin word-stats placement=inline scope=shared tools=1 effects=- image=-
`
	if got := bundle.Describe(); got != want {
		t.Fatalf("Describe() =\n%s\nwant\n%s", got, want)
	}
}
