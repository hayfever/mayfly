package httpapi

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hayfever/mayfly/internal/metrics"
)

// requestIDHeader carries the request id in the request and the response.
const requestIDHeader = "X-Request-ID"

// maxRequestIDLength bounds an id accepted from a caller, so one header cannot
// grow every log line.
const maxRequestIDLength = 128

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

// observeRequests assigns a request id, records the outcome, writes the info
// log line, and feeds the HTTP metrics. The route label is the mux pattern,
// never the raw path, so label cardinality stays bounded.
func (s *Server) observeRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get(requestIDHeader))
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)

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

// sanitizeRequestID accepts an id a caller supplied when it is short and made
// of characters that cannot break a log line.
func sanitizeRequestID(value string) string {
	if value == "" || len(value) > maxRequestIDLength {
		return ""
	}
	for i := range len(value) {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':':
		default:
			return ""
		}
	}
	return value
}

// newRequestID returns a random 128-bit id in hex.
func newRequestID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buf)
}
