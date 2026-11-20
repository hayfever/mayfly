package httpapi

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/hayfever/mayfly/internal/apidoc"
	"github.com/hayfever/mayfly/internal/config"
)

// The tests in this file keep api/openapi.yaml, the router, and the docs from
// drifting apart. TestOpenAPISpecMatchesRoutes proves that every route has an
// operation; these tests prove that the scope, the error codes, and the
// generated reference agree too.

// specSecurity is the part of the spec that the scope check reads.
type specSecurity struct {
	Security []map[string][]string           `yaml:"security"`
	Paths    map[string]map[string]yaml.Node `yaml:"paths"`
}

// TestAPIRouteScopesMatchSpec compares the bearerAuth requirement of every
// operation with the scope in apiRoutes().
func TestAPIRouteScopesMatchSpec(t *testing.T) {
	var doc specSecurity
	loadSpec(t, &doc)

	server := New(config.Default(), nil, nil, testLogger())
	if problems := routeScopeProblems(doc, server.apiRoutes()); len(problems) > 0 {
		t.Fatalf("api/openapi.yaml and the route scopes disagree:\n  %s", strings.Join(problems, "\n  "))
	}
}

// TestRouteScopeCheckCatchesMismatch breaks one scope in an in-memory copy of
// the real spec, so a silent regression in the check itself fails here. The
// file on disk is not touched.
func TestRouteScopeCheckCatchesMismatch(t *testing.T) {
	var doc specSecurity
	loadSpec(t, &doc)
	server := New(config.Default(), nil, nil, testLogger())

	const createSession = "operationId: createSession\nsecurity:\n  - bearerAuth: [sessions:read]\n"
	var replacement yaml.Node
	if err := yaml.Unmarshal([]byte(createSession), &replacement); err != nil {
		t.Fatal(err)
	}
	doc.Paths["/api/v1/sessions"]["post"] = *replacement.Content[0]

	problems := routeScopeProblems(doc, server.apiRoutes())
	if len(problems) != 1 {
		t.Fatalf("want one problem for a wrong scope, got %v", problems)
	}
	want := `operation "POST /api/v1/sessions" has bearerAuth scopes [sessions:read]; want ["sessions:write"]`
	if problems[0] != want {
		t.Fatalf("got %q; want %q", problems[0], want)
	}
}

// routeScopeProblems reports every disagreement between a route and its
// operation. A route with no scope must not require bearerAuth, so the attach
// WebSocket keeps its ticket authentication. Another scheme on the operation,
// such as cookieAuth for the web UI, is ignored.
func routeScopeProblems(doc specSecurity, routes []apiRoute) []string {
	var problems []string
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	for _, route := range routes {
		method, path, ok := strings.Cut(route.pattern, " ")
		if !ok {
			report("route %q has no method", route.pattern)
			continue
		}
		item, ok := doc.Paths[path]
		if !ok {
			report("route %q is missing from api/openapi.yaml", route.pattern)
			continue
		}
		node, ok := item[strings.ToLower(method)]
		if !ok {
			report("route %q has no operation in api/openapi.yaml", route.pattern)
			continue
		}
		var operation struct {
			Security []map[string][]string `yaml:"security"`
		}
		if err := node.Decode(&operation); err != nil {
			report("decode operation %q: %v", route.pattern, err)
			continue
		}

		scopes, hasBearer := bearerAuthScopes(operation.Security)
		switch {
		case route.scope == "" && hasBearer:
			report("route %q has no scope, so its operation must not require bearerAuth", route.pattern)
		case route.scope != "" && !hasBearer:
			if docScopes, inherited := bearerAuthScopes(doc.Security); inherited {
				report("operation %q inherits bearerAuth scopes %v; want [%q] on the operation",
					route.pattern, docScopes, route.scope)
				continue
			}
			report("operation %q requires no bearerAuth; want [%q]", route.pattern, route.scope)
		case route.scope != "" && !equalStrings(scopes, []string{route.scope}):
			report("operation %q has bearerAuth scopes %v; want [%q]", route.pattern, scopes, route.scope)
		}
	}
	return problems
}

