package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hayfever/mayfly/internal/version"
)

// apiServer starts a control plane stub and points MAYFLY_URL at it.
func apiServer(t *testing.T, handler http.Handler) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	t.Setenv("MAYFLY_URL", server.URL)
}

// command returns the single JSON document a command wrote to stdout. A
// second document or any other text makes json.Unmarshal fail.
func command(t *testing.T, fn func() error) any {
	t.Helper()
	out, err := captureStdout(t, fn)
	if err != nil {
		t.Fatalf("command: %v", err)
	}
	var doc any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %q: %v", out, err)
	}
	return doc
}

func object(t *testing.T, v any) map[string]any {
	t.Helper()
	obj, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("document = %T, want an object", v)
	}
	return obj
}

func array(t *testing.T, v any) []any {
	t.Helper()
	items, ok := v.([]any)
	if !ok {
		t.Fatalf("document = %T, want an array", v)
	}
	return items
}

func TestGetJSONKeepsAPIFields(t *testing.T) {
	apiServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions/s1" {
			t.Errorf("path = %q", r.URL.Path)
		}
		io.WriteString(w, `{"session":{"id":"s1","title":"triage","status":"running","agent":"a","created_at":"2026-11-19T10:00:00Z","expires_at":"2026-11-19T11:00:00Z"},"url":"http://ui/s1","added_later":true}`)
	}))
	doc := object(t, command(t, func() error { return cmdGet([]string{"s1", "--json"}) }))
	session := object(t, doc["session"])
	if session["id"] != "s1" || doc["url"] != "http://ui/s1" {
		t.Fatalf("document = %+v, want the session and URL", doc)
	}
	if doc["added_later"] != true {
		t.Fatalf("document = %+v, want the unknown API field", doc)
	}
	if _, ok := session["backend"]; ok {
		t.Fatal("the CLI added a backend field that the API does not send")
	}
}

func TestGetTextSummary(t *testing.T) {
	apiServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"session":{"id":"s1","title":"triage","status":"running","message_count":4,"created_at":"2026-11-19T10:00:00Z","expires_at":"2026-11-19T11:00:00Z"},"url":"http://ui/s1"}`)
	}))
	out, err := captureStdout(t, func() error { return cmdGet([]string{"s1"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"id:        s1", "status:    running", "messages:  4", "url:       http://ui/s1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q does not contain %q", out, want)
		}
	}
}

func TestListJSONReadsEveryPage(t *testing.T) {
	apiServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		switch r.URL.Query().Get("cursor") {
		case "":
			io.WriteString(w, `{"items":[{"id":"s1","status":"running","added_later":1}],"next_cursor":"c2"}`)
		case "c2":
			io.WriteString(w, `{"items":[{"id":"s2","status":"completed"}]}`)
		default:
			t.Errorf("cursor = %q", r.URL.Query().Get("cursor"))
		}
	}))
	items := array(t, command(t, func() error { return cmdList([]string{"--json"}) }))
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	first := object(t, items[0])
	if first["id"] != "s1" || first["added_later"] != float64(1) {
		t.Fatalf("first item = %+v, want the raw API object", first)
	}
	if object(t, items[1])["id"] != "s2" {
		t.Fatalf("second item = %+v", items[1])
	}
}

func TestAgentsJSONKeepsAPIFields(t *testing.T) {
	apiServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/config" {
			t.Errorf("path = %q", r.URL.Path)
		}
		io.WriteString(w, `{"agents":[{"name":"default","description":"built-in","harness":"codex","model":"m","plugins":["lint"],"default":true,"builtin":true}]}`)
	}))
	agents := array(t, command(t, func() error { return cmdAgents([]string{"--json"}) }))
	if len(agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(agents))
	}
	agent := object(t, agents[0])
	if agent["name"] != "default" {
		t.Fatalf("agent = %+v", agent)
	}
	if plugins, ok := agent["plugins"].([]any); !ok || len(plugins) != 1 {
		t.Fatalf("agent = %+v, want the plugins field as the API sent it", agent)
	}
}

func TestEventsJSONKeepsAPIFields(t *testing.T) {
	apiServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions/s1/events" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("after") != "4" {
			t.Errorf("after = %q, want 4", r.URL.Query().Get("after"))
		}
		io.WriteString(w, `{"session_id":"s1","events":[{"seq":5,"time":"2026-11-19T10:00:00Z","type":"status","text":"working","added_later":"x"}]}`)
	}))
	events := array(t, command(t, func() error { return cmdEvents([]string{"s1", "--after", "4", "--json"}) }))
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	event := object(t, events[0])
	if event["seq"] != float64(5) || event["added_later"] != "x" {
		t.Fatalf("event = %+v, want the raw API object", event)
	}
}

func TestRunJSON(t *testing.T) {
	apiServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sessions" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["prompt"] != "hello" || body["agent"] != "triage" {
			t.Errorf("body = %+v", body)
		}
		io.WriteString(w, `{"session":{"id":"s1","status":"pending"},"url":"http://ui/s1","added_later":1}`)
	}))
	doc := object(t, command(t, func() error {
		return cmdRun([]string{"--prompt", "hello", "--agent", "triage", "--json"})
	}))
	if object(t, doc["session"])["id"] != "s1" || doc["url"] != "http://ui/s1" || doc["added_later"] != float64(1) {
		t.Fatalf("document = %+v, want the API response", doc)
	}
}

func TestSendJSON(t *testing.T) {
	apiServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sessions/s1/messages" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, `{"event":{"seq":9,"time":"2026-11-19T10:00:00Z","type":"user_message","text":"hello"},"added_later":1}`)
	}))
	doc := object(t, command(t, func() error { return cmdSend([]string{"s1", "--json", "hello"}) }))
	if object(t, doc["event"])["seq"] != float64(9) || doc["added_later"] != float64(1) {
		t.Fatalf("document = %+v, want the API response", doc)
	}
}

func TestSendKeepsDashesInText(t *testing.T) {
	var got string
	apiServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		got = body.Text
		io.WriteString(w, `{"event":{"seq":1,"type":"user_message"}}`)
	}))
	if _, err := captureStdout(t, func() error { return cmdSend([]string{"s1", "rerun", "with", "--force", "-v"}) }); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got != "rerun with --force -v" {
		t.Fatalf("text = %q, want the message as typed", got)
	}
}

func TestVersionJSON(t *testing.T) {
	doc := object(t, command(t, func() error { return cmdVersion([]string{"--json"}) }))
	if doc["version"] != version.Version || doc["commit"] != version.Commit || doc["date"] != version.Date {
		t.Fatalf("document = %+v, want the build identity", doc)
	}
}

func TestJSONErrorKeepsCode(t *testing.T) {
	apiServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"code":"not_found","message":"no session"}}`)
	}))
	out, err := captureStdout(t, func() error { return cmdGet([]string{"s1", "--json"}) })
	if err == nil || err.Error() != "not_found: no session" {
		t.Fatalf("error = %v, want the error code and message", err)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want no document", out)
	}
}

func TestArtifactOutputAliases(t *testing.T) {
	apiServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions/s1/artifact" {
			t.Errorf("path = %q", r.URL.Path)
		}
		io.WriteString(w, "bundle-bytes")
	}))
	for _, flag := range []string{"--output", "-o"} {
		t.Run(flag, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bundle.tar.gz")
			if _, err := captureStdout(t, func() error {
				return cmdArtifact([]string{"s1", flag, path})
			}); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != "bundle-bytes" {
				t.Fatalf("file = %q, want the bundle bytes", raw)
			}
		})
	}
}
