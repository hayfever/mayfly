package queue

import (
	"bytes"
	"encoding/json"
	"strconv"
	"testing"
)

// FuzzJobJSON pins the two promises of the job codec: arbitrary bytes never
// panic, and a job that decodes re-encodes to an envelope that decodes to the
// same bytes. A stored job is read again after a restart, so a lossy encode is
// a data-loss bug even when the first read succeeds.
func FuzzJobJSON(f *testing.F) {
	f.Add([]byte(`{"version":1,"kind":"session.create","id":"j1","prompt":"hi","trigger":{"kind":"api","name":"http"}}`))
	f.Add([]byte(`{"version":2,"kind":"session.create","id":"j1","prompt":"hi","trigger":{"kind":"api","name":"http"}}`))
	f.Add([]byte(`{"version":1,"kind":"source.run","id":"s1","trigger":{"kind":"schedule","name":"nightly"},"source":{"run_id":"r1","schedule":"nightly","source":"repo","slot":"2026-11-19T02:00:00Z"}}`))
	f.Add([]byte(`{"id":"legacy","kind":"session","options":{"Prompt":"hi"}}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var job Job
		if err := json.Unmarshal(data, &job); err != nil {
			return
		}
		if job.ID == "" {
			t.Fatalf("a decoded job must have an id: %s", data)
		}
		first, err := json.Marshal(job)
		if err != nil {
			t.Fatalf("encode decoded job: %v\npayload: %s", err, data)
		}
		var again Job
		if err := json.Unmarshal(first, &again); err != nil {
			t.Fatalf("decode encoded job: %v\nencoded: %s", err, first)
		}
		second, err := json.Marshal(again)
		if err != nil {
			t.Fatalf("encode job a second time: %v", err)
		}
		if !bytes.Equal(first, second) {
			t.Fatalf("job encoding is not stable:\nfirst:  %s\nsecond: %s", first, second)
		}
	})
}

// FuzzStoredJobRecord covers the records a store reads back after a crash: a
// buried job (deadRecord) and an acknowledged one (doneRecord). A record that
// carries a newer envelope version must still yield its id, because the
// dead-letter tools name it.
func FuzzStoredJobRecord(f *testing.F) {
	f.Add([]byte(`{"job":{"version":1,"kind":"session.create","id":"j1","prompt":"hi","trigger":{"kind":"api","name":"http"}},"error":"boom","failed_at":"2026-11-19T12:05:00Z"}`))
	f.Add([]byte(`{"job":{"version":1,"kind":"session.create","id":"j1","prompt":"hi","trigger":{"kind":"api","name":"http"}},"session_id":"s1","finished_at":"2026-11-19T12:06:00Z"}`))
	f.Add([]byte(`{"job":{"version":9,"kind":"session.create","id":"newer"}}`))
	f.Add([]byte(`{"job":{"version":1,"kind":"session.create","id":"j1","prompt":"hi","trigger":{"kind":"api","name":"http"}}}`))
	f.Add([]byte(`null`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var wire struct {
			Job struct {
				ID string `json:"id"`
			} `json:"job"`
		}
		_ = json.Unmarshal(data, &wire)
		explicitID := wire.Job.ID

		var dead deadRecord
		if err := json.Unmarshal(data, &dead); err == nil && dead.Job.ID != "" {
			got := decodeDead(data)
			if explicitID != "" && got.Job.ID != dead.Job.ID {
				t.Fatalf("decodeDead id = %q, want %q", got.Job.ID, dead.Job.ID)
			}
			if got.Job.ID == "" {
				t.Fatal("decodeDead dropped the job id")
			}
			if !got.FailedAt.Equal(dead.FailedAt) {
				t.Fatalf("decodeDead failed_at = %v, want %v", got.FailedAt, dead.FailedAt)
			}
		}

		var done doneRecord
		if err := json.Unmarshal(data, &done); err != nil || done.Job.ID == "" {
			return
		}
		first, err := json.Marshal(done)
		if err != nil {
			t.Fatalf("encode done record: %v", err)
		}
		var again doneRecord
		if err := json.Unmarshal(first, &again); err != nil {
			t.Fatalf("decode encoded done record: %v", err)
		}
		second, err := json.Marshal(again)
		if err != nil {
			t.Fatalf("encode done record a second time: %v", err)
		}
		if !bytes.Equal(first, second) {
			t.Fatalf("done record encoding is not stable:\nfirst:  %s\nsecond: %s", first, second)
		}
	})
}

// FuzzFormatVersion feeds the persisted format stamp to the reader. The store
// calls checkFormat on bytes an older build wrote, so it must never panic and
// must never accept a version this build cannot read.
func FuzzFormatVersion(f *testing.F) {
	f.Add([]byte(nil))
	f.Add([]byte("1"))
	f.Add([]byte("2"))
	f.Add([]byte("0"))
	f.Add([]byte("01"))
	f.Add([]byte("-1"))
	f.Add([]byte("1\x00"))
	f.Add([]byte("v1"))

	f.Fuzz(func(t *testing.T, stored []byte) {
		missing, err := checkFormat(stored)
		if len(stored) == 0 {
			if missing != true || err != nil {
				t.Fatalf("empty stamp: missing = %v, err = %v", missing, err)
			}
			return
		}
		if missing {
			t.Fatal("non-empty stamp reported as missing")
		}
		if err != nil {
			return
		}
		version, convErr := strconv.Atoi(string(stored))
		if convErr != nil || version != formatVersion {
			t.Fatalf("checkFormat accepted %q", stored)
		}
	})
}
