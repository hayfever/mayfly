package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/hayfever/mayfly/internal/artifact"
	"github.com/hayfever/mayfly/internal/bundle"
	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/k8s"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/runner"
	"github.com/hayfever/mayfly/internal/sessioncfg"
	"github.com/hayfever/mayfly/internal/version"
)

// Events returns retained thread events after a sequence.
func (m *Manager) Events(id string, after int64) ([]model.Event, bool) {
	if _, ok := m.Get(id); !ok {
		return nil, false
	}
	return m.hub.EventsAfter(id, after), true
}

// LastSeq returns the latest event sequence for a session.
func (m *Manager) LastSeq(id string) (int64, bool) {
	if _, ok := m.Get(id); !ok {
		return 0, false
	}
	return m.hub.LastSeq(id), true
}

// SubscribeEvents replays and streams thread events.
func (m *Manager) SubscribeEvents(id string, after int64) (<-chan model.Event, func(), bool) {
	if _, ok := m.Get(id); !ok {
		return nil, nil, false
	}
	ch, cancel := m.hub.SubscribeEvents(id, after)
	return ch, cancel, true
}

// SubscribeInbox replays and streams human messages and control events.
func (m *Manager) SubscribeInbox(id string, after int64) (<-chan model.Event, func(), bool) {
	if _, ok := m.Get(id); !ok {
		return nil, nil, false
	}
	ch, cancel := m.hub.SubscribeInbox(id, after)
	return ch, cancel, true
}

// PostMessage appends a human message to the thread.
func (m *Manager) PostMessage(id, actor, text string) (model.Event, error) {
	inst, ok := m.instance(id)
	if !ok {
		return model.Event{}, ErrNotFound
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return model.Event{}, errors.New("message is empty")
	}
	if len(text) > maxMessageBytes {
		return model.Event{}, fmt.Errorf("message exceeds %d bytes", maxMessageBytes)
	}
	if actor == "" {
		actor = "user"
	}
	inst.mu.Lock()
	status := inst.sess.Status
	inst.mu.Unlock()
	switch status {
	case model.StatusPending, model.StatusRunning, model.StatusClosing:
	default:
		return model.Event{}, ErrClosed
	}
	return m.appendEvent(inst, model.Event{Type: model.EventUserMessage, Actor: actor, Text: text}), nil
}

// EmitRuntimeEvents accepts assistant, tool, and status events from the
// session runtime.
func (m *Manager) EmitRuntimeEvents(id, token string, events []model.Event) ([]model.Event, error) {
	if !m.Authenticate(id, token) {
		return nil, ErrUnauthorized
	}
	inst, ok := m.instance(id)
	if !ok {
		return nil, ErrNotFound
	}
	m.RuntimeConnected(id)
	out := make([]model.Event, 0, len(events))
	for _, ev := range events {
		switch ev.Type {
		case model.EventAssistantMessage, model.EventAssistantDelta, model.EventToolCall,
			model.EventToolResult, model.EventStatus, model.EventError, model.EventSystem,
			model.EventArtifact:
		default:
			continue
		}
		ev.Seq = 0
		ev.SessionID = ""
		out = append(out, m.appendEvent(inst, ev))
	}
	return out, nil
}

func (m *Manager) appendEvent(inst *instance, ev model.Event) model.Event {
	stored := m.hub.Append(inst.sess.ID, ev)
	inst.mu.Lock()
	inst.sess.LastActivity = stored.Time
	switch stored.Type {
	case model.EventUserMessage, model.EventAssistantMessage:
		inst.sess.MessageCount++
	}
	inst.mu.Unlock()
	return stored
}

func (m *Manager) instance(id string) (*instance, bool) {
	m.mu.Lock()
	inst := m.sessions[id]
	m.mu.Unlock()
	return inst, inst != nil
}

// ArtifactInfo returns exported artifact metadata. The bundle stays available
// after the session is removed from memory.
func (m *Manager) ArtifactInfo(id string) (*model.ArtifactInfo, bool) {
	info, ok := m.artifactFor(id)
	if !ok {
		return nil, false
	}
	return &info, true
}

// OpenArtifact opens a stored bundle for download.
func (m *Manager) OpenArtifact(ctx context.Context, id string) (io.ReadCloser, int64, error) {
	info, ok := m.ArtifactInfo(id)
	if !ok {
		return nil, 0, artifact.ErrNotFound
	}
	return m.store.Get(ctx, info.Key)
}

