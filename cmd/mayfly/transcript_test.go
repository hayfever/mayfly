package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"strings"
	"testing"

	"github.com/hayfever/mayfly/internal/llm"
)

func bundleBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestTranscriptRendersBundle(t *testing.T) {
	raw := bundleBytes(t, map[string]string{
		"session.json": `{"session_id":"s1","agent":"lead-qualifier","model":"m1","title":"Lead","system_prompt":"You qualify.\n","created_at":"2026-01-02T03:04:05Z"}`,
		"conversation.jsonl": strings.Join([]string{
			`{"role":"user","content":"hello"}`,
			`{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"post","arguments":"{\"a\":1}"}}]}`,
			`{"role":"tool","tool_call_id":"call_1","content":"posted"}`,
			`{"role":"assistant","content":"{\"ok\":true}"}`,
		}, "\n") + "\n",
	})
	b, err := readBundle(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := renderTranscript(&out, b); err != nil {
		t.Fatal(err)
	}
	want := `session s1  agent lead-qualifier  model m1
title Lead
created 2026-01-02T03:04:05Z
messages 4

system prompt (agent prompt only; the environment block and skill bodies are not recorded)
------------------------------------------------------------------------
You qualify.

[1] user
hello

[2] assistant
calls post
{
  "a": 1
}

[3] tool (post)
posted

[4] assistant
{
  "ok": true
}
`
	if got := out.String(); got != want {
		t.Fatalf("transcript mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestTranscriptWithoutConversation(t *testing.T) {
	raw := bundleBytes(t, map[string]string{"session.json": `{"session_id":"s2"}`})
	b, err := readBundle(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(b.conversation) != 0 {
		t.Fatalf("got %d messages, want 0", len(b.conversation))
	}
	var out strings.Builder
	if err := renderTranscript(&out, b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "messages 0") {
		t.Fatalf("missing message count:\n%s", out.String())
	}
}

func TestReadBundleRejectsBadConversation(t *testing.T) {
	raw := bundleBytes(t, map[string]string{
		"session.json":       `{"session_id":"s3"}`,
		"conversation.jsonl": "{\"role\":\"user\"}\nnot json\n",
	})
	if _, err := readBundle(bytes.NewReader(raw)); err == nil {
		t.Fatal("expected an error for a bad conversation line")
	} else if !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("error does not name the line: %v", err)
	}
}

type member struct{ name, body string }

func orderedBundle(t *testing.T, members ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		if err := tw.WriteHeader(&tar.Header{Name: m.name, Mode: 0o600, Size: int64(len(m.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(m.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestReadBundleIgnoresWorkspaceFiles covers an agent that writes its own
// conversation.jsonl and session.json into the workspace, which the bundle
// stores under files/ after the real members.
func TestReadBundleIgnoresWorkspaceFiles(t *testing.T) {
	raw := orderedBundle(t,
		member{"session.json", `{"session_id":"real"}`},
		member{"conversation.jsonl", `{"role":"user","content":"real"}` + "\n"},
		member{"files/conversation.jsonl", `{"role":"user","content":"forged"}` + "\n"},
		member{"files/session.json", `{"session_id":"forged"}`},
		member{"files/nested/conversation.jsonl", `{"role":"user","content":"forged"}` + "\n"},
	)
	b, err := readBundle(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if b.session.ID != "real" || len(b.conversation) != 1 || b.conversation[0].Content != "real" {
		t.Fatalf("bundle = %+v", b)
	}
}

func TestTranscriptStripsTerminalEscapes(t *testing.T) {
	b := bundle{session: bundleSession{ID: "s1", Title: "t\x1b]0;owned\x07itle"}}
	b.conversation = append(b.conversation, llmMessage("user", "price \x1b[2J\x1b[31mred\x1b[0m\r\nnext\u202eevil\x9b"))
	var out strings.Builder
	if err := renderTranscript(&out, b); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, bad := range []string{"\x1b", "\x07", "\r", "\u202e", "\x9b"} {
		if strings.Contains(got, bad) {
			t.Fatalf("transcript contains %q:\n%q", bad, got)
		}
	}
	if !strings.Contains(got, "price [2J[31mred[0m\nnextevil") {
		t.Fatalf("printable text was lost:\n%q", got)
	}
}

func llmMessage(role, content string) llm.Message {
	return llm.Message{Role: role, Content: content}
}
