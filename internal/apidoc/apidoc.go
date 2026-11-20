// Package apidoc renders the OpenAPI document into a Markdown reference for
// people. The output is deterministic: the same spec bytes always render the
// same Markdown, so a committed reference can be compared byte for byte.
package apidoc

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// GeneratedHeader warns a reader that the file is not hand-written. Drift
// checks assert that a committed reference carries this line.
const GeneratedHeader = "Generated from api/openapi.yaml by `go run ./hack/apidoc`. Do not edit."

// wrapWidth bounds a rendered paragraph. Prose from the spec arrives with the
// YAML author's line breaks; reflowing keeps the output stable when only the
// source wrapping changes.
const wrapWidth = 88

// Render produces the Markdown reference for one OpenAPI document.
func Render(spec []byte) ([]byte, error) {
	d, err := newDocument(spec)
	if err != nil {
		return nil, err
	}
	ops, err := d.operations()
	if err != nil {
		return nil, err
	}

	title := oneLine(member(d.root, "info", "title"))
	if title == "" {
		title = "HTTP API"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# %s reference\n\n%s\n", title, GeneratedHeader)

	for _, tag := range d.allTags(ops) {
		if group := operationsForTag(ops, tag.name); len(group) > 0 {
			writeTag(&b, tag, group)
		}
	}
	if group := operationsForTag(ops, ""); len(group) > 0 {
		writeTag(&b, tagInfo{name: "Other operations"}, group)
	}

	if codes := d.errorCodes(); len(codes) > 0 {
		writeErrorCodes(&b, codes)
	}
	server, client := d.webSocketMessages()
	if len(server) > 0 || len(client) > 0 {
		writeWebSocketMessages(&b, server, client)
	}

	return []byte(b.String()), nil
}

// document wraps the parsed spec so helpers can resolve local references.
type document struct {
	root *yaml.Node
}

func newDocument(spec []byte) (*document, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(spec, &root); err != nil {
		return nil, fmt.Errorf("parse spec: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("parse spec: the document is empty")
	}
	return &document{root: root.Content[0]}, nil
}

type operation struct {
	method      string
	path        string
	tags        []string
	id          string
	summary     string
	description string
	scopes      []string
	parameters  []parameter
	requestBody *requestBody
	responses   []response
}

type parameter struct {
	name     string
	in       string
	typ      string
	required bool
}

type requestBody struct {
	schema   string
	contents []string
	required bool
}

type response struct {
	status      string
	description string
	schema      string
}

type tagInfo struct {
	name        string
	description string
}

// operations lists every method in document order. Order matters: the output
// must not change when only map iteration order would differ.
func (d *document) operations() ([]operation, error) {
	paths := member(d.root, "paths")
	if paths == nil || paths.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("parse spec: the document has no paths")
	}

	var out []operation
	for i := 0; i+1 < len(paths.Content); i += 2 {
		path := paths.Content[i].Value
		item := paths.Content[i+1]
		if item.Kind != yaml.MappingNode {
			continue
		}
		pathParameters := d.parameters(member(item, "parameters"))
		for j := 0; j+1 < len(item.Content); j += 2 {
			method := strings.ToLower(item.Content[j].Value)
			if !isHTTPMethod(method) {
				continue
			}
			node := item.Content[j+1]
			if node.Kind != yaml.MappingNode {
				continue
			}
			op := operation{
				method:      strings.ToUpper(method),
				path:        path,
				tags:        scalars(member(node, "tags")),
				id:          scalar(member(node, "operationId")),
				summary:     oneLine(member(node, "summary")),
				description: text(member(node, "description")),
				scopes:      d.bearerScopes(node),
				parameters:  mergeParameters(pathParameters, d.parameters(member(node, "parameters"))),
				requestBody: d.requestBody(member(node, "requestBody")),
				responses:   d.responses(member(node, "responses")),
			}
			out = append(out, op)
		}
	}
	return out, nil
}

// tags lists the document tags in declaration order. The renderer uses this
// order, and appends any tag that an operation names but the document does not
// declare, so no operation is dropped.
func (d *document) tags() []tagInfo {
	declared := member(d.root, "tags")
	var out []tagInfo
	seen := map[string]bool{}
	if declared != nil && declared.Kind == yaml.SequenceNode {
		for _, node := range declared.Content {
			tag := tagInfo{
				name:        scalar(member(node, "name")),
				description: text(member(node, "description")),
			}
			if tag.name == "" || seen[tag.name] {
				continue
			}
			seen[tag.name] = true
			out = append(out, tag)
		}
	}
	return out
}

// allTags lists the document tags in declaration order, then any tag that an
// operation names but the document does not declare, in first-use order.
func (d *document) allTags(ops []operation) []tagInfo {
	out := d.tags()
	seen := map[string]bool{}
	for _, tag := range out {
		seen[tag.name] = true
	}
	for _, op := range ops {
		for _, name := range op.tags {
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, tagInfo{name: name})
		}
	}
	return out
}

