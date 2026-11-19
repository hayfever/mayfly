package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
)

func (s *Server) attachWebSocket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ticket := r.URL.Query().Get("ticket")
	actor, ok := s.tickets.consume(ticket, id)
	if !ok {
		writeError(w, http.StatusUnauthorized, codeUnauthenticated, "invalid or expired attach ticket")
		return
	}
	if actor == "" {
		actor = "web"
	}
	sess, ok := s.mgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, codeNotFound, "session not found")
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	ch, cancel, ok := s.mgr.SubscribeEvents(id, after)
	if !ok {
		_ = conn.WriteJSON(map[string]any{"type": "error", "message": "session not found"})
		return
	}
	defer cancel()

	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := conn.WriteJSON(map[string]any{"type": "hello", "session": newAPISession(*sess)}); err != nil {
		return
	}

	readerDone := make(chan struct{})
	errCh := make(chan string, 4)
	go func() {
		defer close(readerDone)
		conn.SetReadLimit(64 << 10)
		_ = conn.SetReadDeadline(time.Now().Add(150 * time.Second))
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(150 * time.Second))
		})
		for {
			var msg struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			switch msg.Type {
			case "user_message":
				if _, err := s.mgr.PostMessage(id, actor, msg.Text); err != nil {
					select {
					case errCh <- err.Error():
					default:
					}
				}
			case "ping":
				continue
			}
		}
	}()

	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-readerDone:
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteJSON(map[string]any{"type": "event", "event": ev}); err != nil {
				return
			}
		case message := <-errCh:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteJSON(map[string]any{"type": "error", "message": message}); err != nil {
				return
			}
		case <-ping.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return
			}
		}
	}
}
