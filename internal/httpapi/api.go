package httpapi

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/session"
)

type createRequest struct {
	Prompt     string   `json:"prompt"`
	Title      string   `json:"title"`
	Agent      string   `json:"agent"`
	Model      string   `json:"model"`
	TTL        string   `json:"ttl"`
	Skills     []string `json:"skills"`
	MCPServers []string `json:"mcp_servers"`
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := decodeJSON(w, r, s.cfg.Server.MaxBodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	var ttl time.Duration
	if req.TTL != "" {
		parsed, err := time.ParseDuration(req.TTL)
		if err != nil {
			writeError(w, http.StatusBadRequest, "ttl must be a duration such as 30m")
			return
		}
		ttl = parsed
	}
	opts := session.CreateOptions{
		Prompt:     req.Prompt,
		Title:      req.Title,
		Agent:      req.Agent,
		Model:      req.Model,
		TTL:        ttl,
		Skills:     req.Skills,
		MCPServers: req.MCPServers,
		Trigger:    model.TriggerInfo{Kind: "api", Name: "http"},
	}
	created, err := s.mgr.Create(r.Context(), opts)
	if err != nil {
		if created == nil {
			writeSessionError(w, err)
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "session": created})
		return
	}
	w.Header().Set("Location", s.sessionURL(created.ID))
	writeJSON(w, http.StatusCreated, map[string]any{"session": created, "url": s.sessionURL(created.ID)})
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.mgr.List()})
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.mgr.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": sess, "url": s.sessionURL(sess.ID)})
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Delete(r.PathValue("id")); err != nil {
		writeSessionError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) sessionEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	events, ok := s.mgr.Events(id, after)
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "session_id": id})
}

func (s *Server) postMessage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text  string `json:"text"`
		Actor string `json:"actor"`
	}
	if err := decodeJSON(w, r, 64<<10, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	event, err := s.mgr.PostMessage(r.PathValue("id"), req.Actor, req.Text)
	if err != nil {
		writeSessionError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"event": event})
}

func (s *Server) closeSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
	}
	_ = decodeJSON(w, r, 4<<10, &req)
	if req.Reason == "" {
		req.Reason = "closed"
	}
	if err := s.mgr.Close(r.PathValue("id"), req.Reason); err != nil {
		writeSessionError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "closing"})
}

func (s *Server) createTicket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Actor string `json:"actor"`
	}
	_ = decodeJSON(w, r, 4<<10, &req)
	if _, ok := s.mgr.Get(id); !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	token, expires := s.tickets.issue(id, req.Actor, s.cfg.Server.AttachTicketTTL.Std())
	wsURL, err := s.wsURL(id, token)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot build websocket URL")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ticket":     token,
		"expires_at": expires,
		"ws_url":     wsURL,
	})
}

func (s *Server) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	body, size, err := s.mgr.OpenArtifact(r.Context(), id)
	if err != nil {
		if errors.Is(err, session.ErrNotFound) {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		writeError(w, http.StatusNotFound, "artifact not available")
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "mayfly-"+id+".tar.gz"))
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

func (s *Server) sessionURL(id string) string {
	return strings.TrimRight(s.cfg.Server.PublicURL, "/") + "/#/sessions/" + id
}

func (s *Server) wsURL(id, ticket string) (string, error) {
	u, err := url.Parse(strings.TrimRight(s.cfg.Server.PublicURL, "/"))
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.Path = "/api/v1/sessions/" + url.PathEscape(id) + "/ws"
	u.RawQuery = url.Values{"ticket": {ticket}}.Encode()
	return u.String(), nil
}

func writeSessionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, session.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, session.ErrClosed):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, session.ErrCapacity):
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}