func operationsForTag(ops []operation, tag string) []operation {
	var out []operation
	for _, op := range ops {
		if tag == "" {
			if len(op.tags) == 0 {
				out = append(out, op)
			}
			continue
		}
		for _, name := range op.tags {
			if name == tag {
				out = append(out, op)
				break
			}
		}
	}
	return out
}

func writeTag(b *strings.Builder, tag tagInfo, ops []operation) {
	fmt.Fprintf(b, "\n## %s\n", tag.name)
	if tag.description != "" {
		fmt.Fprintf(b, "\n%s\n", wrapText(tag.description))
	}
	b.WriteString("\n| Method | Path | Operation | Scope | Summary |\n")
	b.WriteString("| --- | --- | --- | --- | --- |\n")
	for _, op := range ops {
		fmt.Fprintf(b, "| `%s` | `%s` | %s | %s | %s |\n",
			op.method, op.path, codeOrDash(op.id), scopeCell(op.scopes), cell(op.summary))
	}
	for _, op := range ops {
		writeOperation(b, op)
	}
}

func writeOperation(b *strings.Builder, op operation) {
	heading := op.id
	if heading == "" {
		heading = op.method + " " + op.path
	}
	fmt.Fprintf(b, "\n### %s\n\n`%s %s`\n", heading, op.method, op.path)

	if op.description != "" {
		fmt.Fprintf(b, "\n%s\n", wrapText(op.description))
	}
	if len(op.parameters) > 0 {
		b.WriteString("\nParameters:\n\n| Name | In | Required | Type |\n| --- | --- | --- | --- |\n")
		for _, p := range op.parameters {
			fmt.Fprintf(b, "| %s | %s | %s | %s |\n",
				cell(p.name), cell(p.in), yesNo(p.required), typeCell(p.typ))
		}
	}
	if body := op.requestBody; body != nil {
		fmt.Fprintf(b, "\nRequest body: %s", typeCell(body.schema))
		if body.required {
			b.WriteString(" (required)")
		}
		if contents := quoteAll(body.contents); contents != "" {
			fmt.Fprintf(b, ". Content types: %s", contents)
		}
		b.WriteString(".\n")
	}

	b.WriteString("\nResponses:\n\n| Status | Description | Schema |\n| --- | --- | --- |\n")
	for _, r := range op.responses {
		fmt.Fprintf(b, "| %s | %s | %s |\n", cell(r.status), cell(r.description), typeCell(r.schema))
	}
}

func writeErrorCodes(b *strings.Builder, codes []string) {
	b.WriteString("\n## Error codes\n\n")
	b.WriteString("Every failed request returns `Error` with one of these `error.code` values:\n\n")
	b.WriteString("| Code |\n| --- |\n")
	for _, code := range codes {
		fmt.Fprintf(b, "| `%s` |\n", code)
	}
}

type wsMessage struct {
	name string
	typ  string
}

func writeWebSocketMessages(b *strings.Builder, server, client []wsMessage) {
	b.WriteString("\n## WebSocket messages\n")
	writeWSGroup := func(title, root string, messages []wsMessage) {
		if len(messages) == 0 {
			return
		}
		fmt.Fprintf(b, "\n%s, as %s:\n\n", title, codeOrDash(root))
		b.WriteString("| Message | `type` |\n| --- | --- |\n")
		for _, message := range messages {
			fmt.Fprintf(b, "| `%s` | `%s` |\n", message.name, message.typ)
		}
	}
	writeWSGroup("Server messages", "WebSocketServerMessage", server)
	writeWSGroup("Client messages", "WebSocketClientMessage", client)
}

