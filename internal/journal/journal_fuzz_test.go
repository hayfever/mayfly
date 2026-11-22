package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// FuzzEntryJSON covers the bytes a journal store reads back through
// json.Unmarshal: a step an agent wrote, which a replacement runtime replays.
// Arbitrary bytes must not panic, and an entry that decodes must survive one
// re-encode without changing.
func FuzzEntryJSON(f *testing.F) {
	f.Add([]byte(`{"session_id":"s1","turn":1,"step":0,"name":"user","input":{"prompt":"hi"},"output":{"type":"user_message"},"conv_len":120,"time":"2026-11-19T12:01:00Z"}`))
	f.Add([]byte(`{"session_id":"s1","turn":1,"step":1,"name":"llm","output":{"type":"tool_call","text":"echo"},"conv_len":260,"time":"2026-11-19T12:01:01Z"}`))
	f.Add([]byte(`{"session_id":"s1","turn":2,"step":0,"name":"assistant","output":{"type":"assistant_message","text":"done"},"conv_len":470,"time":"2026-11-19T12:01:03Z"}`))
	f.Add([]byte(`{"session_id":"s1","turn":1,"step":0,"name":"x","input":{}}`))
	f.Add([]byte(`{"session_id":"","turn":0,"step":-1,"name":"","conv_len":-1,"time":"0001-01-01T00:00:00Z"}`))
	f.Add([]byte(`null`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var e Entry
		if err := json.Unmarshal(data, &e); err != nil {
			return
		}
		_ = validate(e)
		first, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("encode entry: %v\npayload: %s", err, data)
		}
		var again Entry
		if err := json.Unmarshal(first, &again); err != nil {
			t.Fatalf("decode encoded entry: %v\nencoded: %s", err, first)
		}
		second, err := json.Marshal(again)
		if err != nil {
			t.Fatalf("encode entry a second time: %v", err)
		}
		if !bytes.Equal(first, second) {
			t.Fatalf("entry encoding is not stable:\nfirst:  %s\nsecond: %s", first, second)
		}
	})
}

// FuzzStepEventJSON covers an entry's output object, which the control plane
// decodes to rebuild the visible thread. A step event carries a typed payload,
// so the decoder runs on a type name and raw data from a persisted record.
func FuzzStepEventJSON(f *testing.F) {
	f.Add([]byte(`{"type":"user_message","actor":"alice","text":"hi","data":{"kind":"api","name":"http","event":"create"}}`))
	f.Add([]byte(`{"type":"tool_result","text":"ok","data":{"tool":"echo","call_id":"c1","error":false}}`))
	f.Add([]byte(`{"type":"status","data":{"status":"running","reason":"start","restart":2}}`))
	f.Add([]byte(`{"type":"system","data":{"model":"m","skills":["a"],"mcp_servers":[],"plugins":[],"tool_count":3}}`))
	f.Add([]byte(`{"type":"unknown","data":{"anything":1}}`))
	f.Add([]byte(`{"type":"user_message","data":null}`))
	f.Add([]byte(`{"type":"tool_call","data":[]}`))
	f.Add([]byte(`null`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var ev StepEvent
		if err := ev.UnmarshalJSON(data); err != nil {
			return
		}
		first, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("encode step event: %v\npayload: %s", err, data)
		}
		var again StepEvent
		if err := again.UnmarshalJSON(first); err != nil {
			t.Fatalf("decode encoded step event: %v\nencoded: %s", err, first)
		}
		second, err := json.Marshal(again)
		if err != nil {
			t.Fatalf("encode step event a second time: %v", err)
		}
		if !bytes.Equal(first, second) {
			t.Fatalf("step event encoding is not stable:\nfirst:  %s\nsecond: %s", first, second)
		}
	})
}

// FuzzCallJSON covers the ledger record a resume replays. A record that
// decodes is validated and written back; validation must not panic on any
// decoded value.
func FuzzCallJSON(f *testing.F) {
	f.Add([]byte(`{"key":"s1/1/2","session_id":"s1","turn":1,"step":2,"tool":"echo","args_sha256":"abc","instance_id":"i1","state":"pending","attempts":1,"started_at":"2026-11-19T12:01:00Z"}`))
	f.Add([]byte(`{"key":"s1/1/2","session_id":"s1","turn":1,"step":2,"tool":"echo","args_sha256":"abc","state":"done","content":"hi","is_error":true,"interrupted":true,"attempts":2,"started_at":"2026-11-19T12:01:00Z","done_at":"2026-11-19T12:01:02Z"}`))
	f.Add([]byte(`{"key":"","session_id":"","turn":0,"step":0,"tool":"","args_sha256":"","state":"weird"}`))
	f.Add([]byte(`null`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var c Call
		if err := json.Unmarshal(data, &c); err != nil {
			return
		}
		_ = validateCall(c)
		first, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("encode call: %v\npayload: %s", err, data)
		}
		var again Call
		if err := json.Unmarshal(first, &again); err != nil {
			t.Fatalf("decode encoded call: %v\nencoded: %s", err, first)
		}
		second, err := json.Marshal(again)
		if err != nil {
			t.Fatalf("encode call a second time: %v", err)
		}
		if !bytes.Equal(first, second) {
			t.Fatalf("call encoding is not stable:\nfirst:  %s\nsecond: %s", first, second)
		}
	})
}