// TestSecurityScopeTableMatchesRoutes compares the scope table in
// docs/security.md with the route table: every scoped route appears once,
// under its own scope.
func TestSecurityScopeTableMatchesRoutes(t *testing.T) {
	raw, err := os.ReadFile("../../docs/security.md")
	if err != nil {
		t.Fatal(err)
	}
	documented := parseScopeTable(t, string(raw))

	server := New(config.Default(), nil, nil, testLogger())
	want := map[string]map[string]bool{}
	for _, route := range server.apiRoutes() {
		if route.scope == "" {
			continue
		}
		if want[route.scope] == nil {
			want[route.scope] = map[string]bool{}
		}
		want[route.scope][route.pattern] = true
	}

	var problems []string
	for scope, routes := range want {
		for route := range routes {
			if !documented[scope][route] {
				problems = append(problems, fmt.Sprintf("%s is not listed under %q", route, scope))
			}
		}
	}
	for scope, routes := range documented {
		for route := range routes {
			switch {
			case want[scope] == nil:
				problems = append(problems, fmt.Sprintf("%q lists %s, but no route uses that scope", scope, route))
			case !want[scope][route]:
				problems = append(problems, fmt.Sprintf("%s is listed under %q; its route uses %q",
					route, scope, routeScope(server, route)))
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("docs/security.md and the route table disagree:\n  %s", strings.Join(problems, "\n  "))
	}
}

// TestErrorCodesMatchDocsAndSpec keeps the error codes in three places equal:
// the constants in errors.go, the table in docs/api.md, and the enum in
// api/openapi.yaml.
func TestErrorCodesMatchDocsAndSpec(t *testing.T) {
	codeConstants := parseErrorCodeConstants(t, "errors.go")
	docCodes := parseAPIDocErrorCodes(t)
	specCodes := parseSpecErrorCodes(t)

	if !equalStrings(codeConstants, docCodes) {
		t.Errorf("docs/api.md documents %v; errors.go defines %v", docCodes, codeConstants)
	}
	if !equalStrings(codeConstants, specCodes) {
		t.Errorf("api/openapi.yaml enumerates %v; errors.go defines %v", specCodes, codeConstants)
	}
}

// TestAPIDocsCoverEveryRoute proves that the generated reference names every
// route. docs/api.md may describe a route instead; the generated file must
// always carry it, which is what keeps the committed file honest.
func TestAPIDocsCoverEveryRoute(t *testing.T) {
	reference, err := os.ReadFile("../../docs/api-reference.md")
	if err != nil {
		t.Fatalf("%v: regenerate it with `go run ./hack/apidoc`", err)
	}
	if !strings.Contains(string(reference), apidoc.GeneratedHeader) {
		t.Errorf("docs/api-reference.md does not carry the generated-file header; regenerate it with `go run ./hack/apidoc`")
	}
	apiDoc, err := os.ReadFile("../../docs/api.md")
	if err != nil {
		t.Fatal(err)
	}

	server := New(config.Default(), nil, nil, testLogger())
	for _, route := range server.apiRoutes() {
		_, path, ok := strings.Cut(route.pattern, " ")
		if !ok {
			t.Fatalf("route %q has no method", route.pattern)
		}
		if strings.Contains(string(apiDoc), path) || strings.Contains(string(reference), path) {
			continue
		}
		t.Errorf("route %q appears in neither docs/api.md nor docs/api-reference.md", route.pattern)
	}
}

// bearerAuthScopes returns the bearerAuth scopes of a security block. The bool
// is false when the block has no bearerAuth requirement, which is how a
// scope-less route is written.
func bearerAuthScopes(security []map[string][]string) ([]string, bool) {
	for _, requirement := range security {
		if scopes, ok := requirement["bearerAuth"]; ok {
			return scopes, true
		}
	}
	return nil, false
}

var (
	scopeRowPattern   = regexp.MustCompile("^\\| `([a-z0-9:_-]+)` \\|(.+)\\|\\s*$")
	scopeRoutePattern = regexp.MustCompile("`((?:GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)) (/[^`]*)`")
)

// parseScopeTable reads the rows that start with a scope name and collects the
// routes named in the second cell.
func parseScopeTable(t *testing.T, markdown string) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	for _, line := range strings.Split(markdown, "\n") {
		match := scopeRowPattern.FindStringSubmatch(strings.TrimRight(line, " "))
		if match == nil {
			continue
		}
		scope := match[1]
		if out[scope] == nil {
			out[scope] = map[string]bool{}
		}
		for _, route := range scopeRoutePattern.FindAllStringSubmatch(match[2], -1) {
			out[scope][route[1]+" "+route[2]] = true
		}
	}
	if len(out) == 0 {
		t.Fatal("docs/security.md has no scope table rows")
	}
	return out
}

func routeScope(server *Server, pattern string) string {
	for _, route := range server.apiRoutes() {
		if route.pattern == pattern {
			return route.scope
		}
	}
	return ""
}

// parseErrorCodeConstants reads the string constants in errors.go whose name
// starts with "code". Parsing the source avoids exporting a test-only list from
// the package.
func parseErrorCodeConstants(t *testing.T, path string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, decl := range file.Decls {
		group, ok := decl.(*ast.GenDecl)
		if !ok || group.Tok != token.CONST {
			continue
		}
		for _, spec := range group.Specs {
			values, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range values.Names {
				if !strings.HasPrefix(name.Name, "code") || i >= len(values.Values) {
					continue
				}
				literal, ok := values.Values[i].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatalf("errors.go: constant %s: %v", name.Name, err)
				}
				out = append(out, value)
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("errors.go defines no code constants")
	}
	sort.Strings(out)
	return out
}

var apiDocCodePattern = regexp.MustCompile("^\\| `([a-z_]+)` \\| [0-9]{3} \\|")

// parseAPIDocErrorCodes reads the code column of the Errors table.
func parseAPIDocErrorCodes(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../../docs/api.md")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	inErrors := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "## ") {
			inErrors = strings.HasPrefix(line, "## Errors")
			continue
		}
		if !inErrors {
			continue
		}
		if match := apiDocCodePattern.FindStringSubmatch(line); match != nil {
			out = append(out, match[1])
		}
	}
	if len(out) == 0 {
		t.Fatal("docs/api.md has no Errors table")
	}
	sort.Strings(out)
	return out
}

