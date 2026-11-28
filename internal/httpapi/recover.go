package httpapi

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
)

// writeTracker records whether the handler committed response headers before
// a panic reached recoverPanics. A committed status means a second error
// response would corrupt the in-flight body, so recoverPanics writes nothing
// in that case.
type writeTracker struct {
	http.ResponseWriter
	wroteHeader bool
}

// Unwrap lets the request-id lookup and http.ResponseController reach the
// writer underneath.
func (w *writeTracker) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *writeTracker) WriteHeader(status int) {
	// A 1xx response is usually interim: net/http still sends a final status,
	// so an error response after 100/102/103 is legal. 101 Switching Protocols
	// is the exception; it is a final upgrade even without a subsequent Hijack.
	// Record only after the call returns: a writer that panics has not reported
	// a committed status, and the error path then observes that failure instead
	// of hiding it.
	w.ResponseWriter.WriteHeader(status)
	if status >= 200 || status == http.StatusSwitchingProtocols {
		w.wroteHeader = true
	}
}

func (w *writeTracker) Write(p []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(p)
}

// Flush commits the headers, so a later error response would append to the
// stream. A writer without Flush keeps the no-op behaviour of the other
// wrappers and does not count as committed.
func (w *writeTracker) Flush() {
	flusher, ok := w.ResponseWriter.(http.Flusher)
	if !ok {
		return
	}
	flusher.Flush()
	w.wroteHeader = true
}

// Hijack hands the connection to the handler, which owns the response from
// then on. A successful hijack counts as committed so recoverPanics never
// writes to the closed HTTP response.
func (w *writeTracker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer cannot be hijacked")
	}
	conn, buf, err := hijacker.Hijack()
	if err == nil {
		w.wroteHeader = true
	}
	return conn, buf, err
}

// ReadFrom keeps an artifact download on the underlying writer's fast path.
func (w *writeTracker) ReadFrom(r io.Reader) (int64, error) {
	w.wroteHeader = true
	if readerFrom, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return readerFrom.ReadFrom(r)
	}
	return io.Copy(struct{ io.Writer }{w}, r)
}

// isAbortHandler reports whether a panic value is http.ErrAbortHandler, either
// directly or wrapped. The runtime uses that panic to end a handler after it
// took over the connection; swallowing it would break that contract.
func isAbortHandler(rec any) bool {
	err, ok := rec.(error)
	return ok && errors.Is(err, http.ErrAbortHandler)
}
