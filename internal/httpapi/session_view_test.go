package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hayfever/mayfly/internal/model"
)

// TestPublicSessionHidesInternals checks the projection that keeps runtime
// backend, pod name, artifact store URI, and the in-flight reservation out of
// the API. Usage and the resolved budget are public.
func TestPublicSessionHidesInternals(t *testing.T) {
	view := newAPISession(model.Session{
		ID:      "s1",
		Backend: "kubernetes",
		PodName: "mayfly-s1",
		Artifact: &model.ArtifactInfo{
			Key:    "sessions/s1/bundle.tar.gz",
			URI:    "file:///var/lib/mayfly/artifacts/s1",
			Size:   10,
			SHA256: strings.Repeat("a", 64),
		},
		LLMUsage:         &model.LLMUsage{PromptTokens: 5, CompletionTokens: 6, Requests: 1, EstimatedCalls: 1},
		LLMBudget:        &model.LLMBudget{MaxTokens: 100, MaxRequests: 2},
		LLMPendingTokens: 42,
	})
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{`"backend"`, `"pod_name"`, `"uri"`, `"llm_pending_tokens"`} {
		if strings.Contains(string(raw), banned) {
			t.Fatalf("session view exposes %s: %s", banned, raw)
		}
	}
	for _, want := range []string{`"artifact"`, `"llm_usage"`, `"llm_budget"`, `"prompt_tokens"`, `"max_tokens"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("session view is missing %s: %s", want, raw)
		}
	}

	server, mgr := sessionServer(t, 1)
	handler := server.Handler()
	id := mgr.List()[0].ID
	for _, rec := range []*httptest.ResponseRecorder{
		adminRequest(t, handler, http.MethodGet, "/api/v1/sessions", ""),
		adminRequest(t, handler, http.MethodGet, "/api/v1/sessions/"+id, ""),
		adminRequest(t, handler, http.MethodPost, "/api/v1/sessions", `{"prompt":"new one","agent":"helper"}`),
	} {
		if rec.Code >= 300 {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		for _, banned := range []string{`"backend"`, `"pod_name"`, `"uri"`, `"llm_pending_tokens"`} {
			if strings.Contains(rec.Body.String(), banned) {
				t.Fatalf("response exposes %s: %s", banned, rec.Body.String())
			}
		}
	}
}
