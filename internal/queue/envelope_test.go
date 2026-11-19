package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/session"
)

func fullOptions() session.CreateOptions {
	return session.CreateOptions{
		Prompt:     "triage the alert",
		Title:      "Alert",
		Model:      "gpt-5",
		TTL:        45 * time.Minute,
		Skills:     []string{"reporting"},
		MCPServers: []string{"echo"},
		Agent:      "triage",
		Trigger: model.TriggerInfo{
			Kind:  "github",
			Name:  "repo-hooks",
			Event: "pull_request",
			Actor: "alice",
		},
		Artifacts: []session.ArtifactRef{{
			Key:         "runs/a.json",
			SHA256:      strings.Repeat("a", 64),
			Bytes:       128,
			ContentType: "application/json",
		}},
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	job := NewJob("repo-hooks", "delivery-1", fullOptions(), now)
	job.Metadata = map[string]string{"team": "platform"}
	job.Attempts = 2

	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"version":1`, `"kind":"session.create"`, `"idempotency_key":"delivery-1"`, `"created_at":"2026-11-18T09:00:00Z"`, `"prompt":"triage the alert"`, `"trigger":{"kind":"github"`} {
		if !bytes.Contains(raw, []byte(field)) {
			t.Fatalf("envelope %s does not contain %s", raw, field)
		}
	}
	if bytes.Contains(raw, []byte(`"options"`)) {
		t.Fatalf("envelope still carries the legacy options object: %s", raw)
	}

	var got Job
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, job) {
		t.Fatalf("round trip =\n%+v\nwant\n%+v", got, job)
	}

	again, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, raw) {
		t.Fatalf("second encode =\n%s\nwant\n%s", again, raw)
	}
}

func TestEnvelopeSourceRoundTrip(t *testing.T) {
	slot := time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC)
	job := NewSourceJob("schedule:watch:2026-11-13T07:00:00Z", SourceJob{
		Schedule: "watch",
		Source:   "pages",
		Slot:     slot,
	}, slot)

	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	var got Job
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, job) {
		t.Fatalf("round trip =\n%+v\nwant\n%+v", got, job)
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.Kind != KindSource || env.Source == nil || env.Source.Schedule != "watch" {
		t.Fatalf("source envelope = %+v", env)
	}
	if env.Trigger.Kind != "schedule" || env.Trigger.Name != "watch" {
		t.Fatalf("trigger = %+v", env.Trigger)
	}
}

func TestEnvelopeIgnoresUnknownFields(t *testing.T) {
	job := NewJob("generic", "delivery-1", fullOptions(), time.Now())
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["future_field"] = map[string]any{"nested": true}
	doc["session"].(map[string]any)["future_option"] = "kept out of the way"
	doc["trigger"].(map[string]any)["future_trigger_field"] = 1
	raw, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	var got Job
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unknown fields rejected: %v", err)
	}
	if got.ID != job.ID || got.Options.Prompt != job.Options.Prompt || got.Options.Agent != job.Options.Agent {
		t.Fatalf("known fields lost: %+v", got)
	}
}

func TestEnvelopeRejectsUnknownVersion(t *testing.T) {
	raw := []byte(`{"version":2,"kind":"session.create","created_at":"2026-11-18T09:00:00Z","prompt":"hi","trigger":{"kind":"generic"}}`)
	var job Job
	err := json.Unmarshal(raw, &job)
	var versionErr *UnsupportedVersionError
	if !errors.As(err, &versionErr) {
		t.Fatalf("error = %v, want UnsupportedVersionError", err)
	}
	if versionErr.Version != 2 || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("version error = %v", err)
	}

	if err := json.Unmarshal([]byte(`{"version":0,"kind":"session.create","prompt":"hi","trigger":{"kind":"generic"}}`), &job); err == nil {
		t.Fatal("version 0 was accepted")
	}
	if err := json.Unmarshal([]byte(`{"kind":"session.create","prompt":"hi","trigger":{"kind":"generic"}}`), &job); err == nil {
		t.Fatal("an envelope without a version was accepted")
	}
}

func TestEnvelopeRejectsUnknownKindAndMissingTrigger(t *testing.T) {
	cases := map[string]string{
		"unknown kind":    `{"version":1,"kind":"session.delete","created_at":"2026-11-18T09:00:00Z","prompt":"hi","trigger":{"kind":"generic"}}`,
		"missing kind":    `{"version":1,"kind":"","created_at":"2026-11-18T09:00:00Z","prompt":"hi","trigger":{"kind":"generic"}}`,
		"missing trigger": `{"version":1,"kind":"session.create","created_at":"2026-11-18T09:00:00Z","prompt":"hi"}`,
		"missing prompt":  `{"version":1,"kind":"session.create","created_at":"2026-11-18T09:00:00Z","trigger":{"kind":"generic"}}`,
		"empty prompt":    `{"version":1,"kind":"session.create","created_at":"2026-11-18T09:00:00Z","prompt":"","trigger":{"kind":"generic"}}`,
		"missing source":  `{"version":1,"kind":"source.run","created_at":"2026-11-18T09:00:00Z","trigger":{"kind":"schedule"}}`,
		"incomplete source": `{"version":1,"kind":"source.run","created_at":"2026-11-18T09:00:00Z","trigger":{"kind":"schedule"},` +
			`"source":{"schedule":"watch"}}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			var job Job
			if err := json.Unmarshal([]byte(raw), &job); err == nil {
				t.Fatalf("accepted %s", raw)
			}
		})
	}
}

