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
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/hayfever/mayfly/internal/artifact"
	"github.com/hayfever/mayfly/internal/bundle"
	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/k8s"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/runner"
	"github.com/hayfever/mayfly/internal/sessioncfg"
	"github.com/hayfever/mayfly/internal/textutil"
	"github.com/hayfever/mayfly/internal/version"
)

// Limits that bound one runtime event. StepKey stays identifier-sized because
// the hub remembers it for duplicate detection; the event text and every
// payload string use sessions.max_event_bytes.
const (
	maxEventStepKeyBytes = 512
	maxEventActorBytes   = 256
	eventTruncationMark  = "…[truncated]"
)

// ErrEmptyMessage rejects a human message with no text.
var ErrEmptyMessage = errors.New("message is empty")

// ErrMessageTooLong rejects a human message over maxMessageBytes.
var ErrMessageTooLong = errors.New("message is too long")

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
		return model.Event{}, ErrEmptyMessage
	}
	if len(text) > maxMessageBytes {
		return model.Event{}, fmt.Errorf("%w: %d bytes (limit %d)", ErrMessageTooLong, len(text), maxMessageBytes)
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
	// The message's sequence becomes a turn key, so it must be durably
	// reserved before the hub hands it out.
	if err := m.reserveSequence(inst); err != nil {
		return model.Event{}, fmt.Errorf("reserve turn sequence: %w", err)
	}
	stored := m.appendEvent(inst, model.Event{Type: model.EventUserMessage, Actor: actor, Text: text})
	// The sender is acknowledged only after the message is durable. Adoption
	// redelivers it under this same sequence if the runtime never starts it.
	if err := m.persistPending(inst, stored); err != nil {
		return model.Event{}, err
	}
	return stored, nil
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
	limit := m.maxEventBytes()
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
		ev = boundRuntimeEvent(ev, limit)
		stored := m.appendEvent(inst, ev)
		if stored.Seq == 0 {
			continue
		}
		out = append(out, stored)
	}
	return out, nil
}

// maxEventBytes returns the configured cap on one runtime event, or the
// default when the configuration leaves it unset. It bounds every event the
// manager puts on the thread, including one restored from the journal.
func (m *Manager) maxEventBytes() int {
	if limit := m.cfg.Sessions.MaxEventBytes; limit > 0 {
		return limit
	}
	return config.DefaultMaxEventBytes
}

// boundRuntimeEvent truncates the unbounded strings of one runtime event so a
// hostile runtime cannot retain an oversized message. Text and every payload
// string end with a marker, and the cut never splits a UTF-8 sequence. StepKey
// and Actor get smaller identifier-sized bounds because the hub keeps a
// StepKey until its event is evicted.
func boundRuntimeEvent(ev model.Event, max int) model.Event {
	if max <= 0 {
		return ev
	}
	ev.StepKey = textutil.Truncate(ev.StepKey, maxEventStepKeyBytes)
	ev.Actor = textutil.Truncate(ev.Actor, maxEventActorBytes)
	ev.Text = truncateEventString(ev.Text, max)
	switch d := ev.Data.(type) {
	case model.UserMessageData:
		d.Kind = truncateEventString(d.Kind, max)
		d.Name = truncateEventString(d.Name, max)
		d.Event = truncateEventString(d.Event, max)
		ev.Data = d
	case model.ToolCallData:
		d.Tool = truncateEventString(d.Tool, max)
		d.CallID = truncateEventString(d.CallID, max)
		d.Arguments = truncateEventString(d.Arguments, max)
		ev.Data = d
	case model.ToolResultData:
		d.Tool = truncateEventString(d.Tool, max)
		d.CallID = truncateEventString(d.CallID, max)
		ev.Data = d
	case model.StatusData:
		d.Reason = truncateEventString(d.Reason, max)
		d.Error = truncateEventString(d.Error, max)
		ev.Data = d
	case model.SystemData:
		d.Model = truncateEventString(d.Model, max)
		d.Skills = truncateEach(d.Skills, max)
		d.MCPServers = truncateEach(d.MCPServers, max)
		d.Plugins = truncateEach(d.Plugins, max)
		ev.Data = d
	case model.ArtifactData:
		d.Key = truncateEventString(d.Key, max)
		d.URI = truncateEventString(d.URI, max)
		d.SHA256 = truncateEventString(d.SHA256, max)
		ev.Data = d
	case model.ControlData:
		d.Action = truncateEventString(d.Action, max)
		d.Reason = truncateEventString(d.Reason, max)
		ev.Data = d
	}
	return ev
}

// truncateEventString returns s with at most max bytes. A longer value ends
// with eventTruncationMark. A value at or below max is returned as is.
func truncateEventString(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= len(eventTruncationMark) {
		return textutil.Truncate(s, max)
	}
	return textutil.Truncate(s, max-len(eventTruncationMark)) + eventTruncationMark
}

