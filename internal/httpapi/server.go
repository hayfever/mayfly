// Package httpapi exposes the control plane HTTP API, web UI, webhook
// receivers, and internal runtime endpoints.
package httpapi

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hayfever/mayfly/internal/broker"
	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/gateway"
	"github.com/hayfever/mayfly/internal/metrics"
	"github.com/hayfever/mayfly/internal/queue"
	"github.com/hayfever/mayfly/internal/ratelimit"
	"github.com/hayfever/mayfly/internal/session"
)

//go:embed web/*
var webAssets embed.FS

type ticket struct {
	sessionID string
	actor     string
	expires   time.Time
}

type ticketStore struct {
	mu      sync.Mutex
	tickets map[string]ticket
}

func newTicketStore() *ticketStore {
	return &ticketStore{tickets: map[string]ticket{}}
}

func (t *ticketStore) issue(sessionID, actor string, ttl time.Duration) (string, time.Time) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	token := hex.EncodeToString(buf)
	expires := time.Now().Add(ttl)
	t.mu.Lock()
	for key, value := range t.tickets {
		if time.Now().After(value.expires) {
			delete(t.tickets, key)
		}
	}
	t.tickets[token] = ticket{sessionID: sessionID, actor: actor, expires: expires}
	t.mu.Unlock()
	return token, expires
}

func (t *ticketStore) consume(token, sessionID string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	value, ok := t.tickets[token]
	if !ok {
		return "", false
	}
	delete(t.tickets, token)
	if value.sessionID != sessionID || time.Now().After(value.expires) {
		return "", false
	}
	return value.actor, true
}

// Server is the HTTP layer.
type Server struct {
	cfg         *config.Config
	mgr         *session.Manager
	queue       queue.Store
	broker      *broker.Broker
	gateway     *gateway.Gateway
	logger      *slog.Logger
	tokens      []config.APITokenConfig
	triggers    map[string]config.TriggerConfig
	limiters    map[string]*ratelimit.Limiter
	mcpServers  map[string]config.MCPServerConfig
	tickets     *ticketStore
	webSessions *webSessionStore
	web         fs.FS
	upgrader    websocket.Upgrader
	schedules   *ScheduleService
	readyCheck  func() error
	metrics     *metrics.Metrics
}

// New builds the HTTP server. queue may be nil in tests that do not exercise
// webhooks.
func New(cfg *config.Config, mgr *session.Manager, q queue.Store, logger *slog.Logger, opts ...Option) *Server {
	sub, _ := fs.Sub(webAssets, "web")
	triggersByName := map[string]config.TriggerConfig{}
	limiters := map[string]*ratelimit.Limiter{}
	for _, t := range cfg.Triggers {
		triggersByName[t.Name] = t
		if t.RateLimit != nil {
			limiters[t.Name] = ratelimit.New(t.RateLimit.Requests, t.RateLimit.Interval.Std(), t.RateLimit.Burst)
		}
	}
	mcpServers := map[string]config.MCPServerConfig{}
	for _, m := range cfg.MCP.Servers {
		mcpServers[m.Name] = m
	}
	server := &Server{
		cfg:         cfg,
		mgr:         mgr,
		queue:       q,
		logger:      logger,
		tokens:      cfg.APITokens(),
		triggers:    triggersByName,
		limiters:    limiters,
		mcpServers:  mcpServers,
		tickets:     newTicketStore(),
		webSessions: newWebSessionStore(),
		web:         sub,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if origin == "" {
					return true
				}
				return strings.HasSuffix(origin, "://"+r.Host)
			},
		},
	}
	for _, opt := range opts {
		opt(server)
	}
	if server.broker == nil {
		server.broker = broker.New(cfg.LLM, logger)
	}
	return server
}

// WithBroker reuses one broker, so the HTTP API and the tool gateway reach
// upstream servers through the same transport policy and the same credentials.
func WithBroker(b *broker.Broker) Option {
	return func(s *Server) {
		if b != nil {
			s.broker = b
		}
	}
}

// WithGateway gives the API the tool gateway that the internal tool routes
// serve. Without it those routes report that the gateway is not configured.
func WithGateway(g *gateway.Gateway) Option {
	return func(s *Server) {
		s.gateway = g
	}
}

// WithMetrics gives the server the registry it records requests in and, when
// server.metrics has no separate address, the endpoint it serves.
func WithMetrics(m *metrics.Metrics) Option {
	return func(s *Server) {
		s.metrics = m
	}
}

// WithReadyCheck adds a startup dependency to /readyz. An error keeps the
// replica out of the Service until the dependency recovers.
func WithReadyCheck(check func() error) Option {
	return func(s *Server) {
		s.readyCheck = check
	}
}

// apiRoute is one registered admin API route. The table drives routing, token
// scopes, and the spec drift test, so a route cannot exist without a matching
// OpenAPI operation and an explicit scope.
type apiRoute struct {
	pattern string
	// scope is the API token scope the route requires. An empty scope means
	// the handler authenticates the request itself: the attach WebSocket uses
	// a one-time ticket, and the auth session routes use the session cookie.
	scope   string
	handler http.HandlerFunc
}

