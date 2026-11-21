package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hayfever/mayfly/internal/agents"
	"github.com/hayfever/mayfly/internal/artifact"
	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/hub"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/project"
	"github.com/hayfever/mayfly/internal/session"
)

// fuzzSessionServer mirrors sessionServer for a fuzz target, which needs the
// test fixture before it registers the target function. The fixture holds real
// started sessions so the list and message routes see a live manager.
func fuzzSessionServer(f *testing.F, count int) (*Server, *session.Manager) {
	f.Helper()
	cfg := config.Default()
	cfg.AdminToken = "test-admin"
	cfg.Runtime.Backend = "local"
	cfg.Runtime.StateDir = f.TempDir()
	cfg.Runtime.Image = "example/agent:1"
	cfg.Server.InternalURL = "http://mayfly:8080"
	cfg.LLM.Model = "stub"
	bundle := &project.Bundle{Agents: []agents.Agent{
		{Definition: agents.Definition{Name: "helper"}, Source: "test"},
	}}
	store, err := artifact.NewLocal(f.TempDir())
	if err != nil {
		f.Fatal(err)
	}
	mgr, err := session.New(cfg, bundle, store, hub.New(10), journal.NewMemory(), &recordingRunner{}, nil, testLogger())
	if err != nil {
		f.Fatal(err)
	}
	for i := range count {
		if _, err := mgr.Create(context.Background(), session.CreateOptions{
			Prompt: fmt.Sprintf("page probe %d", i),
			Agent:  "helper",
		}); err != nil {
			f.Fatal(err)
		}
	}
	return New(cfg, mgr, nil, testLogger()), mgr
}

// FuzzCreateSessionBody drives the public session-create route with arbitrary
// bodies. The route reads untrusted JSON with unknown-field rejection, so the
// only acceptable outcomes are a decoded request or a 4xx. A 5xx means the
// decoder panicked or a malformed body reached the manager.
func FuzzCreateSessionBody(f *testing.F) {
	f.Add([]byte(`{"prompt":"hello","agent":"helper"}`))
	f.Add([]byte(`{"prompt":"hello","ttl":"30m","skills":["repo"],"mcp_servers":["echo"]}`))
	f.Add([]byte(`{"prompt":"hello","ttl":"not-a-duration"}`))
	f.Add([]byte(`{"prompt":"hello","unknown":true}`))
	f.Add([]byte(`{"prompt":"hello"}{"prompt":"again"}`))
	f.Add([]byte(`{"prompt":`))
	f.Add([]byte(`null`))
	f.Add([]byte(``))

	server, _ := fuzzSessionServer(f, 0)
	// One active session is enough to exercise a valid create; later valid
	// bodies report capacity, which is still a 4xx.
	server.cfg.Sessions.MaxConcurrent = 1
	handler := server.Handler()

	f.Fuzz(func(t *testing.T, body []byte) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-admin")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code >= http.StatusInternalServerError {
			t.Fatalf("status = %d body=%s\nrequest body: %s", rec.Code, rec.Body.String(), body)
		}
	})
}

// FuzzPostMessageBody drives the message route, which decodes a strict body and
// then posts the text to the session thread.
func FuzzPostMessageBody(f *testing.F) {
	f.Add([]byte(`{"text":"hello","actor":"alice"}`))
	f.Add([]byte(`{"text":""}`))
	f.Add([]byte(`{"text":"hello","unknown":true}`))
	f.Add([]byte(`{"text":"hello","actor":"alice","text":"again"}`))
	f.Add([]byte(`{"text":`))
	f.Add([]byte(`null`))
	f.Add([]byte(``))

	server, mgr := fuzzSessionServer(f, 1)
	handler := server.Handler()
	target := "/api/v1/sessions/" + mgr.List()[0].ID + "/messages"

	f.Fuzz(func(t *testing.T, body []byte) {
		req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-admin")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code >= http.StatusInternalServerError {
			t.Fatalf("status = %d body=%s\nrequest body: %s", rec.Code, rec.Body.String(), body)
		}
	})
}

// FuzzCursor covers the pagination cursor, which clients echo back verbatim.
// It is base64 of JSON, so any string reaches json.Unmarshal.
func FuzzCursor(f *testing.F) {
	f.Add("")
	f.Add("eyJ0IjoiMjAyNi0xMS0xOVQxMjowMDowMFoiLCJpZCI6InMxIn0")
	f.Add("bm90LWpzb24")
	f.Add("e30")
	f.Add("ISEh")

	f.Fuzz(func(t *testing.T, value string) {
		key, err := decodeCursor(value)
		if err != nil {
			return
		}
		if key.ID == "" {
			t.Fatalf("decodeCursor accepted a cursor with no key: %q", value)
		}
	})
}
