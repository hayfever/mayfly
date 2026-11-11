// Package httpapi exposes the control plane HTTP API, web UI, webhook
// receivers, and internal runtime endpoints.
package httpapi

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hayfever/mayfly/internal/broker"
	"github.com/hayfever/mayfly/internal/config"
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
	cfg        *config.Config
	mgr        *session.Manager
	queue      queue.Store
	broker     *broker.Broker
	logger     *slog.Logger
	triggers   map[string]config.TriggerConfig
	limiters   map[string]*ratelimit.Limiter
	mcpServers map[string]config.MCPServerConfig
	tickets    *ticketStore
	web        fs.FS
	upgrader   websocket.Upgrader
}

// New builds the HTTP server. queue may be nil in tests that do not exercise
// webhooks.
func New(cfg *config.Config, mgr *session.Manager, q queue.Store, logger *slog.Logger) *Server {
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
	return &Server{
		cfg:        cfg,
		mgr:        mgr,
		queue:      q,
		broker:     broker.New(cfg.LLM, logger),
		logger:     logger,
		triggers:   triggersByName,
		limiters:   limiters,
		mcpServers: mcpServers,
		tickets:    newTicketStore(),
		web:        sub,
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

	mux.HandleFunc("POST /api/v1/sessions", s.admin(s.createSession))
	mux.HandleFunc("GET /api/v1/sessions", s.admin(s.listSessions))
	mux.HandleFunc("GET /api/v1/sessions/{id}", s.admin(s.getSession))
	mux.HandleFunc("DELETE /api/v1/sessions/{id}", s.admin(s.deleteSession))
	mux.HandleFunc("GET /api/v1/sessions/{id}/events", s.admin(s.sessionEvents))
	mux.HandleFunc("POST /api/v1/sessions/{id}/messages", s.admin(s.postMessage))
	mux.HandleFunc("POST /api/v1/sessions/{id}/close", s.admin(s.closeSession))
	mux.HandleFunc("POST /api/v1/sessions/{id}/attach-ticket", s.admin(s.createTicket))
	mux.HandleFunc("GET /api/v1/sessions/{id}/artifact", s.admin(s.downloadArtifact))
	mux.HandleFunc("GET /api/v1/config", s.admin(s.publicConfig))
	mux.HandleFunc("GET /api/v1/queue", s.admin(s.queueStats))
	mux.HandleFunc("GET /api/v1/sessions/{id}/ws", s.attachWebSocket)

	mux.HandleFunc("POST /internal/v1/sessions/{id}/events", s.internalEvents)
	mux.HandleFunc("GET /internal/v1/sessions/{id}/inbox", s.internalInbox)
	mux.HandleFunc("POST /internal/v1/sessions/{id}/artifacts", s.internalArtifact)
	mux.HandleFunc("POST /internal/v1/sessions/{id}/finalize", s.internalFinalize)
	mux.HandleFunc("POST /internal/v1/llm/chat/completions", s.internalLLM)
	mux.HandleFunc("/internal/v1/mcp/{server}", s.internalMCP)

	return s.commonHeaders(s.recoverPanics(s.logRequests(mux)))
}

// ready reports whether this replica has finished starting. It checks local
// state only: the artifact store and other remote dependencies stay out of the
// probe, because removing the only replica from the Service would drop every
// live session that reaches the control plane through it.
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Ready(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "error": err.Error()})
		return
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
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		s.logger.Debug("http request", "method", r.Method, "path", r.URL.Path, "duration_ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		expected := s.cfg.AdminToken
		if expected == "" {
			writeError(w, http.StatusUnauthorized, "admin token is not configured")
			return
		}
		got := bearerToken(r)
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
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

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
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
	})
}

func (s *Server) triggerNames() []string {
	out := make([]string, 0, len(s.triggers))
	for name := range s.triggers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
