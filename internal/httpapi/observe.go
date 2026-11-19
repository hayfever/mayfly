package httpapi

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hayfever/mayfly/internal/metrics"
)

// requestIDHeader carries the request id in the request and the response.
const requestIDHeader = "X-Request-Id"

// requestRecorder records the status and body size of one response and
// answers the request id for the error envelope. It forwards the optional
// interfaces the handlers need, so streaming, WebSocket upgrades, and artifact
// downloads keep working through it.
type requestRecorder struct {
	http.ResponseWriter
	id          string
	status      int
	bytes       int64
	hijacked    bool
	wroteHeader bool
}

func (r *requestRecorder) requestID() string { return r.id }

// Unwrap lets error shaping and the error envelope reach this recorder.
func (r *requestRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *requestRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *requestRecorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *requestRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *requestRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer cannot be hijacked")
	}
	conn, buf, err := hijacker.Hijack()
	if err == nil {
		r.hijacked = true
	}
	return conn, buf, err
}

// ReadFrom keeps an artifact download on the underlying writer's fast path
// and still counts the bytes.
func (r *requestRecorder) ReadFrom(src io.Reader) (int64, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	if readerFrom, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		n, err := readerFrom.ReadFrom(src)
		r.bytes += n
		return n, err
	}
	return io.Copy(struct{ io.Writer }{r}, src)
}

// observeRequests is the single request-id path. It keeps a valid caller id or
// generates one, echoes it in the response header, exposes it in the request
// context for the audit log, and records the outcome in the request log and the
// HTTP metrics. The route label is the mux pattern, never the raw path, so
// label cardinality stays bounded.
func (s *Server) observeRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !validRequestID(id) {
			id = newRequestID()
		}
		if id != "" {
			w.Header().Set(requestIDHeader, id)
		}
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))

		rec := &requestRecorder{ResponseWriter: w, id: id}
		start := time.Now()
		next.ServeHTTP(rec, r)
		duration := time.Since(start)

		status, route := rec.status, routeLabel(r)
		switch {
		case rec.hijacked:
			status = http.StatusSwitchingProtocols
		case status == 0:
			status = http.StatusOK
		}
		s.metrics.HTTPRequest(route, r.Method, status, duration)
		s.logger.Info("http request",
			"request_id", id,
			"method", r.Method,
			"route", route,
			"status", status,
			"duration_ms", duration.Milliseconds(),
			"bytes", rec.bytes,
		)
	})
}

// routeLabel returns the pattern that matched. An unmatched request has no
// pattern, and its raw path would carry identifiers.
func routeLabel(r *http.Request) string {
	if r.Pattern == "" {
		return metrics.RouteUnmatched
	}
	if _, path, ok := strings.Cut(r.Pattern, " "); ok {
		if path == "/{$}" {
			return "/"
		}
		return path
	}
	return r.Pattern
}