func truncateEach(values []string, max int) []string {
	if values == nil {
		return nil
	}
	changed := false
	for _, v := range values {
		if len(v) > max {
			changed = true
			break
		}
	}
	if !changed {
		return values
	}
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = truncateEventString(v, max)
	}
	return out
}

// reserveSequence keeps the hub's next sequence inside the range the journal
// has durably reserved for the session. It runs before the append, so a crash
// can never leave a turn key that a later control-plane lifetime reuses.
func (m *Manager) reserveSequence(inst *instance) error {
	if m.journal == nil {
		return nil
	}
	next := m.hub.LastSeq(inst.sess.ID) + 1
	if next <= inst.seqFloor.Load() {
		return nil
	}
	target := next + sequenceBlock - 1
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.journal.Reserve(ctx, inst.sess.ID, target); err != nil {
		return fmt.Errorf("reserve sequence through %d: %w", target, err)
	}
	inst.seqFloor.Store(target)
	return nil
}

func (m *Manager) appendEvent(inst *instance, ev model.Event) model.Event {
	// A failed reservation only risks a reused key after a restart, and the
	// runtime rejects a turn whose journaled event does not match. Turn keys
	// are reserved by their creator (Create or PostMessage) before use, so
	// this call covers the non-turn events in between.
	if err := m.reserveSequence(inst); err != nil {
		m.logger.Warn("reserve session sequence", "session", inst.sess.ID, "error", err)
	}
	stored := m.hub.Append(inst.sess.ID, ev)
	if stored.Seq == 0 {
		return stored
	}
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
	m.persistInstance(inst)

	m.hub.Append(id, model.Event{
		Type:  model.EventControl,
		Actor: "mayfly",
		Data:  model.ControlData{Action: "close", Reason: reason},
	})
	m.hub.Append(id, model.Event{
		Type:  model.EventStatus,
		Actor: "mayfly",
		Text:  "Close requested.",
		Data:  model.StatusData{Status: model.StatusClosing, Reason: reason},
	})
	m.logger.Info("close requested", "session", id, "reason", reason)
	return nil
}

// Delete force-stops a session and marks it failed. A session restored from
// the record store has no runtime to stop, so Delete removes what the previous
// control plane left behind (a stray pod, supporting objects, and the journal)
// and drops its record. That also settles a session adoption gave up on.
//
// Delete and adoption register under the same lock. The tombstone below makes
// the outcome one of two: Delete removes a session that adoption already
// registered, or adoption refuses to register a session Delete removed. A
// retry in flight therefore cannot resurrect a deleted session.
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	inst := m.sessions[id]
	if inst == nil {
		_, stored := m.history[id]
		_, pending := m.adoptPending[id]
		if !stored && !pending {
			m.mu.Unlock()
			return ErrNotFound
		}
		m.deleted[id] = time.Now().UTC()
		delete(m.adoptPending, id)
		delete(m.history, id)
		m.mu.Unlock()
		m.deleteRecord(id)
		m.cleanupLeftovers(id)
		return nil
	}
	m.deleted[id] = time.Now().UTC()
	m.mu.Unlock()

	inst.mu.Lock()
	already := inst.finalized
	inst.mu.Unlock()
	if !already {
		m.finalize(inst, model.StatusFailed, "deleted", "session deleted by user")
	}
	m.cleanup(inst, true)
	return nil
}

// registerAdopted adds an instance that adoption rebuilt. It refuses an ID
// that DELETE removed while the pass was reading the cluster, so the retry
// cannot bring the session back.
func (m *Manager) registerAdopted(inst *instance) bool {
	id := inst.sess.ID
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, gone := m.deleted[id]; gone {
		return false
	}
	m.sessions[id] = inst
	m.tokenIndex[inst.tokenHash] = id
	return true
}

// cleanupLeftovers removes the pods, supporting objects, and journal of a
// session this control plane does not run. Object names come from the
// labelled objects while they exist, and fall back to the conventional names.
func (m *Manager) cleanupLeftovers(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if m.kube != nil {
		pods, err := m.kube.SessionPods(ctx)
		if err != nil {
			m.logger.Warn("cleanup stored session: list pods", "session", id, "error", err)
		}
		for _, pod := range pods {
			if pod.Labels[k8s.LabelSession] != id {
				continue
			}
			if err := m.kube.DeletePod(ctx, pod.Name, 0); err != nil {
				m.logger.Warn("cleanup stored session: delete pod", "session", id, "pod", pod.Name, "error", err)
			}
		}
		orphans, err := m.kube.OrphanSessions(ctx, m.liveSessionIDs())
		if err != nil {
			m.logger.Warn("cleanup stored session: list objects", "session", id, "error", err)
		}
		objs := orphans[id]
		spec := &k8s.SessionSpec{
			Namespace:           m.cfg.Sessions.Namespace,
			SessionID:           id,
			PodName:             "mayfly-" + id,
			SecretName:          orDefault(objs.Secret, "mayfly-"+id),
			ConfigMapName:       orDefault(objs.ConfigMap, "mayfly-"+id),
			PVCName:             orDefault(objs.PVC, "mayfly-"+id+"-workspace"),
			NetworkPolicyName:   "mayfly-" + id + "-netpol",
			PersistentWorkspace: true,
		}
		if err := m.kube.DeleteSession(ctx, spec, 0); err != nil {
			m.logger.Warn("cleanup stored session: delete objects", "session", id, "error", err)
		}
	}
	m.deleteJournal(id)
}

