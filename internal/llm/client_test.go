package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hayfever/mayfly/internal/content"
)

func TestCompleteSendsImageParts(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	imageBytes := []byte("png bytes")
	client := New(server.URL, "", "model")
	_, _, err := client.Complete(context.Background(), []Message{
		{Role: "system", Content: "be brief"},
		{Role: "tool", ToolCallID: "call_1", Content: "caption\n[image image/png, 9 bytes]", Parts: []content.Part{
			content.TextPart("caption"),
			content.ImagePart("image/png", imageBytes),
		}},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	var request struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	if len(request.Messages) != 2 {
		t.Fatalf("messages = %d", len(request.Messages))
	}
	var system string
	if err := json.Unmarshal(request.Messages[0].Content, &system); err != nil || system != "be brief" {
		t.Fatalf("system content = %s err=%v", request.Messages[0].Content, err)
	}
	var parts []wirePart
	if err := json.Unmarshal(request.Messages[1].Content, &parts); err != nil {
		t.Fatalf("tool content is not an array: %s", request.Messages[1].Content)
	}
	if len(parts) != 2 || parts[0].Type != "text" || parts[0].Text != "caption" {
		t.Fatalf("parts = %+v", parts)
	}
	wantURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(imageBytes)
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil || parts[1].ImageURL.URL != wantURL {
		t.Fatalf("image part = %+v", parts[1])
	}
	if strings.Contains(string(body), "parts") {
		t.Fatal("request leaked the conversation log shape")
	}
}

func TestCompleteKeepsTextContentString(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	client := New(server.URL, "", "model")
	if _, _, err := client.Complete(context.Background(), []Message{
		{Role: "tool", ToolCallID: "call_1", Content: "plain text"},
	}, nil, nil); err != nil {
		t.Fatal(err)
	}
	var request struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	var text string
	if err := json.Unmarshal(request.Messages[0].Content, &text); err != nil || text != "plain text" {
		t.Fatalf("content = %s err=%v", request.Messages[0].Content, err)
	}
}

func TestCompleteRejectsStaleImageAsText(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	client := New(server.URL, "", "model")
	if _, _, err := client.Complete(context.Background(), []Message{
		{Role: "tool", ToolCallID: "call_1", Parts: []content.Part{
			content.ImagePart("image/svg+xml", []byte("svg")),
		}},
	}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"image_url"`) {
		t.Fatalf("request carried an invalid image: %s", body)
	}
	if !strings.Contains(string(body), "invalid image part") {
		t.Fatalf("request = %s", body)
	}
}
