package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hayfever/mayfly/internal/model"
)

func (s *Server) internalEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := s.internalSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req struct {
		Events []model.Event `json:"events"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	stored, err := s.mgr.EmitRuntimeEvents(id, bearerToken(r), req.Events)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": stored})
}

func (s *Server) internalInbox(w http.ResponseWriter, r *http.Request) {
	id, ok := s.internalSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	s.mgr.RuntimeConnected(id)
	if last, ok := s.mgr.LastSeq(id); !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	} else if after > last {
		// The control plane restarted and sequence numbering restarted. Tell
		// the runtime to reconnect from zero instead of silently missing events.
		after = 0
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: reset\ndata: {}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return
	}
	ch, cancel, ok := s.mgr.SubscribeInbox(id, after)
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	fmt.Fprint(w, "retry: 1000\n: connected\n\n")
	if flusher != nil {
		flusher.Flush()
	}

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
			if flusher != nil {
				flusher.Flush()
			}
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func (s *Server) internalArtifact(w http.ResponseWriter, r *http.Request) {
	id, ok := s.internalSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if r.ContentLength <= 0 {
		writeError(w, http.StatusLengthRequired, "Content-Length is required")
		return
	}
	partial := strings.EqualFold(r.Header.Get("X-Mayfly-Partial"), "true")
	info, err := s.mgr.UploadArtifact(r.Context(), id, bearerToken(r), r.Body, r.ContentLength, r.Header.Get("X-Mayfly-SHA256"), partial)
	if err != nil {
		s.logger.Warn("artifact upload failed", "session", id, "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) internalFinalize(w http.ResponseWriter, r *http.Request) {
	id, ok := s.internalSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
		Error  string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	status := model.SessionStatus(req.Status)
	if status != model.StatusCompleted && status != model.StatusFailed {
		writeError(w, http.StatusBadRequest, "status must be completed or failed")
		return
	}
	if err := s.mgr.Complete(id, bearerToken(r), status, req.Reason, req.Error); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": string(status)})
}

func (s *Server) internalLLM(w http.ResponseWriter, r *http.Request) {
	id, ok := s.mgr.SessionIDForToken(bearerToken(r))
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	sess, ok := s.mgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	s.broker.ServeLLM(w, r, sess.Model)
}

func (s *Server) internalMCP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost, http.MethodGet, http.MethodDelete:
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, ok := s.mgr.SessionIDForToken(bearerToken(r))
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	server := r.PathValue("server")
	sess, ok := s.mgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	allowed := false
	for _, name := range sess.MCPServers {
		if name == server {
			allowed = true
			break
		}
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "MCP server is not assigned to this session")
		return
	}
	def, ok := s.mcpServers[server]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown MCP server")
		return
	}
	s.broker.ServeMCP(w, r, def)
}
