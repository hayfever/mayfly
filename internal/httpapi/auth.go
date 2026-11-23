package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hayfever/mayfly/internal/config"
)

// maxRequestIDLength bounds a caller-supplied X-Request-Id. Longer values are
// replaced, so a client cannot flood a log line.
const maxRequestIDLength = 64

type requestIDKey struct{}

// validRequestID accepts a short printable token so a request id cannot inject
// into a log line.
func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLength {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

func newRequestID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}

// requestID returns the id that observeRequests attached, or an empty string
// when the middleware is absent.
func requestID(r *http.Request) string {
	id, _ := r.Context().Value(requestIDKey{}).(string)
	return id
}

// auditRecord carries the object id a mutating handler acted on when the id is
// not part of the route, such as a newly created session.
type auditRecord struct {
	target string
}

type auditRecordKey struct{}

// setAuditTarget names the object the current mutating request acted on. It is
// a no-op on requests that are not audited.
func setAuditTarget(r *http.Request, id string) {
	if id == "" {
		return
	}
	if record, ok := r.Context().Value(auditRecordKey{}).(*auditRecord); ok {
		record.target = id
	}
}

// How a request authenticated. An audit line records it in the via field.
const (
	authViaBearer = "bearer"
	authViaWeb    = "web"
)

// authInfo is the resolved caller of one request.
type authInfo struct {
	token config.APITokenConfig
	// via is authViaBearer or authViaWeb.
	via string
	// csrf is the session token that a mutating cookie request must echo.
	csrf string
}

// authenticate resolves the request credential. A Bearer header always decides
// on its own: a wrong bearer token is never rescued by a valid cookie.
// Otherwise a web session cookie resolves to the API token it was created for.
func (s *Server) authenticate(r *http.Request) (config.APITokenConfig, bool) {
	info, ok := s.authenticateRequest(r)
	return info.token, ok
}

func (s *Server) authenticateRequest(r *http.Request) (authInfo, bool) {
	if presented := bearerToken(r); presented != "" {
		token, ok := s.matchToken(presented)
		if !ok || token.Expired(s.now()) {
			return authInfo{}, false
		}
		return authInfo{token: token, via: authViaBearer}, true
	}
	_, token, session, ok := s.webSession(r)
	if !ok {
		return authInfo{}, false
	}
	return authInfo{token: token, via: authViaWeb, csrf: session.csrf}, true
}

// matchToken returns the token whose secret equals presented. Every token is
// compared in constant time, and the loop never stops early, so the position
// of a match does not change the work done.
func (s *Server) matchToken(presented string) (config.APITokenConfig, bool) {
	if presented == "" {
		return config.APITokenConfig{}, false
	}
	var match config.APITokenConfig
	found := 0
	for _, token := range s.tokens {
		if subtle.ConstantTimeCompare([]byte(presented), []byte(token.Secret)) == 1 {
			match = token
			found = 1
		}
	}
	return match, found == 1
}

// tokenByName finds the configured token a web session refers to. A missing
// token ends the session.
func (s *Server) tokenByName(name string) (config.APITokenConfig, bool) {
	for _, token := range s.tokens {
		if token.Name == name {
			return token, true
		}
	}
	return config.APITokenConfig{}, false
}

