// Package sources runs declarative data sources for scheduled workflows. A
// source is a chain of HTTP requests described in a project bundle; the
// package contains no provider-specific code.
package sources

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hayfever/mayfly/internal/prompt"
)

// TypeHTTPJSON is the only source type in this build.
const TypeHTTPJSON = "http.json"

// Connection is a named HTTP origin with its credentials. Headers are resolved
// from the environment when the project loads, so the runner never sees an
// env: reference.
type Connection struct {
	Name        string            `yaml:"name"`
	BaseURL     string            `yaml:"baseURL"`
	Headers     map[string]string `yaml:"headers,omitempty"`
	RequiredEnv []string          `yaml:"requiredEnv,omitempty"`
	OptionalEnv []string          `yaml:"optionalEnv,omitempty"`
	Timeout     string            `yaml:"timeout,omitempty"`
	AllowHTTP   bool              `yaml:"allowHTTP,omitempty"`
	// AllowPrivate lets the connection reach loopback, private, and
	// link-local addresses. It is for local stubs and tests; the SSRF guard
	// stays on for every other connection, including allowHTTP ones.
	AllowPrivate bool   `yaml:"allowPrivate,omitempty"`
	Scope        string `yaml:"scope,omitempty" json:"-"`

	ResolvedHeaders map[string]string `yaml:"-"`
	ResolvedTimeout time.Duration     `yaml:"-"`
}

