package e2e

import (
	"slices"
	"strings"
	"testing"
)

func TestUnterminatedLeftovers(t *testing.T) {
	terminating := func(kind, name string) leftover {
		return leftover{Kind: kind, Name: name, Terminating: true}
	}
	live := func(kind, name string) leftover {
		return leftover{Kind: kind, Name: name}
	}
	for _, tc := range []struct {
		name string
		objs []leftover
		want []string
	}{
		{
			name: "nothing left",
		},
		{
			name: "all terminating",
			objs: []leftover{
				terminating("pod", "mayfly-a"),
				terminating("persistentvolumeclaim", "mayfly-a-workspace"),
			},
		},
		{
			name: "one unterminated among terminating",
			objs: []leftover{
				terminating("pod", "mayfly-a"),
				live("configmap", "mayfly-a"),
				terminating("secret", "mayfly-a"),
			},
			want: []string{"configmap/mayfly-a"},
		},
		{
			name: "all unterminated in input order",
			objs: []leftover{
				live("secret", "mayfly-z"),
				live("pod", "mayfly-a"),
				live("persistentvolumeclaim", "mayfly-m-workspace"),
			},
			want: []string{"secret/mayfly-z", "pod/mayfly-a", "persistentvolumeclaim/mayfly-m-workspace"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unterminatedLeftovers(tc.objs); !slices.Equal(got, tc.want) {
				t.Fatalf("unterminatedLeftovers() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseLeftovers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		out     string
		want    []leftover
		wantErr bool
	}{
		{
			name: "empty output",
		},
		{
			name: "blank lines",
			out:  "\n\n\n",
		},
		{
			name: "terminating and live",
			out: "persistentvolumeclaim/mayfly-a-workspace\t2026-11-27T04:13:04Z\n" +
				"pod/mayfly-a\t\n" +
				"secret/mayfly-a\t2026-11-27T04:13:04.5Z\r\n",
			want: []leftover{
				{Kind: "persistentvolumeclaim", Name: "mayfly-a-workspace", Terminating: true},
				{Kind: "pod", Name: "mayfly-a"},
				{Kind: "secret", Name: "mayfly-a", Terminating: true},
			},
		},
		{
			name:    "no tab",
			out:     "pod/mayfly-a 2026-11-27T04:13:04Z\n",
			wantErr: true,
		},
		{
			name:    "no slash",
			out:     "pod\t2026-11-27T04:13:04Z\n",
			wantErr: true,
		},
		{
			name:    "empty name",
			out:     "pod/\t2026-11-27T04:13:04Z\n",
			wantErr: true,
		},
		{
			name:    "invalid timestamp",
			out:     "pod/mayfly-a\tyesterday\n",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLeftovers(tc.out)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseLeftovers(%q) = %v, want an error", tc.out, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLeftovers(%q): %v", tc.out, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("parseLeftovers(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

// FuzzLeftoverParse feeds arbitrary kubectl jsonpath output to the leak
// check's parser. A parsed listing keeps one object per non-blank line, in
// order, and classifies an object as terminating only when its timestamp is
// present, so an object without one never passes for a delete the API server
// accepted. No input panics.
func FuzzLeftoverParse(f *testing.F) {
	f.Add("")
	f.Add("\n\n")
	f.Add("pod/mayfly-a\t2026-11-27T04:13:04Z\n")
	f.Add("persistentvolumeclaim/mayfly-a-workspace\t2026-11-27T04:13:04Z\nsecret/mayfly-a\t\n")
	f.Add("pod/mayfly-a\t2026-11-27T04:13:04Z\r\n")
	f.Add("pod/mayfly-a\tyesterday\n")
	f.Add("pod/mayfly-a\n")
	f.Add("bad\t\t\n")
	f.Fuzz(func(t *testing.T, out string) {
		objs, err := parseLeftovers(out)
		if err != nil {
			return
		}
		var lines []string
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSuffix(line, "\r")
			if strings.TrimSpace(line) != "" {
				lines = append(lines, line)
			}
		}
		if len(objs) != len(lines) {
			t.Fatalf("parsed %d objects from %d lines: %q", len(objs), len(lines), out)
		}
		var wantLeaks []string
		for i, line := range lines {
			id, stamp, _ := strings.Cut(line, "\t")
			if objs[i].String() != id {
				t.Fatalf("object %d is %s, want %s", i, objs[i], id)
			}
			if objs[i].Terminating != (stamp != "") {
				t.Fatalf("object %s: terminating=%v with timestamp %q", objs[i], objs[i].Terminating, stamp)
			}
			if stamp == "" {
				wantLeaks = append(wantLeaks, id)
			}
		}
		if got := unterminatedLeftovers(objs); !slices.Equal(got, wantLeaks) {
			t.Fatalf("unterminatedLeftovers() = %v, want %v for %q", got, wantLeaks, out)
		}
	})
}
