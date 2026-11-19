package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/artifact"
	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/project"
	"github.com/hayfever/mayfly/internal/queue"
	"github.com/hayfever/mayfly/internal/session"
	"github.com/hayfever/mayfly/internal/sources"
	"github.com/hayfever/mayfly/internal/state"
)

type memArtifacts struct {
	mu     sync.Mutex
	values map[string][]byte
}

func (m *memArtifacts) Put(_ context.Context, key string, r io.Reader, _ int64, _ string, _ string) (string, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.values == nil {
		m.values = map[string][]byte{}
	}
	m.values[key] = raw
	return key, nil
}

func (m *memArtifacts) Get(_ context.Context, key string) (io.ReadCloser, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, ok := m.values[key]
	if !ok {
		return nil, 0, artifact.ErrNotFound
	}
	return io.NopCloser(strings.NewReader(string(raw))), int64(len(raw)), nil
}

type fakeSessions struct {
	mu    sync.Mutex
	calls []session.CreateOptions
}

func (f *fakeSessions) Create(_ context.Context, opts session.CreateOptions) (*model.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, opts)
	return &model.Session{ID: fmt.Sprintf("session-%d", len(f.calls))}, nil
}

// providerStub serves the Notion query and the Firecrawl scrape.
type providerStub struct {
	mu      sync.Mutex
	scrapes int
}

func (p *providerStub) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/databases/db/query", func(w http.ResponseWriter, r *http.Request) {
		writeStubJSON(w, map[string]any{
			"has_more": false,
			"results": []any{
				map[string]any{
					"archived": false,
					"in_trash": false,
					"properties": map[string]any{
						"Name":      map[string]any{"type": "title", "title": []any{map[string]any{"plain_text": "Acme"}}},
						"Active":    map[string]any{"type": "checkbox", "checkbox": true},
						"Pricing":   map[string]any{"type": "url", "url": "https://acme.test/pricing"},
						"Changelog": map[string]any{"type": "url", "url": "https://acme.test/changelog"},
					},
				},
				map[string]any{
					"archived": true,
					"properties": map[string]any{
						"Name":    map[string]any{"type": "title", "title": []any{map[string]any{"plain_text": "Old"}}},
						"Active":  map[string]any{"type": "checkbox", "checkbox": true},
						"Pricing": map[string]any{"type": "url", "url": "https://old.test/pricing"},
					},
				},
			},
		})
	})
	mux.HandleFunc("POST /v2/scrape", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode scrape body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		p.scrapes++
		run := p.scrapes
		p.mu.Unlock()
		// Runs 1 and 2 establish the baseline. Run 3 changes one price. Later
		// runs report no change.
		status := "new"
		diff := ""
		switch {
		case run <= 2:
		case run <= 4:
			status = "same"
			if strings.HasSuffix(body.URL, "/pricing") {
				status = "changed"
				diff = "-Pro $10\n+Pro $12\n"
			}
		default:
			status = "same"
		}
		writeStubJSON(w, map[string]any{
			"success": true,
			"data": map[string]any{
				"changeTracking": map[string]any{
					"changeStatus": status,
					"diff":         map[string]any{"text": diff},
				},
			},
		})
	})
	return mux
}

func writeStubJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func testBundle(t *testing.T, notionURL, firecrawlURL string) *project.Bundle {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("NOTION_DATABASE_ID", "db")
	t.Setenv("NOTION_TOKEN", "notion-token")
	t.Setenv("FIRECRAWL_API_KEY", "firecrawl-token")
	t.Setenv("WATCH_CONTEXT", "we are cheaper")
	writeFile(t, dir, "agent.yaml", `schemaVersion: 1
name: test
agents:
  - name: watcher
    systemPrompt: watch
    schedules:
      - name: watch
        cron: "0 7 * * *"
        source: pages
        optionalEnv: [WATCH_CONTEXT]
        context: '{{ env "WATCH_CONTEXT" }}'
        title: 'Watch {{ .Date }}'
        prompt: |
          {{ if .Context }}Our position: {{ .Context }}
          {{ end }}{{ range .Changes }}{{ .Name }} {{ .Status }}
          {{ .Diff }}{{ end }}checked {{ .Totals.Pages }} pages: {{ .Totals.Changed }} changed, {{ .Totals.New }} new, {{ .Totals.Failed }} failed
`)
	writeFile(t, dir, "sources.yaml", fmt.Sprintf(`schemaVersion: 1
name: test-sources
connections:
  - name: notion
    baseURL: %s
    allowHTTP: true
    allowPrivate: true
    requiredEnv: [NOTION_TOKEN]
  - name: firecrawl
    baseURL: %s
    allowHTTP: true
    allowPrivate: true
    requiredEnv: [FIRECRAWL_API_KEY]
sources:
  - name: watch-list
    type: http.json
    connection: notion
    requiredEnv: [NOTION_DATABASE_ID]
    request:
      method: POST
      path: /v1/databases/{{ env "NOTION_DATABASE_ID" }}/query
      headers:
        Authorization: 'Bearer {{ env "NOTION_TOKEN" }}'
      body:
        page_size: 100
    paginate:
      morePath: has_more
      cursorPath: next_cursor
    extract:
      list: results
      fields:
        name:
          path: properties.Name.title[*].plain_text
        active:
          path: properties.Active.checkbox
        archived:
          path: archived
        trashed:
          path: in_trash
        pricing:
          paths:
            - properties.Pricing.url
            - properties.Pricing.rich_text[*].plain_text
        changelog:
          paths:
            - properties.Changelog.url
            - properties.Changelog.rich_text[*].plain_text
    filter:
      notTrue: [archived, trashed]
      equals:
        active: true
      notEmpty: [name]
      presentAny: [pricing, changelog]
  - name: pages
    type: http.json
    needs: [watch-list]
    connection: firecrawl
    fanout:
      from: watch-list
      urls: [pricing, changelog]
    request:
      method: POST
      path: /v2/scrape
      headers:
        Authorization: 'Bearer {{ env "FIRECRAWL_API_KEY" }}'
      body:
        url: "{{ .url }}"
        onlyMainContent: true
        formats:
          - markdown
          - type: changeTracking
            modes: [git-diff]
            tag: page-watch
    retry:
      attempts: 3
      delay: 1ms
    change:
      statusPath: data.changeTracking.changeStatus
      diffPath: data.changeTracking.diff.text
      errorPath: error
      maxLines: 40
      maxLineChars: 300
`, notionURL, firecrawlURL))
	bundle, err := project.Load([]string{dir}, project.Options{Concurrency: 2, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHandleWritesArtifactAndTriggersOnlyOnChange(t *testing.T) {
	providers := &providerStub{}
	server := httptest.NewServer(providers.handler(t))
	defer server.Close()
	bundle := testBundle(t, server.URL, server.URL)

	ctx := context.Background()
	queueStore, err := queue.New(ctx, config.QueueConfig{Backend: "memory", VisibilityTimeout: config.Duration(5 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	defer queueStore.Close()
	store := &memArtifacts{}
	sessions := &fakeSessions{}
	now := time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC)
	runner, err := New(bundle, Options{
		Store:    store,
		Queue:    queueStore,
		State:    state.NewMemory(),
		Sessions: sessions,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	runs := 0
	run := func() queue.Job {
		t.Helper()
		runs++
		job := queue.NewSourceJob(fmt.Sprintf("manual:watch:%d", runs), queue.SourceJob{
			Schedule: "watch",
			Source:   "pages",
			Slot:     now,
		}, now)
		if _, err := runner.Handle(ctx, job); err != nil {
			t.Fatal(err)
		}
		return job
	}

	// First run: every page is a baseline, so no session.
	first := run()
	if len(sessions.calls) != 0 {
		t.Fatalf("baseline created %d sessions", len(sessions.calls))
	}
	var artifact runArtifact
	raw := store.values[runKey("watch", first.ID)]
	if err := json.Unmarshal(raw, &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact.Totals.Pages != 2 || artifact.Totals.New != 2 {
		t.Fatalf("totals = %+v", artifact.Totals)
	}
	if artifact.Version != sources.FormatVersion {
		t.Fatalf("artifact version = %d, want %d", artifact.Version, sources.FormatVersion)
	}

	// Second run: pricing changed, changelog unchanged.
	run()
	if len(sessions.calls) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions.calls))
	}
	opts := sessions.calls[0]
	if opts.Title != "Watch 2026-11-13" {
		t.Fatalf("title = %q", opts.Title)
	}
	if opts.Agent != "watcher" || opts.TTL != 30*time.Minute {
		t.Fatalf("agent/ttl = %q/%s", opts.Agent, opts.TTL)
	}
	if len(opts.Artifacts) != 1 || !strings.HasPrefix(opts.Artifacts[0].Key, "runs/watch/") {
		t.Fatalf("artifacts = %+v", opts.Artifacts)
	}
	if !strings.HasPrefix(opts.Prompt, untrustedNotice) {
		t.Fatalf("prompt does not start with the untrusted notice: %q", opts.Prompt)
	}
	for _, want := range []string{
		"Our position: we are cheaper",
		"Acme changed",
		"-Pro $10",
		"+Pro $12",
		"checked 2 pages: 1 changed, 0 new, 0 failed",
	} {
		if !strings.Contains(opts.Prompt, want) {
			t.Fatalf("prompt is missing %q:\n%s", want, opts.Prompt)
		}
	}
	if strings.Contains(opts.Prompt, "Old") {
		t.Fatalf("archived page reached the prompt:\n%s", opts.Prompt)
	}

	// Third run: no change, no session.
	run()
	if len(sessions.calls) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions.calls))
	}
}

func TestSchedulerCatchUpEnqueuesOneSlot(t *testing.T) {
	providers := &providerStub{}
	server := httptest.NewServer(providers.handler(t))
	defer server.Close()
	bundle := testBundle(t, server.URL, server.URL)

	ctx := context.Background()
	queueStore, err := queue.New(ctx, config.QueueConfig{Backend: "memory", VisibilityTimeout: config.Duration(5 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	defer queueStore.Close()
	now := time.Date(2026, 11, 13, 8, 30, 0, 0, time.UTC)
	watermarks := state.NewMemory()
	runner, err := New(bundle, Options{
		Store:    &memArtifacts{},
		Queue:    queueStore,
		State:    watermarks,
		Sessions: &fakeSessions{},
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	// A first start records the most recent slot and waits for the next tick.
	runner.sched.catchUp(ctx)
	slot, ok, err := watermarks.LastSlot(ctx, "watch")
	if err != nil || !ok {
		t.Fatalf("watermark = %v/%v/%v", slot, ok, err)
	}
	if want := time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC); !slot.Equal(want) {
		t.Fatalf("watermark = %s, want %s", slot, want)
	}
	if stats, _ := queueStore.Stats(ctx); stats.Pending != 0 {
		t.Fatalf("first start enqueued %d jobs", stats.Pending)
	}

	// A two-day gap replays only the most recent slot.
	if err := watermarks.SetLastSlot(ctx, "watch", time.Date(2026, 11, 11, 7, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	runner.sched.catchUp(ctx)
	leases, err := queueStore.Claim(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 {
		t.Fatalf("leases = %d, want 1", len(leases))
	}
	if got := leases[0].Job.Source.Slot.UTC(); !got.Equal(time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("catch-up slot = %s", got)
	}

	// The same slot is idempotent: nothing new becomes claimable.
	runner.sched.catchUp(ctx)
	again, err := queueStore.Claim(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("catch-up repeated the slot: %d jobs", len(again))
	}
}

func TestMostRecentSlotIsUTC(t *testing.T) {
	slot, ok := mostRecentSlot("0 7 * * *", time.Date(2026, 11, 13, 23, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("no slot")
	}
	if slot.Location() != time.UTC {
		t.Fatalf("slot location = %s", slot.Location())
	}
	if want := time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC); !slot.Equal(want) {
		t.Fatalf("slot = %s, want %s", slot, want)
	}
}

func TestBudgetDiffTruncatesPerDocumentAndOverall(t *testing.T) {
	big := strings.Repeat("a", 20<<10)
	budget := promptChangesBytes
	one := budgetDiff(big, &budget)
	if !strings.HasPrefix(one, strings.Repeat("a", promptDiffBytes)) {
		t.Fatal("diff was not truncated at the per-document cap")
	}
	if !strings.HasSuffix(one, truncatedMarker) {
		t.Fatal("truncation marker missing")
	}
	if len(one) > promptDiffBytes+len("\n"+truncatedMarker) {
		t.Fatalf("diff = %d bytes", len(one))
	}

	budget = 100
	first := budgetDiff(strings.Repeat("b", 80), &budget)
	second := budgetDiff(strings.Repeat("c", 80), &budget)
	third := budgetDiff(strings.Repeat("d", 80), &budget)
	if first != strings.Repeat("b", 80) {
		t.Fatalf("first = %q", first)
	}
	if !strings.HasPrefix(second, strings.Repeat("c", 20)) || !strings.HasSuffix(second, truncatedMarker) {
		t.Fatalf("second = %q", second)
	}
	if third != omittedMarker {
		t.Fatalf("third = %q", third)
	}
	if budget != 0 {
		t.Fatalf("budget = %d", budget)
	}
	if got := budgetDiff("", &budget); got != "" {
		t.Fatalf("empty diff = %q", got)
	}
}

// TestLoadArtifactFormatVersions covers an artifact written before the stamp
// existed and one written by a newer build.
func TestLoadArtifactFormatVersions(t *testing.T) {
	ctx := context.Background()
	store := &memArtifacts{values: map[string][]byte{}}
	runner := &Runner{store: store, maxBytes: 1 << 20}
	key := runKey("watch", "run-1")
	write := func(version int) {
		t.Helper()
		payload := runArtifact{
			Version:   version,
			RunID:     "run-1",
			Schedule:  "watch",
			Source:    "pages",
			Documents: []sources.Document{{Source: "pages", Name: "a", Status: "new"}},
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Put(ctx, key, bytes.NewReader(raw), int64(len(raw)), "", "application/json"); err != nil {
			t.Fatal(err)
		}
	}

	write(0)
	result, _, _, found, err := runner.loadArtifact(ctx, key, "watch", "run-1")
	if err != nil || !found {
		t.Fatalf("legacy artifact: found=%v err=%v", found, err)
	}
	if len(result.Documents) != 1 {
		t.Fatalf("documents = %+v", result.Documents)
	}

	write(sources.FormatVersion)
	if _, _, _, _, err := runner.loadArtifact(ctx, key, "watch", "run-1"); err != nil {
		t.Fatalf("current artifact: %v", err)
	}

	write(sources.FormatVersion + 1)
	if _, _, _, _, err := runner.loadArtifact(ctx, key, "watch", "run-1"); err == nil || !strings.Contains(err.Error(), "format version") {
		t.Fatalf("newer artifact error = %v, want a format version error", err)
	}
}
