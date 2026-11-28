package httpapi

import (
	"bufio"
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hayfever/mayfly/internal/config"
)

const (
	// apiPanicBody is the envelope recoverPanics writes when no handler wrote
	// first.
	apiPanicBody = "{\"error\":{\"code\":\"internal\",\"message\":\"internal server error\"}}\n"
	// internalPanicBody is the plain runtime protocol body.
	internalPanicBody = "internal server error\n"
)

// logSink collects log lines so a test can assert on the panic record.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func newPanicServer(t *testing.T) (*Server, *logSink) {
	t.Helper()
	sink := &logSink{}
	logger := slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return New(config.Default(), nil, nil, logger), sink
}

// headerCounter counts the WriteHeader calls that reach the underlying writer,
// so a second status line from the middleware is visible.
type headerCounter struct {
	*httptest.ResponseRecorder
	writeHeaders int
}

func newHeaderCounter() *headerCounter {
	return &headerCounter{ResponseRecorder: httptest.NewRecorder()}
}

func (c *headerCounter) WriteHeader(status int) {
	c.writeHeaders++
	c.ResponseRecorder.WriteHeader(status)
}

func TestPanicRecoveryRepanicsAbortHandler(t *testing.T) {
	wrapped := fmt.Errorf("handler aborted: %w", http.ErrAbortHandler)
	cases := []struct {
		name  string
		value any
	}{
		{name: "direct", value: http.ErrAbortHandler},
		{name: "wrapped", value: wrapped},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, logs := newPanicServer(t)
			handler := s.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				panic(tc.value)
			}))
			rec := newHeaderCounter()
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil))
			}()
			if recovered != tc.value {
				t.Fatalf("recovered panic = %v (%T), want %v (%T)", recovered, recovered, tc.value, tc.value)
			}
			if rec.writeHeaders != 0 {
				t.Fatalf("WriteHeader calls = %d, want 0", rec.writeHeaders)
			}
			if rec.Body.Len() != 0 {
				t.Fatalf("body = %q, want empty", rec.Body.String())
			}
			if got := logs.String(); strings.Contains(got, "panic serving request") {
				t.Fatalf("logged an error for an abort panic: %s", got)
			}
		})
	}
}