func orDefault(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
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
	m.persistInstance(inst)
	m.notifyFinished(final)

	m.removeToken(hash)
	text := "Session " + string(final) + "."
	if errMsg != "" {
		text += " " + errMsg
	}
	m.hub.Append(inst.sess.ID, model.Event{
		Type:  model.EventStatus,
		Actor: "mayfly",
		Text:  text,
		Data:  model.StatusData{Status: final, Reason: reason, Error: errMsg},
	})
	// Shutdown waits for cleanup. A control plane that exits partway through
	// would otherwise leave objects that adoption reads as a resumable
	// session. Once shutdown has started, cleanup runs inline instead.
	force := final == model.StatusFailed
	if !m.dispatch(func() { m.cleanup(inst, force) }) {
		m.cleanup(inst, force)
	}
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
	stale := append([]string(nil), inst.stalePods...)
	id := inst.sess.ID
	inst.mu.Unlock()
	if handle.Backend != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := m.runner.Stop(ctx, handle, force); err != nil {
			m.logger.Warn("cleanup session resources", "session", id, "error", err)
		}
		for _, pod := range stale {
			if err := m.kube.DeletePod(ctx, pod, 0); err != nil {
				m.logger.Warn("cleanup stale session pod", "session", id, "pod", pod, "error", err)
			}
		}
	}
	// The journal delete below removes the session's ledger records, which are
	// the only remaining reason to keep the grant.
	m.dropGrant(id)
	m.deleteJournal(id)
}