// UploadArtifact stores a runtime-produced bundle.
func (m *Manager) UploadArtifact(ctx context.Context, id, token string, body io.Reader, size int64, digest string, partial bool) (model.ArtifactInfo, error) {
	if !m.Authenticate(id, token) {
		return model.ArtifactInfo{}, ErrUnauthorized
	}
	if size <= 0 || size > m.cfg.Artifacts.MaxBytes {
		return model.ArtifactInfo{}, fmt.Errorf("artifact size %d is out of range", size)
	}
	digest = strings.ToLower(strings.TrimSpace(digest))
	if len(digest) != 64 {
		return model.ArtifactInfo{}, errors.New("X-Mayfly-SHA256 must be a 64 character hex digest")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return model.ArtifactInfo{}, errors.New("X-Mayfly-SHA256 is not valid hex")
	}
	tmp, err := os.CreateTemp("", "mayfly-artifact-*")
	if err != nil {
		return model.ArtifactInfo{}, err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hasher), io.LimitReader(body, size+1))
	if err != nil {
		return model.ArtifactInfo{}, fmt.Errorf("read artifact: %w", err)
	}
	if n != size {
		return model.ArtifactInfo{}, fmt.Errorf("artifact size mismatch: got %d, want %d", n, size)
	}
	actual := hex.EncodeToString(hasher.Sum(nil))
	if actual != digest {
		return model.ArtifactInfo{}, fmt.Errorf("artifact digest mismatch: got %s, want %s", actual, digest)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return model.ArtifactInfo{}, err
	}
	key := artifact.Key(id, time.Now())
	uri, err := m.store.Put(ctx, key, tmp, size, digest, "application/gzip")
	if err != nil {
		return model.ArtifactInfo{}, err
	}
	info := model.ArtifactInfo{
		Key:       key,
		URI:       uri,
		Size:      size,
		SHA256:    digest,
		CreatedAt: time.Now().UTC(),
		Partial:   partial,
	}
	m.setArtifact(id, info)
	m.logger.Info("artifact stored", "session", id, "key", key, "bytes", size)
	return info, nil
}

// Complete records the runtime's terminal report.
func (m *Manager) Complete(id, token string, status model.SessionStatus, reason, errMsg string) error {
	if !m.Authenticate(id, token) {
		return ErrUnauthorized
	}
	inst, ok := m.instance(id)
	if !ok {
		return ErrNotFound
	}
	m.finalize(inst, status, reason, errMsg)
	return nil
}

// Close asks the runtime to finalize and stop. It is safe to call twice.
func (m *Manager) Close(id, reason string) error {
	inst, ok := m.instance(id)
	if !ok {
		return ErrNotFound
	}
	inst.mu.Lock()
	switch inst.sess.Status {
	case model.StatusCompleted, model.StatusFailed, model.StatusExpired:
		inst.mu.Unlock()
		return nil
	case model.StatusClosing:
		inst.mu.Unlock()
		return nil
	}
	inst.sess.Status = model.StatusClosing
	inst.closeReason = reason
	inst.closeRequestedAt = time.Now().UTC()
	if reason == "" {
		reason = "closed"
	}
	inst.mu.Unlock()

	m.hub.Append(id, model.Event{
		Type:  model.EventControl,
		Actor: "mayfly",
		Data:  map[string]any{"action": "close", "reason": reason},
	})
	m.hub.Append(id, model.Event{
		Type:  model.EventStatus,
		Actor: "mayfly",
		Text:  "Close requested.",
		Data:  map[string]any{"status": string(model.StatusClosing), "reason": reason},
	})
	m.logger.Info("close requested", "session", id, "reason", reason)
	return nil
}

// Delete force-stops a session and marks it failed.
func (m *Manager) Delete(id string) error {
	inst, ok := m.instance(id)
	if !ok {
		return ErrNotFound
	}
	inst.mu.Lock()
	already := inst.finalized
	inst.mu.Unlock()
	if !already {
		m.finalize(inst, model.StatusFailed, "deleted", "session deleted by user")
	}
	m.cleanup(inst, true)
	return nil
}

func (m *Manager) finalize(inst *instance, status model.SessionStatus, reason, errMsg string) {
	inst.mu.Lock()
	if inst.finalized {
		inst.mu.Unlock()
		return
	}
	inst.finalized = true
	now := time.Now().UTC()
	final := status
	if final == model.StatusCompleted {
		if inst.closeReason == "expired" || reason == "ttl" || reason == "expired" {
			final = model.StatusExpired
		}
	}
	inst.sess.Status = final
	inst.sess.Error = errMsg
	inst.sess.ClosedAt = &now
	inst.sess.LastActivity = now
	hash := inst.tokenHash
	inst.mu.Unlock()

	m.removeToken(hash)
	text := "Session " + string(final) + "."
	if errMsg != "" {
		text += " " + errMsg
	}
	m.hub.Append(inst.sess.ID, model.Event{
		Type:  model.EventStatus,
		Actor: "mayfly",
		Text:  text,
		Data:  map[string]any{"status": string(final), "reason": reason, "error": errMsg},
	})
	go m.cleanup(inst, final == model.StatusFailed)
	m.logger.Info("session finalized", "session", inst.sess.ID, "status", final, "reason", reason)
}

