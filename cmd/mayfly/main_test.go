package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hayfever/mayfly/internal/model"
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

// TestPrintSessionShowsTokenUsage keeps the readable summary in step with the
// public session fields: usage and a token budget when either is present.
func TestPrintSessionShowsTokenUsage(t *testing.T) {
	cases := []struct {
		name    string
		usage   *model.LLMUsage
		budget  *model.LLMBudget
		want    string
		missing string
	}{
		{
			name:   "usage and budget",
			usage:  &model.LLMUsage{PromptTokens: 120, CompletionTokens: 40, Requests: 2},
			budget: &model.LLMBudget{MaxTokens: 1000},
			want:   "tokens:    120 prompt / 40 completion (budget 1000)",
		},
		{
			name:    "usage only",
			usage:   &model.LLMUsage{PromptTokens: 7, CompletionTokens: 3},
			want:    "tokens:    7 prompt / 3 completion",
			missing: "budget",
		},
		{
			name:   "budget only",
			budget: &model.LLMBudget{MaxTokens: 500},
			want:   "tokens:    0 prompt / 0 completion (budget 500)",
		},
		{
			name:    "request budget only",
			budget:  &model.LLMBudget{MaxRequests: 3},
			want:    "tokens:    0 prompt / 0 completion",
			missing: "budget",
		},
		{
			name:    "no ledger",
			want:    "messages:",
			missing: "tokens:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := captureStdout(t, func() error {
				printSession(sessionResponse{Session: model.Session{
					ID:        "s1",
					Status:    model.StatusRunning,
					LLMUsage:  tc.usage,
					LLMBudget: tc.budget,
				}})
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("output does not contain %q:\n%s", tc.want, out)
			}
			if tc.missing != "" && strings.Contains(out, tc.missing) {
				t.Fatalf("output contains %q:\n%s", tc.missing, out)
			}
		})
	}
}