// bearerScopes returns the scopes of the bearerAuth requirement. An operation
// without its own security block inherits the document-level one.
func (d *document) bearerScopes(op *yaml.Node) []string {
	security := member(op, "security")
	if security == nil {
		security = member(d.root, "security")
	}
	if security == nil || security.Kind != yaml.SequenceNode {
		return nil
	}
	for _, entry := range security.Content {
		if scopes := member(entry, "bearerAuth"); scopes != nil {
			return scalars(scopes)
		}
	}
	return nil
}

func (d *document) parameters(node *yaml.Node) []parameter {
	node = d.deref(node)
	if node == nil || node.Kind != yaml.SequenceNode {
		return nil
	}
	var out []parameter
	for _, raw := range node.Content {
		node := d.deref(raw)
		if node == nil || node.Kind != yaml.MappingNode {
			continue
		}
		param := parameter{
			name:     scalar(member(node, "name")),
			in:       scalar(member(node, "in")),
			typ:      d.schemaName(member(node, "schema")),
			required: scalar(member(node, "required")) == "true",
		}
		if param.name == "" {
			continue
		}
		out = append(out, param)
	}
	return out
}

// mergeParameters lists the path-level parameters first and lets an operation
// parameter override the same name and location.
func mergeParameters(pathLevel, operationLevel []parameter) []parameter {
	out := make([]parameter, 0, len(pathLevel)+len(operationLevel))
	index := map[string]int{}
	for _, p := range pathLevel {
		index[p.name+"\x00"+p.in] = len(out)
		out = append(out, p)
	}
	for _, p := range operationLevel {
		if at, ok := index[p.name+"\x00"+p.in]; ok {
			out[at] = p
			continue
		}
		index[p.name+"\x00"+p.in] = len(out)
		out = append(out, p)
	}
	return out
}

func (d *document) requestBody(node *yaml.Node) *requestBody {
	node = d.deref(node)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	body := &requestBody{required: scalar(member(node, "required")) == "true"}
	content := member(node, "content")
	if content != nil && content.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(content.Content); i += 2 {
			body.contents = append(body.contents, content.Content[i].Value)
			if body.schema == "" {
				body.schema = d.schemaName(member(content.Content[i+1], "schema"))
			}
		}
	}
	if body.schema == "" {
		body.schema = "inline"
	}
	return body
}

func (d *document) responses(node *yaml.Node) []response {
	node = d.deref(node)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	var out []response
	for i := 0; i+1 < len(node.Content); i += 2 {
		raw := d.deref(node.Content[i+1])
		r := response{status: node.Content[i].Value}
		if raw != nil {
			r.description = oneLine(member(raw, "description"))
			r.schema = d.contentSchema(member(raw, "content"))
		}
		out = append(out, r)
	}
	return out
}

// contentSchema names the schema of the first media type in a content block.
func (d *document) contentSchema(content *yaml.Node) string {
	content = d.deref(content)
	if content == nil || content.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(content.Content); i += 2 {
		if name := d.schemaName(member(content.Content[i+1], "schema")); name != "" {
			return name
		}
	}
	return ""
}

// schemaName reports a reference name where the schema has one, and a short
// structural name otherwise.
func (d *document) schemaName(node *yaml.Node) string {
	if node == nil {
		return ""
	}
	if ref := scalar(member(node, "$ref")); ref != "" {
		return refName(ref)
	}
	node = d.deref(node)
	if node == nil {
		return ""
	}
	for _, union := range []string{"oneOf", "anyOf", "allOf"} {
		if member(node, union) != nil {
			return union
		}
	}
	typ := scalar(member(node, "type"))
	if typ == "array" {
		if item := d.schemaName(member(node, "items")); item != "" {
			return "array of " + item
		}
	}
	if typ != "" {
		return typ
	}
	if member(node, "properties") != nil {
		return "object"
	}
	return ""
}

// errorCodes reads the Error schema enum. It follows the `error` reference
// into ErrorBody so a schema rename does not silently drop the section.
func (d *document) errorCodes() []string {
	schemas := member(d.root, "components", "schemas")
	if schemas == nil {
		return nil
	}
	node := member(schemas, "Error")
	if node == nil {
		return nil
	}
	node = d.deref(member(node, "properties", "error"))
	if node == nil {
		node = member(schemas, "Error")
	}
	code := d.deref(member(node, "properties", "code"))
	if code == nil {
		return nil
	}
	enum := member(code, "enum")
	if enum == nil || enum.Kind != yaml.SequenceNode {
		return nil
	}
	return scalars(enum)
}

