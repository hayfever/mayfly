package mcp

import (
	"encoding/json"
	"os"
	"testing"
)

// TestSanitizeMatchesPluginTable pins sanitize to the table shared with
// internal/plugin, so mcp_<server>_<tool> and plugin_<plugin>_<tool> cannot
// drift apart.
func TestSanitizeMatchesPluginTable(t *testing.T) {
	raw, err := os.ReadFile("../plugin/testdata/sanitize.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		In  string `json:"in"`
		Out string `json:"out"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("shared sanitizer table is empty")
	}
	for _, tc := range cases {
		if got := sanitize(tc.In); got != tc.Out {
			t.Fatalf("sanitize(%q) = %q, want %q", tc.In, got, tc.Out)
		}
		if got := toolName("srv", tc.In); got != "mcp_srv_"+tc.Out {
			t.Fatalf("toolName(srv, %q) = %q", tc.In, got)
		}
	}
}
