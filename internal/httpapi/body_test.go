package httpapi

import (
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
