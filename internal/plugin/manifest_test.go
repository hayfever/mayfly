package plugin

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// originPath is the manifest path the error messages must name.
const originPath = "/bundle/plugins/demo/plugin.yaml"

func inlineManifest() Manifest {
	return Manifest{
		SchemaVersion: SchemaVersion,
		Name:          "demo",
		Placement:     Inline,
		Origin:        originPath,
		Image:         "example.com/mayfly-plugin-demo:1",
		Command:       "bin/demo",
		Tools:         []Tool{{Name: "run", Idempotent: true}},
	}
}

func sidecarManifest() Manifest {
	m := inlineManifest()
	m.Placement = Sidecar
	m.Sidecar = &SidecarSpec{}
	return m
}

func serviceManifest() Manifest {
	m := inlineManifest()
	m.Placement = Service
	m.Service = &ServiceSpec{}
	return m
}

func remoteManifest() Manifest {
	m := inlineManifest()
	m.Placement = Service
	m.Image, m.Command, m.Args = "", "", nil
	m.Service = &ServiceSpec{MCPServer: "crm"}
	return m
}

func TestParseRejectsUnknownFields(t *testing.T) {
	cases := []struct{ name, raw string }{
		{"top level", "schemaVersion: 1\nname: demo\nbogus: 1\n"},
		{"nested block", "schemaVersion: 1\nname: demo\ninline:\n  bogus: 1\n"},
		{"tool field", "schemaVersion: 1\nname: demo\ntools:\n  - name: run\n    retries: 2\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.raw), originPath); err == nil {
				t.Fatal("unknown field accepted")
			} else if !strings.Contains(err.Error(), originPath) {
				t.Fatalf("error does not name the manifest: %v", err)
			}
		})
	}

	manifest, err := Parse([]byte("schemaVersion: 1\nname: demo\n"), "relative/plugin.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Origin != "relative/plugin.yaml" {
		t.Fatalf("origin = %q", manifest.Origin)
	}
}

