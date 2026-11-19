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

// SetRecords attaches the store that keeps session records and the artifact
// index across restarts. Call it before Start, so startup loads the same store
// the manager writes to.
func (m *Manager) SetRecords(store RecordStore) {
	if store == nil {
		store = NewMemoryRecords()
	}
	m.records = store
}

// loadRecords restores the sessions and artifact metadata a previous
// control-plane lifetime wrote. It does not judge a non-terminal record:
// settleInterrupted does that once adoption has reported what it can see.
func (m *Manager) loadRecords(ctx context.Context) error {
	if m.records == nil {
		return nil
	}
	records, err := m.records.List(ctx)
	if err != nil {
		return fmt.Errorf("load session records: %w", err)
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
func (m *Manager) settleInterrupted() {
	if m.adoptIncomplete.Load() {
		return
	}
	now := time.Now().UTC()
	m.mu.Lock()
	pending := m.adoptPending
	var stale []model.Session
	for id, sess := range m.history {
		if _, live := m.sessions[id]; live {
			// Adoption rebuilt this session; the live instance owns it now.
			delete(m.history, id)
			continue
		}
		if terminalStatus(sess.Status) {
			continue
		}
		if _, ok := pending[id]; ok {
			continue
		}
		stale = append(stale, sess)
	}
	m.mu.Unlock()
	for _, sess := range stale {
		sess.Status = model.StatusFailed
		sess.Error = "control plane restarted before the session finished"
		if sess.ClosedAt == nil {
			sess.ClosedAt = &now
		}
		m.persistRecord(sess)
		m.mu.Lock()
		m.history[sess.ID] = sess
		m.mu.Unlock()
	}
}

// setAdoptPending records the session IDs the last adoption pass could not
// rebuild yet.
func (m *Manager) setAdoptPending(pending map[string]struct{}) {
	m.mu.Lock()
	m.adoptPending = pending
	m.mu.Unlock()
}

// adoptNeeded reports whether adoption has unfinished work to retry.
func (m *Manager) adoptNeeded() bool {
	if m.kube == nil {
		return false
	}
	if m.adoptIncomplete.Load() {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.adoptPending) > 0
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
	inst.persistMu.Lock()
	defer inst.persistMu.Unlock()
	m.persistRecord(m.copySession(inst))
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

// historySession returns a finished session loaded from the record store.
func (m *Manager) historySession(id string) (model.Session, bool) {
	m.mu.Lock()
	sess, ok := m.history[id]
	m.mu.Unlock()
	return sess, ok
}

// dropHistory removes a finished session from the list and the record store.
func (m *Manager) dropHistory(id string) bool {
	m.mu.Lock()
	_, ok := m.history[id]
	delete(m.history, id)
	m.mu.Unlock()
	if !ok {
		return false
	}
	m.deleteRecord(id)
	return true
}

// pruneRecords drops stored sessions past sessions.retention and artifact
// index entries past artifacts.index_retention. Retention of zero keeps a
// record until an operator removes it.
func (m *Manager) pruneRecords(now time.Time) {
	m.mu.Lock()
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