// apiRoutes returns the /api/v1 surface. Every entry must appear in
// api/openapi.yaml; internal_test.go fails when the two disagree.
func (s *Server) apiRoutes() []apiRoute {
	return []apiRoute{
		{"POST /api/v1/sessions", config.ScopeSessionsWrite, s.createSession},
		{"GET /api/v1/sessions", config.ScopeSessionsRead, s.listSessions},
		{"GET /api/v1/sessions/{id}", config.ScopeSessionsRead, s.getSession},
		{"DELETE /api/v1/sessions/{id}", config.ScopeSessionsWrite, s.deleteSession},
		{"GET /api/v1/sessions/{id}/events", config.ScopeSessionsRead, s.sessionEvents},
		{"POST /api/v1/sessions/{id}/messages", config.ScopeSessionsWrite, s.postMessage},
		{"POST /api/v1/sessions/{id}/close", config.ScopeSessionsWrite, s.closeSession},
		{"POST /api/v1/sessions/{id}/attach-ticket", config.ScopeSessionsWrite, s.createTicket},
		{"GET /api/v1/sessions/{id}/artifact", config.ScopeSessionsRead, s.downloadArtifact},
		{"GET /api/v1/sessions/{id}/ws", "", s.attachWebSocket},
		{"GET /api/v1/config", config.ScopeSessionsRead, s.publicConfig},
		{"POST /api/v1/auth/session", "", s.createWebSession},
		{"GET /api/v1/auth/session", "", s.getWebSession},
		{"DELETE /api/v1/auth/session", "", s.deleteWebSession},
		{"GET /api/v1/schedules", config.ScopeSchedulesRead, s.listSchedules},
		{"POST /api/v1/schedules/{name}/run", config.ScopeSchedulesWrite, s.runSchedule},
		{"GET /api/v1/queue", config.ScopeQueueRead, s.queueStats},
		{"GET /api/v1/queue/jobs/{id}", config.ScopeQueueRead, s.queueJob},
		{"GET /api/v1/queue/dead", config.ScopeQueueRead, s.queueDead},
		{"POST /api/v1/queue/dead/{id}/retry", config.ScopeQueueWrite, s.queueRetryDead},
		{"DELETE /api/v1/queue/dead/{id}", config.ScopeQueueWrite, s.queueDeleteDead},
	}
}

// Handler returns the fully wired HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.ready)

	mux.HandleFunc("GET /{$}", s.serveIndex)
	mux.HandleFunc("GET /app.js", s.serveStatic("app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /style.css", s.serveStatic("style.css", "text/css; charset=utf-8"))

	mux.HandleFunc("POST /hooks/{name}", s.receiveWebhook)

	for _, route := range s.apiRoutes() {
		mux.HandleFunc(route.pattern, s.authorizedRoute(route))
	}

	mux.HandleFunc("POST /internal/v1/sessions/{id}/events", s.internalEvents)
	mux.HandleFunc("GET /internal/v1/sessions/{id}/journal", s.internalJournal)
	mux.HandleFunc("POST /internal/v1/sessions/{id}/journal", s.internalJournal)
	mux.HandleFunc("GET /internal/v1/sessions/{id}/tools", s.internalTools)
	mux.HandleFunc("POST /internal/v1/sessions/{id}/tools/calls", s.internalToolCall)
	mux.HandleFunc("POST /internal/v1/sessions/{id}/tools/calls/complete", s.internalToolComplete)
	mux.HandleFunc("GET /internal/v1/sessions/{id}/inbox", s.internalInbox)
	mux.HandleFunc("POST /internal/v1/sessions/{id}/artifacts", s.internalArtifact)
	mux.HandleFunc("GET /internal/v1/sessions/{id}/artifacts", s.internalReadArtifact)
	mux.HandleFunc("POST /internal/v1/sessions/{id}/finalize", s.internalFinalize)
	mux.HandleFunc("POST /internal/v1/llm/chat/completions", s.internalLLM)
	mux.HandleFunc("/internal/v1/mcp/{server}", s.internalMCP)

	if handler := s.metricsHandler(); handler != nil && s.cfg.Server.MetricsOnMainPort() {
		mux.Handle("GET /metrics", handler)
	}

	return s.commonHeaders(s.observeRequests(s.recoverPanics(shapeAPIErrors(mux))))
}

// metricsHandler serves the registry. When server.metrics.require_auth asks
// for it, the caller must present a token with the admin scope; it returns nil
// when no registry is wired.
func (s *Server) metricsHandler() http.Handler {
	if s.metrics == nil {
		return nil
	}
	handler := s.metrics.Handler()
	if !s.cfg.Server.MetricsRequireAuth() {
		return handler
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := s.authenticate(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthenticated, "missing or invalid bearer token")
			return
		}
		if !token.HasScope(config.ScopeAdmin) {
			writeError(w, http.StatusForbidden, codeForbidden, "token lacks the required scope: "+config.ScopeAdmin)
			return
		}
		handler.ServeHTTP(w, r)
	})
}

