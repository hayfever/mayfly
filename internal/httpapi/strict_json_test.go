package httpapi

import (
	"net/http"
	"testing"
)

// TestUnknownRequestFieldsRejected pins the public request contract: a field
// this build does not know is a 400, not something to ignore. A client must
// not believe an option took effect when the control plane never read it.
func TestUnknownRequestFieldsRejected(t *testing.T) {
	server, mgr := sessionServer(t, 1)
	handler := server.Handler()
	id := mgr.List()[0].ID

	cases := []struct {
		name   string
		target string
		body   string
	}{
		{"session create", "/api/v1/sessions", `{"prompt":"x","agent":"helper","unknown":true}`},
		{"message", "/api/v1/sessions/" + id + "/messages", `{"text":"hi","unknown":true}`},
		{"close", "/api/v1/sessions/" + id + "/close", `{"reason":"done","unknown":true}`},
		{"attach ticket", "/api/v1/sessions/" + id + "/attach-ticket", `{"actor":"alice","unknown":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := adminRequest(t, handler, http.MethodPost, tc.target, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			if body := decodeEnvelope(t, rec); body.Code != codeInvalidRequest {
				t.Fatalf("code = %q", body.Code)
			}
		})
	}

	rec := adminRequest(t, handler, http.MethodPost, "/api/v1/sessions/"+id+"/close", `{"reason":"done"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("valid close status = %d body=%s", rec.Code, rec.Body.String())
	}
}
