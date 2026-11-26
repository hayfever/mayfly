package stub

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postLLM(t *testing.T, server *httptest.Server, body string) *http.Response {
	t.Helper()
	resp, err := server.Client().Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	return resp
}

func decodeLLMJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

func sseChunks(t *testing.T, resp *http.Response) []map[string]any {
	t.Helper()
	var out []map[string]any
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			break
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("decode chunk %q: %v", data, err)
		}
		out = append(out, chunk)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func usageOf(t *testing.T, payload map[string]any) (prompt, completion, total float64) {
	t.Helper()
	usage, ok := payload["usage"].(map[string]any)
	if !ok {
		t.Fatalf("payload has no usage object: %+v", payload)
	}
	prompt, _ = usage["prompt_tokens"].(float64)
	completion, _ = usage["completion_tokens"].(float64)
	total, _ = usage["total_tokens"].(float64)
	if prompt <= 0 || completion <= 0 {
		t.Fatalf("usage = %+v, want positive prompt and completion counts", usage)
	}
	if total != prompt+completion {
		t.Fatalf("total_tokens = %v, want %v", total, prompt+completion)
	}
	return prompt, completion, total
}

func TestLLMJSONUsageIsStable(t *testing.T) {
	server := httptest.NewServer(LLMHandler())
	defer server.Close()

	body := `{"model":"stub","messages":[{"role":"user","content":"hello world"}]}`
	first, firstCompletion, _ := usageOf(t, decodeLLMJSON(t, postLLM(t, server, body)))
	second, secondCompletion, _ := usageOf(t, decodeLLMJSON(t, postLLM(t, server, body)))
	if first != second || firstCompletion != secondCompletion {
		t.Fatalf("usage changed between identical requests: %v/%v then %v/%v", first, firstCompletion, second, secondCompletion)
	}

	longer := `{"model":"stub","messages":[{"role":"user","content":"` + strings.Repeat("hello world ", 20) + `"}]}`
	bigger, _, _ := usageOf(t, decodeLLMJSON(t, postLLM(t, server, longer)))
	if bigger <= first {
		t.Fatalf("prompt tokens = %v for a longer prompt, want more than %v", bigger, first)
	}
}

func TestLLMStreamUsageOnlyWhenRequested(t *testing.T) {
	server := httptest.NewServer(LLMHandler())
	defer server.Close()

	withUsage := `{"model":"stub","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`
	chunks := sseChunks(t, postLLM(t, server, withUsage))
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want at least 2", len(chunks))
	}
	last := chunks[len(chunks)-1]
	if choices, _ := last["choices"].([]any); len(choices) != 0 {
		t.Fatalf("last chunk choices = %v, want empty", last["choices"])
	}
	usageOf(t, last)
	withCount := 0
	for _, chunk := range chunks {
		if chunk["usage"] != nil {
			withCount++
		}
	}
	if withCount != 1 {
		t.Fatalf("usage-bearing chunks = %d, want 1", withCount)
	}

	without := `{"model":"stub","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	for i, chunk := range sseChunks(t, postLLM(t, server, without)) {
		if chunk["usage"] != nil {
			t.Fatalf("chunk %d carries usage although include_usage was not set: %+v", i, chunk)
		}
		if choices, _ := chunk["choices"].([]any); len(choices) == 0 {
			t.Fatalf("chunk %d has empty choices although usage was not requested: %+v", i, chunk)
		}
	}
}

func TestLLMNoUsageDirective(t *testing.T) {
	server := httptest.NewServer(LLMHandler())
	defer server.Close()

	jsonBody := `{"model":"stub","messages":[{"role":"user","content":"do it\nno-usage"}]}`
	body := decodeLLMJSON(t, postLLM(t, server, jsonBody))
	if usage, ok := body["usage"]; ok {
		t.Fatalf("usage = %+v, want it omitted", usage)
	}

	streamBody := `{"model":"stub","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"do it\nno-usage"}]}`
	for i, chunk := range sseChunks(t, postLLM(t, server, streamBody)) {
		if chunk["usage"] != nil {
			t.Fatalf("chunk %d carries usage although the prompt asked for none: %+v", i, chunk)
		}
	}
}
