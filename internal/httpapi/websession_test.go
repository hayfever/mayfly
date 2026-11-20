package httpapi

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/config"
)

func webTestServer(t *testing.T, publicURL string, tokens ...config.APITokenConfig) (*Server, http.Handler) {
	t.Helper()
	cfg := config.Default()
	cfg.Server.PublicURL = publicURL
	cfg.Server.Tokens = tokens
	if len(tokens) == 0 {
		cfg.Server.Tokens = []config.APITokenConfig{{Name: "web", Scopes: []string{config.ScopeAdmin}, Secret: "web-secret"}}
	}
	s := New(cfg, nil, nil, testLogger())
	return s, s.Handler()
}

type loginCookies struct {
	session *http.Cookie
	csrf    *http.Cookie
}

// login posts a token to the session route and returns the cookies the
// response set.
func login(t *testing.T, handler http.Handler, token string) (*httptest.ResponseRecorder, loginCookies) {
	t.Helper()
	body, err := json.Marshal(webLoginRequest{Token: token})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, webSessionRoute, strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var cookies loginCookies
	for _, cookie := range rec.Result().Cookies() {
		switch cookie.Name {
		case webSessionCookie:
			cookies.session = cookie
		case webCSRFCookie:
			cookies.csrf = cookie
		}
	}
	return rec, cookies
}

func (c loginCookies) attach(req *http.Request) {
	if c.session != nil {
		req.AddCookie(c.session)
	}
	if c.csrf != nil {
		req.AddCookie(c.csrf)
	}
}

// setClock pins the session clock to the value now points at, so a test can
// advance it.
func setClock(s *Server, now *time.Time) {
	s.webSessions.now = func() time.Time { return *now }
}

func TestWebLoginSetsCookies(t *testing.T) {
	_, handler := webTestServer(t, "http://localhost:8080")
	rec, cookies := login(t, handler, "web-secret")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if cookies.session == nil || cookies.csrf == nil {
		t.Fatalf("cookies = %+v", cookies)
	}
	if cookies.session.HttpOnly != true {
		t.Error("session cookie is not HttpOnly")
	}
	if cookies.csrf.HttpOnly {
		t.Error("CSRF cookie is HttpOnly, so the page cannot read it")
	}
	for _, cookie := range []*http.Cookie{cookies.session, cookies.csrf} {
		if cookie.SameSite != http.SameSiteStrictMode {
			t.Errorf("%s SameSite = %v", cookie.Name, cookie.SameSite)
		}
		if cookie.Path != "/" {
			t.Errorf("%s path = %q", cookie.Name, cookie.Path)
		}
		if cookie.Secure {
			t.Errorf("%s is Secure on a plain request", cookie.Name)
		}
		if cookie.MaxAge != int(webSessionTTL/time.Second) {
			t.Errorf("%s MaxAge = %d, want %d", cookie.Name, cookie.MaxAge, int(webSessionTTL/time.Second))
		}
	}
	if cookies.session.Value == "" || cookies.session.Value == cookies.csrf.Value {
		t.Fatalf("session and CSRF values are not distinct: %q %q", cookies.session.Value, cookies.csrf.Value)
	}

	var body webSessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.Name != "web" || len(body.Scopes) != 1 || body.Scopes[0] != config.ScopeAdmin {
		t.Fatalf("body = %+v", body)
	}
	if body.ExpiresAt.IsZero() {
		t.Fatal("body has no expires_at")
	}
}

func TestWebLoginSecureCookies(t *testing.T) {
	_, handler := webTestServer(t, "https://mayfly.example")
	_, cookies := login(t, handler, "web-secret")
	if cookies.session == nil || !cookies.session.Secure || !cookies.csrf.Secure {
		t.Fatalf("cookies are not Secure behind https: %+v", cookies)
	}

	// A direct TLS request also marks the cookies Secure.
	cfg := config.Default()
	cfg.Server.PublicURL = "http://localhost:8080"
	cfg.Server.Tokens = []config.APITokenConfig{{Name: "web", Scopes: []string{config.ScopeAdmin}, Secret: "web-secret"}}
	s := New(cfg, nil, nil, testLogger())
	req := httptest.NewRequest(http.MethodPost, webSessionRoute, strings.NewReader(`{"token":"web-secret"}`))
	req.TLS = &tls.ConnectionState{}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	for _, cookie := range rec.Result().Cookies() {
		if !cookie.Secure {
			t.Errorf("%s is not Secure over TLS", cookie.Name)
		}
	}
}

