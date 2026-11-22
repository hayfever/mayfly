package sources

import (
	"encoding/json"
	"strings"
	"testing"
)

// FuzzHTTPJSONProjection decodes arbitrary upstream JSON and projects it with
// arbitrary configured paths. It asserts that a malformed path or an
// unexpected JSON shape returns an error or an error document, never a panic,
// and that the error and diff bounds hold.
func FuzzHTTPJSONProjection(f *testing.F) {
	f.Add([]byte(`{"items":[{"name":"a","change":"changed","diff":"one\ntwo"}]}`), "items[0]", "change", "diff")
	f.Add([]byte(`{"a":{"b":[1,2,3]}}`), "a.b[1]", "a.b[0]", "a.b[2]")
	f.Add([]byte(`[]`), "", "", "")
	f.Add([]byte(`null`), "a", "b", "c")
	f.Add([]byte(`{"name":"x","change":"new"}`), "name[0]", "change[", "diff]")
	f.Fuzz(func(t *testing.T, raw []byte, path, statusPath, diffPath string) {
		if len(raw) > 1<<16 {
			raw = raw[:1<<16]
		}
		if len(path) > 256 {
			path = path[:256]
		}
		if len(statusPath) > 256 {
			statusPath = statusPath[:256]
		}
		if len(diffPath) > 256 {
			diffPath = diffPath[:256]
		}
		var response any
		if err := json.Unmarshal(raw, &response); err != nil {
			return
		}
		def := Definition{
			Name: "fuzz",
			Extract: Extract{
				List:   path,
				Fields: map[string]Field{"value": {Paths: []string{path}}},
			},
			Change: &ChangeConfig{
				StatusPath:   statusPath,
				DiffPath:     diffPath,
				MaxLines:     3,
				MaxLineChars: 5,
			},
		}
		field, err := compileField("value", def.Extract.Fields["value"], "fuzz")
		if err != nil {
			return
		}
		s := &httpJSON{def: def, fields: []compiledField{field}}
		items, err := s.items(response)
		if err != nil {
			if err.Error() == "" {
				t.Fatal("items returned an empty error")
			}
			return
		}
		for _, item := range items {
			document := s.document(item, nil, "", "")
			if document == nil {
				continue
			}
			if len(document.Error) > 300 {
				t.Fatalf("error is %d bytes, want at most 300", len(document.Error))
			}
			lines := strings.Split(document.Diff, "\n")
			if len(lines) > def.Change.MaxLines {
				t.Fatalf("diff has %d lines, want at most %d", len(lines), def.Change.MaxLines)
			}
			for _, line := range lines {
				if len(line) > def.Change.MaxLineChars {
					t.Fatalf("diff line %q is %d bytes, want at most %d", line, len(line), def.Change.MaxLineChars)
				}
			}
		}
	})
}
