package sources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// listServer serves rows, of which only every fifteenth is active.
func listServer(t *testing.T, rows int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		items := make([]any, 0, rows)
		for i := 0; i < rows; i++ {
			items = append(items, map[string]any{"name": fmt.Sprintf("row %d", i), "active": i%15 == 0})
		}
		writeJSON(w, map[string]any{"results": items})
	}))
	t.Cleanup(server.Close)
	return server
}

func TestMaxItemsCountsFilteredDocuments(t *testing.T) {
	server := listServer(t, 45)
	def := Definition{
		Request: Request{Method: http.MethodPost, Path: "/query"},
		Extract: Extract{List: "results", Fields: map[string]Field{"name": {Path: "name"}, "active": {Path: "active"}}},
		Filter:  &Filter{Equals: map[string]any{"active": true}},
		Limits:  Limits{MaxItems: 40},
	}
	documents, err := newTestSource(t, server.URL, def).Run(context.Background(), Input{})
	if err != nil {
		t.Fatalf("45 rows with 3 active, maxItems 40: %v", err)
	}
	if len(documents) != 3 {
		t.Fatalf("documents = %d, want 3", len(documents))
	}
	def.Limits.MaxItems = 2
	if _, err := newTestSource(t, server.URL, def).Run(context.Background(), Input{}); err == nil || !strings.Contains(err.Error(), "limits.maxItems=2") {
		t.Fatalf("3 kept documents over maxItems 2: err = %v", err)
	}
}

func TestMaxPagesFailsWhenMorePagesRemain(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"has_more": true, "next_cursor": "next", "results": []any{map[string]any{"name": "x"}}})
	}))
	defer server.Close()
	source := newTestSource(t, server.URL, Definition{
		Request:  Request{Path: "/query"},
		Paginate: &Paginate{MaxPages: 2, MorePath: "has_more", CursorPath: "next_cursor"},
		Extract:  Extract{List: "results", Fields: map[string]Field{"name": {Path: "name"}}},
	})
	if _, err := source.Run(context.Background(), Input{}); err == nil || !strings.Contains(err.Error(), "paginate.maxPages=2") {
		t.Fatalf("err = %v, want maxPages failure", err)
	}
}

func fanoutParents(urls map[string]string) []Document {
	fields := map[string]any{"name": "Acme"}
	for k, v := range urls {
		fields[k] = v
	}
	return []Document{{Source: "parents", Name: "Acme", Fields: fields}}
}

func TestFanoutFilterRejectsWithoutEmptyDocument(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"keep": false})
	}))
	defer server.Close()
	source := newTestSource(t, server.URL, Definition{
		Needs:   []string{"parents"},
		Request: Request{Path: "/scrape", Body: map[string]any{"url": "{{ .url }}"}},
		Fanout:  &Fanout{From: "parents", URLs: []string{"pricing"}},
		Extract: Extract{Fields: map[string]Field{"keep": {Path: "keep"}}},
		Filter:  &Filter{Equals: map[string]any{"keep": true}},
	})
	documents, err := source.Run(context.Background(), Input{Deps: map[string][]Document{"parents": fanoutParents(map[string]string{"pricing": "https://a.test/p"})}})
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 0 || Count(documents).Pages != 0 {
		t.Fatalf("filtered documents = %+v", documents)
	}
}

func TestFanoutDedupesIdenticalRequests(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, map[string]any{"changeTracking": map[string]any{"changeStatus": "changed", "diff": map[string]any{"text": "+b"}}})
	}))
	defer server.Close()
	source := newTestSource(t, server.URL, Definition{
		Needs:   []string{"parents"},
		Request: Request{Path: "/scrape", Body: map[string]any{"url": "{{ .url }}", "tag": "watch"}},
		Fanout:  &Fanout{From: "parents", URLs: []string{"pricing", "changelog"}},
		Change:  &ChangeConfig{StatusPath: "changeTracking.changeStatus", DiffPath: "changeTracking.diff.text"},
	})
	parents := fanoutParents(map[string]string{"pricing": "https://a.test/same", "changelog": "https://a.test/same"})
	documents, err := source.Run(context.Background(), Input{Deps: map[string][]Document{"parents": parents}})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("requests = %d, want 1", calls.Load())
	}
	if len(documents) != 2 || documents[0].Status != "changed" || documents[1].Status != "changed" {
		t.Fatalf("documents = %+v", documents)
	}
	if documents[0].Label != "pricing" || documents[1].Label != "changelog" {
		t.Fatalf("labels = %q, %q", documents[0].Label, documents[1].Label)
	}
}

