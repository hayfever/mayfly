package session

import (
	"context"
	"fmt"
	"time"

	"github.com/hayfever/mayfly/internal/model"
)

// recordTimeout bounds one write to the record store, so a stalled volume
// slows a session transition but never blocks a request.
const recordTimeout = 5 * time.Second

// deletedRetention bounds how long DELETE remembers an ID. An adoption pass
// that could still register the session completes long before this; the
// tombstone only has to outlive one pass.
const deletedRetention = 24 * time.Hour

// SetRecords attaches the store that keeps session records and the artifact
// index across restarts. Call it before Start, so startup loads the same store
// the manager writes to.
func (m *Manager) SetRecords(store RecordStore) {
	if store == nil {
		store = NewMemoryRecords()
	}
	m.records = store
}

// listRecords reads every stored session record. Start reads them once, so
// adoption can seed a live session from its record before loadRecords restores
// the rest.
func (m *Manager) listRecords(ctx context.Context) ([]Record, error) {
	if m.records == nil {
		return nil, nil
	}
	records, err := m.records.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("load session records: %w", err)
	}
	return records, nil
}

// loadRecords restores the sessions and artifact metadata a previous
// control-plane lifetime wrote. records is the list Start already read.
// It does not judge a non-terminal record: settleInterrupted does that once
// adoption has reported what it can see.
func (m *Manager) loadRecords(ctx context.Context, records []Record) error {
	if m.records == nil {
		return nil
	}
	artifacts, err := m.records.ListArtifacts(ctx)
	if err != nil {
		return fmt.Errorf("load artifact index: %w", err)
	}
	m.mu.Lock()
	if m.history == nil {
		m.history = map[string]model.Session{}
	}
	for _, entry := range artifacts {
		m.artifacts[entry.SessionID] = entry.Info
	}
	m.mu.Unlock()
	for _, rec := range records {
		sess := rec.Session
		if _, live := m.instance(sess.ID); live {
			// Adoption owns the record of a session it rebuilt.
			continue
		}
		m.mu.Lock()
		m.history[sess.ID] = sess
		m.mu.Unlock()
	}
	m.logger.Info("loaded session records", "sessions", len(records), "artifacts", len(artifacts))
	return nil
}