func TestPanicRecoveryAfterWriteKeepsHandlerResponse(t *testing.T) {
	cases := []struct {
		name    string
		write   func(http.ResponseWriter)
		status  int
		body    string
		headers int
	}{
		{
			name:    "status only",
			write:   func(w http.ResponseWriter) { w.WriteHeader(http.StatusOK) },
			status:  http.StatusOK,
			headers: 1,
		},
		{
			name: "status and partial body",
			write: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"partial":`))
			},
			status:  http.StatusAccepted,
			body:    `{"partial":`,
			headers: 1,
		},
		{
			name:    "implicit status from body",
			write:   func(w http.ResponseWriter) { _, _ = w.Write([]byte("chunk")) },
			status:  http.StatusOK,
			body:    "chunk",
			headers: 0,
		},
	}
	paths := []string{"/api/v1/sessions", "/internal/v1/sessions/x/tools"}
	for _, tc := range cases {
		for _, path := range paths {
			t.Run(tc.name+" "+path, func(t *testing.T) {
				s, logs := newPanicServer(t)
				handler := s.recoverPanics(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					tc.write(w)
					panic("boom")
				}))
				rec := newHeaderCounter()
				handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

				if rec.writeHeaders != tc.headers {
					t.Fatalf("WriteHeader calls = %d, want %d", rec.writeHeaders, tc.headers)
				}
				if rec.Code != tc.status {
					t.Fatalf("status = %d, want %d", rec.Code, tc.status)
				}
				if got := rec.Body.String(); got != tc.body {
					t.Fatalf("body = %q, want %q", got, tc.body)
				}
				got := logs.String()
				if !strings.Contains(got, "panic serving request") {
					t.Fatalf("panic was not logged: %s", got)
				}
				if !strings.Contains(got, "headers_sent=true") {
					t.Fatalf("log does not report sent headers: %s", got)
				}
			})
		}
	}
}

func TestPanicRecoveryBeforeWriteSends500(t *testing.T) {
	cases := []struct {
		path string
		body string
	}{
		{path: "/api/v1/sessions", body: apiPanicBody},
		{path: "/internal/v1/sessions/x/tools", body: internalPanicBody},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			s, logs := newPanicServer(t)
			handler := s.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				panic("boom")
			}))
			rec := newHeaderCounter()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if rec.writeHeaders != 1 {
				t.Fatalf("WriteHeader calls = %d, want 1", rec.writeHeaders)
			}
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
			if got := rec.Body.String(); got != tc.body {
				t.Fatalf("body = %q, want %q", got, tc.body)
			}
			got := logs.String()
			if !strings.Contains(got, "panic serving request") {
				t.Fatalf("panic was not logged: %s", got)
			}
			if !strings.Contains(got, "headers_sent=false") {
				t.Fatalf("log does not report unsent headers: %s", got)
			}
		})
	}
}

func TestPanicRecoveryAfterFlushSkipsErrorBody(t *testing.T) {
	s, logs := newPanicServer(t)
	handler := s.recoverPanics(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
		panic("boom")
	}))
	rec := newHeaderCounter()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil))

	if rec.writeHeaders != 0 {
		t.Fatalf("WriteHeader calls = %d, want 0", rec.writeHeaders)
	}
	if !rec.Flushed {
		t.Fatal("handler did not reach the underlying Flusher")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", rec.Body.String())
	}
	if got := logs.String(); !strings.Contains(got, "headers_sent=true") {
		t.Fatalf("log does not report sent headers: %s", got)
	}
}

// hijackCounter records a hijack and any response write that follows it.
type hijackCounter struct {
	*httptest.ResponseRecorder
	hijacks      int
	writeHeaders int
	writes       int
	server       net.Conn
	client       net.Conn
}

func (h *hijackCounter) WriteHeader(status int) {
	h.writeHeaders++
	h.ResponseRecorder.WriteHeader(status)
}

func (h *hijackCounter) Write(p []byte) (int, error) {
	h.writes++
	return h.ResponseRecorder.Write(p)
}

func (h *hijackCounter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	server, client := net.Pipe()
	h.server, h.client = server, client
	h.hijacks++
	return server, bufio.NewReadWriter(bufio.NewReader(server), bufio.NewWriter(server)), nil
}

func TestPanicRecoveryAfterHijackWritesNothing(t *testing.T) {
	s, logs := newPanicServer(t)
	handler := s.recoverPanics(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, _, err := http.NewResponseController(w).Hijack(); err != nil {
			t.Fatalf("hijack: %v", err)
		}
		panic("boom")
	}))
	rec := &hijackCounter{ResponseRecorder: httptest.NewRecorder()}
	t.Cleanup(func() {
		rec.server.Close()
		rec.client.Close()
	})
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil))

	if rec.hijacks != 1 {
		t.Fatalf("hijack calls = %d, want 1", rec.hijacks)
	}
	if rec.writeHeaders != 0 {
		t.Fatalf("WriteHeader calls = %d, want 0", rec.writeHeaders)
	}
	if rec.writes != 0 {
		t.Fatalf("Write calls = %d, want 0", rec.writes)
	}
	if got := logs.String(); !strings.Contains(got, "headers_sent=true") {
		t.Fatalf("log does not report sent headers: %s", got)
	}
}

func TestRecoverPanicsPreservesWriterInterfaces(t *testing.T) {
	s, _ := newPanicServer(t)
	var flusher, hijacker, unwrapper bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, flusher = w.(http.Flusher)
		_, hijacker = w.(http.Hijacker)
		_, unwrapper = w.(interface{ Unwrap() http.ResponseWriter })
	})
	chain := s.commonHeaders(s.observeRequests(s.recoverPanics(shapeAPIErrors(inner))))
	chain.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil))

	if !flusher {
		t.Fatal("handler lost http.Flusher through the middleware chain")
	}
	if !hijacker {
		t.Fatal("handler lost http.Hijacker through the middleware chain")
	}
	if !unwrapper {
		t.Fatal("handler lost Unwrap through the middleware chain")
	}
}
