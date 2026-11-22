package httpapi

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hayfever/mayfly/internal/metrics"
)

func TestWebSocketReaderRecoversPanic(t *testing.T) {
	var logs bytes.Buffer
	registry := metrics.New(metrics.Sources{}, metrics.BuildInfo{})
	s := &Server{
		logger:  slog.New(slog.NewTextHandler(&logs, nil)),
		metrics: registry,
	}

	// The deferred helper is the one the reader goroutine defers. If it did
	// not recover, this test binary would exit.
	func() {
		defer s.recoverWebSocketReader("session-42")
		panic("client message blew up")
	}()

	text := logs.String()
	for _, want := range []string{
		"websocket reader panicked",
		"session=session-42",
		`panic="client message blew up"`,
		"stack=",
		"goroutine",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("panic log is missing %q:\n%s", want, text)
		}
	}

	rec := httptest.NewRecorder()
	registry.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if body := rec.Body.String(); !strings.Contains(body, `mayfly_panics_total{component="http"} 1`) {
		t.Fatalf("metrics do not record one http panic:\n%s", body)
	}
}