func TestValidateRules(t *testing.T) {
	longTool := strings.Repeat("a", 60)
	cases := []struct {
		name   string
		base   func() Manifest
		mutate func(*Manifest)
		opts   ValidateOptions
		want   string
	}{
		{
			name:   "rule 1 schema version",
			mutate: func(m *Manifest) { m.SchemaVersion = 2 },
			want:   "schemaVersion",
		},
		{
			name:   "rule 1 name",
			mutate: func(m *Manifest) { m.Name = "Demo Plugin" },
			want:   "name",
		},
		{
			name:   "rule 3 placement",
			mutate: func(m *Manifest) { m.Placement = "remote" },
			want:   "placement",
		},
		{
			name:   "rule 4 command",
			mutate: func(m *Manifest) { m.Command = "" },
			want:   "command",
		},
		{
			name:   "rule 4 image",
			mutate: func(m *Manifest) { m.Image = "" },
			opts:   ValidateOptions{RequireImage: true},
			want:   "image",
		},
		{
			name:   "rule 6 workdir",
			mutate: func(m *Manifest) { m.Inline = &InlineSpec{Workdir: "elsewhere"} },
			want:   "inline.workdir",
		},
		{
			name:   "rule 7 runtime env",
			mutate: func(m *Manifest) { m.Env = map[string]string{"PATH": "/usr/bin"} },
			want:   "env",
		},
		{
			name:   "rule 7 reserved env",
			mutate: func(m *Manifest) { m.Env = map[string]string{"MAYFLY_SESSION_TOKEN": "x"} },
			want:   "env",
		},
		{
			name:   "rule 7 denied env",
			mutate: func(m *Manifest) { m.Env = map[string]string{"OPENAI_API_KEY": "x"} },
			opts:   ValidateOptions{DeniedEnv: []string{"OPENAI_API_KEY"}},
			want:   "env",
		},
		{
			name:   "rule 7 env reference",
			mutate: func(m *Manifest) { m.Env = map[string]string{"TOKEN": "env:DEMO_TOKEN"} },
			want:   "env",
		},
		{
			name: "rule 7 env and secret overlap",
			mutate: func(m *Manifest) {
				m.Env = map[string]string{"DEMO_TOKEN": "x"}
				m.Secrets = []Secret{{Env: "DEMO_TOKEN", Secret: "mayfly-plugin-demo", Key: "token"}}
			},
			want: "secrets[0].env",
		},
		{
			name: "rule 7 secret env name",
			mutate: func(m *Manifest) {
				m.Secrets = []Secret{{Env: "DEMO-TOKEN", Secret: "mayfly-plugin-demo", Key: "token"}}
			},
			want: "secrets[0].env",
		},
		{
			name: "rule 8 secret prefix",
			mutate: func(m *Manifest) {
				m.Secrets = []Secret{{Env: "DEMO_TOKEN", Secret: "demo-secret", Key: "token"}}
			},
			want: "secrets[0].secret",
		},
		{
			name: "rule 8 secret subdomain",
			mutate: func(m *Manifest) {
				m.Secrets = []Secret{{Env: "DEMO_TOKEN", Secret: "mayfly-plugin-Demo", Key: "token"}}
			},
			want: "secrets[0].secret",
		},
		{
			name: "rule 8 secret key",
			mutate: func(m *Manifest) {
				m.Secrets = []Secret{{Env: "DEMO_TOKEN", Secret: "mayfly-plugin-demo", Key: "bad key"}}
			},
			want: "secrets[0].key",
		},
		{
			name:   "rule 9 no tools",
			mutate: func(m *Manifest) { m.Tools = nil },
			want:   "tools",
		},
		{
			name:   "rule 9 empty tool",
			mutate: func(m *Manifest) { m.Tools = []Tool{{Name: " "}} },
			want:   "tools[0].name",
		},
		{
			name:   "rule 9 duplicate tool",
			mutate: func(m *Manifest) { m.Tools = []Tool{{Name: "run"}, {Name: "run"}} },
			want:   "tools[1].name",
		},
		{
			name:   "rule 9 long tool name",
			mutate: func(m *Manifest) { m.Tools = []Tool{{Name: longTool}} },
			want:   "tools[0].name",
		},
		{
			name:   "rule 9 sanitized collision",
			mutate: func(m *Manifest) { m.Tools = []Tool{{Name: "a-b"}, {Name: "a_b"}} },
			want:   "tools[1].name",
		},
		{
			name:   "rule 10 call timeout duration",
			mutate: func(m *Manifest) { m.Limits.CallTimeout = "soon" },
			want:   "limits.callTimeout",
		},
		{
			name:   "rule 10 call timeout zero",
			mutate: func(m *Manifest) { m.Limits.CallTimeout = "0s" },
			want:   "limits.callTimeout",
		},
		{
			name:   "rule 10 call timeout ceiling",
			mutate: func(m *Manifest) { m.Limits.CallTimeout = "20m" },
			want:   "limits.callTimeout",
		},
		{
			name:   "rule 10 tool timeout",
			mutate: func(m *Manifest) { m.Tools = []Tool{{Name: "run", Timeout: "nope"}} },
			want:   "tools[0].timeout",
		},
		{
			name:   "rule 10 max restarts",
			mutate: func(m *Manifest) { m.Inline = &InlineSpec{MaxRestarts: new(11)} },
			want:   "inline.maxRestarts",
		},
		{
			name:   "rule 10 max output",
			mutate: func(m *Manifest) { m.Limits.MaxOutputBytes = 70000 },
			want:   "limits.maxOutputBytes",
		},
		{
			name:   "rule 10 stderr bytes",
			mutate: func(m *Manifest) { m.Inline = &InlineSpec{StderrBytes: 32 << 20} },
			want:   "inline.stderrBytes",
		},
		{
			name:   "rule 10 rate requests",
			mutate: func(m *Manifest) { m.Limits.Rate = &Rate{Requests: 0, Interval: "1m"} },
			want:   "limits.rate.requests",
		},
		{
			name:   "rule 10 rate interval",
			mutate: func(m *Manifest) { m.Limits.Rate = &Rate{Requests: 1, Interval: "500ms"} },
			want:   "limits.rate.interval",
		},
		{
			name:   "rule 10 rate burst",
			mutate: func(m *Manifest) { m.Limits.Rate = &Rate{Requests: 1, Interval: "1m", Burst: -1} },
			want:   "limits.rate.burst",
		},
		{
			name:   "rule 11 run as user",
			base:   sidecarManifest,
			mutate: func(m *Manifest) { m.Sidecar.RunAsUser = new(int64(0)) },
			want:   "sidecar.runAsUser",
		},
		{
			name:   "rule 11 workspace",
			base:   sidecarManifest,
			mutate: func(m *Manifest) { m.Sidecar.Workspace = "all" },
			want:   "sidecar.workspace",
		},
		{
			name: "rule 11 quantity",
			base: sidecarManifest,
			mutate: func(m *Manifest) {
				m.Sidecar.Resources = Resources{Requests: map[string]string{"cpu": "lots"}}
			},
			want: "sidecar.resources.requests.cpu",
		},
		{
			name:   "rule 12 replicas",
			base:   serviceManifest,
			mutate: func(m *Manifest) { m.Service.Replicas = new(int32(21)) },
			want:   "service.replicas",
		},
		{
			name:   "rule 12 concurrency",
			base:   serviceManifest,
			mutate: func(m *Manifest) { m.Service.Concurrency = 300 },
			want:   "service.concurrency",
		},
		{
			name:   "rule 12 transport",
			base:   serviceManifest,
			mutate: func(m *Manifest) { m.Service.Transport = "grpc" },
			want:   "service.transport",
		},
		{
			name:   "rule 12 port",
			base:   serviceManifest,
			mutate: func(m *Manifest) { m.Service.Port = 70000 },
			want:   "service.port",
		},
		{
			name:   "rule 12 path",
			base:   serviceManifest,
			mutate: func(m *Manifest) { m.Service.Path = "mcp" },
			want:   "service.path",
		},
		{
			name: "rule 12 egress cidr",
			base: serviceManifest,
			mutate: func(m *Manifest) {
				m.Service.Egress = []Egress{{CIDR: "10.0.0.0/33", Ports: []int32{443}}}
			},
			want: "service.egress[0].cidr",
		},
		{
			name: "rule 12 egress port",
			base: serviceManifest,
			mutate: func(m *Manifest) {
				m.Service.Egress = []Egress{{CIDR: "10.0.0.0/16", Ports: []int32{0}}}
			},
			want: "service.egress[0].ports[0]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := inlineManifest
			if tc.base != nil {
				base = tc.base
			}
			manifest := base()
			tc.mutate(&manifest)
			err := manifest.Validate(tc.opts)
			if err == nil {
				t.Fatalf("accepted, want %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), originPath) {
				t.Fatalf("error %q does not name the manifest path", err)
			}
		})
	}
}

