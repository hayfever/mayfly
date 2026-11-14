package session

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/runner"
)

const (
	podMissingGrace   = 3 * time.Second
	podDeletingGrace  = 30 * time.Second
	podNotReadyGrace  = 60 * time.Second
	restartStartGrace = 90 * time.Second

	// podDeleteAttempts bounds how often a replacement tries to delete the
	// pod it replaces. The waits grow linearly from podDeleteBackoff.
	podDeleteAttempts = 4
)

// podDeleteBackoff is a variable so tests do not wait for it.
var podDeleteBackoff = 500 * time.Millisecond

func cloneEnv(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// watchLocal replaces the process exit handler for the local backend. The
// channel identity guards against an old process reporting exit after a
// replacement already started.
func (m *Manager) watchLocal(inst *instance, h runner.Handle) {
	if h.Done == nil {
		return
	}
	var ctxDone <-chan struct{}
	if m.ctx != nil {
		ctxDone = m.ctx.Done()
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		select {
		case <-h.Done:
			time.Sleep(2 * time.Second)
			m.localRuntimeExited(inst, h)
		case <-ctxDone:
		}
	}()
}

func (m *Manager) localRuntimeExited(inst *instance, h runner.Handle) {
	inst.mu.Lock()
	if inst.finalized || inst.handle.Done != h.Done {
		inst.mu.Unlock()
		return
	}
	status := inst.sess.Status
	id := inst.sess.ID
	inst.mu.Unlock()
	switch status {
	case model.StatusPending, model.StatusRunning:
		m.restart(inst, "session process exited")
	default:
		m.fallback(id, "runtime exited without finalizing")
	}
}

// restartSoon runs a replacement in the background. Pod polling is shared by
// every session, so a replacement that waits for its grace period must not
// stop failure detection for the others.
func (m *Manager) restartSoon(inst *instance, reason string) {
	inst.mu.Lock()
	if inst.finalized || inst.restarting {
		inst.mu.Unlock()
		return
	}
	inst.mu.Unlock()
	m.dispatch(func() { m.restart(inst, reason) })
}

// restart launches a replacement container for an active session. It reuses
// the session token, ConfigMap, Secret, NetworkPolicy, and workspace claim, so
// the replacement's journal replay can pick up the interrupted turn. The old
// pod is removed first and fallback runs only after every replacement attempt
// failed.
func (m *Manager) restart(inst *instance, reason string) {
	inst.mu.Lock()
	if inst.finalized || inst.restarting {
		inst.mu.Unlock()
		return
	}
	if m.ctx != nil && m.ctx.Err() != nil {
		inst.mu.Unlock()
		return
	}
	status := inst.sess.Status
	if status != model.StatusPending && status != model.StatusRunning {
		inst.mu.Unlock()
		return
	}
	id := inst.sess.ID
	old := inst.handle
	if old.Backend == "kubernetes" && !old.PersistentWorkspace {
		inst.mu.Unlock()
		m.fallback(id, reason+": the workspace is not persistent, so the turn cannot resume")
		return
	}
	if inst.restarts >= m.cfg.Sessions.MaxRestarts {
		inst.mu.Unlock()
		m.fallback(id, reason)
		return
	}
	inst.restarting = true
	inst.restarts++
	attempt := inst.restarts
	token := inst.token
	image, command, args, env := inst.image, inst.command, inst.args, cloneEnv(inst.env)
	expires := inst.sess.ExpiresAt
	inst.podMissingSince = time.Time{}
	inst.podDeletingSince = time.Time{}
	inst.podNotReadySince = time.Time{}
	inst.podTerminalSince = time.Time{}
	inst.launchedAt = time.Now().UTC()
	inst.mu.Unlock()

	m.hub.Append(id, model.Event{
		Type:  model.EventSystem,
		Actor: "mayfly",
		Text:  fmt.Sprintf("Session container failed (%s). Starting replacement %d of %d.", reason, attempt, m.cfg.Sessions.MaxRestarts),
	})
	m.logger.Warn("restarting session container", "session", id, "attempt", attempt, "reason", reason)

	base := context.Background()
	if m.ctx != nil {
		base = m.ctx
	}
	ctx, cancel := context.WithTimeout(base, restartStartGrace)
	defer cancel()

	if old.Backend == "kubernetes" && old.PodName != "" && m.kube != nil {
		if err := m.deletePod(ctx, old.PodName); err != nil {
			// The old pod still holds the ReadWriteOnce claim, so the
			// replacement may not start. Cleanup removes it later.
			m.logger.Warn("delete failed session pod", "session", id, "pod", old.PodName, "error", err)
			inst.mu.Lock()
			inst.stalePods = append(inst.stalePods, old.PodName)
			inst.mu.Unlock()
		}
	}

	podName := old.PodName
	if old.Backend == "kubernetes" {
		podName = fmt.Sprintf("mayfly-%s-r%d", id, attempt)
	}
	ttl := time.Until(expires)
	if ttl <= 0 {
		ttl = time.Minute
	}

	inst.mu.Lock()
	k8sSpec, prepErr := m.prepare(ctx, inst, podName, token, ttl, image, command, args, env)
	if k8sSpec != nil {
		k8sSpec.SecretName = old.SecretName
		k8sSpec.ConfigMapName = old.ConfigMapName
		k8sSpec.PersistentWorkspace = old.PersistentWorkspace
		if old.PVCName != "" {
			k8sSpec.PVCName = old.PVCName
		}
	}
	runtimeCfg := inst.runtimeCfg
	inst.mu.Unlock()

	var h runner.Handle
	err := prepErr
	var pluginSecrets map[string]map[string]string
	if err == nil {
		// A local replacement re-reads the plugin secrets, so an operator's
		// changed environment reaches the replacement.
		pluginSecrets, err = m.pluginSecretValues(runtimeCfg.Plugins)
	}
	if err == nil {
		stateDir := old.StateDir
		if stateDir == "" {
			stateDir = filepath.Join(m.stateDir, "sessions", id)
		}
		h, err = m.runner.Start(ctx, runner.Spec{
			Backend:       m.cfg.Runtime.Backend,
			SessionID:     id,
			Namespace:     m.cfg.Sessions.Namespace,
			PodName:       podName,
			SecretName:    old.SecretName,
			ConfigMapName: old.ConfigMapName,
			StateDir:      stateDir,
			Token:         token,
			Env:           env,
			Runtime:       runtimeCfg,
			PluginSecrets: pluginSecrets,
			Reuse:         old.Backend == "kubernetes",
			K8sSpec:       k8sSpec,
		})
	}
	if err != nil {
		inst.mu.Lock()
		inst.restarting = false
		more := inst.restarts < m.cfg.Sessions.MaxRestarts
		inst.mu.Unlock()
		m.logger.Error("session replacement failed", "session", id, "attempt", attempt, "error", err)
		m.hub.Append(id, model.Event{Type: model.EventError, Actor: "mayfly", Text: "Replacement container failed: " + err.Error()})
		if more {
			m.restart(inst, reason)
			return
		}
		m.fallback(id, reason+": replacement failed: "+err.Error())
		return
	}

	inst.mu.Lock()
	inst.handle = h
	inst.restarting = false
	inst.podMissingSince = time.Time{}
	inst.podDeletingSince = time.Time{}
	inst.podNotReadySince = time.Time{}
	inst.podTerminalSince = time.Time{}
	inst.sess.PodName = h.PodName
	finalized := inst.finalized
	inst.mu.Unlock()
	if finalized {
		// The runtime finalized while the replacement was starting.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = m.runner.Stop(stopCtx, h, true)
		stopCancel()
		return
	}

	m.hub.Append(id, model.Event{
		Type:  model.EventStatus,
		Actor: "mayfly",
		Text:  "Session container restarted; resuming the journaled turn.",
		Data:  map[string]any{"status": string(model.StatusRunning), "restart": attempt},
	})
	m.logger.Info("session container replaced", "session", id, "pod", podName, "attempt", attempt)
	if h.Done != nil {
		m.watchLocal(inst, h)
	}
}

// deletePod removes the pod a replacement takes over from. A live old pod
// keeps the ReadWriteOnce workspace claim and can keep appending to the
// journal, so a transient API error is retried a few times before the
// replacement goes ahead without it.
func (m *Manager) deletePod(ctx context.Context, name string) error {
	var err error
	for attempt := 1; ; attempt++ {
		if err = m.kube.DeletePod(ctx, name, 0); err == nil || attempt == podDeleteAttempts {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(attempt) * podDeleteBackoff):
		}
	}
}

