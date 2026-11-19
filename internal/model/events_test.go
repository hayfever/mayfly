package model

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

var update = flag.Bool("update", false, "rewrite the event golden files")

type eventCase struct {
	typ    EventType
	schema string
	data   EventData
	event  Event
}

func eventCases() []eventCase {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	mk := func(typ EventType, schema string, data EventData) eventCase {
		return eventCase{
			typ:    typ,
			schema: schema,
			data:   data,
			event: Event{
				Seq:       7,
				SessionID: "abc123",
				StepKey:   "1/2/result",
				Time:      at,
				Type:      typ,
				Actor:     "mayfly",
				Text:      "text",
				Data:      data,
			},
		}
	}
	return []eventCase{
		mk(EventUserMessage, "UserMessageEventData", UserMessageData{Kind: "github", Name: "repo", Event: "push"}),
		mk(EventAssistantMessage, "AssistantMessageEventData", AssistantMessageData{FinishReason: "tool_calls", ToolCalls: 2}),
		mk(EventAssistantDelta, "", nil),
		mk(EventToolCall, "ToolCallEventData", ToolCallData{Tool: "echo", CallID: "call_1", Arguments: `{"text":"hi"}`}),
		mk(EventToolResult, "ToolResultEventData", ToolResultData{Tool: "echo", CallID: "call_1", Error: true, Interrupted: true}),
		mk(EventStatus, "StatusEventData", StatusData{Status: StatusCompleted, Reason: "done", Error: "boom", Restart: 2}),
		mk(EventError, "", nil),
		mk(EventSystem, "SystemEventData", SystemData{
			Model:      "test-model",
			Skills:     []string{"review"},
			MCPServers: []string{"echo"},
			Plugins:    []string{"inline"},
			ToolCount:  3,
		}),
		mk(EventArtifact, "ArtifactEventData", ArtifactData{
			Key:     "sessions/abc123/artifact.tar.gz",
			URI:     "file:///artifacts/abc123.tar.gz",
			Size:    42,
			SHA256:  "0123456789abcdef",
			Partial: true,
		}),
		mk(EventControl, "ControlEventData", ControlData{Action: "close", Reason: "done"}),
	}
}

func TestEventRoundTrip(t *testing.T) {
	for _, c := range eventCases() {
		t.Run(string(c.typ), func(t *testing.T) {
			raw, err := json.Marshal(c.event)
			if err != nil {
				t.Fatal(err)
			}
			var got Event
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.event) {
				t.Fatalf("round trip = %#v, want %#v", got, c.event)
			}
		})
	}
}