func TestWebLoginRejectsBadTokens(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	tokens := []config.APITokenConfig{
		{Name: "web", Scopes: []string{config.ScopeAdmin}, Secret: "web-secret"},
		{Name: "old", Scopes: []string{config.ScopeAdmin}, Secret: "old-secret", ExpiresAt: &past},
	}
	_, handler := webTestServer(t, "http://localhost:8080", tokens...)
	for _, token := range []string{"wrong", "", "old-secret"} {
		rec, cookies := login(t, handler, token)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("token %q status = %d body=%s", token, rec.Code, rec.Body.String())
		}
		if body := decodeEnvelope(t, rec); body.Code != codeUnauthenticated {
			t.Errorf("token %q code = %q", token, body.Code)
		}
		if cookies.session != nil || cookies.csrf != nil {
			t.Errorf("token %q set a cookie: %+v", token, cookies)
		}
	}

	req := httptest.NewRequest(http.MethodPost, webSessionRoute, strings.NewReader(`{"token":`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body status = %d", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("malformed body set a cookie")
	}
}

func TestCookieAuthEnforcesScopes(t *testing.T) {
	tokens := []config.APITokenConfig{{Name: "reader", Scopes: []string{config.ScopeSessionsRead}, Secret: "reader-secret"}}
	s, handler := webTestServer(t, "http://localhost:8080", tokens...)
	_, cookies := login(t, handler, "reader-secret")
	if cookies.session == nil {
		t.Fatal("login set no cookie")
	}

	read := s.authorizedRoute(apiRoute{pattern: "GET /api/v1/sessions", scope: config.ScopeSessionsRead, handler: func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	cookies.attach(req)
	rec := httptest.NewRecorder()
	read.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("cookie read status = %d body=%s", rec.Code, rec.Body.String())
	}

	write := s.authorizedRoute(apiRoute{pattern: "DELETE /api/v1/sessions/{id}", scope: config.ScopeSessionsWrite, handler: func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler ran without the write scope")
	}})
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/sess-1", nil)
	cookies.attach(req)
	req.Header.Set(webCSRFHeader, cookies.csrf.Value)
	rec = httptest.NewRecorder()
	write.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cookie write status = %d", rec.Code)
	}
	if body := decodeEnvelope(t, rec); body.Code != codeForbidden || !strings.Contains(body.Message, "scope") {
		t.Fatalf("body = %+v", body)
	}
}

