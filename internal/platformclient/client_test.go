package platformclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

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
