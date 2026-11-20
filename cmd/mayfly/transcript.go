package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hayfever/mayfly/internal/llm"
)

// The session bundle holds JSON files written by the runtime: session.json is
// the config the runtime ran with, conversation.jsonl is every message the
// model saw. The transcript command prints those two as reading text.

type bundleSession struct {
	ID        string    `json:"session_id"`
	Agent     string    `json:"agent"`
	Model     string    `json:"model"`
	Title     string    `json:"title"`
	System    string    `json:"system_prompt"`
	CreatedAt time.Time `json:"created_at"`
}

type bundle struct {
	session      bundleSession
	conversation []llm.Message
}

// readBundle reads the members this command needs and ignores the rest. A
// missing conversation.jsonl is an empty conversation, not an error: a session
// can finish before the model runs. Only the top-level members count, and the
// first one wins: the agent's workspace is archived under files/ after them,
// so a workspace file named conversation.jsonl cannot replace the transcript.
func readBundle(r io.Reader) (bundle, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return bundle{}, fmt.Errorf("read bundle: %w", err)
	}
	defer func() { _ = gz.Close() }()
	var out bundle
	found := false
	seen := map[string]bool{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return bundle{}, fmt.Errorf("read bundle: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if !hdr.FileInfo().Mode().IsRegular() || seen[name] {
			continue
		}
		switch name {
		case "session.json", "conversation.jsonl":
			seen[name] = true
		}
		switch name {
		case "session.json":
			raw, err := io.ReadAll(io.LimitReader(tr, 16<<20))
			if err != nil {
				return bundle{}, fmt.Errorf("read session.json: %w", err)
			}
			if err := json.Unmarshal(raw, &out.session); err != nil {
				return bundle{}, fmt.Errorf("decode session.json: %w", err)
			}
			found = true
		case "conversation.jsonl":
			msgs, err := decodeConversation(tr)
			if err != nil {
				return bundle{}, err
			}
			out.conversation = msgs
			found = true
		}
	}
	if !found {
		return bundle{}, errors.New("bundle has neither session.json nor conversation.jsonl")
	}
	return out, nil
}

func decodeConversation(r io.Reader) ([]llm.Message, error) {
	var msgs []llm.Message
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for line := 1; sc.Scan(); line++ {
		text := bytes.TrimSpace(sc.Bytes())
		if len(text) == 0 {
			continue
		}
		var m llm.Message
		if err := json.Unmarshal(text, &m); err != nil {
			return nil, fmt.Errorf("decode conversation.jsonl line %d: %w", line, err)
		}
		msgs = append(msgs, m)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read conversation.jsonl: %w", err)
	}
	return msgs, nil
}

// renderTranscript writes a header, the agent prompt, and each message. Tool
// results name the call they answer, so a reader can follow a call into its
// output without matching IDs.
func renderTranscript(out io.Writer, b bundle) error {
	// Every string here comes from the session, including page text an agent
	// read, so terminal control sequences are removed before printing.
	w := cleanWriter{out}
	var head strings.Builder
	head.WriteString("session " + orDash(b.session.ID))
	if b.session.Agent != "" {
		head.WriteString("  agent " + b.session.Agent)
	}
	if b.session.Model != "" {
		head.WriteString("  model " + b.session.Model)
	}
	head.WriteString("\n")
	if b.session.Title != "" {
		head.WriteString("title " + b.session.Title + "\n")
	}
	if !b.session.CreatedAt.IsZero() {
		head.WriteString("created " + b.session.CreatedAt.UTC().Format(time.RFC3339) + "\n")
	}
	_, _ = fmt.Fprintf(&head, "messages %d\n", len(b.conversation))
	if _, err := io.WriteString(w, head.String()); err != nil {
		return err
	}
	if strings.TrimSpace(b.session.System) != "" {
		// The runtime builds the system message from this prompt, the
		// environment, and the skills. Only the prompt is in the bundle.
		_, _ = fmt.Fprintf(w, "\nsystem prompt (agent prompt only; the environment block and skill bodies are not recorded)\n%s\n%s\n", rule(), strings.TrimRight(b.session.System, "\n"))
	}
	names := map[string]string{}
	for _, m := range b.conversation {
		for _, call := range m.ToolCalls {
			names[call.ID] = call.Function.Name
		}
	}
	for i, m := range b.conversation {
		label := m.Role
		if m.Role == "tool" {
			name := names[m.ToolCallID]
			if name == "" {
				name = "unknown call"
			}
			label = "tool (" + name + ")"
		}
		_, _ = fmt.Fprintf(w, "\n[%d] %s\n", i+1, label)
		if content := strings.TrimRight(m.Content, "\n"); content != "" {
			_, _ = fmt.Fprint(w, prettyJSON(content)+"\n")
		}
		for _, call := range m.ToolCalls {
			_, _ = fmt.Fprintf(w, "calls %s\n", call.Function.Name)
			if args := strings.TrimSpace(call.Function.Arguments); args != "" {
				_, _ = fmt.Fprint(w, prettyJSON(args)+"\n")
			}
		}
	}
	return nil
}

// cleanWriter drops terminal control characters: C0 controls other than tab
// and newline (ESC included), DEL, C1 controls, and bidirectional overrides.
// Each write is one complete formatted string, so no rune is split.
type cleanWriter struct{ w io.Writer }

func (c cleanWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(c.w, sanitizeTerminal(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

func sanitizeTerminal(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			return -1
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return -1
		case r == utf8.RuneError:
			return -1
		}
		return r
	}, s)
}

func prettyJSON(text string) string {
	trimmed := strings.TrimSpace(text)
	if !json.Valid([]byte(trimmed)) || (trimmed[0] != '{' && trimmed[0] != '[') {
		return text
	}
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
		return text
	}
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return text
	}
	return string(pretty)
}

func rule() string { return strings.Repeat("-", 72) }

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// transcriptFlags holds the flags of mayfly transcript. The command reads one
// ID and prints text, so the flag set is empty.
type transcriptFlags struct {
	*flag.FlagSet
}

// newTranscriptFlags builds the flag set that mayfly transcript parses.
func newTranscriptFlags() *transcriptFlags {
	return &transcriptFlags{FlagSet: flag.NewFlagSet("transcript", flag.ContinueOnError)}
}

func cmdTranscript(args []string) error {
	f := newTranscriptFlags()
	positional, err := parseArgs(f.FlagSet, args)
	if err != nil {
		return err
	}
	if len(positional) < 1 {
		return errors.New("usage: mayfly transcript ID")
	}
	body, err := openArtifact(positional[0])
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(body, 64<<20))
	if err != nil {
		return fmt.Errorf("read bundle: %w", err)
	}
	b, err := readBundle(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	return renderTranscript(os.Stdout, b)
}