// parseSpecErrorCodes reads the enum of the Error envelope code property. It
// follows the `error` reference so a rename of ErrorBody keeps the check.
func parseSpecErrorCodes(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse api/openapi.yaml: %v", err)
	}

	components := yamlObject(t, doc, "components")
	schemas := yamlObject(t, components, "schemas")
	errSchema := yamlObject(t, schemas, "Error")
	body := yamlObject(t, yamlObject(t, errSchema, "properties"), "error")
	if ref, ok := body["$ref"].(string); ok {
		resolved, ok := specPointer(doc, ref).(map[string]any)
		if !ok {
			t.Fatalf("api/openapi.yaml: %q does not resolve to an object", ref)
		}
		body = resolved
	}
	code := yamlObject(t, yamlObject(t, body, "properties"), "code")
	values, ok := code["enum"].([]any)
	if !ok {
		t.Fatal("api/openapi.yaml: the Error code property has no enum")
	}
	var out []string
	for _, value := range values {
		text, ok := value.(string)
		if !ok {
			t.Fatalf("api/openapi.yaml: the Error code enum holds a non-string %v", value)
		}
		out = append(out, text)
	}
	sort.Strings(out)
	return out
}

// yamlObject returns the mapping stored under key.
func yamlObject(t *testing.T, node any, key string) map[string]any {
	t.Helper()
	object, ok := node.(map[string]any)
	if !ok {
		t.Fatalf("api/openapi.yaml: expected an object at %q", key)
	}
	child, ok := object[key].(map[string]any)
	if !ok {
		t.Fatalf("api/openapi.yaml: expected an object at %q", key)
	}
	return child
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
