package queue

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/session"
)

// EnvelopeVersion is the major version of the job envelope described by
// api/job.v1.schema.json. A change that removes, renames, or retypes a field,
// or that changes what an existing field means, increments this number.
// Optional fields may be added without a new version; readers ignore fields
// they do not know.
const EnvelopeVersion = 1

// UnsupportedVersionError reports a job envelope this build cannot read.
type UnsupportedVersionError struct {
	Version int
}

func (e *UnsupportedVersionError) Error() string {
	if e.Version > EnvelopeVersion {
		return fmt.Sprintf("job envelope version %d is newer than this build reads (%d); upgrade mayflyd", e.Version, EnvelopeVersion)
	}
	return fmt.Sprintf("job envelope version %d is not supported; this build reads %d", e.Version, EnvelopeVersion)
}

// Envelope is the versioned JSON form of a job. The queue stores it as the job
// payload, and an external producer writes the same JSON when it enqueues
// through a queue backend adapter. Job encodes and decodes through this type.
type Envelope struct {
	Version        int               `json:"version"`
	Kind           Kind              `json:"kind"`
	ID             string            `json:"id,omitempty"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	Attempts       int               `json:"attempts,omitempty"`
	Agent          string            `json:"agent,omitempty"`
	Prompt         string            `json:"prompt,omitempty"`
	Session        *SessionSpec      `json:"session,omitempty"`
	Trigger        TriggerSpec       `json:"trigger"`
	Source         *SourceJob        `json:"source,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

// SessionSpec holds the session options of a job. The prompt and agent are
// top-level envelope fields because every session job needs them.
type SessionSpec struct {
	Title      string         `json:"title,omitempty"`
	Model      string         `json:"model,omitempty"`
	TTL        string         `json:"ttl,omitempty"`
	Skills     []string       `json:"skills,omitempty"`
	MCPServers []string       `json:"mcp_servers,omitempty"`
	Artifacts  []ArtifactSpec `json:"artifacts,omitempty"`
}

// ArtifactSpec is one stored input a session may read.
type ArtifactSpec struct {
	Key         string `json:"key"`
	SHA256      string `json:"sha256"`
	Bytes       int64  `json:"bytes,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

// TriggerSpec identifies what caused a job. Delivery and job id are envelope
// fields, so they are not repeated here.
type TriggerSpec struct {
	Kind  string `json:"kind"`
	Name  string `json:"name,omitempty"`
	Event string `json:"event,omitempty"`
	Actor string `json:"actor,omitempty"`
}

// MarshalJSON writes the envelope a producer hands to a queue backend.
func (j Job) MarshalJSON() ([]byte, error) {
	return json.Marshal(Envelope{
		Version:        EnvelopeVersion,
		Kind:           j.JobKind(),
		ID:             j.ID,
		IdempotencyKey: j.IdempotencyKey,
		CreatedAt:      j.AcceptedAt,
		Attempts:       j.Attempts,
		Agent:          j.Options.Agent,
		Prompt:         j.Options.Prompt,
		Session:        sessionSpec(j.Options),
		Trigger:        triggerSpec(j),
		Source:         j.Source,
		Metadata:       j.Metadata,
	})
}

// UnmarshalJSON reads a v1 envelope or a job stored by a build before the
// envelope existed. A newer major version is an UnsupportedVersionError so a
// store can bury the job with that reason.
func (j *Job) UnmarshalJSON(data []byte) error {
	var probe struct {
		Version *int            `json:"version"`
		Options json.RawMessage `json:"options"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	switch {
	case probe.Version != nil:
		if *probe.Version != EnvelopeVersion {
			return &UnsupportedVersionError{Version: *probe.Version}
		}
		var env Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			return err
		}
		return j.fromEnvelope(env)
	case len(probe.Options) > 0:
		// Every job written before the envelope carried the options object.
		return j.fromLegacy(data)
	default:
		return errors.New("job payload has no version")
	}
}

func (j *Job) fromEnvelope(env Envelope) error {
	kind := env.Kind
	if kind == "" {
		return errors.New("job envelope kind is required")
	}
	if kind != KindSession && kind != KindSource {
		return fmt.Errorf("job envelope kind %q is not supported by this build", env.Kind)
	}
	// The schema requires a prompt for session.create and a source for
	// source.run. The worker reports those as ordinary job failures, so a
	// record this codec writes for a buried job still decodes.
	if env.Trigger.Kind == "" {
		return errors.New("job envelope trigger.kind is required")
	}
	if env.ID == "" {
		env.ID = deriveJobID(kind, env.Trigger.Name, env.IdempotencyKey)
	}
	options, err := createOptions(env)
	if err != nil {
		return err
	}
	j.ID = env.ID
	j.Kind = kind
	j.IdempotencyKey = env.IdempotencyKey
	j.Trigger = env.Trigger.Name
	switch {
	case kind == KindSource:
		// Scheduled jobs keep the trigger name they have always reported.
		j.Trigger = "schedule"
	case j.Trigger == "":
		j.Trigger = env.Trigger.Kind
	}
	j.Options = options
	j.Source = env.Source
	j.AcceptedAt = env.CreatedAt
	j.Attempts = env.Attempts
	j.Metadata = env.Metadata
	return nil
}

