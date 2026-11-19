package e2e

import (
	"slices"
	"testing"
)

func TestSessionListIDs(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{
			name: "sessions envelope",
			body: `{"sessions":[{"id":"a"},{"id":"b"}]}`,
			want: []string{"a", "b"},
		},
		{
			name: "paginated envelope",
			body: `{"items":[{"id":"a"},{"id":"b"}],"next_cursor":"next"}`,
			want: []string{"a", "b"},
		},
		{
			name: "empty page",
			body: `{"items":[],"next_cursor":""}`,
			want: []string{},
		},
		{
			name: "empty sessions",
			body: `{"sessions":[]}`,
			want: []string{},
		},
	} {
		got, err := sessionListIDs([]byte(tc.body))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: sessionListIDs() = %v, want %v", tc.name, got, tc.want)
		}
	}
	if _, err := sessionListIDs([]byte("not json")); err == nil {
		t.Error("sessionListIDs accepted invalid JSON")
	}
}
