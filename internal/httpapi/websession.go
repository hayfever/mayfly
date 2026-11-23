package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hayfever/mayfly/internal/config"
)

// The web UI exchanges an API token for a session cookie once. The token
// secret never reaches browser storage.
const (
	webSessionCookie = "mayfly_session"
	webCSRFCookie    = "mayfly_csrf"
	webCSRFHeader    = "X-Mayfly-CSRF"

	// webSessionTTL bounds a browser session. A token with an earlier
	// expires_at caps it further.
	webSessionTTL = 12 * time.Hour
	// webSessionLimit bounds the in-memory map. The oldest entry is evicted
	// when a new login would exceed it.
	webSessionLimit = 1024

	webSessionRoute = "/api/v1/auth/session"
)

// webSession is one authenticated browser session. The raw cookie value is
// never stored; the map key is its SHA-256.
type webSession struct {
	tokenName string
	csrf      string
	created   time.Time
	expires   time.Time
}

// webSessionStore holds browser sessions in memory. A control-plane restart
// clears every session, so the operator must sign in again. The chart runs one
// replica, so no peer needs the state.
type webSessionStore struct {
	mu       sync.Mutex
	sessions map[string]webSession
	// now is the clock. Tests replace it to exercise expiry.
	now func() time.Time
}

func newWebSessionStore() *webSessionStore {
	return &webSessionStore{sessions: map[string]webSession{}, now: time.Now}
}

