package platformclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/gateway"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/model"
)

func TestInboxResetsAfterSequenceGap(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		n := requests.Add(1)
		if n == 1 {
			if got := r.URL.Query().Get("after"); got != "9" {
				t.Errorf("first after = %q", got)
			}
			fmt.Fprint(w, "event: reset\ndata: {}\n\n")
			return
		}
		if got := r.URL.Query().Get("after"); got != "0" {
			t.Errorf("second after = %q", got)
		}
		fmt.Fprint(w, "event: message\ndata: {\"seq\":1,\"type\":\"user_message\",\"text\":\"hello\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	client := New(server.URL, "sess", "token")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got []model.Event
	resets := 0
	err := client.Inbox(ctx, 9, func(ev model.Event) {
		got = append(got, ev)
		cancel()
	}, func() { resets++ })
	if err == nil {
		t.Fatal("expected context cancellation")
	}
	if len(got) != 1 || got[0].Text != "hello" {
		t.Fatalf("events = %+v", got)
	}
	if resets != 1 {
		t.Fatalf("reset callback ran %d times", resets)
	}
}

func TestJournalRetryBudget(t *testing.T) {
	old := journalRetryBudget
	journalRetryBudget = 50 * time.Millisecond
	defer func() { journalRetryBudget = old }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := New(server.URL, "sess", "token")
	start := time.Now()
	err := client.AppendJournal(context.Background(), journal.Entry{SessionID: "sess", Turn: 1, Step: 0, Name: "user"})
	if err == nil {
		t.Fatal("a permanently failing journal write returned no error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("journal retry took %s despite a 50ms budget", elapsed)
	}
}

func TestPlatformClientToolCalls(t *testing.T) {
	const session = "sess"
	base := "/internal/v1/sessions/" + session
	var sawComplete gateway.CompleteRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("%s authorization = %q", r.URL.Path, got)
		}
		switch r.URL.Path {
		case base + "/tools":
			if r.Method != http.MethodGet {
				t.Errorf("tools method = %s", r.Method)
			}
			fmt.Fprint(w, `{"tools":[`+
				`{"name":"plugin_remote","owner":"remote","placement":"mcp-http","description":"Get","input_schema":{"type":"object"},"idempotent":true,"timeout_ms":120000},`+
				`{"name":"plugin_local","owner":"local","placement":"inline","idempotent":false,"timeout_ms":30000}],`+
				`"sidecars":{"notify":{"discovery_token":"v1.discovery"}},`+
				`"errors":[{"owner":"tickets","required":false,"error":"no ready replica"}]}`)
		case base + "/tools/calls":
			var req gateway.BeginRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode begin request: %v", err)
			}
			if req.Turn != 3 || req.Step != 4 || req.CallID != "call_ab" {
				t.Errorf("begin request = %+v", req)
			}
			switch req.Tool {
			case "plugin_remote":
				fmt.Fprint(w, `{"result":{"content":"remote answer","is_error":true,"replayed":false,"interrupted":false}}`)
			case "plugin_local":
				fmt.Fprint(w, `{"dispatch":{"key":"sess/3/4","timeout_ms":30000,"token":"v1.call"}}`)
			case "plugin_denied":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"error":"tool is not granted to this session"}`)
			case "plugin_conflict":
				http.Error(w, "call key was used", http.StatusConflict)
			case "plugin_limited":
				w.Header().Set("Retry-After", "7")
				http.Error(w, "plugin is rate limited", http.StatusTooManyRequests)
			case "plugin_busy":
				http.Error(w, "too many tool calls are in flight", http.StatusServiceUnavailable)
			default:
				http.Error(w, "unknown call", http.StatusNotFound)
			}
		case base + "/tools/calls/complete":
			if err := json.NewDecoder(r.Body).Decode(&sawComplete); err != nil {
				t.Errorf("decode complete request: %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	client := New(server.URL, session, "tok")

	discovery, err := client.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Tools) != 2 || discovery.Tools[0].Name != "plugin_remote" || !discovery.Tools[0].Idempotent {
		t.Fatalf("discovery tools = %+v", discovery.Tools)
	}
	if string(discovery.Tools[0].InputSchema) != `{"type":"object"}` || discovery.Tools[0].TimeoutMS != 120000 {
		t.Fatalf("remote tool = %+v", discovery.Tools[0])
	}
	if string(discovery.Tools[1].Placement) != "inline" || discovery.Tools[1].Idempotent || discovery.Tools[1].InputSchema != nil {
		t.Fatalf("local tool = %+v", discovery.Tools[1])
	}
	if discovery.Sidecars["notify"].DiscoveryToken != "v1.discovery" {
		t.Fatalf("sidecar tokens = %+v", discovery.Sidecars)
	}
	if len(discovery.Errors) != 1 || discovery.Errors[0].Owner != "tickets" || discovery.Errors[0].Required {
		t.Fatalf("discovery errors = %+v", discovery.Errors)
	}

	request := func(tool string) gateway.BeginRequest {
		return gateway.BeginRequest{
			Turn:      3,
			Step:      4,
			CallID:    "call_ab",
			Tool:      tool,
			Arguments: json.RawMessage(`{"a":1}`),
		}
	}

	remote, err := client.BeginCall(ctx, request("plugin_remote"))
	if err != nil {
		t.Fatal(err)
	}
	if remote.Result == nil || remote.Dispatch != nil {
		t.Fatalf("remote response = %+v", remote)
	}
	if remote.Result.Content != "remote answer" || !remote.Result.IsError || remote.Result.Replayed || remote.Result.Interrupted {
		t.Fatalf("remote result = %+v", remote.Result)
	}

	local, err := client.BeginCall(ctx, request("plugin_local"))
	if err != nil {
		t.Fatal(err)
	}
	if local.Dispatch == nil || local.Result != nil {
		t.Fatalf("local response = %+v", local)
	}
	if local.Dispatch.Key != "sess/3/4" || local.Dispatch.TimeoutMS != 30000 || local.Dispatch.Token != "v1.call" {
		t.Fatalf("dispatch = %+v", local.Dispatch)
	}

	if err := client.CompleteCall(ctx, gateway.CompleteRequest{Key: "sess/3/4", Content: "done"}); err != nil {
		t.Fatal(err)
	}
	if sawComplete.Key != "sess/3/4" || sawComplete.Content != "done" || sawComplete.IsError {
		t.Fatalf("complete request = %+v", sawComplete)
	}

	cases := []struct {
		tool   string
		kind   error
		status int
	}{
		{"plugin_denied", ErrToolNotGranted, http.StatusForbidden},
		{"plugin_conflict", ErrToolConflict, http.StatusConflict},
		{"plugin_limited", ErrToolRateLimited, http.StatusTooManyRequests},
		{"plugin_busy", ErrToolUnavailable, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		_, err := client.BeginCall(ctx, request(tc.tool))
		if !errors.Is(err, tc.kind) {
			t.Fatalf("%s: error = %v, want %v", tc.tool, err, tc.kind)
		}
		var toolErr *ToolError
		if !errors.As(err, &toolErr) {
			t.Fatalf("%s: error is not a ToolError: %v", tc.tool, err)
		}
		if toolErr.Status != tc.status {
			t.Fatalf("%s: status = %d, want %d", tc.tool, toolErr.Status, tc.status)
		}
		if tc.status == http.StatusTooManyRequests && toolErr.RetryAfter != 7*time.Second {
			t.Fatalf("rate limit RetryAfter = %s, want 7s", toolErr.RetryAfter)
		}
		if tc.status == http.StatusForbidden && toolErr.Message != "tool is not granted to this session" {
			t.Fatalf("forbidden message = %q", toolErr.Message)
		}
	}
}