// TestEventGolden pins the JSON of every event type. Run with -update after a
// deliberate wire change and review the diff.
func TestEventGolden(t *testing.T) {
	for _, c := range eventCases() {
		t.Run(string(c.typ), func(t *testing.T) {
			raw, err := json.MarshalIndent(c.event, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			raw = append(raw, '\n')
			path := filepath.Join("testdata", "events", string(c.typ)+".json")
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != string(want) {
				t.Fatalf("wire JSON changed; got:\n%s\nwant:\n%s", raw, want)
			}
		})
	}
}

type specSchema struct {
	Type       string                  `yaml:"type"`
	Format     string                  `yaml:"format"`
	Required   []string                `yaml:"required"`
	Properties map[string]specProperty `yaml:"properties"`
	Items      *specProperty           `yaml:"items"`
	OneOf      []struct {
		Ref string `yaml:"$ref"`
	} `yaml:"oneOf"`
}

type specProperty struct {
	Type        string        `yaml:"type"`
	Ref         string        `yaml:"$ref"`
	Items       *specProperty `yaml:"items"`
	Enum        []string      `yaml:"enum"`
	Description string        `yaml:"description"`
}

type specDoc struct {
	Components struct {
		Schemas map[string]specSchema `yaml:"schemas"`
	} `yaml:"components"`
}

func loadSpec(t *testing.T) specDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc specDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// TestEventSchemaMatchesPayload checks that every payload schema in the
// OpenAPI document has the same properties and required fields as its Go
// type, and that docs/events.md maps each event type to its schema.
func TestEventSchemaMatchesPayload(t *testing.T) {
	doc := loadSpec(t)
	for _, c := range eventCases() {
		if c.schema == "" {
			if c.data != nil {
				t.Fatalf("%s has data but no schema", c.typ)
			}
			continue
		}
		schema, ok := doc.Components.Schemas[c.schema]
		if !ok {
			t.Errorf("schema %s for %s is missing", c.schema, c.typ)
			continue
		}
		if schema.Type != "object" {
			t.Errorf("schema %s type = %q, want object", c.schema, schema.Type)
		}
		want, wantRequired := expectedPayload(reflect.TypeOf(c.data))
		if len(schema.Properties) != len(want) {
			t.Errorf("schema %s has %d properties, want %d (%v)", c.schema, len(schema.Properties), len(want), want)
		}
		for name, typ := range want {
			prop, ok := schema.Properties[name]
			if !ok {
				t.Errorf("schema %s is missing property %q", c.schema, name)
				continue
			}
			if prop.Type != typ {
				t.Errorf("schema %s property %q type = %q, want %q", c.schema, name, prop.Type, typ)
			}
			if typ == "array" && (prop.Items == nil || prop.Items.Type != "string") {
				t.Errorf("schema %s property %q items = %+v, want string", c.schema, name, prop.Items)
			}
		}
		if c.typ == EventStatus {
			got := append([]string(nil), schema.Properties["status"].Enum...)
			sort.Strings(got)
			want := statusValues()
			if !reflect.DeepEqual(got, want) {
				t.Errorf("schema %s status enum = %v, want %v", c.schema, got, want)
			}
		}
		gotRequired := append([]string(nil), schema.Required...)
		sort.Strings(gotRequired)
		if !reflect.DeepEqual(gotRequired, wantRequired) {
			t.Errorf("schema %s required = %v, want %v", c.schema, gotRequired, wantRequired)
		}
	}

	event, ok := doc.Components.Schemas["Event"]
	if !ok {
		t.Fatal("Event schema is missing")
	}
	dataProp, ok := event.Properties["data"]
	if !ok {
		t.Fatal("Event.data is missing")
	}
	if dataProp.Type != "object" || dataProp.Ref != "" {
		t.Errorf("Event.data = %+v, want a free-form object", dataProp)
	}
	if !strings.Contains(dataProp.Description, "docs/events.md") {
		t.Errorf("Event.data description does not point at docs/events.md")
	}

	// The per-type schemas are not reachable from Event.data: the shape
	// depends on Event.type. docs/events.md is the mapping between them.
	docs, err := os.ReadFile(filepath.Join("..", "..", "docs", "events.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range eventCases() {
		if c.schema == "" {
			continue
		}
		row := "| `" + string(c.typ) + "` | `" + c.schema + "` |"
		if !strings.Contains(string(docs), row) {
			t.Errorf("docs/events.md does not map %s to %s", c.typ, c.schema)
		}
	}
}

// expectedPayload returns the property types and required names that the Go
// payload type declares through its JSON tags. A field with omitempty is
// optional on the wire.
func expectedPayload(t reflect.Type) (map[string]string, []string) {
	props := map[string]string{}
	var required []string
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name, rest, _ := strings.Cut(tag, ",")
		props[name] = schemaType(f.Type)
		if !strings.Contains(rest, "omitempty") {
			required = append(required, name)
		}
	}
	sort.Strings(required)
	return props, required
}

func schemaType(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "integer"
	case reflect.Bool:
		return "boolean"
	case reflect.Slice:
		return "array"
	default:
		return t.Kind().String()
	}
}

func statusValues() []string {
	out := []string{
		string(StatusPending), string(StatusRunning), string(StatusClosing),
		string(StatusCompleted), string(StatusFailed), string(StatusExpired),
	}
	sort.Strings(out)
	return out
}

func TestEventDecodeIgnoresUnknownTypeAndFields(t *testing.T) {
	var ev Event
	raw := `{"seq":1,"type":"future_event","time":"2026-01-02T03:04:05Z","data":{"unknown":true}}`
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("unknown event type: %v", err)
	}
	if ev.Data != nil {
		t.Fatalf("unknown type payload = %#v, want nil", ev.Data)
	}

	raw = `{"seq":2,"type":"tool_result","data":{"tool":"echo","call_id":"c1","error":false,"future":1}}`
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("unknown payload field: %v", err)
	}
	data, ok := DataAs[ToolResultData](ev.Data)
	if !ok || data.Tool != "echo" {
		t.Fatalf("payload = %#v, want tool echo", ev.Data)
	}
}