// MetricsHandler returns the metrics endpoint for a listener of its own. It
// returns nil when no registry is wired, and it requires a token with the
// admin scope when server.metrics.require_auth asks for it.
func (s *Server) MetricsHandler() http.Handler { return s.metricsHandler() }

// ready reports whether this replica has finished starting. It checks local
// state only: the artifact store and other remote dependencies stay out of the
// probe, because removing the only replica from the Service would drop every
// live session that reaches the control plane through it.
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Ready(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "error": err.Error()})
		return
	}
	if s.readyCheck != nil {
		if err := s.readyCheck(); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) commonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/internal/") && !strings.HasPrefix(r.URL.Path, "/hooks/") {
			h.Set("Content-Security-Policy", "default-src 'self'; connect-src 'self' ws: wss:; img-src 'self' data:; style-src 'self'; script-src 'self'")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("panic serving request", "path", r.URL.Path, "panic", rec)
				// The runtime protocol keeps its plain body and status.
				if strings.HasPrefix(r.URL.Path, "/internal/") {
					http.Error(w, "internal server error", http.StatusInternalServerError)
					return
				}
				writeError(w, http.StatusInternalServerError, codeInternal, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// shapeAPIErrors replaces the plain-text bodies that the mux writes for an
// unknown path or method with the API error envelope. Handlers that write
// their own JSON are untouched, and a 405 keeps its Allow header.
func shapeAPIErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/hooks/") {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(&errorShapedWriter{ResponseWriter: w}, r)
	})
}

type errorShapedWriter struct {
	http.ResponseWriter
	shaped bool
}

// Unwrap lets the error envelope reach the request recorder underneath.
func (w *errorShapedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *errorShapedWriter) WriteHeader(status int) {
	if w.shaped {
		return
	}
	shaped := status == http.StatusNotFound || status == http.StatusMethodNotAllowed
	if shaped && strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		shaped = false
	}
	if !shaped {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.shaped = true
	delete(w.Header(), "Content-Type")
	delete(w.Header(), "Content-Length")
	code, message := codeNotFound, "no such endpoint"
	if status == http.StatusMethodNotAllowed {
		code, message = codeMethodNotAllowed, "method not allowed"
	}
	writeError(w.ResponseWriter, status, code, message)
}

func (w *errorShapedWriter) Write(p []byte) (int, error) {
	if w.shaped {
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}

// Hijack keeps the WebSocket upgrade working through the wrapper.
func (w *errorShapedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer cannot be hijacked")
	}
	return hijacker.Hijack()
}

// Flush keeps streamed responses working through the wrapper.
func (w *errorShapedWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// ReadFrom keeps an artifact download on the underlying writer's fast path
// instead of copying through an extra buffer.
func (w *errorShapedWriter) ReadFrom(r io.Reader) (int64, error) {
	if w.shaped {
		return io.Copy(io.Discard, r)
	}
	if readerFrom, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return readerFrom.ReadFrom(r)
	}
	return io.Copy(struct{ io.Writer }{w}, r)
}

func (s *Server) internalSession(r *http.Request) (string, bool) {
	id := r.PathValue("id")
	token := bearerToken(r)
	if id == "" || token == "" {
		return "", false
	}
	return id, s.mgr.Authenticate(id, token)
}

func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if strings.HasPrefix(header, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	}
	return ""
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(s.web, "index.html")
	if err != nil {
		http.Error(w, "ui not available", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (s *Server) serveStatic(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := fs.ReadFile(s.web, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(data)
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, max int64, dst any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, dst)
}

// decodeJSONStrict decodes a public /api/v1 request body. An unknown field is
// an error, so a client learns about a typo instead of having it ignored. The
// internal runtime protocol keeps decodeJSON: its shapes are versioned with
// the runtime image, not with this API.
func decodeJSONStrict(w http.ResponseWriter, r *http.Request, max int64, dst any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
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

func (s *Server) publicConfig(w http.ResponseWriter, r *http.Request) {
	models := []string{s.cfg.LLM.Model}
	seen := map[string]bool{s.cfg.LLM.Model: true}
	for _, model := range s.cfg.LLM.AllowModels {
		if !seen[model] {
			seen[model] = true
			models = append(models, model)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"default_model":       s.cfg.LLM.Model,
		"allowed_models":      models,
		"skills":              s.mgr.SkillNames(),
		"mcp_servers":         s.mgr.MCPServerNames(),
		"agents":              s.mgr.Agents(),
		"default_ttl_seconds": int(s.cfg.Sessions.DefaultTTL.Std().Seconds()),
		"max_ttl_seconds":     int(s.cfg.Sessions.MaxTTL.Std().Seconds()),
		"triggers":            s.triggerNames(),
		"schedules":           s.scheduleList(),
	})
}

func (s *Server) scheduleList() []ScheduleInfo {
	if s.schedules == nil || s.schedules.List == nil {
		return []ScheduleInfo{}
	}
	return s.schedules.List
}

func (s *Server) triggerNames() []string {
	out := make([]string, 0, len(s.triggers))
	for name := range s.triggers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
