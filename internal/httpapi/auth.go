package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
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

// authenticate resolves the bearer token against the configured set. Every
// token is compared in constant time, and the loop never stops early, so the
// position of a match does not change the work done.
func (s *Server) authenticate(r *http.Request) (config.APITokenConfig, bool) {
	presented := bearerToken(r)
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
	if found == 0 || match.Expired(time.Now()) {
		return config.APITokenConfig{}, false
	}
	return match, true
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
		token, ok := s.authenticate(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthenticated, "missing or invalid bearer token")
			return
		}
		if !token.HasScope(route.scope) {
			if isMutating(r.Method) {
				s.writeAudit(r, token.Name, path, auditTarget(r), http.StatusForbidden)
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
			s.writeAudit(r, token.Name, path, target, status)
		}()
		route.handler(recorder, r)
		done = true
	}
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
// token secret never appears in a log line.
func (s *Server) writeAudit(r *http.Request, tokenName, route, target string, status int) {
	s.logger.Info("audit",
		"token_name", tokenName,
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