// Request describes one HTTP call. Path, query, header values, and body
// strings are templates over the run data.
type Request struct {
	Method  string            `yaml:"method,omitempty"`
	Path    string            `yaml:"path"`
	Query   map[string]string `yaml:"query,omitempty"`
	Body    map[string]any    `yaml:"body,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
}

// Paginate continues a request while the response reports more pages.
type Paginate struct {
	MaxPages   int            `yaml:"maxPages,omitempty"`
	MorePath   string         `yaml:"morePath"`
	CursorPath string         `yaml:"cursorPath"`
	Body       map[string]any `yaml:"body,omitempty"`
}

// Fanout runs the request once per URL field of every document produced by
// another source.
type Fanout struct {
	From string   `yaml:"from"`
	URLs []string `yaml:"urls"`
}

// Field extracts one value from a document. Paths are tried in order; the
// first that resolves wins. Array results are joined with Join.
type Field struct {
	Path  string   `yaml:"path,omitempty"`
	Paths []string `yaml:"paths,omitempty"`
	Join  string   `yaml:"join,omitempty"`
}

// Extract selects the item list and the fields of each item.
type Extract struct {
	List   string           `yaml:"list,omitempty"`
	Fields map[string]Field `yaml:"fields"`
}

// Filter keeps the documents that match every clause. A missing field fails
// equals and notEmpty, and passes notTrue.
type Filter struct {
	Equals     map[string]any `yaml:"equals,omitempty"`
	NotEquals  map[string]any `yaml:"notEquals,omitempty"`
	NotTrue    []string       `yaml:"notTrue,omitempty"`
	NotEmpty   []string       `yaml:"notEmpty,omitempty"`
	PresentAny []string       `yaml:"presentAny,omitempty"`
}

// ChangeConfig reads a provider's change status and diff. The engine treats
// changed and removed as material, new as a baseline, and same as no change.
type ChangeConfig struct {
	StatusPath   string `yaml:"statusPath"`
	DiffPath     string `yaml:"diffPath"`
	ErrorPath    string `yaml:"errorPath,omitempty"`
	MaxLines     int    `yaml:"maxLines,omitempty"`
	MaxLineChars int    `yaml:"maxLineChars,omitempty"`
}

// Retry repeats a 429, a 5xx, or a transport failure. Delay grows linearly:
// attempt 1 waits one delay, attempt 2 waits two.
type Retry struct {
	Attempts int    `yaml:"attempts,omitempty"`
	Delay    string `yaml:"delay,omitempty"`
}

// Limits bounds one source run.
type Limits struct {
	MaxItems int `yaml:"maxItems,omitempty"`
}

// Definition is one source as resolved for an agent.
type Definition struct {
	Name        string        `yaml:"name"`
	Type        string        `yaml:"type"`
	Needs       []string      `yaml:"needs,omitempty"`
	Connection  string        `yaml:"connection"`
	Request     Request       `yaml:"request"`
	RequiredEnv []string      `yaml:"requiredEnv,omitempty"`
	OptionalEnv []string      `yaml:"optionalEnv,omitempty"`
	Paginate    *Paginate     `yaml:"paginate,omitempty"`
	Fanout      *Fanout       `yaml:"fanout,omitempty"`
	Extract     Extract       `yaml:"extract"`
	Filter      *Filter       `yaml:"filter,omitempty"`
	Change      *ChangeConfig `yaml:"change,omitempty"`
	Retry       Retry         `yaml:"retry,omitempty"`
	Limits      Limits        `yaml:"limits,omitempty"`
	Scope       string        `yaml:"scope,omitempty" json:"-"`
}

// Document is one item produced by a source.
type Document struct {
	Source string         `json:"source"`
	Name   string         `json:"name,omitempty"`
	Label  string         `json:"label,omitempty"`
	URL    string         `json:"url,omitempty"`
	Status string         `json:"status,omitempty"`
	Diff   string         `json:"diff,omitempty"`
	Error  string         `json:"error,omitempty"`
	Fields map[string]any `json:"fields,omitempty"`
}

// Totals summarizes one run.
type Totals struct {
	Pages   int `json:"pages"`
	Changed int `json:"changed"`
	New     int `json:"new"`
	Same    int `json:"same"`
	Failed  int `json:"failed"`
}

// Input carries the run identity and the documents of completed steps.
type Input struct {
	RunID    string
	Schedule string
	Slot     time.Time
	Now      time.Time
	Deps     map[string][]Document
}

// FormatVersion is the layout of a stored Result. A reader accepts version 1
// and rejects anything newer.
const FormatVersion = 1

// Result is the output of one plan. Version records the layout the result was
// written in; a result written before the stamp existed decodes as version 0.
type Result struct {
	Version   int        `json:"version"`
	Source    string     `json:"source"`
	Documents []Document `json:"documents"`
	Totals    Totals     `json:"totals"`
}

// Source produces documents for one run.
type Source interface {
	// Name is the source name from the project bundle.
	Name() string
	// Run executes the source. Per-item failures are returned as error
	// documents; a returned error aborts the whole run.
	Run(ctx context.Context, in Input) ([]Document, error)
}

// Step is one source in execution order.
type Step struct {
	Source
}

// Plan is a terminal source and its transitive dependencies in execution
// order.
type Plan struct {
	Schedule string
	Steps    []Step
	Terminal string
}

// Run executes every step and returns the terminal source's documents.
func (p *Plan) Run(ctx context.Context, in Input) (Result, error) {
	docs := make(map[string][]Document, len(p.Steps))
	for _, step := range p.Steps {
		in.Deps = docs
		out, err := step.Run(ctx, in)
		if err != nil {
			return Result{}, fmt.Errorf("source %q: %w", step.Name(), err)
		}
		docs[step.Name()] = out
	}
	result := Result{Version: FormatVersion, Source: p.Terminal, Documents: docs[p.Terminal]}
	result.Totals = Count(result.Documents)
	return result, nil
}

// Count summarizes documents by change status.
func Count(docs []Document) Totals {
	var t Totals
	for _, doc := range docs {
		t.Pages++
		switch doc.Status {
		case "changed", "removed":
			t.Changed++
		case "new":
			t.New++
		case "same":
			t.Same++
		case "error":
			t.Failed++
		}
	}
	return t
}

// Options carries process-wide source limits.
type Options struct {
	Concurrency int
	// Timeout bounds one source step: every request it sends, with retries.
	Timeout time.Duration
	// DeniedEnv lists control-plane secret variables no template may read.
	DeniedEnv []string
}

// TemplateEnv is the environment a source's templates may read: the
// connection's and the source's requiredEnv and optionalEnv, minus
// control-plane secrets.
func TemplateEnv(def Definition, conn Connection, denied []string) prompt.EnvPolicy {
	allow := make([]string, 0, len(conn.RequiredEnv)+len(conn.OptionalEnv)+len(def.RequiredEnv)+len(def.OptionalEnv))
	allow = append(allow, conn.RequiredEnv...)
	allow = append(allow, conn.OptionalEnv...)
	allow = append(allow, def.RequiredEnv...)
	allow = append(allow, def.OptionalEnv...)
	return prompt.EnvPolicy{Allow: allow, Deny: denied}
}

// Factory builds one source from its definition and connection.
type Factory func(def Definition, conn Connection, opts Options) (Source, error)

var factories = map[string]Factory{
	TypeHTTPJSON: newHTTPJSON,
}

// Build validates a resolved definition and constructs its runner.
func Build(def Definition, conn Connection, opts Options) (Source, error) {
	def.Name = strings.TrimSpace(def.Name)
	if def.Name == "" {
		return nil, errors.New("source name is required")
	}
	if def.Type == "" {
		return nil, fmt.Errorf("source %q: type is required", def.Name)
	}
	if _, ok := factories[def.Type]; !ok {
		return nil, fmt.Errorf("source %q: unsupported type %q (supported: %s)", def.Name, def.Type, strings.Join(SortedTypes(), ", "))
	}
	if def.Connection == "" {
		return nil, fmt.Errorf("source %q: connection is required", def.Name)
	}
	if strings.TrimSpace(def.Request.Path) == "" {
		return nil, fmt.Errorf("source %q: request.path is required", def.Name)
	}
	if def.Paginate != nil && def.Fanout != nil {
		return nil, fmt.Errorf("source %q: paginate and fanout cannot be combined", def.Name)
	}
	if def.Fanout != nil {
		if def.Fanout.From == "" {
			return nil, fmt.Errorf("source %q: fanout.from is required", def.Name)
		}
		if len(def.Fanout.URLs) == 0 {
			return nil, fmt.Errorf("source %q: fanout.urls is required", def.Name)
		}
		if !contains(def.Needs, def.Fanout.From) {
			return nil, fmt.Errorf("source %q: fanout.from %q must be listed in needs", def.Name, def.Fanout.From)
		}
	}
	if def.Change != nil && (def.Change.StatusPath == "" || def.Change.DiffPath == "") {
		return nil, fmt.Errorf("source %q: change.statusPath and change.diffPath are required", def.Name)
	}
	return factories[def.Type](def, conn, opts)
}

// SortedTypes lists the registered source types.
func SortedTypes() []string {
	out := make([]string, 0, len(factories))
	for name := range factories {
		out = append(out, name)
	}
	sortStrings(out)
	return out
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func sortStrings(list []string) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j] < list[j-1]; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}