// webSocketMessages lists the union members of the WebSocket server and client
// message schemas, with the `type` constant of each member.
func (d *document) webSocketMessages() (server, client []wsMessage) {
	schemas := member(d.root, "components", "schemas")
	if schemas == nil {
		return nil, nil
	}
	collect := func(root string) []wsMessage {
		node := d.deref(member(schemas, root))
		union := member(node, "oneOf")
		if union == nil || union.Kind != yaml.SequenceNode {
			return nil
		}
		var out []wsMessage
		for _, entry := range union.Content {
			ref := scalar(member(entry, "$ref"))
			if ref == "" {
				continue
			}
			name := refName(ref)
			memberNode := d.deref(entry)
			typ := scalar(member(memberNode, "properties", "type", "const"))
			if name == "" || typ == "" {
				continue
			}
			out = append(out, wsMessage{name: name, typ: typ})
		}
		return out
	}
	return collect("WebSocketServerMessage"), collect("WebSocketClientMessage")
}

// --- node helpers -------------------------------------------------------

// member walks nested mapping keys and returns the value node, or nil.
func member(node *yaml.Node, keys ...string) *yaml.Node {
	for _, key := range keys {
		if node == nil || node.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == key {
				next = node.Content[i+1]
				break
			}
		}
		node = next
	}
	return node
}

func scalar(node *yaml.Node) string {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag == "!!null" {
		return ""
	}
	return node.Value
}

func scalars(node *yaml.Node) []string {
	if node == nil || node.Kind != yaml.SequenceNode {
		return nil
	}
	var out []string
	for _, item := range node.Content {
		if value := scalar(item); value != "" {
			out = append(out, value)
		}
	}
	return out
}

// deref follows a local $ref or an alias to the target node.
func (d *document) deref(node *yaml.Node) *yaml.Node {
	for depth := 0; node != nil && depth < 16; depth++ {
		if node.Kind == yaml.AliasNode {
			node = node.Alias
			continue
		}
		ref := scalar(member(node, "$ref"))
		if ref == "" {
			return node
		}
		node = d.lookup(ref)
	}
	return nil
}

// lookup resolves a local JSON pointer such as #/components/schemas/Error.
func (d *document) lookup(ref string) *yaml.Node {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	node := d.root
	for _, token := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		node = member(node, token)
		if node == nil {
			return nil
		}
	}
	return node
}

func refName(ref string) string {
	if !strings.HasPrefix(ref, "#/") {
		return ref
	}
	at := strings.LastIndex(ref, "/")
	if at < 0 {
		return ref
	}
	name := ref[at+1:]
	return strings.ReplaceAll(strings.ReplaceAll(name, "~1", "/"), "~0", "~")
}

func isHTTPMethod(name string) bool {
	switch name {
	case "get", "put", "post", "delete", "options", "head", "patch", "trace":
		return true
	}
	return false
}

// --- text helpers -------------------------------------------------------

// text normalizes a description into single-spaced paragraphs.
func text(node *yaml.Node) string {
	raw := scalar(node)
	if raw == "" {
		return ""
	}
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	var paragraphs []string
	for _, paragraph := range strings.Split(raw, "\n\n") {
		if fields := strings.Fields(paragraph); len(fields) > 0 {
			paragraphs = append(paragraphs, strings.Join(fields, " "))
		}
	}
	return strings.Join(paragraphs, "\n\n")
}

// oneLine collapses a description into one line for a table cell.
func oneLine(node *yaml.Node) string {
	return strings.Join(strings.Fields(scalar(node)), " ")
}

// wrapText wraps each paragraph of pre-normalized text at wrapWidth.
func wrapText(text string) string {
	paragraphs := strings.Split(text, "\n\n")
	for i, paragraph := range paragraphs {
		paragraphs[i] = wrapLine(paragraph, wrapWidth)
	}
	return strings.Join(paragraphs, "\n\n")
}

func wrapLine(line string, width int) string {
	var out strings.Builder
	column := 0
	for _, word := range strings.Fields(line) {
		switch {
		case column == 0:
			out.WriteString(word)
			column = len(word)
		case column+1+len(word) <= width:
			out.WriteByte(' ')
			out.WriteString(word)
			column += 1 + len(word)
		default:
			out.WriteByte('\n')
			out.WriteString(word)
			column = len(word)
		}
	}
	return out.String()
}