// settleInterrupted marks a restored record failed when no runtime can
// continue it. Adoption owns an ID it saw but could not rebuild, and a pass
// that failed did not see every session, so those records wait for the retry
// rather than being reported as failed while their pod may still run.
//
// A pending session the record store never held gets a failed record at once,
// so it can be listed and deleted, but it stays in the retry set with the same
// backoff and cap as a session that had a record. Only the cap settles a
// pending session for good; settlement then removes its leftovers. DELETE does
// the same on request. Nothing here touches a session that is live.
func (m *Manager) settleInterrupted() {
	if m.adoptIncomplete.Load() {
		return
	}
	now := time.Now().UTC()
	type settle struct {
		session model.Session
		reason  string
	}
	m.mu.Lock()
	var stale []settle
	var cleanup []string
	for id, sess := range m.history {
		if _, gone := m.deleted[id]; gone {
			delete(m.history, id)
			delete(m.adoptPending, id)
			continue
		}
		if _, live := m.sessions[id]; live {
			// Adoption rebuilt this session; the live instance owns it now.
			delete(m.history, id)
			delete(m.adoptPending, id)
			continue
		}
		entry, pending := m.adoptPending[id]
		if pending && entry.Attempts < maxAdoptAttempts {
			// Keep retrying, whatever the record says. A session this
			// control plane already settled as failed stays listed, but its
			// reason must carry the current attempt count instead of the
			// count from the first failed pass.
			if sess.Status == model.StatusFailed {
				stale = append(stale, settle{session: sess, reason: adoptAttemptReason(entry)})
			}
			continue
		}
		if terminalStatus(sess.Status) {
			if pending {
				// Nothing left to settle, but the cap is reached, so the
				// leftovers must go.
				cleanup = append(cleanup, id)
				delete(m.adoptPending, id)
			}
			continue
		}
		reason := "control plane restarted before the session finished"
		if pending {
			reason = adoptAttemptReason(entry)
			cleanup = append(cleanup, id)
			delete(m.adoptPending, id)
		}
		stale = append(stale, settle{session: sess, reason: reason})
	}
	for id, entry := range m.adoptPending {
		if _, gone := m.deleted[id]; gone {
			delete(m.adoptPending, id)
			continue
		}
		if _, ok := m.history[id]; ok {
			continue
		}
		if _, live := m.sessions[id]; live {
			// Adoption owns it, so there is nothing to retry.
			delete(m.adoptPending, id)
			continue
		}
		// The previous lifetime left no record, so nothing can list or delete
		// this session. Write one now. The retry continues until the cap; the
		// settlement time stands in for the times that are unknown.
		closed := now
		stale = append(stale, settle{
			session: model.Session{
				ID:           id,
				Title:        "Session " + id,
				Status:       model.StatusFailed,
				Backend:      m.cfg.Runtime.Backend,
				CreatedAt:    now,
				ExpiresAt:    now,
				LastActivity: now,
				ClosedAt:     &closed,
			},
			reason: adoptAttemptReason(entry),
		})
		if entry.Attempts >= maxAdoptAttempts {
			cleanup = append(cleanup, id)
			delete(m.adoptPending, id)
		}
	}
	m.mu.Unlock()
	for _, item := range stale {
		sess := item.session
		if _, live := m.instance(sess.ID); live {
			// Adoption rebuilt it after the snapshot. Never touch a live
			// session's objects or journal.
			m.mu.Lock()
			delete(m.history, sess.ID)
			delete(m.adoptPending, sess.ID)
			m.mu.Unlock()
			continue
		}
		sess.Status = model.StatusFailed
		sess.Error = item.reason
		if sess.ClosedAt == nil {
			sess.ClosedAt = &now
		}
		m.persistRecord(sess)
		m.mu.Lock()
		if _, gone := m.deleted[sess.ID]; gone {
			delete(m.history, sess.ID)
			m.mu.Unlock()
			m.deleteRecord(sess.ID)
			continue
		}
		m.history[sess.ID] = sess
		m.mu.Unlock()
	}
	for _, id := range cleanup {
		if _, live := m.instance(id); live {
			continue
		}
		m.cleanupLeftovers(id)
	}
}

// adoptAttemptReason names the attempts in a settlement reason.
func adoptAttemptReason(entry adoptFailure) string {
	word := "attempts"
	if entry.Attempts == 1 {
		word = "attempt"
	}
	return fmt.Sprintf("session could not be adopted after %d %s: %s", entry.Attempts, word, entry.Reason)
}

// mergeAdoptFailures replaces the pending set with the sessions this pass
// could not rebuild. An ID that was pending and is not unresolved now is
// resolved. general, when set, keeps every previously pending ID because the
// pass could not examine it.
func (m *Manager) mergeAdoptFailures(failed map[string]string, general string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := make(map[string]adoptFailure, len(failed)+len(m.adoptPending))
	for id, reason := range failed {
		if _, gone := m.deleted[id]; gone {
			continue
		}
		next[id] = adoptFailure{Attempts: m.adoptPending[id].Attempts + 1, Reason: reason}
	}
	if general != "" {
		for id, entry := range m.adoptPending {
			if _, gone := m.deleted[id]; gone {
				continue
			}
			if _, ok := next[id]; ok {
				continue
			}
			entry.Attempts++
			entry.Reason = general
			next[id] = entry
		}
	}
	m.adoptPending = next
}

// pendingAdoptions returns the session IDs a previous pass could not rebuild.
func (m *Manager) pendingAdoptions() map[string]struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]struct{}, len(m.adoptPending))
	for id := range m.adoptPending {
		out[id] = struct{}{}
	}
	return out
}

// pendingAdoption reports whether id is waiting for adoption.
func (m *Manager) pendingAdoption(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.adoptPending[id]
	return ok
}

// liveSessionIDs returns the sessions this control plane is running. Objects
// of these sessions must never be collected.
func (m *Manager) liveSessionIDs() map[string]struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]struct{}, len(m.sessions))
	for id := range m.sessions {
		out[id] = struct{}{}
	}
	return out
}

