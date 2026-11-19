package httpapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/hayfever/mayfly/internal/config"
)

// TestOpenAPISpecMatchesRoutes keeps api/openapi.yaml and the router in step. A
// route without an operation, or an /api/v1 operation without a route, fails.
func TestOpenAPISpecMatchesRoutes(t *testing.T) {
	var doc struct {
		OpenAPI string                    `yaml:"openapi"`
		Paths   map[string]map[string]any `yaml:"paths"`
	}
	loadSpec(t, &doc)
	if !strings.HasPrefix(doc.OpenAPI, "3.1.") {
		t.Fatalf("spec version = %q, want 3.1.x", doc.OpenAPI)
	}

	spec := map[string]bool{}
	for path, operations := range doc.Paths {
		for method := range operations {
			if !isHTTPMethod(method) {
				continue
			}
			spec[strings.ToUpper(method)+" "+path] = true
		}
	}

	server := New(config.Default(), nil, nil, testLogger())
	routes := map[string]bool{}
	for _, route := range server.apiRoutes() {
		method, path, ok := strings.Cut(route.pattern, " ")
		if !ok {
			t.Fatalf("route %q has no method", route.pattern)
		}
		routes[method+" "+path] = true
		if !spec[method+" "+path] {
			t.Errorf("route %q is missing from api/openapi.yaml", route.pattern)
		}
	}
	for operation := range spec {
		path := strings.SplitN(operation, " ", 2)[1]
		if strings.HasPrefix(path, "/api/v1") && !routes[operation] {
			t.Errorf("spec operation %q has no registered route", operation)
		}
	}
}

// TestOpenAPISpecReferencesResolve catches a $ref that points at nothing, which
// a YAML parser alone cannot see.
func TestOpenAPISpecReferencesResolve(t *testing.T) {
	var doc any
	loadSpec(t, &doc)
	for _, ref := range specRefs(doc) {
		if !strings.HasPrefix(ref, "#/") {
			// An external reference is a file beside the spec, such as the
			// job envelope schema under api/.
			if _, err := os.Stat(filepath.Join("..", "..", "api", filepath.FromSlash(ref))); err != nil {
				t.Errorf("external reference %q does not resolve: %v", ref, err)
			}
			continue
		}
		if specPointer(doc, ref) == nil {
			t.Errorf("reference %q does not resolve", ref)
		}
	}
}

// TestOpenAPISessionHidesInternals keeps the runtime backend, the pod name,
// and the artifact store URI out of the public session object.
func TestOpenAPISessionHidesInternals(t *testing.T) {
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Required   []string                  `yaml:"required"`
				Properties map[string]map[string]any `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	loadSpec(t, &doc)

	session, ok := doc.Components.Schemas["Session"]
	if !ok {
		t.Fatal("spec has no Session schema")
	}
	for _, name := range []string{"backend", "pod_name"} {
		if _, ok := session.Properties[name]; ok {
			t.Errorf("Session schema exposes %q", name)
		}
		for _, required := range session.Required {
			if required == name {
				t.Errorf("Session schema requires the internal field %q", name)
			}
		}
	}
	artifact, ok := doc.Components.Schemas["Artifact"]
	if !ok {
		t.Fatal("spec has no Artifact schema")
	}
	if _, ok := artifact.Properties["uri"]; ok {
		t.Error("Artifact schema exposes the internal store uri")
	}
	for _, required := range artifact.Required {
		if required == "uri" {
			t.Error("Artifact schema requires the internal store uri")
		}
	}
}

// TestOpenAPIEventTypeIsOpen keeps Event.type extensible: a new type is an
// additive change, so the schema must not enumerate a closed set.
func TestOpenAPIEventTypeIsOpen(t *testing.T) {
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Enum        []string `yaml:"enum"`
					Description string   `yaml:"description"`
				} `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	loadSpec(t, &doc)
	typ, ok := doc.Components.Schemas["Event"].Properties["type"]
	if !ok {
		t.Fatal("spec Event has no type property")
	}
	if len(typ.Enum) != 0 {
		t.Fatalf("Event.type is a closed enum: %v", typ.Enum)
	}
	if !strings.Contains(strings.Join(strings.Fields(typ.Description), " "), "ignore an unknown type") {
		t.Fatalf("Event.type description does not state the compatibility rule: %q", typ.Description)
	}
}

func loadSpec(t *testing.T, dst any) {
	t.Helper()
	raw, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, dst); err != nil {
		t.Fatalf("parse api/openapi.yaml: %v", err)
	}
}

func isHTTPMethod(name string) bool {
	switch strings.ToLower(name) {
	case "get", "put", "post", "delete", "options", "head", "patch", "trace":
		return true
	}
	return false
}

// specRefs collects every $ref value in the document.
func specRefs(node any) []string {
	var out []string
	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "$ref" {
				if ref, ok := child.(string); ok {
					out = append(out, ref)
				}
				continue
			}
			out = append(out, specRefs(child)...)
		}
	case []any:
		for _, child := range value {
			out = append(out, specRefs(child)...)
		}
	}
	return out
}

// specPointer resolves a local JSON pointer such as #/components/schemas/Error.
func specPointer(doc any, ref string) any {
	node := doc
	for _, token := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		object, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node = object[token]
		if node == nil {
			return nil
		}
	}
	return node
}