func TestEnvelopeDecodesLegacySession(t *testing.T) {
	// The exact shape v0.1.0 wrote: capitalized options, a numeric TTL, and a
	// kind before "session.create" existed.
	raw := []byte(`{
		"id":"d-legacy",
		"kind":"session",
		"delivery":"delivery-1",
		"trigger":"repo-hooks",
		"options":{
			"Prompt":"legacy prompt",
			"Title":"Legacy",
			"Model":"gpt-5",
			"TTL":900000000000,
			"Skills":["reporting"],
			"MCPServers":["echo"],
			"Agent":"triage",
			"Trigger":{"kind":"github","name":"repo-hooks","event":"pull_request","delivery":"delivery-1","job_id":"d-legacy"},
			"Artifacts":[{"Key":"runs/a.json","SHA256":"` + strings.Repeat("a", 64) + `","Bytes":128,"ContentType":"application/json"}],
			"IdempotencyKey":""
		},
		"source":null,
		"accepted_at":"2026-01-02T03:04:05Z",
		"attempts":1
	}`)
	var job Job
	if err := json.Unmarshal(raw, &job); err != nil {
		t.Fatalf("legacy decode: %v", err)
	}
	if job.ID != "d-legacy" || job.JobKind() != KindSession || job.IdempotencyKey != "delivery-1" {
		t.Fatalf("identity = %+v", job)
	}
	if job.Options.Prompt != "legacy prompt" || job.Options.TTL != 15*time.Minute ||
		job.Options.Agent != "triage" || len(job.Options.Artifacts) != 1 {
		t.Fatalf("options = %+v", job.Options)
	}
	if job.Options.Trigger.JobID != "d-legacy" || job.Options.Trigger.Delivery != "delivery-1" {
		t.Fatalf("trigger = %+v", job.Options.Trigger)
	}
	if job.Options.IdempotencyKey != "d-legacy" {
		t.Fatalf("idempotency key = %q, want the job id", job.Options.IdempotencyKey)
	}
	if job.Attempts != 1 || !job.AcceptedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("state = %+v", job)
	}

	// Re-encoding migrates the record to the v1 envelope.
	upgraded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(upgraded, []byte(`"version":1`)) || bytes.Contains(upgraded, []byte(`"options"`)) {
		t.Fatalf("upgraded record = %s", upgraded)
	}
}

func TestEnvelopeDecodesLegacySourceAndUnknownKinds(t *testing.T) {
	raw := []byte(`{
		"id":"s-legacy",
		"kind":"source",
		"delivery":"schedule:watch:2026-11-13T07:00:00Z",
		"trigger":"schedule",
		"options":{"Prompt":""},
		"source":{"run_id":"","schedule":"watch","source":"pages","slot":"2026-11-13T07:00:00Z"},
		"accepted_at":"2026-11-13T07:00:00Z",
		"attempts":0
	}`)
	var job Job
	if err := json.Unmarshal(raw, &job); err != nil {
		t.Fatalf("legacy source decode: %v", err)
	}
	if job.JobKind() != KindSource || job.Source == nil || job.Source.Schedule != "watch" {
		t.Fatalf("source job = %+v", job)
	}

	var bad Job
	if err := json.Unmarshal([]byte(`{"id":"x","kind":"future","options":{},"accepted_at":"2026-11-13T07:00:00Z"}`), &bad); err == nil {
		t.Fatal("legacy payload with an unknown kind was accepted")
	}
}

