package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hayfever/mayfly/internal/artifact"
	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/hub"
	"github.com/hayfever/mayfly/internal/session"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestReadyReflectsManagerState(t *testing.T) {
	cfg := config.Default()
	cfg.Runtime.Image = "example/agent:1"
	store, err := artifact.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := session.New(cfg, store, hub.New(10), nil, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, mgr, testLogger())
	handler := s.Handler()

	check := func(want int) {
		t.Helper()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != want {
			t.Fatalf("readyz = %d body=%s", rec.Code, rec.Body.String())
		}
	}

	check(http.StatusServiceUnavailable)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	check(http.StatusOK)
	mgr.Shutdown(context.Background())
	check(http.StatusServiceUnavailable)
}
