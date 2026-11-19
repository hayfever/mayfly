package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestResolveToken(t *testing.T) {
	cases := []struct {
		name      string
		env       map[string]string
		want      string
		wantWarn  bool
		wantAlias bool
	}{
		{
			name: "primary only",
			env:  map[string]string{"MAYFLY_API_TOKEN": "primary-secret"},
			want: "primary-secret",
		},
		{
			name:      "alias only warns",
			env:       map[string]string{"MAYFLY_TOKEN": "alias-secret"},
			want:      "alias-secret",
			wantWarn:  true,
			wantAlias: true,
		},
		{
			name: "primary wins over alias",
			env:  map[string]string{"MAYFLY_API_TOKEN": "primary-secret", "MAYFLY_TOKEN": "alias-secret"},
			want: "primary-secret",
		},
		{
			name: "neither is empty",
			env:  map[string]string{},
		},
		{
			name: "whitespace is trimmed",
			env:  map[string]string{"MAYFLY_API_TOKEN": "  primary-secret\n"},
			want: "primary-secret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(key string) string { return tc.env[key] }
			var warn bytes.Buffer
			got := resolveToken(getenv, &warn)
			if got != tc.want {
				t.Fatalf("token = %q, want %q", got, tc.want)
			}
			if tc.wantWarn != (warn.Len() > 0) {
				t.Fatalf("warning = %q, want warning %v", warn.String(), tc.wantWarn)
			}
			if tc.wantAlias {
				if !strings.Contains(warn.String(), "MAYFLY_TOKEN is deprecated") || !strings.Contains(warn.String(), "MAYFLY_API_TOKEN") {
					t.Fatalf("warning does not name both variables: %q", warn.String())
				}
				if strings.Contains(warn.String(), "alias-secret") {
					t.Fatalf("warning leaked the token: %q", warn.String())
				}
			}
		})
	}
}