// cell escapes a table cell.
func cell(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	value = strings.ReplaceAll(value, "|", `\|`)
	if value == "" {
		return "-"
	}
	return value
}

func typeCell(name string) string {
	if name == "" {
		return "-"
	}
	return "`" + name + "`"
}

func codeOrDash(value string) string {
	if value == "" {
		return "-"
	}
	return "`" + value + "`"
}

func scopeCell(scopes []string) string {
	if len(scopes) == 0 {
		return "none"
	}
	var quoted []string
	for _, scope := range scopes {
		quoted = append(quoted, "`"+scope+"`")
	}
	return strings.Join(quoted, ", ")
}

func quoteAll(values []string) string {
	var quoted []string
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			quoted = append(quoted, "`"+value+"`")
		}
	}
	return strings.Join(quoted, ", ")
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

// --- diff ---------------------------------------------------------------

// diffLineLimit bounds the quadratic line diff. A reference document is a few
// hundred lines; a larger input gets a summary instead of an allocation spike.
const diffLineLimit = 2000

// UnifiedDiff returns a unified diff between two files. It returns an empty
// string when the contents are equal.
func UnifiedDiff(oldName, newName string, oldBytes, newBytes []byte) string {
	if bytes.Equal(oldBytes, newBytes) {
		return ""
	}
	oldLines, newLines := splitLines(oldBytes), splitLines(newBytes)
	if len(oldLines) > diffLineLimit || len(newLines) > diffLineLimit {
		return fmt.Sprintf("%s and %s differ (%d and %d lines); the diff is too large to print\n",
			oldName, newName, len(oldLines), len(newLines))
	}

	ops := lineOps(oldLines, newLines)
	var changes []int
	for i, op := range ops {
		if op.kind != ' ' {
			changes = append(changes, i)
		}
	}
	if len(changes) == 0 {
		return ""
	}

	const context = 3
	type span struct{ from, to int }
	var spans []span
	for _, at := range changes {
		if n := len(spans); n > 0 && at-spans[n-1].to <= 2*context {
			spans[n-1].to = at + 1
			continue
		}
		from := at - context
		if from < 0 {
			from = 0
		}
		spans = append(spans, span{from: from, to: at + 1})
	}
	for i := range spans {
		if to := spans[i].to + context; to < len(ops) {
			spans[i].to = to
		} else {
			spans[i].to = len(ops)
		}
	}

	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", oldName, newName)
	oldLine, newLine, pos := 1, 1, 0
	for _, s := range spans {
		for pos < s.from {
			if ops[pos].kind == ' ' {
				oldLine++
				newLine++
			}
			pos++
		}
		oldCount, newCount := 0, 0
		for _, op := range ops[s.from:s.to] {
			if op.kind != '+' {
				oldCount++
			}
			if op.kind != '-' {
				newCount++
			}
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", oldLine, oldCount, newLine, newCount)
		for _, op := range ops[s.from:s.to] {
			out.WriteByte(op.kind)
			out.WriteString(op.line)
			out.WriteByte('\n')
			if op.kind != '+' {
				oldLine++
			}
			if op.kind != '-' {
				newLine++
			}
		}
		pos = s.to
	}
	return out.String()
}

type diffOp struct {
	kind byte
	line string
}

func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if last := len(lines) - 1; lines[last] == "" {
		lines = lines[:last]
	}
	return lines
}

// lineOps computes a longest-common-subsequence diff.
func lineOps(oldLines, newLines []string) []diffOp {
	n, m := len(oldLines), len(newLines)
	table := make([][]int32, n+1)
	for i := range table {
		table[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case oldLines[i] == newLines[j]:
				table[i][j] = table[i+1][j+1] + 1
			case table[i+1][j] >= table[i][j+1]:
				table[i][j] = table[i+1][j]
			default:
				table[i][j] = table[i][j+1]
			}
		}
	}

	ops := make([]diffOp, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case oldLines[i] == newLines[j]:
			ops = append(ops, diffOp{kind: ' ', line: oldLines[i]})
			i++
			j++
		case table[i+1][j] >= table[i][j+1]:
			ops = append(ops, diffOp{kind: '-', line: oldLines[i]})
			i++
		default:
			ops = append(ops, diffOp{kind: '+', line: newLines[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{kind: '-', line: oldLines[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{kind: '+', line: newLines[j]})
	}
	return ops
}
