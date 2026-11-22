package workflow

import (
	"context"
	"strings"
	"testing"
)

// FuzzRunArtifactDecode feeds arbitrary stored bytes to the resume path. A
// torn, wrong-run, unknown-field, or newer-version artifact must return an
// error, never a panic, so a queue retry cannot be turned into a crash by a
// damaged artifact.
func FuzzRunArtifactDecode(f *testing.F) {
	f.Add([]byte(`{"version":1,"run_id":"run","schedule":"watch","source":"pages","slot":"2026-11-13T07:00:00Z","generated_at":"2026-11-13T07:00:00Z","totals":{},"documents":[]}`))
	f.Add([]byte(`{"run_id":"run","schedule":"watch","source":"pages","slot":"2026-11-13T07:00:00Z","generated_at":"2026-11-13T07:00:00Z","totals":{},"documents":[{"fields":{"a":1}}]}`))
	f.Add([]byte(`{"run_id":"run","schedule":"watch","documents":[]} trailing`))
	f.Add([]byte(`{"version":2,"run_id":"run","schedule":"watch","documents":[]}`))
	f.Add([]byte(`{"version":1,"run_id":"run","schedule":"watch","documents":[],"unknown":true}`))
	f.Add([]byte(`{"run_id":`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 1<<16 {
			raw = raw[:1<<16]
		}
		store := &memArtifacts{values: map[string][]byte{"run/key": raw}}
		runner := &Runner{store: store, maxBytes: 1 << 16}
		result, _, _, found, err := runner.loadArtifact(context.Background(), "run/key", "watch", "run")
		if err != nil {
			if strings.TrimSpace(err.Error()) == "" {
				t.Fatal("loadArtifact returned an empty error")
			}
			return
		}
		if !found {
			t.Fatal("loadArtifact returned no result and no error")
		}
		if result.Documents == nil {
			t.Fatal("decoded artifact has no documents slice")
		}
	})
}