func TestPlacementBlocks(t *testing.T) {
	cases := []struct {
		name string
		base func() Manifest
		want string
	}{
		{"service block on inline", func() Manifest {
			m := inlineManifest()
			m.Service = &ServiceSpec{}
			return m
		}, "service"},
		{"inline block on sidecar", func() Manifest {
			m := sidecarManifest()
			m.Inline = &InlineSpec{}
			return m
		}, "inline"},
		{"sidecar block on service", func() Manifest {
			m := serviceManifest()
			m.Sidecar = &SidecarSpec{}
			return m
		}, "sidecar"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := tc.base()
			err := manifest.Validate(ValidateOptions{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want field %q", err, tc.want)
			}
		})
	}

	remote := []struct {
		name   string
		mutate func(*Manifest)
		want   string
	}{
		{"image", func(m *Manifest) { m.Image = "example.com/demo:1" }, "image"},
		{"command", func(m *Manifest) { m.Command = "bin/demo" }, "command"},
		{"args", func(m *Manifest) { m.Args = []string{"--flag"} }, "args"},
		{"local", func(m *Manifest) { m.Local = &LocalRun{Command: "demo"} }, "local"},
		{"env", func(m *Manifest) { m.Env = map[string]string{"DEMO": "1"} }, "env"},
		{"secrets", func(m *Manifest) {
			m.Secrets = []Secret{{Env: "DEMO_TOKEN", Secret: "mayfly-plugin-demo", Key: "token"}}
		}, "secrets"},
		{"replicas", func(m *Manifest) { m.Service.Replicas = new(int32(2)) }, "service.replicas"},
		{"concurrency", func(m *Manifest) { m.Service.Concurrency = 4 }, "service.concurrency"},
		{"resources", func(m *Manifest) {
			m.Service.Resources = Resources{Limits: map[string]string{"cpu": "10m"}}
		}, "service.resources"},
		{"egress", func(m *Manifest) {
			m.Service.Egress = []Egress{{CIDR: "10.0.0.0/16", Ports: []int32{443}}}
		}, "service.egress"},
		{"transport", func(m *Manifest) { m.Service.Transport = "http" }, "service.transport"},
		{"port", func(m *Manifest) { m.Service.Port = 8080 }, "service.port"},
		{"path", func(m *Manifest) { m.Service.Path = "/mcp" }, "service.path"},
	}
	for _, tc := range remote {
		t.Run("mcpServer rejects "+tc.name, func(t *testing.T) {
			manifest := remoteManifest()
			tc.mutate(&manifest)
			err := manifest.Validate(ValidateOptions{RequireImage: true})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want field %q", err, tc.want)
			}
		})
	}

	manifest := remoteManifest()
	if err := manifest.Validate(ValidateOptions{RequireImage: true}); err != nil {
		t.Fatalf("remote service rejected: %v", err)
	}
}

