package queue

import (
	"encoding/json"
	"time"
)

// doneRecord is kept after acknowledgement for queue.tombstone_ttl so a sender
// can map a job to the session it created.
type doneRecord struct {
	Job        Job       `json:"job"`
	SessionID  string    `json:"session_id,omitempty"`
	FinishedAt time.Time `json:"finished_at"`
}

// deadRecord is a job that exhausted queue.max_attempts.
type deadRecord struct {
	Job      Job       `json:"job"`
	Error    string    `json:"error,omitempty"`
	FailedAt time.Time `json:"failed_at"`
}

// UnmarshalJSON reads a buried job. A store buries a payload it cannot decode
// with the identity it knows from the queue entry, so the job inside such a
// record does not decode either; the error and id must still be readable.
func (r *deadRecord) UnmarshalJSON(data []byte) error {
	var wire struct {
		Job      json.RawMessage `json:"job"`
		Error    string          `json:"error,omitempty"`
		FailedAt time.Time       `json:"failed_at"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	r.Error = wire.Error
	r.FailedAt = wire.FailedAt
	if len(wire.Job) == 0 {
		return nil
	}
	if err := json.Unmarshal(wire.Job, &r.Job); err == nil {
		return nil
	}
	// Keep the id of a job this build cannot decode so the dead-letter tools
	// can name it. Everything else about the payload is unknown.
	var summary struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(wire.Job, &summary)
	r.Job = Job{ID: summary.ID}
	return nil
}

func causeString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
