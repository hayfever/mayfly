package httpapi

import (
	"net/http"

	"github.com/hayfever/mayfly/internal/queue"
)

// acceptedJob is the one 202 body for an endpoint that accepted work into the
// queue. It reports the versioned job contract fields: the job id, the
// idempotency key ("delivery"), and the initial state.
type acceptedJob struct {
	Accepted  bool   `json:"accepted"`
	Duplicate bool   `json:"duplicate"`
	JobID     string `json:"job_id"`
	Delivery  string `json:"delivery"`
	Status    string `json:"status"`
}

func acceptedFrom(job queue.Job, duplicate bool) acceptedJob {
	return acceptedJob{
		Accepted:  true,
		Duplicate: duplicate,
		JobID:     job.ID,
		Delivery:  job.IdempotencyKey,
		Status:    string(queue.StateQueued),
	}
}

func writeAccepted(w http.ResponseWriter, job queue.Job, duplicate bool) {
	writeJSON(w, http.StatusAccepted, acceptedFrom(job, duplicate))
}