func TestDefaults(t *testing.T) {
	manifest := inlineManifest()
	manifest.Args = []string{"--serve"}
	if err := manifest.Validate(ValidateOptions{}); err != nil {
		t.Fatal(err)
	}
	if manifest.Placement != Inline {
		t.Fatalf("placement = %q", manifest.Placement)
	}
	if manifest.Local == nil || manifest.Local.Command != "bin/demo" {
		t.Fatalf("local = %+v", manifest.Local)
	}
	if strings.Join(manifest.Local.Args, " ") != "--serve" {
		t.Fatalf("local args = %v", manifest.Local.Args)
	}
	if manifest.Inline == nil {
		t.Fatal("inline block not defaulted")
	}
	if manifest.Inline.Workdir != "plugin" {
		t.Fatalf("workdir = %q", manifest.Inline.Workdir)
	}
	if got := *manifest.Inline.MaxRestarts; got != 2 {
		t.Fatalf("maxRestarts = %d", got)
	}
	if manifest.Inline.StderrBytes != 1<<20 {
		t.Fatalf("stderrBytes = %d", manifest.Inline.StderrBytes)
	}
	if manifest.Limits.InitTimeout != "30s" || manifest.Limits.CallTimeout != "2m0s" {
		t.Fatalf("limits = %+v", manifest.Limits)
	}
	if manifest.Limits.MaxOutputBytes != MaxOutputBytes {
		t.Fatalf("maxOutputBytes = %d", manifest.Limits.MaxOutputBytes)
	}
	if got := manifest.InitTimeout(); got != 30*time.Second {
		t.Fatalf("InitTimeout = %s", got)
	}
	if got := manifest.CallTimeout(); got != 120*time.Second {
		t.Fatalf("CallTimeout = %s", got)
	}
	if got := manifest.ToolTimeout("run"); got != 120*time.Second {
		t.Fatalf("ToolTimeout(run) = %s", got)
	}

	tool := inlineManifest()
	tool.Tools = []Tool{{Name: "run", Timeout: "45s"}}
	if err := tool.Validate(ValidateOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := tool.ToolTimeout("run"); got != 45*time.Second {
		t.Fatalf("ToolTimeout = %s", got)
	}
	if got := tool.ToolTimeout("absent"); got != 120*time.Second {
		t.Fatalf("unknown tool timeout = %s", got)
	}

	rate := inlineManifest()
	rate.Limits.Rate = &Rate{Requests: 20, Interval: "1m"}
	if err := rate.Validate(ValidateOptions{}); err != nil {
		t.Fatal(err)
	}
	if rate.Limits.Rate.Burst != 20 {
		t.Fatalf("burst = %d", rate.Limits.Rate.Burst)
	}

	sidecar := sidecarManifest()
	if err := sidecar.Validate(ValidateOptions{}); err != nil {
		t.Fatal(err)
	}
	if sidecar.Sidecar.RunAsUser == nil || *sidecar.Sidecar.RunAsUser != 65532 {
		t.Fatalf("runAsUser = %v", sidecar.Sidecar.RunAsUser)
	}
	if sidecar.Sidecar.Workspace != "none" {
		t.Fatalf("workspace = %q", sidecar.Sidecar.Workspace)
	}

	bareSidecar := inlineManifest()
	bareSidecar.Placement = Sidecar
	if err := bareSidecar.Validate(ValidateOptions{}); err != nil {
		t.Fatal(err)
	}
	if bareSidecar.Sidecar == nil || *bareSidecar.Sidecar.RunAsUser != 65532 {
		t.Fatalf("sidecar block = %+v", bareSidecar.Sidecar)
	}

	service := serviceManifest()
	if err := service.Validate(ValidateOptions{}); err != nil {
		t.Fatal(err)
	}
	// The service block itself is optional; the defaults still apply.
	bareService := inlineManifest()
	bareService.Placement = Service
	if err := bareService.Validate(ValidateOptions{}); err != nil {
		t.Fatal(err)
	}
	if bareService.Service == nil || bareService.Service.Port != 8080 {
		t.Fatalf("service block = %+v", bareService.Service)
	}

	want := ServiceSpec{Replicas: new(int32(1)), Concurrency: 8, Transport: "stdio", Port: 8080, Path: "/mcp"}
	got := *service.Service
	if *got.Replicas != *want.Replicas || got.Concurrency != want.Concurrency ||
		got.Transport != want.Transport || got.Port != want.Port || got.Path != want.Path {
		t.Fatalf("service = %+v", got)
	}
}

func TestEffects(t *testing.T) {
	manifest := sidecarManifest()
	manifest.Tools = []Tool{
		{Name: "recent", Idempotent: true},
		{Name: "post"},
		{Name: "delete"},
	}
	if got := strings.Join(manifest.Effects(), ","); got != "delete,post" {
		t.Fatalf("effects = %q", got)
	}
	if got := len(inlineManifest().Effects()); got != 0 {
		t.Fatalf("effects = %d", got)
	}
}

func TestSpecHash(t *testing.T) {
	manifest := serviceManifest()
	manifest.Dir = "/bundle/plugins/demo"
	first := manifest.SpecHash()
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(first) {
		t.Fatalf("hash = %q", first)
	}
	moved := manifest
	moved.Dir = "/other/plugins/demo"
	moved.Origin = "/other/plugins/demo/plugin.yaml"
	if got := moved.SpecHash(); got != first {
		t.Fatalf("path changed the hash: %s != %s", got, first)
	}
	changed := manifest
	changed.Command = "bin/other"
	if got := changed.SpecHash(); got == first {
		t.Fatal("spec change did not change the hash")
	}
}

func TestToolNameMatchesMCP(t *testing.T) {
	raw, err := os.ReadFile("testdata/sanitize.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		In  string `json:"in"`
		Out string `json:"out"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("shared sanitizer table is empty")
	}
	for _, tc := range cases {
		if got := sanitize(tc.In); got != tc.Out {
			t.Fatalf("sanitize(%q) = %q, want %q", tc.In, got, tc.Out)
		}
		want := "plugin_" + tc.Out + "_post"
		if got := ToolName(tc.In, "post"); got != want {
			t.Fatalf("ToolName(%q, post) = %q, want %q", tc.In, got, want)
		}
	}
	if got := ToolName("notify", "post"); got != "plugin_notify_post" {
		t.Fatalf("ToolName = %q", got)
	}
}

func TestIsManifestPath(t *testing.T) {
	cases := []struct {
		rel      string
		name     string
		manifest bool
		skip     bool
	}{
		{"plugins/word-stats/plugin.yaml", "word-stats", true, false},
		{"plugins/word-stats/server.py", "", false, true},
		{"plugins/word-stats/bin/count", "", false, true},
		{"plugins/word-stats/Dockerfile", "", false, true},
		{"plugins/notes.yaml", "", false, false},
		{"plugins/plugin.yaml", "", false, false},
		{"plugins/crm.plugin.yaml", "crm", true, false},
		{"groups/team/crm.plugin.yaml", "crm", true, false},
		{"crm.plugin.yaml", "crm", true, false},
		{"plugins/word-stats/nested/extra.yaml", "", false, true},
		{"a/plugins/x/plugin.yaml", "", false, false},
		{"agents.yaml", "", false, false},
		{"schedules/nightly.json", "", false, false},
	}
	for _, tc := range cases {
		name, manifest, skip := IsManifestPath(tc.rel)
		if name != tc.name || manifest != tc.manifest || skip != tc.skip {
			t.Fatalf("IsManifestPath(%q) = %q, %t, %t; want %q, %t, %t",
				tc.rel, name, manifest, skip, tc.name, tc.manifest, tc.skip)
		}
	}
}

func TestContainerAndObjectNames(t *testing.T) {
	if got := ContainerName("notify"); got != "plugin-notify" {
		t.Fatalf("ContainerName = %q", got)
	}
	if got := ObjectName("notify"); got != "mayfly-plugin-notify" {
		t.Fatalf("ObjectName = %q", got)
	}
	if got := ObjectName("Word_Stats.v2"); got != "mayfly-plugin-word-stats-v2" {
		t.Fatalf("ObjectName = %q", got)
	}
	if got := GatewayKeyName("notify"); got != "mayfly-gateway-notify" {
		t.Fatalf("GatewayKeyName = %q", got)
	}
	if GatewayKeyName("notify") == ObjectName("notify") {
		t.Fatal("gateway key name matches a plugin object name")
	}

	label := regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)
	hashSuffix := regexp.MustCompile(`-[0-9a-f]{8}$`)
	shared := strings.Repeat("same", 15) // 60 characters
	long := shared + "alpha"
	short := shared + "beta"
	for _, name := range []string{ContainerName(long), ObjectName(long)} {
		if len(name) != dnsMaxLength {
			t.Fatalf("%q is %d characters", name, len(name))
		}
		if !label.MatchString(name) {
			t.Fatalf("%q is not a DNS label", name)
		}
		if !hashSuffix.MatchString(name) {
			t.Fatalf("%q lacks an 8-character hash suffix", name)
		}
	}
	if ObjectName(long) == ObjectName(short) {
		t.Fatalf("long names collided on %q", ObjectName(long))
	}
	if ContainerName(long) == ObjectName(long) {
		t.Fatalf("container and object names match: %q", ContainerName(long))
	}
}

func TestDocManifests(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), FlatSuffix) {
			continue
		}
		count++
		t.Run(entry.Name(), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := Parse(raw, originPath)
			if err != nil {
				t.Fatal(err)
			}
			err = manifest.Validate(ValidateOptions{RequireImage: true})
			if entry.Name() == "reference"+FlatSuffix {
				// The doc's field reference lists all three placement blocks
				// under one placement, so rule 3 must reject it.
				if err == nil || !strings.Contains(err.Error(), "inline") {
					t.Fatalf("err = %v, want the mismatched inline block", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s does not validate: %v", entry.Name(), err)
			}
		})
	}
	if count == 0 {
		t.Fatal("no doc manifests in testdata")
	}
}

func TestLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "word-stats")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "word-stats"+FlatSuffix))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, ManifestFile)
	if err := os.WriteFile(file, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, err := Load(dir, ValidateOptions{RequireImage: true})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Dir != dir {
		t.Fatalf("dir = %q, want %q", manifest.Dir, dir)
	}
	if manifest.Origin != file {
		t.Fatalf("origin = %q", manifest.Origin)
	}
	if !path.IsAbs(manifest.Dir) {
		t.Fatalf("dir %q is not absolute", manifest.Dir)
	}
	if _, err := Load(filepath.Join(dir, "missing"), ValidateOptions{}); err == nil {
		t.Fatal("missing manifest accepted")
	}
}
