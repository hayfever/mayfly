package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hayfever/mayfly/internal/llm"
)

func TestConversationReloadsAndTruncates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversation.jsonl")
	c, err := openConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	msgs := []llm.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "second"},
		{Role: "tool", ToolCallID: "call_1", Content: "third"},
	}
	var sizes []int64
	for _, m := range msgs {
		if err := c.Append(m); err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, c.Size())
	}
	if sizes[len(sizes)-1] != fileSize(t, path) {
		t.Fatalf("Size = %d, file = %d", c.Size(), fileSize(t, path))
	}
	if c.Count() != 3 {
		t.Fatalf("Count = %d", c.Count())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Count() != 3 || reopened.Size() != sizes[len(sizes)-1] {
		t.Fatalf("reopened Count = %d Size = %d", reopened.Count(), reopened.Size())
	}
	if got := reopened.MessagesForModel("sys", 0); len(got) != 4 || got[1].Content != "first" {
		t.Fatalf("messages = %+v", got)
	}

	if err := reopened.Truncate(sizes[1]); err != nil {
		t.Fatal(err)
	}
	if reopened.Count() != 2 || reopened.Size() != sizes[1] {
		t.Fatalf("after truncate Count = %d Size = %d, want 2/%d", reopened.Count(), reopened.Size(), sizes[1])
	}
	if got := fileSize(t, path); got != sizes[1] {
		t.Fatalf("file size = %d, want %d", got, sizes[1])
	}
	if err := reopened.Truncate(sizes[2]); err == nil {
		t.Fatal("truncate past the end was accepted")
	}
}

func TestConversationDropsTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversation.jsonl")
	good := `{"role":"user","content":"complete"}` + "\n"
	if err := os.WriteFile(path, []byte(good+`{"role":"assistant","cont`), 0o640); err != nil {
		t.Fatal(err)
	}
	c, err := openConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Count() != 1 || c.Size() != int64(len(good)) {
		t.Fatalf("Count = %d Size = %d", c.Count(), c.Size())
	}
	if got := fileSize(t, path); got != int64(len(good)) {
		t.Fatalf("file size = %d, want %d", got, len(good))
	}
}

func TestConversationDropsCorruptCompleteLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversation.jsonl")
	good := `{"role":"user","content":"complete"}` + "\n"
	bad := "not json\n"
	tail := `{"role":"assistant","content":"gone"}` + "\n"
	if err := os.WriteFile(path, []byte(good+bad+tail), 0o640); err != nil {
		t.Fatal(err)
	}
	c, err := openConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Count() != 1 || c.Size() != int64(len(good)) {
		t.Fatalf("Count = %d Size = %d", c.Count(), c.Size())
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}