func TestCookieMutationRequiresCSRF(t *testing.T) {
	tokens := []config.APITokenConfig{{Name: "writer", Scopes: []string{config.ScopeSessionsWrite}, Secret: "writer-secret"}}
	s, handler := webTestServer(t, "http://localhost:8080", tokens...)
	_, cookies := login(t, handler, "writer-secret")
	route := s.authorizedRoute(apiRoute{pattern: "POST /api/v1/sessions", scope: config.ScopeSessionsWrite, handler: func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}})

	cases := []struct {
		name   string
		header string
		origin string
		want   int
	}{
		{"no header", "", "", http.StatusForbidden},
		{"wrong header", "not-the-token", "", http.StatusForbidden},
		{"right header", cookies.csrf.Value, "", http.StatusNoContent},
		{"cross-site origin", cookies.csrf.Value, "https://evil.example", http.StatusForbidden},
		{"same host origin", cookies.csrf.Value, "http://example.com", http.StatusNoContent},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", nil)
		cookies.attach(req)
		if tc.header != "" {
			req.Header.Set(webCSRFHeader, tc.header)
		}
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		rec := httptest.NewRecorder()
		route.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s = %d, want %d body=%s", tc.name, rec.Code, tc.want, rec.Body.String())
		}
		if tc.want == http.StatusForbidden {
			if body := decodeEnvelope(t, rec); body.Code != codeForbidden {
				t.Errorf("%s code = %q", tc.name, body.Code)
			}
		}
	}

	// The public URL decides the scheme when TLS ends at a proxy.
	s.cfg.Server.PublicURL = "https://mayfly.example"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", nil)
	cookies.attach(req)
	req.Header.Set(webCSRFHeader, cookies.csrf.Value)
	req.Header.Set("Origin", "https://mayfly.example")
	rec := httptest.NewRecorder()
	route.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("public origin status = %d body=%s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/api/v1/sessions", nil)
	cookies.attach(req)
	req.Header.Set(webCSRFHeader, cookies.csrf.Value)
	req.Header.Set("Origin", "http://mayfly.example")
	rec = httptest.NewRecorder()
	route.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("downgraded origin status = %d", rec.Code)
	}

	// With no published https URL, a TLS-terminating proxy leaves the internal
	// request plain, so the matching host is enough.
	s.cfg.Server.PublicURL = "http://localhost:8080"
	req = httptest.NewRequest(http.MethodPost, "/api/v1/sessions", nil)
	cookies.attach(req)
	req.Header.Set(webCSRFHeader, cookies.csrf.Value)
	req.Header.Set("Origin", "https://example.com")
	rec = httptest.NewRecorder()
	route.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("proxy https origin status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCookieAuthThroughHandler(t *testing.T) {
	tokens := []config.APITokenConfig{{Name: "reader", Scopes: []string{config.ScopeSchedulesRead}, Secret: "reader-secret"}}
	_, handler := webTestServer(t, "http://localhost:8080", tokens...)
	_, cookies := login(t, handler, "reader-secret")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/schedules", nil)
	cookies.attach(req)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cookie read status = %d body=%s", rec.Code, rec.Body.String())
	}

	// A cookie write through the real router needs the CSRF header.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/schedules/nightly/run", nil)
	cookies.attach(req)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cookie write without CSRF = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBearerAuthUnchanged(t *testing.T) {
	tokens := []config.APITokenConfig{{Name: "writer", Scopes: []string{config.ScopeSessionsWrite}, Secret: "writer-secret"}}
	s, handler := webTestServer(t, "http://localhost:8080", tokens...)
	_, cookies := login(t, handler, "writer-secret")
	route := s.authorizedRoute(apiRoute{pattern: "POST /api/v1/sessions", scope: config.ScopeSessionsWrite, handler: func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}})

	// A bearer request needs no CSRF header.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer writer-secret")
	rec := httptest.NewRecorder()
	route.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("bearer status = %d", rec.Code)
	}

	// A wrong bearer token is not rescued by a valid cookie.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/sessions", nil)
	cookies.attach(req)
	req.Header.Set("Authorization", "Bearer wrong-secret")
	req.Header.Set(webCSRFHeader, cookies.csrf.Value)
	rec = httptest.NewRecorder()
	route.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer with cookie = %d, want 401", rec.Code)
	}
}

