package queue

import "time"

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

func causeString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