func TestEnvelopeDerivesIDFromIdempotencyKey(t *testing.T) {
	raw := []byte(`{"version":1,"kind":"session.create","idempotency_key":"k","created_at":"2026-11-18T09:00:00Z","prompt":"hi","trigger":{"kind":"generic","name":"hooks"}}`)
	var first, second Job
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &second); err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || first.ID != second.ID {
		t.Fatalf("derived ids = %q and %q", first.ID, second.ID)
	}
	if first.IdempotencyKey != "k" || first.Options.Trigger.JobID != first.ID {
		t.Fatalf("derived identity = %+v", first)
	}

	other := []byte(`{"version":1,"kind":"session.create","idempotency_key":"k","created_at":"2026-11-18T09:00:00Z","prompt":"hi","trigger":{"kind":"generic","name":"other"}}`)
	var third Job
	if err := json.Unmarshal(other, &third); err != nil {
		t.Fatal(err)
	}
	if third.ID == first.ID {
		t.Fatalf("keys with different trigger scopes shared id %q", third.ID)
	}

	random := []byte(`{"version":1,"kind":"session.create","created_at":"2026-11-18T09:00:00Z","prompt":"hi","trigger":{"kind":"generic"}}`)
	var a, b Job
	if err := json.Unmarshal(random, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(random, &b); err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.ID == b.ID {
		t.Fatalf("jobs without a key got ids %q and %q", a.ID, b.ID)
	}
}

func TestProducersValidateAgainstEnvelopeSchema(t *testing.T) {
	schema := compileJobSchema(t)
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)

	sessionJob := NewJob("repo-hooks", "delivery-1", fullOptions(), now)
	sourceJob := NewSourceJob("schedule:watch:2026-11-13T07:00:00Z", SourceJob{
		Schedule: "watch",
		Source:   "pages",
		Slot:     now,
	}, now)
	plainJob := NewJob("generic", "", session.CreateOptions{Prompt: "hello"}, now)

	for name, job := range map[string]Job{
		"webhook session":     sessionJob,
		"scheduled source":    sourceJob,
		"session without key": plainJob,
	} {
		t.Run(name, func(t *testing.T) {
			validateJobJSON(t, schema, job)
		})
	}
}

func TestStoreDedupesByIdempotencyKey(t *testing.T) {
	store, err := New(context.Background(), testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)

	first := NewJob("generic", "same-key", session.CreateOptions{Prompt: "first"}, now)
	second := NewJob("generic", "same-key", session.CreateOptions{Prompt: "different prompt"}, now.Add(time.Minute))
	if first.ID != second.ID {
		t.Fatalf("same key produced ids %q and %q", first.ID, second.ID)
	}
	if err := store.Accept(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.Accept(ctx, second); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("second accept = %v, want ErrDuplicate", err)
	}

	other := NewJob("other-trigger", "same-key", session.CreateOptions{Prompt: "first"}, now)
	if other.ID == first.ID {
		t.Fatalf("same key in another trigger scope produced id %q", other.ID)
	}
	if err := store.Accept(ctx, other); err != nil {
		t.Fatalf("scoped key was deduped: %v", err)
	}
}

func compileJobSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	path := filepath.Join("..", "..", "api", "job.v1.schema.json")
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	doc, err := jsonschema.UnmarshalJSON(file)
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("job.v1.schema.json", doc); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("job.v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func validateJobJSON(t *testing.T, schema *jsonschema.Schema, job Job) {
	t.Helper()
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(instance); err != nil {
		t.Fatalf("producer output does not match api/job.v1.schema.json: %v\n%s", err, raw)
	}
}

// TestEnvelopeDecodesPreKindSession covers a job written before jobs carried a
// kind. The empty kind means a session job.
func TestEnvelopeDecodesPreKindSession(t *testing.T) {
	raw := []byte(`{"id":"d-old","delivery":"delivery-old","trigger":"generic","options":{"Prompt":"old prompt"},"accepted_at":"2026-01-02T03:04:05Z"}`)
	var job Job
	if err := json.Unmarshal(raw, &job); err != nil {
		t.Fatalf("legacy decode: %v", err)
	}
	if job.JobKind() != KindSession || job.Options.Prompt != "old prompt" || job.ID != "d-old" {
		t.Fatalf("pre-kind job = %+v", job)
	}
}
