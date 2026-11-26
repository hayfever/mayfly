package httpapi

import (
	"time"

	"github.com/hayfever/mayfly/internal/model"
)

// apiSession is the public session object. It leaves out the runtime backend,
// the pod name, and the artifact store URI: the control plane needs them, but
// they are not part of the API contract.
type apiSession struct {
	ID           string              `json:"id"`
	Title        string              `json:"title"`
	Status       model.SessionStatus `json:"status"`
	Prompt       string              `json:"prompt"`
	Agent        string              `json:"agent"`
	Harness      string              `json:"harness"`
	Model        string              `json:"model"`
	Skills       []string            `json:"skills"`
	MCPServers   []string            `json:"mcp_servers"`
	Plugins      []string            `json:"plugins"`
	Trigger      model.TriggerInfo   `json:"trigger"`
	TTLSeconds   int                 `json:"ttl_seconds"`
	CreatedAt    time.Time           `json:"created_at"`
	StartedAt    *time.Time          `json:"started_at,omitempty"`
	ExpiresAt    time.Time           `json:"expires_at"`
	ClosedAt     *time.Time          `json:"closed_at,omitempty"`
	LastActivity time.Time           `json:"last_activity"`
	MessageCount int                 `json:"message_count"`
	Artifact     *apiArtifact        `json:"artifact,omitempty"`
	LLMUsage     *model.LLMUsage     `json:"llm_usage,omitempty"`
	LLMBudget    *model.LLMBudget    `json:"llm_budget,omitempty"`
	Error        string              `json:"error,omitempty"`
}

// apiArtifact is the public artifact metadata of a finalized session. The
// store URI stays internal, behind GET /api/v1/sessions/{id}/artifact.
type apiArtifact struct {
	Key       string    `json:"key"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
	Partial   bool      `json:"partial"`
}

func newAPISession(sess model.Session) apiSession {
	out := apiSession{
		ID:           sess.ID,
		Title:        sess.Title,
		Status:       sess.Status,
		Prompt:       sess.Prompt,
		Agent:        sess.Agent,
		Harness:      sess.Harness,
		Model:        sess.Model,
		Skills:       sess.Skills,
		MCPServers:   sess.MCPServers,
		Plugins:      sess.Plugins,
		Trigger:      sess.Trigger,
		TTLSeconds:   sess.TTLSeconds,
		CreatedAt:    sess.CreatedAt,
		StartedAt:    sess.StartedAt,
		ExpiresAt:    sess.ExpiresAt,
		ClosedAt:     sess.ClosedAt,
		LastActivity: sess.LastActivity,
		MessageCount: sess.MessageCount,
		Error:        sess.Error,
	}
	if sess.Artifact != nil {
		out.Artifact = &apiArtifact{
			Key:       sess.Artifact.Key,
			Size:      sess.Artifact.Size,
			SHA256:    sess.Artifact.SHA256,
			CreatedAt: sess.Artifact.CreatedAt,
			Partial:   sess.Artifact.Partial,
		}
	}
	if sess.LLMUsage != nil {
		usage := *sess.LLMUsage
		out.LLMUsage = &usage
	}
	if sess.LLMBudget != nil {
		budget := *sess.LLMBudget
		out.LLMBudget = &budget
	}
	return out
}

func newAPISessions(sessions []model.Session) []apiSession {
	out := make([]apiSession, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, newAPISession(sess))
	}
	return out
}
