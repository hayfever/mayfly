package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"testing"
)

// FuzzSSEUsage checks the streamed usage tee against arbitrary bytes split at
// arbitrary boundaries. The split result must equal the single-chunk result,
// the tee must report every byte it was given, and the line buffer must stay
// within its cap.
func FuzzSSEUsage(f *testing.F) {
	f.Add([]byte("data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4}}\n\n"), []byte{0})
	f.Add([]byte("data: {\"choices\":[]}\r\n\r\ndata:{\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\n"), []byte{2, 9})
	f.Add([]byte("data: {\"usage\":null}\n\ndata: [DONE]\n\n"), []byte{})
	f.Add([]byte(""), []byte{1, 2, 3})
	f.Fuzz(func(t *testing.T, stream, cuts []byte) {
		var whole usageScanner
		if n, err := whole.Write(stream); n != len(stream) || err != nil {
			t.Fatalf("Write returned (%d, %v) for %d bytes", n, err, len(stream))
		}
		wantUsage, wantFound := whole.Result()
		if len(whole.line) > maxUsageLineBytes {
			t.Fatalf("line buffer grew to %d bytes", len(whole.line))
		}

		var split usageScanner
		write := func(chunk []byte) {
			if n, err := split.Write(chunk); n != len(chunk) || err != nil {
				t.Fatalf("Write returned (%d, %v) for %d bytes", n, err, len(chunk))
			}
			if len(split.line) > maxUsageLineBytes {
				t.Fatalf("split line buffer grew to %d bytes", len(split.line))
			}
		}
		prev := 0
		for _, cut := range cuts {
			if prev >= len(stream) {
				break
			}
			end := prev + int(cut) + 1
			if end > len(stream) {
				end = len(stream)
			}
			write(stream[prev:end])
			prev = end
		}
		write(stream[prev:])

		gotUsage, gotFound := split.Result()
		if gotUsage != wantUsage || gotFound != wantFound {
			t.Fatalf("split result = %+v/%v, single chunk = %+v/%v", gotUsage, gotFound, wantUsage, wantFound)
		}
		if split.total != int64(len(stream)) || whole.total != int64(len(stream)) {
			t.Fatalf("total = %d/%d, want %d", split.total, whole.total, len(stream))
		}
		if !bytes.Equal(split.line, whole.line) || split.skip != whole.skip {
			t.Fatalf("split state = %q/%v, single chunk = %q/%v", split.line, split.skip, whole.line, whole.skip)
		}
	})
}

// FuzzUsageJSON checks the non-streamed usage decoder: counts are never
// negative, a huge or fractional number saturates or truncates, and anything
// that is not a JSON object with a non-null usage object is unreported.
func FuzzUsageJSON(f *testing.F) {
	f.Add([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	f.Add([]byte(`{"usage":{"prompt_tokens":1e400,"completion_tokens":-5}}`))
	f.Add([]byte(`{"usage":{"prompt_tokens":1.9,"completion_tokens":9223372036854775808}}`))
	f.Add([]byte(`{"usage":null}`))
	f.Add([]byte(`{"choices":[]}`))
	f.Add([]byte(`[1,2,3]`))
	f.Add([]byte(`null`))
	f.Add([]byte(``))
	f.Fuzz(func(t *testing.T, body []byte) {
		usage, reported := decodeUsage(body)
		if usage.Prompt < 0 || usage.Completion < 0 {
			t.Fatalf("negative usage: %+v", usage)
		}
		if usage.Prompt > math.MaxInt64 || usage.Completion > math.MaxInt64 {
			t.Fatalf("usage overflowed: %+v", usage)
		}
		if !reported && (usage.Prompt != 0 || usage.Completion != 0) {
			t.Fatalf("unreported usage carries counts: %+v", usage)
		}
	})
}

// FuzzInjectStreamOptions checks the stream_options rewrite: non-streamed
// bodies are untouched, a streamed body stays valid JSON with every client key
// preserved, existing stream_options keys survive, and include_usage is true.
func FuzzInjectStreamOptions(f *testing.F) {
	f.Add([]byte(`{"model":"stub","stream":true,"messages":[]}`), true)
	f.Add([]byte(`{"model":"stub","stream":true,"stream_options":{"include_usage":false,"service_tier":"auto"}}`), true)
	f.Add([]byte(`{"model":"stub","stream":true,"stream_options":null}`), true)
	f.Add([]byte(`{"model":"stub","stream":true,"stream_options":[1]}`), true)
	f.Add([]byte(`{"model":"stub","stream":true,"nested":{"a":[1,2.5,"x"]}}`), true)
	f.Add([]byte(`null`), true)
	f.Add([]byte(`[1,2]`), true)
	f.Add([]byte(`"text"`), true)
	f.Add([]byte(`{"model":"stub"}`), false)
	f.Fuzz(func(t *testing.T, body []byte, stream bool) {
		out, err := injectStreamOptions(body, stream)
		if !stream {
			if err != nil {
				t.Fatalf("non-stream body rejected: %v", err)
			}
			if !bytes.Equal(out, body) {
				t.Fatalf("non-stream body changed: %q -> %q", body, out)
			}
			return
		}

		original, ok := decodeObject(body)
		if !ok {
			if err == nil {
				t.Fatalf("non-object body accepted: %q", body)
			}
			return
		}
		if err != nil {
			t.Fatalf("object body rejected: %q: %v", body, err)
		}
		rewritten, ok := decodeObject(out)
		if !ok {
			t.Fatalf("output is not a JSON object: %q", out)
		}

		for key, want := range original {
			if key == "stream_options" {
				continue
			}
			got, present := rewritten[key]
			if !present {
				t.Fatalf("key %q was dropped", key)
			}
			if !sameValue(got, want) {
				t.Fatalf("key %q changed: %v -> %v", key, want, got)
			}
		}
		for key := range rewritten {
			if _, present := original[key]; !present && key != "stream_options" {
				t.Fatalf("unexpected key %q added", key)
			}
		}

		options, ok := rewritten["stream_options"].(map[string]any)
		if !ok {
			t.Fatalf("stream_options = %v, want an object", rewritten["stream_options"])
		}
		if options["include_usage"] != true {
			t.Fatalf("include_usage = %v, want true", options["include_usage"])
		}
		if existing, ok := original["stream_options"].(map[string]any); ok {
			for key, want := range existing {
				if key == "include_usage" {
					continue
				}
				if got, present := options[key]; !present || !sameValue(got, want) {
					t.Fatalf("stream_options key %q changed: %v -> %v", key, want, got)
				}
			}
		}
	})
}

// decodeObject decodes body as a JSON object with exact number literals.
func decodeObject(body []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		// Trailing data means the body is not one JSON value, which
		// injectStreamOptions also rejects.
		return nil, false
	}
	object, ok := value.(map[string]any)
	return object, ok
}

// sameValue compares two JSON values decoded with UseNumber.
func sameValue(a, b any) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(left, right)
}