func (m *Manager) cleanup(inst *instance, force bool) {
	inst.mu.Lock()
	if inst.resourcesDeleted {
		inst.mu.Unlock()
		return
	}
	inst.resourcesDeleted = true
	handle := inst.handle
	inst.mu.Unlock()
	if handle.Backend == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := m.runner.Stop(ctx, handle, force); err != nil {
		m.logger.Warn("cleanup session resources", "session", inst.sess.ID, "error", err)
	}
}

// fallback exports logs and retained events when the runtime did not finalize.
func (m *Manager) fallback(id, reason string) {
	inst, ok := m.instance(id)
	if !ok {
		return
	}
	inst.mu.Lock()
	if inst.finalized {
		inst.mu.Unlock()
		return
	}
	sess := inst.sess
	handle := inst.handle
	runtimeCfg := inst.runtimeCfg
	inst.mu.Unlock()

	m.logger.Warn("exporting fallback artifact", "session", id, "reason", reason)
	m.hub.Append(id, model.Event{Type: model.EventError, Actor: "mayfly", Text: "Session failed: " + reason})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	logs, err := m.runner.Logs(ctx, handle, 512<<10)
	if err != nil {
		m.logger.Warn("collect session logs", "session", id, "error", err)
	}
	var events bytes.Buffer
	for _, ev := range m.hub.EventsAfter(id, 0) {
		line, err := json.Marshal(ev)
		if err == nil {
			events.Write(line)
			events.WriteByte('\n')
		}
	}
	cfgJSON, _ := json.MarshalIndent(runtimeCfg, "", "  ")

	tmp, err := os.CreateTemp("", "mayfly-fallback-*")
	if err != nil {
		m.failFallback(inst, reason, err)
		return
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	hasher := sha256.New()
	manifest := bundle.Manifest{
		Version:      1,
		SessionID:    id,
		Title:        sess.Title,
		Agent:        sess.Agent,
		Harness:      sess.Harness,
		Model:        sess.Model,
		Status:       "failed",
		Reason:       reason,
		CreatedAt:    sess.CreatedAt,
		FinishedAt:   time.Now().UTC(),
		AgentVersion: version.Version,
		Skills:       sess.Skills,
		MCPServers:   sess.MCPServers,
		MessageCount: sess.MessageCount,
		Partial:      true,
		Note:         "Runtime did not export an artifact. This bundle contains control-plane logs and retained events.",
	}
	if len(cfgJSON) > 0 {
		manifest.Note += " Session configuration is included."
	}
	bw := bundle.New(io.MultiWriter(tmp, hasher), manifest, 8<<20)
	if cfgJSON != nil {
		_ = bw.AddBytes("session.json", cfgJSON)
	}
	if err := bw.AddBytes("logs.txt", []byte(logs)); err != nil {
		m.failFallback(inst, reason, err)
		return
	}
	if err := bw.AddBytes("events.jsonl", events.Bytes()); err != nil {
		m.failFallback(inst, reason, err)
		return
	}
	if err := bw.Close(); err != nil {
		m.failFallback(inst, reason, err)
		return
	}
	if err := tmp.Sync(); err != nil {
		m.failFallback(inst, reason, err)
		return
	}
	info, err := tmp.Stat()
	if err != nil {
		m.failFallback(inst, reason, err)
		return
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		m.failFallback(inst, reason, err)
		return
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	key := artifact.Key(id, time.Now())
	uri, err := m.store.Put(ctx, key, tmp, info.Size(), digest, "application/gzip")
	if err != nil {
		m.failFallback(inst, reason, err)
		return
	}
	art := model.ArtifactInfo{
		Key:       key,
		URI:       uri,
		Size:      info.Size(),
		SHA256:    digest,
		CreatedAt: time.Now().UTC(),
		Partial:   true,
	}
	inst.mu.Lock()
	closeReason := inst.closeReason
	inst.mu.Unlock()
	m.setArtifact(id, art)
	m.hub.Append(id, model.Event{
		Type:  model.EventArtifact,
		Actor: "mayfly",
		Text:  "Partial artifact exported after runtime failure.",
		Data:  map[string]any{"key": key, "uri": uri, "size": info.Size(), "sha256": digest, "partial": true},
	})
	finalStatus := model.StatusFailed
	if closeReason == "expired" {
		finalStatus = model.StatusExpired
	}
	m.finalize(inst, finalStatus, reason, "runtime did not finalize: "+reason)
	m.cleanup(inst, true)
}

func (m *Manager) failFallback(inst *instance, reason string, err error) {
	m.logger.Error("fallback artifact export failed", "session", inst.sess.ID, "error", err)
	m.hub.Append(inst.sess.ID, model.Event{Type: model.EventError, Actor: "mayfly", Text: "Fallback artifact export failed: " + err.Error()})
	m.finalize(inst, model.StatusFailed, reason, "runtime did not finalize: "+reason)
}

func (m *Manager) adopt(ctx context.Context) error {
	pods, err := m.kube.SessionPods(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for i := range pods {
		pod := &pods[i]
		id := pod.Labels[k8s.LabelSession]
		if id == "" {
			continue
		}
		if _, ok := m.instance(id); ok {
			continue
		}
		raw, err := m.kube.SessionConfig(ctx, pod.Name)
		if err != nil {
			m.logger.Warn("adopt: read config", "pod", pod.Name, "error", err)
			continue
		}
		var cfg sessioncfg.Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			m.logger.Warn("adopt: parse config", "pod", pod.Name, "error", err)
			continue
		}
		token, err := m.kube.SessionToken(ctx, pod.Name)
		if err != nil {
			m.logger.Warn("adopt: read token", "pod", pod.Name, "error", err)
			continue
		}
		inst := &instance{
			sess: model.Session{
				ID:           id,
				Title:        cfg.Title,
				Status:       model.StatusPending,
				Prompt:       cfg.Prompt,
				Model:        cfg.Model,
				Skills:       skillNames(cfg.Skills),
				MCPServers:   mcpNames(cfg.MCPServers),
				Trigger:      model.TriggerInfo{Kind: "adopted"},
				Backend:      "kubernetes",
				PodName:      pod.Name,
				TTLSeconds:   cfg.TTLSeconds,
				CreatedAt:    cfg.CreatedAt,
				ExpiresAt:    cfg.CreatedAt.Add(time.Duration(cfg.TTLSeconds) * time.Second),
				LastActivity: now,
			},
			tokenHash:  hashToken(token),
			runtimeCfg: cfg,
			launchedAt: now,
			handle: runner.Handle{
				Backend:       "kubernetes",
				SessionID:     id,
				Namespace:     pod.Namespace,
				PodName:       pod.Name,
				SecretName:    pod.Name,
				ConfigMapName: pod.Name,
				NetworkPolicy: config.Bool(m.cfg.Runtime.NetworkPolicy, true),
			},
		}
		if inst.sess.Title == "" {
			inst.sess.Title = "Session " + id
		}
		m.mu.Lock()
		m.sessions[id] = inst
		m.tokenIndex[inst.tokenHash] = id
		m.mu.Unlock()
		m.hub.Create(id)
		m.hub.Append(id, model.Event{Type: model.EventSystem, Actor: "mayfly", Text: "Session recovered by control plane restart."})
		m.logger.Info("adopted session", "session", id, "pod", pod.Name)
	}
	return nil
}

func skillNames(in []sessioncfg.Skill) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, s.Name)
	}
	return out
}

