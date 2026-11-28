package httpapi

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// fuzzPanicLogger keeps each fuzz execution free of log I/O.
var fuzzPanicLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// fuzzPanicType is a panic value that is not an error and not a string.
type fuzzPanicType struct{ Code int }

// panicRecorder models the final-response state of a real connection: a 1xx
// status is interim, and a Write or Flush commits 200 when no final status is
// set.
type panicRecorder struct {
	header http.Header
	codes  []int
	final  bool
	body   bytes.Buffer
}

func (r *panicRecorder) Header() http.Header {
	if r.header == nil {
		r.header = make(http.Header)
	}
	return r.header
}

func (r *panicRecorder) WriteHeader(status int) {
	r.codes = append(r.codes, status)
	if status >= 200 || status == http.StatusSwitchingProtocols {
		r.final = true
	}
}

func (r *panicRecorder) Write(p []byte) (int, error) {
	if !r.final {
		r.codes = append(r.codes, http.StatusOK)
		r.final = true
	}
	return r.body.Write(p)
}

func (r *panicRecorder) Flush() {
	if !r.final {
		r.codes = append(r.codes, http.StatusOK)
		r.final = true
	}
}

// panicOps replays a fuzz-chosen write pattern and records the response that
// the handler alone produced.
type panicOps struct {
	codes []int
	final bool
	body  bytes.Buffer
}

func (o *panicOps) apply(w http.ResponseWriter, ops []byte) {
	for i := 0; i < len(ops); i++ {
		switch ops[i] % 4 {
		case 0:
			code := http.StatusOK
			if i+2 < len(ops) {
				code = 100 + (int(ops[i+1])<<8|int(ops[i+2]))%500
				i += 2
			}
			o.codes = append(o.codes, code)
			if code >= 200 || code == http.StatusSwitchingProtocols {
				o.final = true
			}
			w.WriteHeader(code)
		case 1:
			if !o.final {
				o.codes = append(o.codes, http.StatusOK)
				o.final = true
			}
			chunk := []byte{ops[i]}
			_, _ = w.Write(chunk)
			o.body.Write(chunk)
		case 2:
			if !o.final {
				o.codes = append(o.codes, http.StatusOK)
				o.final = true
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}
}

func fuzzPanicValue(kind uint8) (value any, abort bool) {
	switch kind % 7 {
	case 0:
		return "boom", false
	case 1:
		return errors.New("boom"), false
	case 2:
		return http.ErrAbortHandler, true
	case 3:
		return fmt.Errorf("handler aborted: %w", http.ErrAbortHandler), true
	case 4:
		return 42, false
	case 5:
		return (*fuzzPanicType)(nil), false
	default:
		return fuzzPanicType{Code: 7}, false
	}
}

// FuzzRecoverPanics drives recoverPanics with every panic value kind and a
// fuzz-chosen write pattern before the panic. It asserts that an abort panic
// propagates unchanged and writes nothing, that any other panic stays
// recovered, that the middleware adds no bytes or status line when the handler
// already committed the response, and that it writes exactly one 500 when the
// handler wrote nothing.
func FuzzRecoverPanics(f *testing.F) {
	patterns := [][]byte{
		nil,            // no write
		{0, 0, 102},    // WriteHeader(202), header only
		{0, 0, 102, 1}, // WriteHeader(202) then Write
		{1},            // Write, implicit 200
		{2},            // Flush, implicit 200
		{0, 0, 3},      // WriteHeader(103), interim only
		{0, 0, 1},      // WriteHeader(101), Switching Protocols
	}
	for kind := uint8(0); kind < 7; kind++ {
		for _, pattern := range patterns {
			f.Add(kind, pattern, false)
			f.Add(kind, pattern, true)
		}
	}

	f.Fuzz(func(t *testing.T, kind uint8, ops []byte, internal bool) {
		if len(ops) > 64 {
			ops = ops[:64]
		}
		value, abort := fuzzPanicValue(kind)
		s := &Server{logger: fuzzPanicLogger}
		var handler panicOps
		handlerFn := s.recoverPanics(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			handler.apply(w, ops)
			panic(value)
		}))
		path := "/api/v1/sessions"
		if internal {
			path = "/internal/v1/sessions/x/tools"
		}
		rec := &panicRecorder{}
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			handlerFn.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		}()

		if abort {
			if recovered != value {
				t.Fatalf("abort panic = %v (%T), want %v (%T)", recovered, recovered, value, value)
			}
		} else if recovered != nil {
			t.Fatalf("panic escaped recovery: %v", recovered)
		}

		wantCodes := handler.codes
		wantBody := handler.body.Bytes()
		if !abort && !handler.final {
			wantCodes = append(append([]int{}, handler.codes...), http.StatusInternalServerError)
			if internal {
				wantBody = []byte(internalPanicBody)
			} else {
				wantBody = []byte(apiPanicBody)
			}
		}

		if !slices.Equal(rec.codes, wantCodes) {
			t.Fatalf("status lines = %v, want %v (panic kind %d, ops %v)", rec.codes, wantCodes, kind%7, ops)
		}
		if !bytes.Equal(rec.body.Bytes(), wantBody) {
			t.Fatalf("body = %q, want %q (panic kind %d, ops %v)", rec.body.String(), wantBody, kind%7, ops)
		}
		if len(rec.codes) > len(handler.codes)+1 {
			t.Fatalf("middleware wrote more than one status line: %v after handler %v", rec.codes, handler.codes)
		}
	})
}
