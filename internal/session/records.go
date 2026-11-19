package session

import (
	"context"
	"fmt"
	"sync"

	"github.com/hayfever/mayfly/internal/model"
)

// RecordFormatVersion is the layout of a stored Record. A reader accepts one
// version and rejects anything newer.
const RecordFormatVersion = 1

// Record is one session as the control plane persists it. It holds what List
// and Get serve for a finished session, and no credential: a session that
// survives a restart gets its token back from its Secret during adoption.
type Record struct {
	Version int           `json:"version"`
	Session model.Session `json:"session"`
}

// ArtifactEntry is one artifact metadata record, keyed by session ID.
type ArtifactEntry struct {
	SessionID string
	Info      model.ArtifactInfo
}

// RecordStore keeps session records and artifact metadata across restarts.
// The manager reads the whole store at startup and then writes through, so a
// read never waits for the volume.
type RecordStore interface {
	Save(ctx context.Context, rec Record) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context) ([]Record, error)
	SaveArtifact(ctx context.Context, id string, info model.ArtifactInfo) error
	DeleteArtifact(ctx context.Context, id string) error
	ListArtifacts(ctx context.Context) ([]ArtifactEntry, error)
	Close() error
}

// checkRecordVersion reports whether a stored record can be read. A record
// without a version was written before the stamp existed and holds the
// version 1 layout.
func checkRecordVersion(version int) error {
	switch {
	case version == 0, version == RecordFormatVersion:
		return nil
	case version > RecordFormatVersion:
		return fmt.Errorf("session record format version %d is newer than this build reads (%d); upgrade mayflyd", version, RecordFormatVersion)
	}
	return fmt.Errorf("session record format version %d has no migration path", version)
}

// memoryRecords is the record store for the memory backend and for tests.
type memoryRecords struct {
	mu        sync.Mutex
	sessions  map[string]Record
	artifacts map[string]model.ArtifactInfo
}

// NewMemoryRecords returns a record store that lives in this process. It
// serves the memory backend, where a restart is not expected to keep state.
func NewMemoryRecords() RecordStore {
	return &memoryRecords{
		sessions:  map[string]Record{},
		artifacts: map[string]model.ArtifactInfo{},
	}
}

func (s *memoryRecords) Save(ctx context.Context, rec Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if rec.Session.ID == "" {
		return fmt.Errorf("session record has no id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[rec.Session.ID] = rec
	return nil
}

func (s *memoryRecords) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	return nil
}

func (s *memoryRecords) List(ctx context.Context) ([]Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(s.sessions))
	for _, rec := range s.sessions {
		out = append(out, rec)
	}
	return out, nil
}

func (s *memoryRecords) SaveArtifact(ctx context.Context, id string, info model.ArtifactInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.artifacts[id] = info
	return nil
}

func (s *memoryRecords) DeleteArtifact(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.artifacts, id)
	return nil
}

func (s *memoryRecords) ListArtifacts(ctx context.Context) ([]ArtifactEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ArtifactEntry, 0, len(s.artifacts))
	for id, info := range s.artifacts {
		out = append(out, ArtifactEntry{SessionID: id, Info: info})
	}
	return out, nil
}

func (s *memoryRecords) Close() error { return nil }