// FuzzBoltStoredStep feeds raw values into the store's buckets and then reads
// them through Turn and Get. A damaged journal file must surface an error, not
// panic, because a session adoption reads the journal before anything else.
func FuzzBoltStoredStep(f *testing.F) {
	f.Add([]byte(`{"session_id":"s1","turn":1,"step":0,"name":"user","output":{"type":"user_message"},"conv_len":120,"time":"2026-11-19T12:01:00Z"}`))
	f.Add([]byte(`{"key":"s1/1/2","session_id":"s1","turn":1,"step":2,"tool":"echo","args_sha256":"abc","state":"pending","attempts":1,"started_at":"2026-11-19T12:01:00Z"}`))
	f.Add([]byte(`{`))
	f.Add([]byte(`[]`))
	f.Add([]byte(nil))

	store, err := OpenBolt(filepath.Join(f.TempDir(), "journal.db"))
	if err != nil {
		f.Fatal(err)
	}
	// The journal is thrown away, so skip the fsync that would dominate the
	// fuzz rate.
	store.db.NoSync = true
	f.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	const sessionID = "fuzz-session"

	f.Fuzz(func(t *testing.T, payload []byte) {
		err := store.db.Update(func(tx *bolt.Tx) error {
			steps, err := tx.Bucket(bucketJournal).CreateBucketIfNotExists([]byte(sessionID))
			if err != nil {
				return err
			}
			if err := steps.Put(entryKey(1, 0), payload); err != nil {
				return err
			}
			calls, err := tx.Bucket(bucketCalls).CreateBucketIfNotExists([]byte(sessionID))
			if err != nil {
				return err
			}
			return calls.Put([]byte("call"), payload)
		})
		if err != nil {
			t.Fatalf("seed store: %v", err)
		}

		entries, err := store.Turn(ctx, sessionID, 1)
		if err != nil {
			if len(entries) != 0 {
				t.Fatalf("Turn returned %d entries with an error", len(entries))
			}
		} else if len(entries) != 1 {
			t.Fatalf("Turn returned %d entries, want 1", len(entries))
		}

		call, found, err := store.Get(ctx, sessionID, "call")
		if err != nil {
			if found {
				t.Fatal("Get reported a record with a decode error")
			}
		} else if !found {
			t.Fatal("Get did not find the seeded record")
		} else if _, err := json.Marshal(call); err != nil {
			t.Fatalf("encode decoded call: %v", err)
		}
	})
}

// FuzzPendingJSON covers the durable record that holds a message accepted
// before its turn started. Adoption decodes it after a control-plane restart,
// so arbitrary bytes must not panic and a record that decodes must re-encode
// stably. Malformed records are rejected rather than delivered.
func FuzzPendingJSON(f *testing.F) {
	f.Add([]byte(`{"session_id":"s1","seq":3,"event":{"seq":3,"time":"2026-11-22T10:00:00Z","type":"user_message","actor":"trigger","text":"hello","data":{"kind":"api","name":"hook","event":"create"}}}`))
	f.Add([]byte(`{"session_id":"s2","seq":1,"event":{"seq":1,"time":"2026-11-22T10:00:00Z","type":"user_message","actor":"user","text":"x"}}`))
	f.Add([]byte(`{"session_id":"","seq":0,"event":{"type":"user_message","text":""}}`))
	f.Add([]byte(`{"session_id":"s1","seq":2,"event":{"seq":1,"time":"2026-11-22T10:00:00Z","type":"user_message","text":"mismatch"}}`))
	f.Add([]byte(`{"session_id":"s1","seq":1,"event":{"seq":1,"time":"2026-11-22T10:00:00Z","type":"status","text":"not a message"}}`))
	f.Add([]byte(`{"session_id":"s1","seq":1,"event":{"seq":1,"type":"user_message","text":"no time"}}`))
	f.Add([]byte(`{"session_id":"s1","seq":1,"event":null}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"session_id":"s1","seq":1,"event":{"seq":1,"time":"2026-11-22T10:00:00Z","type":"user_message","text":"truncated"`))

	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := decodePending(data)
		if err != nil {
			return
		}
		first, err := encodePending(p)
		if err != nil {
			t.Fatalf("encode pending record: %v\npayload: %s", err, data)
		}
		again, err := decodePending(first)
		if err != nil {
			t.Fatalf("decode encoded pending record: %v\nencoded: %s", err, first)
		}
		second, err := encodePending(again)
		if err != nil {
			t.Fatalf("encode pending record a second time: %v", err)
		}
		if !bytes.Equal(first, second) {
			t.Fatalf("pending encoding is not stable:\nfirst:  %s\nsecond: %s", first, second)
		}
	})
}