func TestDeleteWebSession(t *testing.T) {
	tokens := []config.APITokenConfig{{Name: "web", Scopes: []string{config.ScopeAdmin}, Secret: "web-secret"}}
	_, handler := webTestServer(t, "http://localhost:8080", tokens...)
	_, cookies := login(t, handler, "web-secret")

	get := func(c loginCookies) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, webSessionRoute, nil)
		c.attach(req)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := get(cookies); rec.Code != http.StatusOK {
		t.Fatalf("session get status = %d", rec.Code)
	}

	// A logout without the CSRF header leaves the session in place.
	req := httptest.NewRequest(http.MethodDelete, webSessionRoute, nil)
	cookies.attach(req)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("logout without CSRF = %d", rec.Code)
	}
	if rec := get(cookies); rec.Code != http.StatusOK {
		t.Fatalf("session ended without CSRF: %d", rec.Code)
	}

	// A logout without a session is unauthenticated.
	req = httptest.NewRequest(http.MethodDelete, webSessionRoute, nil)
	req.Header.Set(webCSRFHeader, cookies.csrf.Value)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("logout without a session = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodDelete, webSessionRoute, nil)
	cookies.attach(req)
	req.Header.Set(webCSRFHeader, cookies.csrf.Value)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d body=%s", rec.Code, rec.Body.String())
	}
	cleared := 0
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name != webSessionCookie && cookie.Name != webCSRFCookie {
			continue
		}
		cleared++
		if cookie.Value != "" || cookie.MaxAge > 0 {
			t.Errorf("%s was not expired: %+v", cookie.Name, cookie)
		}
	}
	if cleared != 2 {
		t.Fatalf("expired cookies = %d, want 2", cleared)
	}
	if rec := get(cookies); rec.Code != http.StatusUnauthorized {
		t.Fatalf("session survived logout: %d", rec.Code)
	}
}

func TestWebSessionExpiry(t *testing.T) {
	future := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	tokens := []config.APITokenConfig{{Name: "web", Scopes: []string{config.ScopeAdmin}, Secret: "web-secret"}}
	s, handler := webTestServer(t, "http://localhost:8080", tokens...)
	now := future
	setClock(s, &now)
	_, cookies := login(t, handler, "web-secret")

	get := func() int {
		req := httptest.NewRequest(http.MethodGet, webSessionRoute, nil)
		cookies.attach(req)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := get(); code != http.StatusOK {
		t.Fatalf("fresh session = %d", code)
	}
	now = future.Add(webSessionTTL - time.Second)
	if code := get(); code != http.StatusOK {
		t.Fatalf("session before expiry = %d", code)
	}
	now = future.Add(webSessionTTL)
	if code := get(); code != http.StatusUnauthorized {
		t.Fatalf("session at expiry = %d", code)
	}
}

func TestWebSessionCappedByTokenExpiry(t *testing.T) {
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	expiry := now.Add(time.Hour)
	tokens := []config.APITokenConfig{{Name: "short", Scopes: []string{config.ScopeAdmin}, Secret: "short-secret", ExpiresAt: &expiry}}
	s, handler := webTestServer(t, "http://localhost:8080", tokens...)
	setClock(s, &now)
	rec, cookies := login(t, handler, "short-secret")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	if cookies.session.MaxAge != 3600 {
		t.Fatalf("MaxAge = %d, want 3600", cookies.session.MaxAge)
	}
	var body webSessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.ExpiresAt.Equal(expiry) {
		t.Fatalf("expires_at = %s, want %s", body.ExpiresAt, expiry)
	}
	now = expiry
	req := httptest.NewRequest(http.MethodGet, webSessionRoute, nil)
	cookies.attach(req)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("session past the token expiry = %d", rec.Code)
	}
}

func TestWebSessionEndsWhenTokenIsRemovedOrExpires(t *testing.T) {
	tokens := []config.APITokenConfig{{Name: "web", Scopes: []string{config.ScopeAdmin}, Secret: "web-secret"}}
	s, handler := webTestServer(t, "http://localhost:8080", tokens...)
	_, cookies := login(t, handler, "web-secret")

	get := func() int {
		req := httptest.NewRequest(http.MethodGet, webSessionRoute, nil)
		cookies.attach(req)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := get(); code != http.StatusOK {
		t.Fatalf("session = %d", code)
	}

	past := time.Now().Add(-time.Minute)
	s.tokens = []config.APITokenConfig{{Name: "web", Scopes: []string{config.ScopeAdmin}, Secret: "web-secret", ExpiresAt: &past}}
	if code := get(); code != http.StatusUnauthorized {
		t.Fatalf("expired token session = %d", code)
	}

	s.tokens = nil
	_, cookies = login(t, handler, "web-secret")
	if cookies.session != nil {
		t.Fatal("login succeeded after the token was removed")
	}
}

func TestWebSessionStoresOnlyAHash(t *testing.T) {
	s, handler := webTestServer(t, "http://localhost:8080")
	_, cookies := login(t, handler, "web-secret")
	if cookies.session == nil {
		t.Fatal("login set no cookie")
	}
	raw := cookies.session.Value

	s.webSessions.mu.Lock()
	defer s.webSessions.mu.Unlock()
	if len(s.webSessions.sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(s.webSessions.sessions))
	}
	for key, session := range s.webSessions.sessions {
		if key == raw {
			t.Fatal("the map is keyed by the raw session id")
		}
		if len(key) != 64 || strings.Contains(key, raw) {
			t.Fatalf("map key %q is not a SHA-256 hex digest", key)
		}
		if session.csrf == raw {
			t.Fatal("CSRF token is the session id")
		}
	}
}