func TestFanoutAuthFailureFailsRun(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"invalid key"}`))
		}))
		source := newTestSource(t, server.URL, Definition{
			Needs:   []string{"parents"},
			Request: Request{Path: "/scrape", Body: map[string]any{"url": "{{ .url }}"}},
			Fanout:  &Fanout{From: "parents", URLs: []string{"pricing", "changelog"}},
			Retry:   Retry{Attempts: 1},
		})
		parents := fanoutParents(map[string]string{"pricing": "https://a.test/p", "changelog": "https://a.test/c"})
		_, err := source.Run(context.Background(), Input{Deps: map[string][]Document{"parents": parents}})
		server.Close()
		if !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", status)) {
			t.Fatalf("status %d: err = %v, want ErrAuth", status, err)
		}
	}
}

func TestRemovedWithoutDiffCountsAsChange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"changeTracking": map[string]any{"changeStatus": "removed"}})
	}))
	defer server.Close()
	source := newTestSource(t, server.URL, Definition{
		Request: Request{Path: "/scrape"},
		Change:  &ChangeConfig{StatusPath: "changeTracking.changeStatus", DiffPath: "changeTracking.diff.text"},
	})
	documents, err := source.Run(context.Background(), Input{})
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].Status != "removed" || documents[0].Error != "" {
		t.Fatalf("removed = %+v", documents)
	}
	if got := Count(documents); got.Changed != 1 || got.Failed != 0 {
		t.Fatalf("totals = %+v", got)
	}
}

func TestStepTimeoutKeepsFinishedFanoutPages(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["url"] == "https://a.test/slow" {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		writeJSON(w, map[string]any{"changeTracking": map[string]any{"changeStatus": "changed", "diff": map[string]any{"text": "+b"}}})
	}))
	defer server.Close()
	def := Definition{
		Name:       "test",
		Type:       TypeHTTPJSON,
		Connection: "origin",
		Needs:      []string{"parents"},
		Request:    Request{Path: "/scrape", Body: map[string]any{"url": "{{ .url }}"}},
		Fanout:     &Fanout{From: "parents", URLs: []string{"pricing", "changelog"}},
		Retry:      Retry{Attempts: 1},
		Change:     &ChangeConfig{StatusPath: "changeTracking.changeStatus", DiffPath: "changeTracking.diff.text"},
	}
	source, err := Build(def, Connection{Name: "origin", BaseURL: server.URL, AllowHTTP: true, AllowPrivate: true}, Options{Concurrency: 2, Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	parents := fanoutParents(map[string]string{"pricing": "https://a.test/fast", "changelog": "https://a.test/slow"})
	start := time.Now()
	documents, err := source.Run(context.Background(), Input{Deps: map[string][]Document{"parents": parents}})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("step ran %v past its 200ms timeout", elapsed)
	}
	if len(documents) != 2 || documents[0].Status != "changed" || documents[1].Status != "error" {
		t.Fatalf("documents = %+v", documents)
	}
}

func TestStepTimeoutFailsList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	def := Definition{Name: "test", Type: TypeHTTPJSON, Connection: "origin", Request: Request{Path: "/q"}, Retry: Retry{Attempts: 1}}
	source, err := Build(def, Connection{Name: "origin", BaseURL: server.URL, AllowHTTP: true, AllowPrivate: true}, Options{Concurrency: 1, Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Run(context.Background(), Input{}); err == nil {
		t.Fatal("list step outlived sources.timeout")
	}
}

func TestSSRFGuardStaysOnWithAllowHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
	}))
	defer server.Close()
	def := Definition{Name: "test", Type: TypeHTTPJSON, Connection: "origin", Request: Request{Path: "/q"}, Retry: Retry{Attempts: 1}}
	source, err := Build(def, Connection{Name: "origin", BaseURL: server.URL, AllowHTTP: true}, Options{Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = source.Run(context.Background(), Input{})
	if err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("loopback with allowHTTP only: err = %v, want refused", err)
	}
	if strings.Contains(err.Error(), server.URL) {
		t.Fatalf("transport error leaks the request URL: %v", err)
	}
	for _, raw := range []string{"64:ff9b::a00:1", "64:ff9b:1::1", "10.0.0.1", "::1", "100.64.0.1"} {
		if !blockedAddress(net.ParseIP(raw)) {
			t.Fatalf("%s is not blocked", raw)
		}
	}
	if blockedAddress(net.ParseIP("2606:4700::1")) {
		t.Fatal("public IPv6 is blocked")
	}
}

func TestSourceTemplateEnvAllowlist(t *testing.T) {
	t.Setenv("CONN_TOKEN", "conn")
	t.Setenv("SOURCE_ID", "db")
	t.Setenv("UNDECLARED", "nope")
	t.Setenv("OPENAI_API_KEY", "llm")
	t.Setenv("MAYFLY_API_TOKEN", "admin")
	conn := Connection{Name: "origin", BaseURL: "https://api.test", RequiredEnv: []string{"CONN_TOKEN"}}
	build := func(path string, deny ...string) error {
		def := Definition{Name: "test", Type: TypeHTTPJSON, Connection: "origin", RequiredEnv: []string{"SOURCE_ID"}, OptionalEnv: []string{"OPENAI_API_KEY"}, Request: Request{Path: path}}
		_, err := Build(def, conn, Options{DeniedEnv: deny})
		return err
	}
	if err := build(`/{{ env "SOURCE_ID" }}/{{ env "CONN_TOKEN" }}`); err != nil {
		t.Fatalf("declared variables: %v", err)
	}
	for _, tc := range []struct {
		path string
		deny []string
		want string
	}{
		{`/{{ env "UNDECLARED" }}`, nil, "not declared"},
		{`/{{ env "MAYFLY_API_TOKEN" }}`, nil, "reserved"},
		{`/{{ env "OPENAI_API_KEY" }}`, []string{"OPENAI_API_KEY"}, "control-plane secret"},
		{`/{{ env .name }}`, nil, "string literal"},
		{`/{{ "SOURCE_ID" | env }}`, nil, "string literal"},
		{`/{{ if true }}{{ with 1 }}{{ env "UNDECLARED" }}{{ end }}{{ end }}`, nil, "not declared"},
	} {
		if err := build(tc.path, tc.deny...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.path, err, tc.want)
		}
	}
}