func mcpNames(in []sessioncfg.MCPServer) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, s.Name)
	}
	return out
}

func (m *Manager) maintenance() {
	reaper := time.NewTicker(5 * time.Second)
	defer reaper.Stop()
	var pollC <-chan time.Time
	if m.kube != nil {
		t := time.NewTicker(m.cfg.Runtime.PollInterval.Std())
		defer t.Stop()
		pollC = t.C
	}
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-reaper.C:
			m.reap()
		case <-pollC:
			m.pollPods(m.ctx)
		}
	}
}

func (m *Manager) reap() {
	now := time.Now().UTC()
	retention := m.cfg.Sessions.Retention.Std()
	for _, inst := range m.snapshot() {
		inst.mu.Lock()
		id := inst.sess.ID
		status := inst.sess.Status
		expires := inst.sess.ExpiresAt
		last := inst.sess.LastActivity
		closeAt := inst.closeRequestedAt
		finalized := inst.finalized
		closedAt := inst.sess.ClosedAt
		connectFrom := inst.podRunningSince
		if inst.sess.Backend != "kubernetes" {
			connectFrom = inst.launchedAt
		}
		inst.mu.Unlock()
		if finalized {
			if retention > 0 && closedAt != nil && now.Sub(*closedAt) > retention {
				m.forget(id)
			}
			continue
		}
		switch status {
		case model.StatusPending:
			if now.After(expires) {
				_ = m.Close(id, "expired")
				continue
			}
			if connect := m.cfg.Sessions.ConnectTimeout.Std(); connect > 0 && !connectFrom.IsZero() && now.Sub(connectFrom) > connect {
				m.fallback(id, "runtime did not connect")
			}
		case model.StatusRunning:
			if now.After(expires) {
				_ = m.Close(id, "expired")
				continue
			}
			idle := m.cfg.Sessions.IdleTimeout.Std()
			if idle > 0 && now.Sub(last) > idle+2*time.Minute {
				_ = m.Close(id, "idle")
			}
		case model.StatusClosing:
			if !closeAt.IsZero() && now.Sub(closeAt) > m.cfg.Sessions.CloseGrace.Std() {
				m.fallback(id, "close grace period exceeded")
			}
		}
	}
}