func TestWebLoginAudit(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	cfg := config.Default()
	cfg.Server.Tokens = []config.APITokenConfig{{Name: "web", Scopes: []string{config.ScopeAdmin}, Secret: "web-secret"}}
	s := New(cfg, nil, nil, logger)
	handler := s.Handler()

	post := func(body string) {
		req := httptest.NewRequest(http.MethodPost, webSessionRoute, strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}

	post(`{"token":"not-the-secret"}`)
	lines := auditLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("failed login audit lines = %d: %s", len(lines), buf.String())
	}
	if lines[0]["token_name"] != "unknown" || lines[0]["via"] != authViaWeb || lines[0]["status"] != float64(http.StatusUnauthorized) {
		t.Fatalf("failed login audit line = %+v", lines[0])
	}
	if strings.Contains(buf.String(), "not-the-secret") {
		t.Fatal("failed login audit line contains the presented secret")
	}

	buf.Reset()
	post(`{"token":"web-secret"}`)
	lines = auditLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("successful login audit lines = %d: %s", len(lines), buf.String())
	}
	if lines[0]["token_name"] != "web" || lines[0]["via"] != authViaWeb || lines[0]["status"] != float64(http.StatusCreated) {
		t.Fatalf("successful login audit line = %+v", lines[0])
	}
	if strings.Contains(buf.String(), "web-secret") {
		t.Fatal("successful login audit line contains the token secret")
	}
}

func TestWebSessionStoreBoundsAndSweeps(t *testing.T) {
	store := newWebSessionStore()
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }

	first, _, err := store.create("web", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var last string
	for range webSessionLimit {
		now = now.Add(time.Nanosecond)
		last, _, err = store.create("web", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(store.sessions) != webSessionLimit {
		t.Fatalf("sessions = %d, want %d", len(store.sessions), webSessionLimit)
	}
	if _, ok := store.get(first); ok {
		t.Fatal("the oldest session survived the cap")
	}
	if _, ok := store.get(last); !ok {
		t.Fatal("the newest session is missing")
	}

	// A create after the TTL removes every expired entry.
	now = now.Add(2 * time.Hour)
	if _, _, err := store.create("web", time.Hour); err != nil {
		t.Fatal(err)
	}
	if len(store.sessions) != 1 {
		t.Fatalf("sessions after sweep = %d, want 1", len(store.sessions))
	}
}

func TestWebBundleHasNoTokenStorage(t *testing.T) {
	_, handler := webTestServer(t, "http://localhost:8080")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="token-input"`) {
		t.Fatalf("index status = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/app.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("app.js status = %d", rec.Code)
	}
	script := rec.Body.String()
	for _, forbidden := range []string{`localStorage.setItem("mayfly.token"`, `state.token`, `Authorization`, `"Bearer "`} {
		if strings.Contains(script, forbidden) {
			t.Errorf("app.js still contains %q", forbidden)
		}
	}
	for _, required := range []string{`localStorage.removeItem("mayfly.token")`, `/api/v1/auth/session`, `X-Mayfly-CSRF`} {
		if !strings.Contains(script, required) {
			t.Errorf("app.js does not contain %q", required)
		}
	}
}
