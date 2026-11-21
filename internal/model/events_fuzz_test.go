package model

import (
	"bytes"
	"encoding/json"
	"testing"
)

// FuzzDecodeEventData feeds arbitrary type and data pairs to the event payload
// decoder. Thread events arrive from the runtime, so a payload this build does
// not expect must be an error, never a panic. A payload that decodes must
// encode to bytes that decode to the same payload.
func FuzzDecodeEventData(f *testing.F) {
	f.Add(string(EventUserMessage), []byte(`{"kind":"api","name":"http","event":"create"}`))
	f.Add(string(EventAssistantMessage), []byte(`{"finish_reason":"stop","tool_calls":1}`))
	f.Add(string(EventToolCall), []byte(`{"tool":"echo","call_id":"c1","arguments":"{}"}`))
	f.Add(string(EventToolResult), []byte(`{"tool":"echo","call_id":"c1","error":true}`))
	f.Add(string(EventStatus), []byte(`{"status":"running","reason":"start","error":"","restart":1}`))
	f.Add(string(EventSystem), []byte(`{"model":"m","skills":["a"],"mcp_servers":[],"plugins":[],"tool_count":2}`))
	f.Add(string(EventArtifact), []byte(`{"key":"k","uri":"u","size":1,"sha256":"h","partial":true}`))
	f.Add(string(EventControl), []byte(`{"action":"cancel","reason":"done"}`))
	f.Add(string(EventUserMessage), []byte(`null`))
	f.Add(string(EventUserMessage), []byte(`{}`))
	f.Add(string(EventUserMessage), []byte(`[]`))
	f.Add("unknown", []byte(`{"x":1}`))
	f.Add("", []byte(nil))

	f.Fuzz(func(t *testing.T, typ string, raw []byte) {
		data, err := DecodeEventData(EventType(typ), raw)
		if err != nil {
			return
		}
		if data == nil {
			return
		}
		first, err := json.Marshal(data)
		if err != nil {
			t.Fatalf("encode %q payload: %v", typ, err)
		}
		again, err := DecodeEventData(EventType(typ), first)
		if err != nil {
			t.Fatalf("decode encoded %q payload: %v\nencoded: %s", typ, err, first)
		}
		if again == nil {
			t.Fatalf("%q payload decoded to nil after re-encode: %s", typ, first)
		}
		second, err := json.Marshal(again)
		if err != nil {
			t.Fatalf("encode %q payload a second time: %v", typ, err)
		}
		if !bytes.Equal(first, second) {
			t.Fatalf("%q payload encoding is not stable:\nfirst:  %s\nsecond: %s", typ, first, second)
		}
	})
}

// FuzzEventJSON covers a whole thread event, as the runtime posts it and as a
// bundle replays it. The event type selects the data decoder, so the two must
// agree for any input.
func FuzzEventJSON(f *testing.F) {
	f.Add([]byte(`{"seq":3,"session_id":"s1","time":"2026-11-19T12:01:00Z","type":"user_message","actor":"alice","text":"hi","data":{"kind":"api","name":"http","event":"create"}}`))
	f.Add([]byte(`{"type":"status","data":{"status":"running"}}`))
	f.Add([]byte(`{"type":"tool_result","data":{"tool":"echo","call_id":"c","error":true}}`))
	f.Add([]byte(`{"type":"system","data":null}`))
	f.Add([]byte(`{"type":"unknown","data":{"x":1}}`))
	f.Add([]byte(`{"type":"assistant_delta","text":"part"}`))
	f.Add([]byte(`{"type":"user_message","data":[]}`))
	f.Add([]byte(`null`))

	f.Fuzz(func(t *testing.T, raw []byte) {
		var ev Event
		if err := ev.UnmarshalJSON(raw); err != nil {
			return
		}
		first, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("encode event: %v\npayload: %s", err, raw)
		}
		var again Event
		if err := again.UnmarshalJSON(first); err != nil {
			t.Fatalf("decode encoded event: %v\nencoded: %s", err, first)
		}
		second, err := json.Marshal(again)
		if err != nil {
			t.Fatalf("encode event a second time: %v", err)
		}
		if !bytes.Equal(first, second) {
			t.Fatalf("event encoding is not stable:\nfirst:  %s\nsecond: %s", first, second)
		}
	})
}