// webSessionKey hashes a raw session id for map storage.
func webSessionKey(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// randomSecret returns n random bytes as hex. Hex keeps the value valid in a
// cookie header.
func randomSecret(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// create stores a session for tokenName and returns the raw id, which the
// caller puts in the cookie. Only the hash is kept.
func (s *webSessionStore) create(tokenName string, ttl time.Duration) (string, webSession, error) {
	id, err := randomSecret(32)
	if err != nil {
		return "", webSession{}, err
	}
	csrf, err := randomSecret(32)
	if err != nil {
		return "", webSession{}, err
	}
	now := s.now()
	session := webSession{
		tokenName: tokenName,
		csrf:      csrf,
		created:   now,
		expires:   now.Add(ttl),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(now)
	if len(s.sessions) >= webSessionLimit {
		s.evictOldestLocked()
	}
	s.sessions[webSessionKey(id)] = session
	return id, session, nil
}

func (s *webSessionStore) get(id string) (webSession, bool) {
	if id == "" {
		return webSession{}, false
	}
	key := webSessionKey(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[key]
	if !ok {
		return webSession{}, false
	}
	if !s.now().Before(session.expires) {
		delete(s.sessions, key)
		return webSession{}, false
	}
	return session, true
}

func (s *webSessionStore) delete(id string) {
	if id == "" {
		return
	}
	key := webSessionKey(id)
	s.mu.Lock()
	delete(s.sessions, key)
	s.mu.Unlock()
}

func (s *webSessionStore) sweepLocked(now time.Time) {
	for key, session := range s.sessions {
		if !now.Before(session.expires) {
			delete(s.sessions, key)
		}
	}
}

func (s *webSessionStore) evictOldestLocked() {
	oldestKey := ""
	var oldest time.Time
	for key, session := range s.sessions {
		if oldestKey == "" || session.created.Before(oldest) {
			oldestKey, oldest = key, session.created
		}
	}
	if oldestKey != "" {
		delete(s.sessions, oldestKey)
	}
}

// now is the clock for web-session auth.
func (s *Server) now() time.Time {
	if s.webSessions != nil && s.webSessions.now != nil {
		return s.webSessions.now()
	}
	return time.Now()
}

// webSession resolves the session cookie and the token it was created for. A
// token that was removed or has expired ends the session.
func (s *Server) webSession(r *http.Request) (string, config.APITokenConfig, webSession, bool) {
	cookie, err := r.Cookie(webSessionCookie)
	if err != nil || cookie.Value == "" {
		return "", config.APITokenConfig{}, webSession{}, false
	}
	session, ok := s.webSessions.get(cookie.Value)
	if !ok {
		return "", config.APITokenConfig{}, webSession{}, false
	}
	token, ok := s.tokenByName(session.tokenName)
	if !ok || token.Expired(s.now()) {
		s.webSessions.delete(cookie.Value)
		return "", config.APITokenConfig{}, webSession{}, false
	}
	return cookie.Value, token, session, true
}

// csrfValid checks the double-submit header and, when the browser sends one,
// the Origin header. A bearer request never reaches this function.
func (s *Server) csrfValid(r *http.Request, sessionCSRF string) bool {
	if sessionCSRF == "" {
		return false
	}
	header := r.Header.Get(webCSRFHeader)
	if subtle.ConstantTimeCompare([]byte(header), []byte(sessionCSRF)) != 1 {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		return s.originAllowed(r, origin)
	}
	return true
}

// originAllowed reports whether origin names this deployment. The configured
// public URL is authoritative when it names the origin, because a
// TLS-terminating proxy makes the internal request plain. Otherwise the
// request host decides; an https public URL still requires an https origin,
// while an unknown or plain public URL accepts either scheme for that host.
func (s *Server) originAllowed(r *http.Request, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	public, publicErr := url.Parse(strings.TrimSpace(s.cfg.Server.PublicURL))
	if publicErr == nil && public.Host != "" && u.Host == public.Host {
		return u.Scheme == public.Scheme
	}
	if u.Host != r.Host {
		return false
	}
	if r.TLS != nil {
		return u.Scheme == "https"
	}
	if publicErr == nil && public.Scheme == "https" {
		return u.Scheme == "https"
	}
	return true
}

// webCookiesSecure marks the cookies Secure when the request arrived over TLS
// or the deployment publishes an https public URL.
func (s *Server) webCookiesSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(s.cfg.Server.PublicURL)), "https://")
}

// setWebCookies writes both cookies. The CSRF cookie is readable by the UI,
// which copies it into the request header; the session cookie is not.
func (s *Server) setWebCookies(w http.ResponseWriter, r *http.Request, id, csrf string, maxAge int) {
	secure := s.webCookiesSecure(r)
	http.SetCookie(w, &http.Cookie{
		Name:     webSessionCookie,
		Value:    id,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     webCSRFCookie,
		Value:    csrf,
		Path:     "/",
		MaxAge:   maxAge,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
	})
}

// clearWebCookies expires both cookies.
func (s *Server) clearWebCookies(w http.ResponseWriter, r *http.Request) {
	s.setWebCookies(w, r, "", "", -1)
}

type webLoginRequest struct {
	Token string `json:"token"`
}

// webSessionResponse is the public view of one browser session. It never
// carries the token secret or the session id.
type webSessionResponse struct {
	Name      string    `json:"name"`
	Scopes    []string  `json:"scopes"`
	ExpiresAt time.Time `json:"expires_at"`
}

func newWebSessionResponse(token config.APITokenConfig, expires time.Time) webSessionResponse {
	scopes := make([]string, len(token.Scopes))
	copy(scopes, token.Scopes)
	return webSessionResponse{Name: token.Name, Scopes: scopes, ExpiresAt: expires}
}

// createWebSession exchanges an API token for the two cookies. The body is
// strict JSON, and the token is checked with the same constant-time comparison
// as a bearer header.
//
// Sign-in is itself protected against cross-site requests: the body must be
// application/json, which a cross-site HTML form cannot send without a CORS
// preflight, and an Origin header must name this deployment. Without these
// checks another site could sign a browser in with the attacker's token.
func (s *Server) createWebSession(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && !s.originAllowed(r, origin) {
		s.writeAudit(r, "unknown", authViaWeb, webSessionRoute, "", http.StatusForbidden)
		writeError(w, http.StatusForbidden, codeForbidden, "cross-site sign-in refused")
		return
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "Content-Type must be application/json")
		return
	}
	var req webLoginRequest
	if err := s.decodeJSONStrict(w, r, s.cfg.Server.MaxBodyBytes, &req); err != nil {
		writeBodyError(w, err)
		return
	}
	now := s.now()
	token, found := s.matchToken(req.Token)
	if !found || token.Expired(now) {
		name := "unknown"
		if found {
			name = token.Name
		}
		s.refuseAuth(w, r, name, authViaWeb, webSessionRoute, "")
		return
	}
	ttl := webSessionTTL
	if token.ExpiresAt != nil {
		if remaining := token.ExpiresAt.Sub(now); remaining < ttl {
			ttl = remaining
		}
	}
	if ttl < time.Second {
		// The token expires before a session cookie is useful.
		s.refuseAuth(w, r, token.Name, authViaWeb, webSessionRoute, "")
		return
	}
	// Replace a previous session instead of leaving it live until it expires.
	if old, err := r.Cookie(webSessionCookie); err == nil {
		s.webSessions.delete(old.Value)
	}
	id, session, err := s.webSessions.create(token.Name, ttl)
	if err != nil {
		s.logger.Error("web session create failed", "error", err)
		writeError(w, http.StatusInternalServerError, codeInternal, "could not create a web session")
		return
	}
	s.setWebCookies(w, r, id, session.csrf, int(ttl/time.Second))
	s.writeAudit(r, token.Name, authViaWeb, webSessionRoute, "", http.StatusCreated)
	writeJSON(w, http.StatusCreated, newWebSessionResponse(token, session.expires))
}

// getWebSession reports the cookie session, or 401 when there is none.
func (s *Server) getWebSession(w http.ResponseWriter, r *http.Request) {
	token, session, ok := s.webSessionToken(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, codeUnauthenticated, "no web session")
		return
	}
	writeJSON(w, http.StatusOK, newWebSessionResponse(token, session.expires))
}

// deleteWebSession ends the cookie session. A mutating cookie request needs
// the CSRF header, and an Origin header must match the deployment.
func (s *Server) deleteWebSession(w http.ResponseWriter, r *http.Request) {
	id, token, session, ok := s.webSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, codeUnauthenticated, "no web session")
		return
	}
	if !s.csrfValid(r, session.csrf) {
		s.writeAudit(r, token.Name, authViaWeb, webSessionRoute, "", http.StatusForbidden)
		writeError(w, http.StatusForbidden, codeForbidden, "csrf check failed")
		return
	}
	s.webSessions.delete(id)
	s.clearWebCookies(w, r)
	s.writeAudit(r, token.Name, authViaWeb, webSessionRoute, "", http.StatusNoContent)
	w.WriteHeader(http.StatusNoContent)
}

// webSessionToken is webSession without the raw id.
func (s *Server) webSessionToken(r *http.Request) (config.APITokenConfig, webSession, bool) {
	_, token, session, ok := s.webSession(r)
	return token, session, ok
}
