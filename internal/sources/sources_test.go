package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLookupPath(t *testing.T) {
	var document any
	if err := json.Unmarshal([]byte(`{
		"results": [{"url": "https://a.test/1", "rich_text": [{"plain_text": "one"}, {"plain_text": "two"}]},
		            {"url": "https://a.test/2"}],
		"properties": {"Some Name": {"checkbox": true}}
	}`), &document); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path string
		want string
		ok   bool
	}{
		{"results[0].url", "https://a.test/1", true},
		{"results[*].url", "https://a.test/1https://a.test/2", true},
		{"results[*].url[0]", "", false},
		{"results[0].rich_text[*].plain_text", "onetwo", true},
		{"properties.Some Name.checkbox", "true", true},
		{"properties.some_name.checkbox", "true", true},
		{"properties.some-name.checkbox", "true", true},
		{"properties.Missing", "", false},
		{"results[9].url", "", false},
	}
	for _, tc := range cases {
		segments, err := parsePath(tc.path)
		if err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		value, ok := lookupSegments(document, segments)
		if ok != tc.ok {
			t.Fatalf("%s: ok = %v, want %v", tc.path, ok, tc.ok)
		}
		if ok && Text(value) != tc.want {
			t.Fatalf("%s = %q, want %q", tc.path, Text(value), tc.want)
		}
	}
	if _, err := parsePath("a..b"); err == nil {
		t.Fatal("empty segment accepted")
	}
	if _, err := parsePath("a[x]"); err == nil {
		t.Fatal("invalid index accepted")
	}
}

func TestFilterMatch(t *testing.T) {
	fields := map[string]any{
		"name":     "Acme",
		"active":   true,
		"archived": false,
		"pricing":  "https://acme.test/pricing",
		"blank":    "   ",
	}
	cases := []struct {
		name   string
		filter *Filter
		want   bool
	}{
		{"nil", nil, true},
		{"equals", &Filter{Equals: map[string]any{"active": true}}, true},
		{"equals mismatch", &Filter{Equals: map[string]any{"active": "no"}}, false},
		{"equals missing", &Filter{Equals: map[string]any{"missing": true}}, false},
		{"notTrue passes false", &Filter{NotTrue: []string{"archived"}}, true},
		{"notTrue rejects true", &Filter{NotTrue: []string{"active"}}, false},
		{"notTrue ignores missing", &Filter{NotTrue: []string{"missing"}}, true},
		{"notEmpty rejects blank", &Filter{NotEmpty: []string{"blank"}}, false},
		{"notEmpty rejects missing", &Filter{NotEmpty: []string{"missing"}}, false},
		{"presentAny", &Filter{PresentAny: []string{"missing", "pricing"}}, true},
		{"presentAny none", &Filter{PresentAny: []string{"missing"}}, false},
	}
	for _, tc := range cases {
		if got := tc.filter.match(fields); got != tc.want {
			t.Fatalf("%s: match = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTruncateDiff(t *testing.T) {
	lines := make([]string, 45)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i)
	}
	lines[0] = strings.Repeat("x", 400)
	got := truncateDiff(strings.Join(lines, "\n"), 40, 300)
	parts := strings.Split(got, "\n")
	if len(parts) != 40 {
		t.Fatalf("lines = %d, want 40", len(parts))
	}
	if len(parts[0]) != 300 {
		t.Fatalf("first line = %d chars, want 300", len(parts[0]))
	}
}

func newTestSource(t *testing.T, url string, def Definition) Source {
	t.Helper()
	def.Name = "test"
	def.Type = TypeHTTPJSON
	def.Connection = "origin"
	source, err := Build(def, Connection{Name: "origin", BaseURL: url, AllowHTTP: true, AllowPrivate: true}, Options{Concurrency: 2, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func TestPagination(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("body: %v", err)
		}
		page := calls.Add(1)
		cursor := ""
		if page > 1 {
			cursor, _ = body["start_cursor"].(string)
			if cursor != "next" {
				t.Errorf("page %d cursor = %q", page, cursor)
			}
		}
		writeJSON(w, map[string]any{
			"has_more":    page == 1,
			"next_cursor": "next",
			"results":     []any{map[string]any{"url": fmt.Sprintf("https://a.test/%d", page)}},
		})
	}))
	defer server.Close()

	source := newTestSource(t, server.URL, Definition{
		Request: Request{Method: http.MethodPost, Path: "/query", Body: map[string]any{"page_size": 1}},
		Paginate: &Paginate{
			MaxPages:   3,
			MorePath:   "has_more",
			CursorPath: "next_cursor",
			Body:       map[string]any{"start_cursor": "{{ .cursor }}"},
		},
		Extract: Extract{List: "results", Fields: map[string]Field{"url": {Path: "url"}}},
	})
	documents, err := source.Run(context.Background(), Input{})
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 2 {
		t.Fatalf("documents = %d, want 2", len(documents))
	}
	if documents[1].URL != "https://a.test/2" {
		t.Fatalf("second url = %q", documents[1].URL)
	}
	if calls.Load() != 2 {
		t.Fatalf("requests = %d, want 2", calls.Load())
	}
}

