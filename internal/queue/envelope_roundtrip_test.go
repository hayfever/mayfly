package queue

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestJobEnvelopeRewriteIsStable pins the property a store depends on: a
// stored job decoded from bytes encodes to the same bytes when it is decoded
// and encoded again. Retry rewrites a buried job from its decoded form, so an
// unstable encode would change the stored payload each time it is touched.
func TestJobEnvelopeRewriteIsStable(t *testing.T) {
	payloads := map[string]string{
		"v1 no trigger name": `{"version":1,"kind":"session.create","id":"j1","prompt":"hi","trigger":{"kind":"api"}}`,
		"v1 named trigger":   `{"version":1,"kind":"session.create","id":"j1","prompt":"hi","trigger":{"kind":"api","name":"http"}}`,
		"v1 source":          `{"version":1,"kind":"source.run","id":"s1","trigger":{"kind":"schedule"},"source":{"run_id":"r1","schedule":"nightly","source":"repo","slot":"2026-11-19T02:00:00Z"}}`,
		"legacy no trigger":  `{"id":"legacy","kind":"session","options":{"Prompt":"hi"}}`,
		"legacy trigger":     `{"id":"legacy","kind":"session","delivery":"d","trigger":"github","options":{"Prompt":"hi","Trigger":{"kind":"github","name":"github","event":"push","delivery":"d","job_id":"legacy"}}}`,
	}
	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			var job Job
			if err := json.Unmarshal([]byte(payload), &job); err != nil {
				t.Fatalf("decode: %v", err)
			}
			first, err := json.Marshal(job)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			var again Job
			if err := json.Unmarshal(first, &again); err != nil {
				t.Fatalf("decode encoded job: %v", err)
			}
			second, err := json.Marshal(again)
			if err != nil {
				t.Fatalf("encode again: %v", err)
			}
			if !bytes.Equal(first, second) {
				t.Fatalf("rewrite changed the payload:\nfirst:  %s\nsecond: %s", first, second)
			}
			// Decoding still derives the job trigger from the kind when the
			// envelope names no trigger, so equivalence is the encoded form,
			// not the in-memory string.
		})
	}
}