// pendingCount reports how many sessions adoption still has to settle.
func (m *Manager) pendingCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.adoptPending)
}

// adoptNeeded reports whether adoption has unfinished work to retry.
func (m *Manager) adoptNeeded() bool {
	if m.kube == nil {
		return false
	}
	if m.adoptIncomplete.Load() {
		return true
	}
	return m.pendingCount() > 0
}

// persistRecord writes one session snapshot. A failure is logged, not
// returned: the session keeps working and the next transition writes again.
func (m *Manager) persistRecord(sess model.Session) {
	if m.records == nil || sess.ID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
	defer cancel()
	if err := m.records.Save(ctx, Record{Version: RecordFormatVersion, Session: sess}); err != nil {
		m.logger.Warn("persist session record", "session", sess.ID, "error", err)
	}
}

// persistInstance writes the current snapshot of a live session. The instance
// lock orders snapshot and save together, so a transition that finishes later
// cannot be overwritten by one that copied its state earlier.
func (m *Manager) persistInstance(inst *instance) {
	_ = m.persistInstanceErr(inst)
}

// persistInstanceErr writes the current snapshot and reports the error. The
// model budget uses it: a reservation that is not durable must refuse the
// call instead of running untracked.
func (m *Manager) persistInstanceErr(inst *instance) error {
	if m.records == nil {
		return nil
	}
	inst.persistMu.Lock()
	defer inst.persistMu.Unlock()
	sess := m.copySession(inst)
	ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
	defer cancel()
	if err := m.records.Save(ctx, Record{Version: RecordFormatVersion, Session: sess}); err != nil {
		m.logger.Warn("persist session record", "session", sess.ID, "error", err)
		return err
	}
	return nil
}

func (m *Manager) deleteRecord(id string) {
	if m.records == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
	defer cancel()
	if err := m.records.Delete(ctx, id); err != nil {
		m.logger.Warn("delete session record", "session", id, "error", err)
	}
}

// saveArtifactIndex writes one artifact's metadata so its download survives a
// restart.
func (m *Manager) saveArtifactIndex(id string, info model.ArtifactInfo) {
	if m.records == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
	defer cancel()
	if err := m.records.SaveArtifact(ctx, id, info); err != nil {
		m.logger.Warn("persist artifact index", "session", id, "error", err)
	}
}

func (m *Manager) deleteArtifactIndex(id string) {
	if m.records == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
	defer cancel()
	if err := m.records.DeleteArtifact(ctx, id); err != nil {
		m.logger.Warn("delete artifact index entry", "session", id, "error", err)
	}
}

// pruneRecords drops stored sessions past sessions.retention and artifact
// index entries past artifacts.index_retention. Retention of zero keeps a
// record until an operator removes it.
func (m *Manager) pruneRecords(now time.Time) {
	m.mu.Lock()
	for id, at := range m.deleted {
		if now.Sub(at) > deletedRetention {
			delete(m.deleted, id)
		}
	}
	var expired []string
	if retention := m.cfg.Sessions.Retention.Std(); retention > 0 {
		for id, sess := range m.history {
			last := sess.CreatedAt
			if sess.ClosedAt != nil {
				last = *sess.ClosedAt
			}
			if now.Sub(last) > retention {
				expired = append(expired, id)
			}
		}
		for _, id := range expired {
			delete(m.history, id)
		}
	}
	var staleArtifacts []string
	if retention := m.cfg.Artifacts.IndexRetention.Std(); retention > 0 {
		for id, info := range m.artifacts {
			if now.Sub(info.CreatedAt) > retention {
				staleArtifacts = append(staleArtifacts, id)
			}
		}
		for _, id := range staleArtifacts {
			delete(m.artifacts, id)
		}
	}
	m.mu.Unlock()
	for _, id := range expired {
		m.deleteRecord(id)
	}
	for _, id := range staleArtifacts {
		m.deleteArtifactIndex(id)
	}
}

// terminalStatus reports whether a session can no longer change state.
func terminalStatus(status model.SessionStatus) bool {
	switch status {
	case model.StatusCompleted, model.StatusFailed, model.StatusExpired:
		return true
	}
	return false
}
