package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/hayfever/mayfly/internal/config"
)

// errBodyReadTimeout reports that the server's body read deadline fired
// before the request body arrived.
var errBodyReadTimeout = errors.New("request body read timed out")

// bodyReadTimeout returns how long one request body may take to arrive.
func (s *Server) bodyReadTimeout() time.Duration {
	if timeout := s.cfg.Server.BodyReadTimeout.Std(); timeout > 0 {
		return timeout
	}
	return config.DefaultBodyReadTimeout
}

// readBody reads at most max bytes of the request body within the configured
// body read deadline. A client that sends headers and then stalls the body
// gets errBodyReadTimeout instead of pinning the handler forever.
//
// The deadline is cleared after a complete read, because net/http keeps the
// connection for the next keep-alive request. After a failed read the deadline
// is moved into the past, so the server's post-handler drain cannot block on
// the same body. A writer without deadline support, such as an in-memory test
// recorder, is left alone.
func (s *Server) readBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, error) {
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(s.bodyReadTimeout())); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return nil, err
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err == nil {
		_ = rc.SetReadDeadline(time.Time{})
		return body, nil
	}
	_ = rc.SetReadDeadline(time.Now())
	if isReadTimeout(err) {
		return nil, fmt.Errorf("%w: %w", errBodyReadTimeout, err)
	}
	return nil, err
}

// isReadTimeout reports whether a body read failed because a deadline passed.
// The error comes from the connection, so it may be a net.Error or wrap
// os.ErrDeadlineExceeded.
func isReadTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// decodeJSON reads one JSON body with readBody and unmarshals it. The internal
// runtime protocol uses it: its shapes are versioned with the runtime image,
// not with this API.
func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, max int64, dst any) error {
	body, err := s.readBody(w, r, max)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, dst)
}

// decodeJSONStrict decodes a public /api/v1 request body. An unknown field is
// an error, so a client learns about a typo instead of having it ignored.
func (s *Server) decodeJSONStrict(w http.ResponseWriter, r *http.Request, max int64, dst any) error {
	body, err := s.readBody(w, r, max)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("body must contain one JSON value")
	}
	return nil
}

// writeBodyError maps a body read failure to the public error envelope: 408
// when the read deadline fired, 400 for anything else.
func writeBodyError(w http.ResponseWriter, err error) {
	if errors.Is(err, errBodyReadTimeout) {
		writeError(w, http.StatusRequestTimeout, codeRequestTimeout, "request body read timed out")
		return
	}
	writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid JSON body")
}

// writeInternalBodyError maps a body read failure to the runtime protocol
// error body.
func writeInternalBodyError(w http.ResponseWriter, err error) {
	if errors.Is(err, errBodyReadTimeout) {
		writeInternalError(w, http.StatusRequestTimeout, "request body read timed out")
		return
	}
	writeInternalError(w, http.StatusBadRequest, "invalid JSON body")
}