// deleteJournal drops the session's recorded steps once the artifact bundle is
// the durable history. A live runtime that keeps appending will be replaced;
// its resumed turn is already exported or failed.
func (m *Manager) deleteJournal(id string) {
	if m.journal == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.journal.Delete(ctx, id); err != nil {
		m.logger.Warn("delete session journal", "session", id, "error", err)
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
	if inst.restarting {
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
		Data:  model.ArtifactData{Key: key, URI: uri, Size: info.Size(), SHA256: digest, Partial: true},
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

// adoptFailure is one session's unresolved adoption: how many passes tried it
// and why the last one failed.
type adoptFailure struct {
	Attempts int
	Reason   string
}

// maxAdoptAttempts bounds how often one session's adoption is retried before
// its record is settled as failed.
const maxAdoptAttempts = 10

// adopt recovers sessions that were running when this control plane started.
// It returns the instances that need a replacement pod: sessions whose pod is
// gone are replaced, while sessions whose pod exists are polled.
func (m *Manager) adopt(ctx context.Context) ([]*instance, error) {
	return m.adoptScan(ctx, true)
}

// adoptScan recovers sessions from their pods and from saved objects. cleanup
// also runs garbage collection, which deletes objects no session owns. Only
// startup does that: a later pass can race a session this control plane is
// already running, and the keep set cannot see a session created after it was
// built.
func (m *Manager) adoptScan(ctx context.Context, cleanup bool) ([]*instance, error) {
	m.adoptIncomplete.Store(true)
	pods, err := m.kube.SessionPods(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	replacements, live, failed := m.adoptPods(ctx, pods, nil, now)

	// A session can lose its pod and keep its objects: the control plane can
	// stop between a replacement's pod deletion and its start. Those objects
	// hold everything a replacement needs, so adopt them instead of deleting.
	orphans, err := m.kube.OrphanSessions(ctx, live)
	if err != nil {
		// Garbage collection deletes anything it cannot see an owner for.
		// Without this list, recovery and collection would disagree about
		// which objects are resumable, so leave everything for the next pass.
		m.logger.Warn("adopt: list sessions without a pod; skipping garbage collection", "error", err)
		m.mergeAdoptFailures(failed, fmt.Sprintf("list sessions without a pod: %v", err))
		return replacements, nil
	}
	for id, objs := range orphans {
		if _, ok := m.instance(id); ok {
			continue
		}
		inst, preserve := m.adoptPodless(ctx, id, objs, now)
		if preserve {
			live[id] = struct{}{}
		}
		if inst == nil && preserve {
			// The objects are resumable but could not be read yet.
			failed[id] = "read the session objects"
		}
		if inst != nil {
			replacements = append(replacements, inst)
		}
	}
	if cleanup {
		// Keep every session this control plane runs. A live session between
		// pods has no row in the lists above, but it owns objects that must
		// survive. Only startup calls this, before the API serves requests,
		// so no session can start after this snapshot.
		for id := range m.liveSessionIDs() {
			live[id] = struct{}{}
		}
		m.garbageCollectSessions(ctx, live)
	}
	m.mergeAdoptFailures(failed, "")
	m.adoptIncomplete.Store(false)
	return replacements, nil
}

// adoptPods rebuilds a session for every pod it is given. only, when non-nil,
// limits the pass to those session IDs. It returns the instances that need a
// replacement, every session a pod still represents, and the sessions it could
// not rebuild with the reason.
func (m *Manager) adoptPods(ctx context.Context, pods []corev1.Pod, only map[string]struct{}, now time.Time) (replacements []*instance, live map[string]struct{}, failed map[string]string) {
	bySession := map[string][]corev1.Pod{}
	for i := range pods {
		id := pods[i].Labels[k8s.LabelSession]
		if id != "" {
			bySession[id] = append(bySession[id], pods[i])
		}
	}
	live = make(map[string]struct{}, len(bySession))
	for id := range bySession {
		live[id] = struct{}{}
	}
	failed = map[string]string{}
	for id, group := range bySession {
		if only != nil {
			if _, ok := only[id]; !ok {
				continue
			}
		}
		if _, ok := m.instance(id); ok {
			continue
		}
		pod := activeSessionPod(group)
		base := "mayfly-" + id
		configName := base
		raw, err := m.kube.SessionConfig(ctx, configName)
		if err != nil {
			configName = pod.Name
			raw, err = m.kube.SessionConfig(ctx, configName)
		}
		if apierrors.IsNotFound(err) {
			// Cleanup deletes the ConfigMap first, so this session was
			// finalized by a control plane that stopped partway through.
			// Nothing can resume it: remove the pod and let garbage
			// collection take the rest.
			m.logger.Warn("adopt: session config is gone; deleting the pod of a finalized session", "session", id, "pod", pod.Name)
			for i := range group {
				if err := m.kube.DeletePod(ctx, group[i].Name, 0); err != nil {
					m.logger.Warn("adopt: delete pod of finalized session", "session", id, "pod", group[i].Name, "error", err)
				}
			}
			delete(live, id)
			continue
		}
		if err != nil {
			m.logger.Warn("adopt: read config", "pod", pod.Name, "error", err)
			failed[id] = "read the session config"
			continue
		}
		var cfg sessioncfg.Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			m.logger.Warn("adopt: parse config", "pod", pod.Name, "error", err)
			continue
		}
		tokenName := base
		keys, err := m.kube.SessionKeys(ctx, tokenName)
		if err != nil {
			tokenName = pod.Name
			keys, err = m.kube.SessionKeys(ctx, tokenName)
		}
		if err != nil {
			m.logger.Warn("adopt: read session credentials", "pod", pod.Name, "error", err)
			failed[id] = "read the session credentials"
			continue
		}
		token := keys.Token
		m.warnMissingGatewayKey(cfg, keys.GatewayKey, id)
		image, command, args, env := podLaunch(pod)
		persistent, pvcName := podWorkspace(pod)
		inst := &instance{
			sess: model.Session{
				ID:           id,
				Title:        cfg.Title,
				Status:       model.StatusPending,
				Prompt:       cfg.Prompt,
				Agent:        cfg.Agent,
				Model:        cfg.Model,
				Skills:       skillNames(cfg.Skills),
				MCPServers:   mcpNames(cfg.MCPServers),
				Plugins:      pluginNames(cfg.Plugins),
				Trigger:      model.TriggerInfo{Kind: "adopted"},
				Backend:      "kubernetes",
				PodName:      pod.Name,
				TTLSeconds:   cfg.TTLSeconds,
				CreatedAt:    cfg.CreatedAt,
				ExpiresAt:    cfg.CreatedAt.Add(time.Duration(cfg.TTLSeconds) * time.Second),
				LastActivity: now,
			},
			token:     token,
			tokenHash: hashToken(token),
			// The key is read back from the Secret, so a replacement pod and
			// this control plane sign the same call tokens.
			sidecarKey: keys.GatewayKey,
			runtimeCfg: cfg,
			// The session may still read the inputs it was started with.
			artifacts:      artifactMap(cfg.Artifacts),
			idempotencyKey: cfg.IdempotencyKey,
			image:          image,
			command:        command,
			args:           args,
			env:            env,
			launchedAt:     now,
			restarts:       replacementAttempt(pod.Name),
			handle: runner.Handle{
				Backend:             "kubernetes",
				SessionID:           id,
				Namespace:           pod.Namespace,
				PodName:             pod.Name,
				SecretName:          tokenName,
				ConfigMapName:       configName,
				PVCName:             pvcName,
				PersistentWorkspace: persistent,
				NetworkPolicyName:   "mayfly-" + id + "-netpol",
			},
		}
		if inst.sess.Title == "" {
			inst.sess.Title = "Session " + id
		}
		// Seed before registering: a session whose turn keys cannot be
		// placed above the previous lifetime's must not accept messages. The
		// pod keeps its objects, so garbage collection leaves it alone.
		empty, err := m.seedSequence(ctx, id)
		if err != nil {
			m.logger.Error("adopt: cannot seed session sequence; leaving pod unadopted", "session", id, "error", err)
			failed[id] = "seed the session sequence"
			continue
		}
		if empty {
			m.logger.Warn("adopt: session has no journal state; was the control-plane volume persisted?", "session", id)
		}
		if !m.registerAdopted(inst) {
			// DELETE removed the session while this pass read its objects.
			delete(live, id)
			m.logger.Info("adopt: session was deleted during the pass", "session", id)
			continue
		}
		m.registerGrant(inst)
		m.hub.Create(id)
		m.persistInstance(inst)
		m.hub.Append(id, model.Event{Type: model.EventSystem, Actor: "mayfly", Text: "Session recovered by control plane restart."})
		m.restoreJournalThread(id)
		m.redeliverPending(inst)
		m.logger.Info("adopted session", "session", id, "pod", pod.Name, "restarts", inst.restarts)
	}
	return replacements, live, failed
}

// adoptPodless rebuilds an instance whose pod is gone but whose ConfigMap,
// Secret, and workspace claim survived. It returns the instance unless a
// transient read failure makes adoption unsafe; preserve asks the caller to
// keep the objects for the next control-plane start in that case.
func (m *Manager) adoptPodless(ctx context.Context, id string, objs k8s.OrphanObjects, now time.Time) (inst *instance, preserve bool) {
	if objs.ConfigMap == "" || objs.Secret == "" || objs.PVC == "" {
		// Nothing can resume without all three; garbage collection owns it.
		return nil, false
	}
	raw, err := m.kube.SessionConfig(ctx, objs.ConfigMap)
	if err != nil {
		m.logger.Warn("adopt: read config of session without a pod", "session", id, "error", err)
		return nil, true
	}
	var cfg sessioncfg.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		m.logger.Warn("adopt: parse config of session without a pod", "session", id, "error", err)
		return nil, false
	}
	keys, err := m.kube.SessionKeys(ctx, objs.Secret)
	if err != nil {
		m.logger.Warn("adopt: read session credentials without a pod", "session", id, "error", err)
		return nil, true
	}
	m.warnMissingGatewayKey(cfg, keys.GatewayKey, id)
	agent, err := m.resolveAgent(cfg.Agent)
	if err != nil {
		// Without the manifest there is no image or command to start.
		m.logger.Warn("adopt: resolve agent of session without a pod", "session", id, "error", err)
		return nil, false
	}
	image := agent.Runtime.Image
	if image == "" {
		image = m.cfg.Runtime.Image
	}
	inst = &instance{
		sess: model.Session{
			ID:           id,
			Title:        cfg.Title,
			Status:       model.StatusRunning,
			Prompt:       cfg.Prompt,
			Agent:        agent.Name,
			Model:        cfg.Model,
			Skills:       skillNames(cfg.Skills),
			MCPServers:   mcpNames(cfg.MCPServers),
			Plugins:      pluginNames(cfg.Plugins),
			Trigger:      model.TriggerInfo{Kind: "adopted"},
			Backend:      "kubernetes",
			TTLSeconds:   cfg.TTLSeconds,
			CreatedAt:    cfg.CreatedAt,
			ExpiresAt:    cfg.CreatedAt.Add(time.Duration(cfg.TTLSeconds) * time.Second),
			LastActivity: now,
		},
		token:     keys.Token,
		tokenHash: hashToken(keys.Token),
		// The key is read back from the Secret, so the replacement pod and
		// this control plane sign the same call tokens.
		sidecarKey: keys.GatewayKey,
		runtimeCfg: cfg,
		// The session may still read the inputs it was started with.
		artifacts:      artifactMap(cfg.Artifacts),
		idempotencyKey: cfg.IdempotencyKey,
		image:          image,
		command:        append([]string(nil), agent.Runtime.Command...),
		args:           append([]string(nil), agent.Runtime.Args...),
		env:            mergeEnv(m.cfg.Runtime.Env, agent.Runtime.Env),
		launchedAt:     now,
		handle: runner.Handle{
			Backend:             "kubernetes",
			SessionID:           id,
			Namespace:           m.cfg.Sessions.Namespace,
			SecretName:          objs.Secret,
			ConfigMapName:       objs.ConfigMap,
			PVCName:             objs.PVC,
			PersistentWorkspace: true,
			NetworkPolicyName:   "mayfly-" + id + "-netpol",
		},
	}
	if inst.sess.Title == "" {
		inst.sess.Title = "Session " + id
	}
	empty, err := m.seedSequence(ctx, id)
	if err != nil {
		m.logger.Error("adopt: cannot seed session sequence; leaving session unadopted", "session", id, "error", err)
		return nil, true
	}
	if empty {
		m.logger.Warn("adopt: session has no journal state; was the control-plane volume persisted?", "session", id)
	}
	if !m.registerAdopted(inst) {
		// DELETE removed the session while this pass read its objects.
		m.hub.Remove(id)
		m.logger.Info("adopt: session was deleted during the pass", "session", id)
		return nil, false
	}
	m.registerGrant(inst)
	m.hub.Create(id)
	m.persistInstance(inst)
	m.hub.Append(id, model.Event{Type: model.EventSystem, Actor: "mayfly", Text: "Session recovered by control plane restart."})
	m.restoreJournalThread(id)
	m.redeliverPending(inst)
	m.logger.Info("adopted session without a pod; starting a replacement", "session", id)
	return inst, true
}

// seedSequence places an adopted session's hub above every sequence number a
// previous control plane could have handed out. Journal turns are keyed by
// that number, so reusing one would replay an old turn instead of handling a
// new message.
//
// It also reports whether the journal held no state at all, which means the
// control plane's volume did not survive and a running pod may be mid-turn on
// a key this lifetime never handed out.
func (m *Manager) seedSequence(ctx context.Context, id string) (empty bool, err error) {
	if m.journal == nil {
		return false, nil
	}
	high, err := m.journal.HighWater(ctx, id)
	if err != nil {
		return false, fmt.Errorf("read session sequence floor: %w", err)
	}
	last, err := m.journal.MaxTurn(ctx, id)
	if err != nil {
		return false, fmt.Errorf("read last journal turn: %w", err)
	}
	floor := max(high, last)
	empty = floor == 0
	if empty {
		floor = lostJournalFloor()
	}
	m.hub.Seed(id, floor)
	return empty, nil
}

// lostJournalFloor is the sequence floor for a session whose journal did not
// survive. Every reserved range lived only in that journal, so nothing bounds
// the keys a previous lifetime handed out, and a runtime may still be
// finishing one of them. The current time in microseconds is above them all:
// a lifetime hands out far fewer than one sequence per microsecond, so even a
// session whose journal was lost before keeps its old keys below the next
// floor. A new message then never shares a turn key with the in-flight turn,
// and that turn's appends pass the usual bound. It stays well inside the
// integers a JSON client can represent exactly.
func lostJournalFloor() int64 {
	return time.Now().UnixMicro()
}

// garbageCollectSessions removes the labelled objects of sessions that no
// live runtime can resume. Nothing else would ever own or delete the Secret,
// ConfigMap, network policy, and workspace claim of such a session.
func (m *Manager) garbageCollectSessions(ctx context.Context, live map[string]struct{}) {
	reaped, err := m.kube.GarbageCollect(ctx, live)
	if err != nil {
		m.logger.Warn("garbage collect orphaned sessions", "error", err)
	}
	for _, id := range reaped {
		m.deleteJournal(id)
		m.logger.Info("garbage collected orphaned session", "session", id)
	}
}

// podWorkspace reports the workspace claim of an adopted pod.
func podWorkspace(pod corev1.Pod) (bool, string) {
	for _, volume := range pod.Spec.Volumes {
		if volume.Name != "workspace" || volume.PersistentVolumeClaim == nil {
			continue
		}
		return true, volume.PersistentVolumeClaim.ClaimName
	}
	return false, ""
}

// activeSessionPod picks the pod still representing the session: a
// non-terminal pod wins, then the newest creation time.
func activeSessionPod(pods []corev1.Pod) corev1.Pod {
	best := pods[0]
	for _, pod := range pods[1:] {
		if betterPod(pod, best) {
			best = pod
		}
	}
	return best
}

func betterPod(a, b corev1.Pod) bool {
	activeA := a.Status.Phase != "Succeeded" && a.Status.Phase != "Failed"
	activeB := b.Status.Phase != "Succeeded" && b.Status.Phase != "Failed"
	if activeA != activeB {
		return activeA
	}
	return a.CreationTimestamp.After(b.CreationTimestamp.Time)
}

// podLaunch recovers the container launch parameters of an adopted pod so a
// later replacement can use the same image and environment.
func podLaunch(pod corev1.Pod) (string, []string, []string, map[string]string) {
	if len(pod.Spec.Containers) == 0 {
		return "", nil, nil, nil
	}
	c := pod.Spec.Containers[0]
	env := map[string]string{}
	for _, item := range c.Env {
		switch item.Name {
		case "HOME", "XDG_CACHE_HOME", "NPM_CONFIG_CACHE":
			continue
		}
		if item.Value == "" || sessioncfg.IsReservedEnv(item.Name) {
			continue
		}
		env[item.Name] = item.Value
	}
	if len(env) == 0 {
		env = nil
	}
	return c.Image, append([]string(nil), c.Command...), append([]string(nil), c.Args...), env
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

// adoptRetryInterval is how long the first retry of an unfinished adoption
// wait, and maxAdoptRetryInterval caps the backoff. They are variables so tests
// do not wait for them.
var (
	adoptRetryInterval    = 15 * time.Second
	maxAdoptRetryInterval = 2 * time.Minute
)

func (m *Manager) maintenance() {
	reaper := time.NewTicker(5 * time.Second)
	defer reaper.Stop()
	var pollC <-chan time.Time
	if m.kube != nil {
		t := time.NewTicker(m.cfg.Runtime.PollInterval.Std())
		defer t.Stop()
		pollC = t.C
	}
	delay := adoptRetryInterval
	var nextAdopt time.Time
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-reaper.C:
			m.reap()
			if m.adoptNeeded() && (nextAdopt.IsZero() || !time.Now().Before(nextAdopt)) {
				before := m.pendingCount()
				m.retryAdopt()
				if m.pendingCount() < before {
					delay = adoptRetryInterval
				} else {
					delay *= 2
					if delay > maxAdoptRetryInterval {
						delay = maxAdoptRetryInterval
					}
				}
				nextAdopt = time.Now().Add(delay)
			}
			if !m.adoptNeeded() {
				nextAdopt = time.Time{}
				delay = adoptRetryInterval
			}
		case <-pollC:
			m.pollPods(m.ctx)
		}
	}
}

// retryAdopt finishes adoption that could not complete. A pass that never saw
// every session is repeated without garbage collection. Otherwise only the
// sessions a previous pass could not rebuild are retried, and no object of any
// other session is scanned for deletion.
func (m *Manager) retryAdopt() {
	if m.kube == nil {
		return
	}
	if m.adoptIncomplete.Load() {
		replacements, err := m.adoptScan(m.ctx, false)
		if err != nil {
			m.logger.Warn("adopt existing sessions", "error", err)
			return
		}
		m.restartAdopted(replacements)
		m.settleInterrupted()
		return
	}
	pending := m.pendingAdoptions()
	if len(pending) == 0 {
		return
	}
	pods, err := m.kube.SessionPods(m.ctx)
	if err != nil {
		m.logger.Warn("adopt pending sessions", "error", err)
		m.mergeAdoptFailures(nil, fmt.Sprintf("list session pods: %v", err))
		return
	}
	now := time.Now().UTC()
	replacements, live, failed := m.adoptPods(m.ctx, pods, pending, now)
	missing := make(map[string]struct{}, len(pending))
	for id := range pending {
		if _, ok := live[id]; ok {
			continue
		}
		if _, ok := failed[id]; ok {
			continue
		}
		missing[id] = struct{}{}
	}
	if len(missing) > 0 {
		// A pending session whose pod is gone can still have saved objects.
		// Read them; this pass never deletes anything.
		orphans, err := m.kube.OrphanSessions(m.ctx, m.liveSessionIDs())
		if err != nil {
			// The pass could not examine every pending session, so a record is
			// left as it is until a later pass does.
			m.logger.Warn("adopt pending sessions: list saved objects", "error", err)
			m.mergeAdoptFailures(failed, fmt.Sprintf("list sessions without a pod: %v", err))
			m.restartAdopted(replacements)
			return
		}
		for id := range missing {
			objs, ok := orphans[id]
			if !ok {
				continue
			}
			inst, preserve := m.adoptPodless(m.ctx, id, objs, now)
			if inst != nil {
				replacements = append(replacements, inst)
			}
			if inst == nil && preserve {
				failed[id] = "read the session objects"
			}
		}
	}
	m.mergeAdoptFailures(failed, "")
	m.restartAdopted(replacements)
	m.settleInterrupted()
}

// restartAdopted starts the replacement pod of every session a retry rebuilt
// without a pod.
func (m *Manager) restartAdopted(replacements []*instance) {
	for _, inst := range replacements {
		m.restartSoon(inst, "pod missing while adopting after a restart")
	}
}

func (m *Manager) reap() {
	now := time.Now().UTC()
	m.pruneRecords(now)
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
		// Connect timeout starts when the container is launched, not when it
		// reports Running: a pod that never schedules must not hold a slot
		// until its TTL expires.
		connectFrom := inst.launchedAt
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
	bySession := map[string][]corev1.Pod{}
	for i := range pods {
		id := pods[i].Labels[k8s.LabelSession]
		if id != "" {
			bySession[id] = append(bySession[id], pods[i])
		}
	}
	for _, inst := range m.snapshot() {
		inst.mu.Lock()
		id := inst.sess.ID
		podName := inst.handle.PodName
		finalized := inst.finalized
		inst.mu.Unlock()
		if finalized || podName == "" {
			continue
		}
		var current *corev1.Pod
		for i := range bySession[id] {
			if bySession[id][i].Name == podName {
				current = &bySession[id][i]
				break
			}
		}
		if current == nil {
			m.podMissing(inst, now)
			continue
		}
		if current.DeletionTimestamp != nil {
			m.podDeleting(inst, now)
			continue
		}
		m.podState(inst, current, now)
	}
}

// podDeleting handles a pod with a deletion timestamp. A graceful delete
// (drain, eviction, preemption, or kubectl delete) normally makes the runtime
// exit and the pod fail on its own, but a node that stopped responding leaves
// the pod terminating for minutes. After the grace period the session needs a
// replacement.
func (m *Manager) podDeleting(inst *instance, now time.Time) {
	inst.mu.Lock()
	if inst.finalized || inst.restarting || inst.handle.PodName == "" {
		inst.mu.Unlock()
		return
	}
	if inst.podDeletingSince.IsZero() {
		inst.podDeletingSince = now
	}
	deletingSince := inst.podDeletingSince
	status := inst.sess.Status
	id := inst.sess.ID
	inst.mu.Unlock()
	if now.Sub(deletingSince) < podDeletingGrace {
		return
	}
	switch status {
	case model.StatusPending, model.StatusRunning:
		m.restartSoon(inst, "pod is terminating")
	case model.StatusClosing:
		m.fallback(id, "pod is terminating")
	}
}

// podState handles the current pod of one session. A Failed pod on an active
// session is replaced; a Succeeded pod that never finalized falls back.
func (m *Manager) podState(inst *instance, pod *corev1.Pod, now time.Time) {
	terminal := pod.Status.Phase == "Succeeded" || pod.Status.Phase == "Failed"
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
		case strings.HasPrefix(failureDetail, "InvalidImageName"),
			strings.HasPrefix(failureDetail, "CreateContainerConfigError"),
			strings.HasPrefix(failureDetail, "CreateContainerError"),
			strings.HasPrefix(failureDetail, "RunContainerError"):
			// These never resolve on their own. Waiting only delays the
			// fallback and burns the replacement budget.
			waitingFailure = true
		case now.Sub(pod.CreationTimestamp.Time) > 30*time.Second:
			// An image pull can still succeed, so confirm it first.
			switch {
			case strings.HasPrefix(failureDetail, "ErrImagePull"),
				strings.HasPrefix(failureDetail, "ImagePullBackOff"):
				waitingFailure = true
			}
		}
	}

	inst.mu.Lock()
	if inst.finalized || inst.handle.PodName != pod.Name {
		inst.mu.Unlock()
		return
	}
	inst.podMissingSince = time.Time{}
	inst.podDeletingSince = time.Time{}
	if terminal {
		if inst.podTerminalSince.IsZero() {
			inst.podTerminalSince = now
		}
	} else {
		inst.podTerminalSince = time.Time{}
	}
	if pod.Status.Phase == "Running" && !podReady(*pod) {
		if inst.podNotReadySince.IsZero() {
			inst.podNotReadySince = now
		}
	} else {
		inst.podNotReadySince = time.Time{}
	}
	terminalSince := inst.podTerminalSince
	notReadySince := inst.podNotReadySince
	status := inst.sess.Status
	id := inst.sess.ID
	finalized := inst.finalized
	restarting := inst.restarting
	inst.mu.Unlock()

	if finalized || restarting {
		return
	}
	if waitingFailure {
		switch status {
		case model.StatusPending, model.StatusRunning:
			m.restartSoon(inst, "session container cannot start: "+failureDetail)
		case model.StatusClosing:
			// The session is ending anyway; export now instead of waiting for
			// a container that will never start.
			m.fallback(id, "pod cannot start: "+failureDetail)
		}
		return
	}
	switch status {
	case model.StatusPending, model.StatusRunning:
		if pod.Status.Phase == "Failed" {
			reason := "pod failed"
			if failureDetail != "" {
				reason = "pod failed: " + failureDetail
			}
			m.restartSoon(inst, reason)
			return
		}
		if !notReadySince.IsZero() && now.Sub(notReadySince) > podNotReadyGrace {
			m.restartSoon(inst, "pod is not ready")
			return
		}
		if terminal && !terminalSince.IsZero() && now.Sub(terminalSince) > podTerminalGrace {
			m.fallback(id, "pod terminated without finalize")
		}
	case model.StatusClosing:
		if terminal && !terminalSince.IsZero() && now.Sub(terminalSince) > podTerminalGrace {
			reason := "pod terminated without finalize"
			if failureDetail != "" {
				reason = "pod failed: " + failureDetail
			}
			m.fallback(id, reason)
		}
	}
}

// podReady reports whether the kubelet marked every container ready.
func podReady(pod corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// podMissing restarts a session whose current pod was deleted. A closing or
// terminal session falls back instead.
func (m *Manager) podMissing(inst *instance, now time.Time) {
	inst.mu.Lock()
	if inst.finalized || inst.restarting || inst.handle.PodName == "" {
		inst.mu.Unlock()
		return
	}
	if inst.podMissingSince.IsZero() {
		inst.podMissingSince = now
	}
	missingSince := inst.podMissingSince
	status := inst.sess.Status
	id := inst.sess.ID
	inst.mu.Unlock()
	if now.Sub(missingSince) < podMissingGrace {
		return
	}
	switch status {
	case model.StatusPending, model.StatusRunning:
		m.restartSoon(inst, "pod disappeared")
	case model.StatusClosing:
		m.fallback(id, "pod disappeared")
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