// authorizedRoute registers one /api/v1 route. A route without a scope is
// authenticated by its own handler. A mutating request writes one audit line:
// after the handler runs, or before the 403 response when the token lacks the
// scope.
func (s *Server) authorizedRoute(route apiRoute) http.HandlerFunc {
	path := routePath(route.pattern)
	return func(w http.ResponseWriter, r *http.Request) {
		if route.scope == "" {
			route.handler(w, r)
			return
		}
		info, ok := s.authenticateRequest(r)
		if !ok {
			s.refuseAuth(w, r, "unknown", authFailureVia(r), path, auditTarget(r))
			return
		}
		token := info.token
		if info.via == authViaWeb && isMutating(r.Method) && !s.csrfValid(r, info.csrf) {
			s.writeAudit(r, token.Name, info.via, path, auditTarget(r), http.StatusForbidden)
			writeError(w, http.StatusForbidden, codeForbidden, "csrf check failed")
			return
		}
		if !token.HasScope(route.scope) {
			if isMutating(r.Method) {
				s.writeAudit(r, token.Name, info.via, path, auditTarget(r), http.StatusForbidden)
			}
			writeError(w, http.StatusForbidden, codeForbidden, "token lacks the required scope: "+route.scope)
			return
		}
		if !isMutating(r.Method) {
			route.handler(w, r)
			return
		}
		record := &auditRecord{}
		r = r.WithContext(context.WithValue(r.Context(), auditRecordKey{}, record))
		recorder := &statusRecorder{ResponseWriter: w}
		done := false
		defer func() {
			status := recorder.status
			if status == 0 {
				status = http.StatusOK
				if !done {
					status = http.StatusInternalServerError
				}
			}
			target := record.target
			if target == "" {
				target = auditTarget(r)
			}
			s.writeAudit(r, token.Name, info.via, path, target, status)
		}()
		route.handler(recorder, r)
		done = true
	}
}

// authFailureVia names the credential a failed request presented. A bearer
// header decides on its own, as it does for authentication; a web session
// cookie is the only other credential a scoped route accepts.
func authFailureVia(r *http.Request) string {
	if bearerToken(r) != "" {
		return authViaBearer
	}
	if _, err := r.Cookie(webSessionCookie); err == nil {
		return authViaWeb
	}
	return authViaBearer
}

// refuseAuth records one failed authentication and writes its response. It
// audits the failure, counts it, and applies the per-client failure limiter.
// Only failures reach this path, so a valid credential is never limited.
func (s *Server) refuseAuth(w http.ResponseWriter, r *http.Request, tokenName, via, route, target string) {
	s.metrics.AuthFailure(via)
	allowed, retry := s.authFailures.fail(s.authLimiterKey(r))
	status := http.StatusUnauthorized
	if !allowed {
		status = http.StatusTooManyRequests
	}
	s.writeAudit(r, tokenName, via, route, target, status)
	if !allowed {
		w.Header().Set("Retry-After", retryAfterSeconds(retry))
		writeError(w, status, codeRateLimited, "too many failed authentication attempts")
		return
	}
	message := "missing or invalid bearer token"
	if via == authViaWeb {
		message = "missing or invalid web session"
	}
	writeError(w, status, codeUnauthenticated, message)
}

// retryAfterSeconds renders a wait as whole seconds, rounded up.
func retryAfterSeconds(wait time.Duration) string {
	seconds := int(math.Ceil(wait.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	return strconv.Itoa(seconds)
}

// isMutating reports whether a method changes state and needs an audit line.
func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// routePath drops the method from a route pattern.
func routePath(pattern string) string {
	if _, path, ok := strings.Cut(pattern, " "); ok {
		return path
	}
	return pattern
}

// auditTarget names the object in the path that the request acted on.
func auditTarget(r *http.Request) string {
	if id := r.PathValue("id"); id != "" {
		return id
	}
	return r.PathValue("name")
}

// writeAudit writes one audit line. The token name identifies the caller; the
// token secret never appears in a log line. via records whether the caller used
// a bearer header or the web session cookie.
func (s *Server) writeAudit(r *http.Request, tokenName, via, route, target string, status int) {
	s.audit.Info("audit",
		"token_name", tokenName,
		"via", via,
		"method", r.Method,
		"route", route,
		"target", target,
		"status", status,
		"request_id", requestID(r),
	)
}

// statusRecorder keeps the status code for the audit line. Mutating routes
// return JSON, so the recorder does not forward Flush or Hijack.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status != 0 {
		return
	}
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(p)
}

// Unwrap lets the response controller reach the connection, so a handler can
// set a body read deadline through this wrapper.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
