package queue

import (
	"encoding/json"
	"testing"
)

// TestLegacyJobRequiresEnvelopeFields pins the decode rule for the shape older
// builds wrote. A store encodes a job again when it is retried, so a legacy
// payload that the envelope reader would reject must be rejected on read; a
// payload without the required field would otherwise be rewritten into one
// this build cannot read.
func TestLegacyJobRequiresEnvelopeFields(t *testing.T) {
	cases := map[string]string{
		"session without prompt":      `{"id":"j1","kind":"session","options":{}}`,
		"missing kind without prompt": `{"id":"j1","options":{"Title":"t"}}`,
		"source without source":       `{"id":"s1","kind":"source","options":{}}`,
		"source without slot":         `{"id":"s1","kind":"source","options":{},"source":{"run_id":"r","schedule":"n","source":"s"}}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			var job Job
			if err := json.Unmarshal([]byte(payload), &job); err == nil {
				t.Fatalf("decoded %+v, want an error", job)
			}
		})
	}
}