func TestFanout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("body: %v", err)
		}
		switch body["url"] {
		case "https://a.test/pricing":
			writeJSON(w, map[string]any{"ok": true})
		default:
			t.Errorf("unexpected url %v", body["url"])
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer server.Close()

	parents := []Document{
		{Source: "parents", Name: "Acme", Fields: map[string]any{
			"name":      "Acme",
			"pricing":   "https://a.test/pricing",
			"changelog": "mailto:watch@a.test",
		}},
	}
	source := newTestSource(t, server.URL, Definition{
		Needs:   []string{"parents"},
		Request: Request{Method: http.MethodPost, Path: "/scrape", Body: map[string]any{"url": "{{ .url }}"}},
		Fanout:  &Fanout{From: "parents", URLs: []string{"pricing", "changelog"}},
	})
	documents, err := source.Run(context.Background(), Input{Deps: map[string][]Document{"parents": parents}})
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 2 {
		t.Fatalf("documents = %d, want 2", len(documents))
	}
	if documents[0].Label != "pricing" || documents[0].URL != "https://a.test/pricing" {
		t.Fatalf("pricing document = %+v", documents[0])
	}
	if documents[0].Name != "Acme" || documents[0].Fields["name"] != "Acme" {
		t.Fatalf("parent fields not copied: %+v", documents[0])
	}
	if documents[1].Status != "error" || !strings.Contains(documents[1].Error, "not an HTTP URL") {
		t.Fatalf("non-HTTP url did not become an error document: %+v", documents[1])
	}
	if got := Count(documents); got.Failed != 1 || got.Pages != 2 {
		t.Fatalf("totals = %+v", got)
	}
}

func TestRetryThenSuccess(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusTooManyRequests)
			writeJSON(w, map[string]any{"error": "slow down"})
		case 2:
			w.WriteHeader(http.StatusBadGateway)
		default:
			writeJSON(w, map[string]any{"items": []any{map[string]any{"url": "https://a.test/1"}}})
		}
	}))
	defer server.Close()

	source := newTestSource(t, server.URL, Definition{
		Request: Request{Method: http.MethodGet, Path: "/items"},
		Retry:   Retry{Attempts: 3, Delay: "1ms"},
		Extract: Extract{List: "items", Fields: map[string]Field{"url": {Path: "url"}}},
	})
	documents, err := source.Run(context.Background(), Input{})
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || calls.Load() != 3 {
		t.Fatalf("documents = %d calls = %d", len(documents), calls.Load())
	}
}

func TestChangeStatus(t *testing.T) {
	statuses := map[string]string{
		"https://a.test/changed": `{"changeTracking":{"changeStatus":"changed","diff":{"text":"-a\n+b"}}}`,
		"https://a.test/empty":   `{"changeTracking":{"changeStatus":"changed","diff":{"text":""}}}`,
		"https://a.test/removed": `{"changeTracking":{"changeStatus":"removed","diff":{"text":"-a"}}}`,
		"https://a.test/unknown": `{"changeTracking":{"changeStatus":"weird"}}`,
		"https://a.test/failed":  `{"changeTracking":{"changeStatus":"same"},"error":"provider exploded"}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("body: %v", err)
		}
		target, _ := body["url"].(string)
		raw, ok := statuses[target]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": "unknown target"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(raw))
	}))
	defer server.Close()

	parents := []Document{{Source: "parents", Name: "Acme", Fields: map[string]any{
		"pricing": "https://a.test/changed",
		"other":   "https://a.test/empty",
	}}}
	source := newTestSource(t, server.URL, Definition{
		Needs:   []string{"parents"},
		Request: Request{Method: http.MethodPost, Path: "/scrape", Body: map[string]any{"url": "{{ .url }}"}},
		Fanout:  &Fanout{From: "parents", URLs: []string{"pricing", "other"}},
		Change: &ChangeConfig{
			StatusPath: "changeTracking.changeStatus",
			DiffPath:   "changeTracking.diff.text",
			ErrorPath:  "error",
		},
	})
	documents, err := source.Run(context.Background(), Input{Deps: map[string][]Document{"parents": parents}})
	if err != nil {
		t.Fatal(err)
	}
	if documents[0].Status != "changed" || documents[0].Diff != "-a\n+b" {
		t.Fatalf("changed = %+v", documents[0])
	}
	if documents[1].Status != "error" || documents[1].Error != "change status is changed but diff is empty" {
		t.Fatalf("empty diff = %+v", documents[1])
	}
	if got := Count(documents); got.Changed != 1 || got.Failed != 1 {
		t.Fatalf("totals = %+v", got)
	}

	for target, want := range map[string]string{
		"https://a.test/removed": "removed",
		"https://a.test/unknown": "error",
		"https://a.test/failed":  "same",
	} {
		one := newTestSource(t, server.URL, Definition{
			Request: Request{Method: http.MethodPost, Path: "/scrape", Body: map[string]any{"url": target}},
			Change: &ChangeConfig{
				StatusPath: "changeTracking.changeStatus",
				DiffPath:   "changeTracking.diff.text",
				ErrorPath:  "error",
			},
		})
		out, err := one.Run(context.Background(), Input{})
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		if len(out) != 1 || out[0].Status != want {
			t.Fatalf("%s: %+v", target, out)
		}
		if target == "https://a.test/unknown" && out[0].Error != `unknown change status "weird"` {
			t.Fatalf("unknown status error = %q", out[0].Error)
		}
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