type legacyJob struct {
	ID         string                `json:"id"`
	Kind       string                `json:"kind"`
	Delivery   string                `json:"delivery"`
	Trigger    string                `json:"trigger"`
	Options    session.CreateOptions `json:"options"`
	Source     *SourceJob            `json:"source"`
	AcceptedAt time.Time             `json:"accepted_at"`
	Attempts   int                   `json:"attempts"`
}

func (j *Job) fromLegacy(data []byte) error {
	var legacy legacyJob
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	kind := KindSession
	switch legacy.Kind {
	case "", "session", string(KindSession):
		kind = KindSession
	case "source", string(KindSource):
		kind = KindSource
	default:
		return fmt.Errorf("job kind %q is not supported by this build", legacy.Kind)
	}
	if legacy.ID == "" {
		return errors.New("job payload has no id")
	}
	j.ID = legacy.ID
	j.Kind = kind
	j.IdempotencyKey = legacy.Delivery
	j.Trigger = legacy.Trigger
	j.Options = legacy.Options
	j.Source = legacy.Source
	j.AcceptedAt = legacy.AcceptedAt
	j.Attempts = legacy.Attempts
	// A session created for this job must stay unique even if the job runs
	// twice between Create and Ack. Earlier builds left this empty for
	// webhook jobs, so the job id becomes the key on read.
	if j.Options.IdempotencyKey == "" {
		j.Options.IdempotencyKey = j.ID
	}
	if j.Options.Trigger.JobID == "" {
		j.Options.Trigger.JobID = j.ID
	}
	if j.Options.Trigger.Delivery == "" {
		j.Options.Trigger.Delivery = j.IdempotencyKey
	}
	return nil
}

func sessionSpec(opts session.CreateOptions) *SessionSpec {
	spec := SessionSpec{
		Title:      opts.Title,
		Model:      opts.Model,
		Skills:     opts.Skills,
		MCPServers: opts.MCPServers,
	}
	if opts.TTL != 0 {
		spec.TTL = opts.TTL.String()
	}
	for _, ref := range opts.Artifacts {
		spec.Artifacts = append(spec.Artifacts, ArtifactSpec{
			Key:         ref.Key,
			SHA256:      ref.SHA256,
			Bytes:       ref.Bytes,
			ContentType: ref.ContentType,
		})
	}
	if spec.Title == "" && spec.Model == "" && spec.TTL == "" && len(spec.Skills) == 0 &&
		len(spec.MCPServers) == 0 && len(spec.Artifacts) == 0 {
		return nil
	}
	return &spec
}

func triggerSpec(j Job) TriggerSpec {
	t := j.Options.Trigger
	if t.Kind == "" && t.Name == "" && t.Event == "" && t.Actor == "" {
		if j.JobKind() == KindSource {
			if j.Source != nil {
				return TriggerSpec{Kind: "schedule", Name: j.Source.Schedule, Event: "schedule"}
			}
			return TriggerSpec{Kind: "schedule"}
		}
		return TriggerSpec{Kind: string(KindSession), Name: j.Trigger}
	}
	name := t.Name
	if name == "" {
		name = j.Trigger
	}
	return TriggerSpec{Kind: t.Kind, Name: name, Event: t.Event, Actor: t.Actor}
}

func createOptions(env Envelope) (session.CreateOptions, error) {
	opts := session.CreateOptions{
		Prompt:         env.Prompt,
		Agent:          env.Agent,
		IdempotencyKey: env.ID,
		Trigger: model.TriggerInfo{
			Kind:     env.Trigger.Kind,
			Name:     env.Trigger.Name,
			Event:    env.Trigger.Event,
			Actor:    env.Trigger.Actor,
			Delivery: env.IdempotencyKey,
			JobID:    env.ID,
		},
	}
	if env.Session == nil {
		return opts, nil
	}
	opts.Title = env.Session.Title
	opts.Model = env.Session.Model
	opts.Skills = env.Session.Skills
	opts.MCPServers = env.Session.MCPServers
	if env.Session.TTL != "" {
		ttl, err := time.ParseDuration(env.Session.TTL)
		if err != nil {
			return opts, fmt.Errorf("job envelope session.ttl %q: %w", env.Session.TTL, err)
		}
		opts.TTL = ttl
	}
	for _, ref := range env.Session.Artifacts {
		opts.Artifacts = append(opts.Artifacts, session.ArtifactRef{
			Key:         ref.Key,
			SHA256:      ref.SHA256,
			Bytes:       ref.Bytes,
			ContentType: ref.ContentType,
		})
	}
	return opts, nil
}

// deriveJobID returns the id the built-in producers use for an idempotency
// key: stable for the same kind, trigger name, and key, and random when the
// key is empty. A backend adapter uses the same rule for a message that does
// not carry an id.
func deriveJobID(kind Kind, triggerName, key string) string {
	if key == "" {
		return randomID()
	}
	scope := triggerName
	prefix := "d-"
	if kind == KindSource {
		scope = "schedule"
		prefix = "s-"
	}
	sum := sha256.Sum256([]byte(scope + "\x00" + key))
	return prefix + hex.EncodeToString(sum[:16])
}
