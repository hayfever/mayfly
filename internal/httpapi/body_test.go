package httpapi

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/config"
)

// stallBody sends request headers and only the first body byte, then waits for
// the server to end the request. It returns how long the connection took to
// close and any response bytes the server wrote. A read that hits the client's
// own deadline means the handler never returned.
func stallBody(t *testing.T, handler http.Handler, method, path string, headers map[string]string) (time.Duration, []byte) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	var req strings.Builder
	fmt.Fprintf(&req, "%s %s HTTP/1.1\r\nHost: example\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n", method, path)
	for name, value := range headers {
		fmt.Fprintf(&req, "%s: %s\r\n", name, value)
	}
	req.WriteString("\r\n{")
	if _, err := io.WriteString(conn, req.String()); err != nil {
		t.Fatal(err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	response, err := io.ReadAll(conn)
	elapsed := time.Since(start)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			t.Fatalf("server did not end the stalled %s %s request within %s", method, path, elapsed)
		}
		// A reset after the server writes its response is acceptable; the
		// elapsed time still proves the handler returned.
	}
	return elapsed, response
}

// checkStalledRequestEnded asserts the server ended a stalled request well
// before the client's own read deadline, with a 408 or a closed connection.
func checkStalledRequestEnded(t *testing.T, elapsed time.Duration, response []byte) {
	t.Helper()
	if elapsed > 2*time.Second {
		t.Fatalf("stalled request took %s to end", elapsed)
	}
	if len(response) > 0 && !strings.Contains(string(response), " 408 ") {
		t.Fatalf("response = %q, want 408 or a closed connection", response)
	}
}

func TestStalledWebhookBodyTimesOut(t *testing.T) {
	cfg := config.Default()
	cfg.Server.BodyReadTimeout = config.Duration(200 * time.Millisecond)
	cfg.Triggers = []config.TriggerConfig{{Name: "generic", Type: "generic", AllowUnsigned: true}}
	handler := newWebhookServer(t, cfg)

	elapsed, response := stallBody(t, handler, http.MethodPost, "/hooks/generic", nil)
	checkStalledRequestEnded(t, elapsed, response)
}

func TestStalledAPIBodyTimesOut(t *testing.T) {
	cfg := config.Default()
	cfg.Server.BodyReadTimeout = config.Duration(200 * time.Millisecond)
	cfg.AdminToken = "test-admin-token-0123456789abcdef"
	handler := New(cfg, nil, nil, testLogger()).Handler()

	elapsed, response := stallBody(t, handler, http.MethodPost, "/api/v1/sessions", map[string]string{
		"Authorization": "Bearer " + cfg.AdminToken,
	})
	checkStalledRequestEnded(t, elapsed, response)
}

// deadlineRecorder records SetReadDeadline calls so tests can see the
// deadline being set and then cleared. ResponseController looks for this
// method on the ResponseWriter.
type deadlineRecorder struct {
	http.ResponseWriter
	deadlines []time.Time
}

func (d *deadlineRecorder) SetReadDeadline(deadline time.Time) error {
	d.deadlines = append(d.deadlines, deadline)
	return nil
}

func TestReadBodyClearsDeadlineAfterSuccess(t *testing.T) {
	cfg := config.Default()
	cfg.Server.BodyReadTimeout = config.Duration(30 * time.Second)
	s := New(cfg, nil, nil, testLogger())

	rec := &deadlineRecorder{ResponseWriter: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"ok":true}`))
	before := time.Now()
	body, err := s.readBody(rec, req, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("body = %q", body)
	}
	if len(rec.deadlines) != 2 {
		t.Fatalf("SetReadDeadline calls = %v, want set then clear", rec.deadlines)
	}
	if rec.deadlines[0].IsZero() || rec.deadlines[0].Before(before) {
		t.Fatalf("first deadline = %v, want in the future", rec.deadlines[0])
	}
	if !rec.deadlines[1].IsZero() {
		t.Fatalf("second deadline = %v, want the zero time that clears it", rec.deadlines[1])
	}
}

// TestKeepAliveSucceedsAfterBodyDeadline proves the connection deadline is
// cleared after a successful body read. A leftover deadline would fail the
// next keep-alive request once the original timeout has passed.
func TestKeepAliveSucceedsAfterBodyDeadline(t *testing.T) {
	cfg := config.Default()
	cfg.Server.BodyReadTimeout = config.Duration(200 * time.Millisecond)
	cfg.Triggers = []config.TriggerConfig{{Name: "generic", Type: "generic", AllowUnsigned: true}}
	srv := httptest.NewServer(newWebhookServer(t, cfg))
	t.Cleanup(srv.Close)

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	send := func() int {
		t.Helper()
		body := `{"prompt":"x"}`
		req := fmt.Sprintf("POST /hooks/generic HTTP/1.1\r\nHost: example\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
		if _, err := io.WriteString(conn, req); err != nil {
			t.Fatal(err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}

	if first := send(); first >= 500 {
		t.Fatalf("first status = %d", first)
	}
	time.Sleep(500 * time.Millisecond)
	if second := send(); second >= 500 {
		t.Fatalf("second keep-alive request status = %d; the body read deadline was probably left on the connection", second)
	}
}