// restoreJournalThread rebuilds the visible part of the last journaled turn
// after a control-plane restart. Events are restored with the step keys the
// runtime's journal publication uses, so a retried append cannot duplicate an
// event that was already restored. Restored events never enter the inbox: a
// live runtime continues on its own, and a replacement resumes from the
// journal rather than from a second delivery of the old message.
func (m *Manager) restoreJournalThread(id string) {
	if m.journal == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	last, err := m.journal.LastTurn(ctx, id)
	if err != nil {
		m.logger.Warn("restore thread: read last turn", "session", id, "error", err)
		return
	}
	if last <= 0 {
		return
	}
	entries, err := m.journal.Turn(ctx, id, last)
	if err != nil {
		m.logger.Warn("restore thread: read turn", "session", id, "turn", last, "error", err)
		return
	}
	restored := 0
	for _, entry := range entries {
		if entry.Step == 0 {
			// Only the user message that started the turn belongs on the
			// restored thread. Anything else was never a human message.
			var ev model.Event
			if err := json.Unmarshal(entry.Input, &ev); err != nil || ev.Type != model.EventUserMessage {
				continue
			}
			ev.Seq = 0
			ev.SessionID = ""
			ev.StepKey = userStepKey(entry.Turn)
			if ev.Time.IsZero() {
				ev.Time = entry.Time
			}
			if stored := m.hub.Restore(id, ev); stored.Seq != 0 {
				restored++
				m.countRestoredMessage(id, ev)
			}
			continue
		}
		for _, ev := range stepThreadEvents(entry) {
			if stored := m.hub.Restore(id, ev); stored.Seq != 0 {
				restored++
				m.countRestoredMessage(id, ev)
			}
		}
	}
	if restored > 0 {
		m.logger.Info("restored journaled thread", "session", id, "turn", last, "events", restored)
	}
}

func (m *Manager) countRestoredMessage(id string, ev model.Event) {
	if ev.Type != model.EventUserMessage && ev.Type != model.EventAssistantMessage {
		return
	}
	inst, ok := m.instance(id)
	if !ok {
		return
	}
	inst.mu.Lock()
	inst.sess.MessageCount++
	inst.mu.Unlock()
}

// replacementAttempt reads the retry number from a replacement pod name.
func replacementAttempt(podName string) int {
	idx := strings.LastIndex(podName, "-r")
	if idx < 0 {
		return 0
	}
	n, err := strconv.Atoi(podName[idx+2:])
	if err != nil || n < 0 {
		return 0
	}
	return n
}
