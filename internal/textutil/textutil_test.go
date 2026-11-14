package textutil

import (
	"testing"
	"unicode/utf8"
)

func TestTruncateKeepsRunes(t *testing.T) {
	cases := []struct {
		in    string
		limit int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello", 3, "hel"},
		{"héllo", 2, "h"},
		{"héllo", 3, "hé"},
		{"日本語", 4, "日"},
		{"日本語", 0, ""},
	}
	for _, tc := range cases {
		got := Truncate(tc.in, tc.limit)
		if got != tc.want || !utf8.ValidString(got) {
			t.Fatalf("Truncate(%q, %d) = %q, want %q", tc.in, tc.limit, got, tc.want)
		}
	}
}