func (m *Manager) pollPods(ctx context.Context) {
	if m.kube == nil {
		return
	}
	pods, err := m.kube.SessionPods(ctx)
	if err != nil {
		m.logger.Warn("list session pods", "error", err)
		return
	}
	now := time.Now().UTC()
	seen := map[string]bool{}
	for i := range pods {
		pod := &pods[i]
		id := pod.Labels[k8s.LabelSession]
		if id == "" {
			continue
		}
		seen[id] = true
		inst, ok := m.instance(id)
		if !ok {
			continue
		}
		terminal := pod.Status.Phase == "Succeeded" || pod.Status.Phase == "Failed"
		inst.mu.Lock()
		if terminal {
			if inst.podTerminalSince.IsZero() {
				inst.podTerminalSince = now
			}
		} else {
			inst.podTerminalSince = time.Time{}
		}
		if pod.Status.Phase == "Running" && inst.podRunningSince.IsZero() {
			inst.podRunningSince = now
		}
		failureDetail := ""
		if pod.Status.Phase == "Pending" || pod.Status.Phase == "Failed" {
			if reason, message := podFailureDetail(pod); reason != "" {
				failureDetail = reason
				if message != "" {
					failureDetail += ": " + message
				}
			}
		}
		waitingFailure := false
		if !terminal && pod.Status.Phase == "Pending" && failureDetail != "" {
			switch {
			case strings.HasPrefix(failureDetail, "InvalidImageName"):
				waitingFailure = true
			case now.Sub(pod.CreationTimestamp.Time) > 30*time.Second:
				switch {
				case strings.HasPrefix(failureDetail, "ErrImagePull"),
					strings.HasPrefix(failureDetail, "ImagePullBackOff"),
					strings.HasPrefix(failureDetail, "CreateContainerConfigError"),
					strings.HasPrefix(failureDetail, "CreateContainerError"),
					strings.HasPrefix(failureDetail, "RunContainerError"):
					waitingFailure = true
				}
			}
		}
		terminalSince := inst.podTerminalSince
		finalized := inst.finalized
		inst.mu.Unlock()
		if !finalized && waitingFailure {
			m.fallback(id, "session container cannot start: "+failureDetail)
			continue
		}
		if terminal && !finalized && !terminalSince.IsZero() && now.Sub(terminalSince) > podTerminalGrace {
			reason := "pod terminated without finalize"
			if failureDetail != "" {
				reason = "pod failed: " + failureDetail
			}
			m.fallback(id, reason)
		}
	}
	for _, inst := range m.snapshot() {
		id := inst.sess.ID
		if seen[id] {
			inst.mu.Lock()
			inst.podMissingSince = time.Time{}
			inst.mu.Unlock()
			continue
		}
		inst.mu.Lock()
		podName := inst.handle.PodName
		finalized := inst.finalized
		if finalized || podName == "" {
			inst.mu.Unlock()
			continue
		}
		if inst.podMissingSince.IsZero() {
			inst.podMissingSince = now
		}
		missingSince := inst.podMissingSince
		inst.mu.Unlock()
		if now.Sub(missingSince) > 30*time.Second {
			m.fallback(id, "pod disappeared")
		}
	}
}

func podFailureDetail(pod *corev1.Pod) (string, string) {
	for _, status := range pod.Status.ContainerStatuses {
		if waiting := status.State.Waiting; waiting != nil {
			return waiting.Reason, waiting.Message
		}
		if terminated := status.State.Terminated; terminated != nil {
			return terminated.Reason, terminated.Message
		}
	}
	if pod.Status.Message != "" {
		return pod.Status.Reason, pod.Status.Message
	}
	return "", ""
}
