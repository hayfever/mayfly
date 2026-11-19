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
// backend, pod name, and artifact store URI out of the API.
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
	})
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{`"backend"`, `"pod_name"`, `"uri"`} {
		if strings.Contains(string(raw), banned) {
			t.Fatalf("session view exposes %s: %s", banned, raw)
		}
	}
	if !strings.Contains(string(raw), `"artifact"`) {
		t.Fatalf("artifact metadata is missing: %s", raw)
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
		for _, banned := range []string{`"backend"`, `"pod_name"`, `"uri"`} {
			if strings.Contains(rec.Body.String(), banned) {
				t.Fatalf("response exposes %s: %s", banned, rec.Body.String())
			}
		}
	}
}
