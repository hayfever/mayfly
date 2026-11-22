package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hayfever/mayfly/internal/prompt"
	"github.com/hayfever/mayfly/internal/textutil"
)

const (
	maxResponseBytes = 32 << 20
	maxErrorBytes    = 8 << 10
)

// httpJSON sends HTTP requests and projects the JSON responses into documents.
type httpJSON struct {
	def    Definition
	conn   Connection
	client *http.Client

	path    *textTemplate
	query   map[string]*textTemplate
	headers map[string]*textTemplate
	body    *valueNode
	page    *valueNode
	fields  []compiledField

	attempts    int
	delay       time.Duration
	maxItems    int
	maxPages    int
	concurrency int
	timeout     time.Duration
}

func newHTTPJSON(def Definition, conn Connection, opts Options) (Source, error) {
	s := &httpJSON{
		def:         def,
		conn:        conn,
		fields:      make([]compiledField, 0, len(def.Extract.Fields)),
		attempts:    def.Retry.Attempts,
		maxItems:    def.Limits.MaxItems,
		maxPages:    1,
		concurrency: opts.Concurrency,
		timeout:     opts.Timeout,
	}
	env := TemplateEnv(def, conn, opts.DeniedEnv)
	if s.attempts <= 0 {
		s.attempts = 3
	}
	delay, err := parseDuration(def.Retry.Delay, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("source %q: retry.delay: %w", def.Name, err)
	}
	s.delay = delay
	if s.maxItems <= 0 {
		s.maxItems = 100
	}
	if s.concurrency <= 0 {
		s.concurrency = 1
	}
	if def.Paginate != nil {
		s.maxPages = def.Paginate.MaxPages
		if s.maxPages <= 0 {
			s.maxPages = 10
		}
		if strings.TrimSpace(def.Paginate.MorePath) == "" || strings.TrimSpace(def.Paginate.CursorPath) == "" {
			return nil, fmt.Errorf("source %q: paginate.morePath and paginate.cursorPath are required", def.Name)
		}
		page, err := compileBody(def.Name, "paginate.body", def.Paginate.Body, env)
		if err != nil {
			return nil, err
		}
		s.page = page
	}
	if def.Request.Method == "" {
		def.Request.Method = http.MethodPost
	}
	s.def.Request.Method = strings.ToUpper(def.Request.Method)
	switch s.def.Request.Method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return nil, fmt.Errorf("source %q: unsupported request method %q", def.Name, def.Request.Method)
	}
	s.path, err = parseTemplate(def.Name+".request.path", def.Request.Path, env)
	if err != nil {
		return nil, err
	}
	if s.query, err = compileTemplates(def.Name+".request.query", def.Request.Query, env); err != nil {
		return nil, err
	}
	if s.headers, err = compileTemplates(def.Name+".request.headers", def.Request.Headers, env); err != nil {
		return nil, err
	}
	if s.body, err = compileBody(def.Name, "request.body", def.Request.Body, env); err != nil {
		return nil, err
	}
	for name, field := range def.Extract.Fields {
		compiled, err := compileField(name, field, def.Name+".extract.fields")
		if err != nil {
			return nil, err
		}
		s.fields = append(s.fields, compiled)
	}
	s.client, err = newHTTPClient(conn)
	if err != nil {
		return nil, fmt.Errorf("source %q: connection %q: %w", def.Name, conn.Name, err)
	}
	return s, nil
}

func compileBody(name, where string, body map[string]any, env prompt.EnvPolicy) (*valueNode, error) {
	if len(body) == 0 {
		return nil, nil
	}
	node, err := compileValue(name+"."+where, body, env)
	if err != nil {
		return nil, err
	}
	return &node, nil
}

// Name implements Source.
func (s *httpJSON) Name() string { return s.def.Name }

// Run implements Source. sources.timeout bounds the step. A fanout step that
// reaches it keeps the pages already fetched and reports the rest as item
// errors, so a provider that already consumed a change does not lose it.
func (s *httpJSON) Run(ctx context.Context, in Input) ([]Document, error) {
	stepCtx := ctx
	if s.timeout > 0 {
		var cancel context.CancelFunc
		stepCtx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}
	if s.def.Fanout != nil {
		return s.runFanout(ctx, stepCtx, in)
	}
	return s.runList(stepCtx, in)
}

func (s *httpJSON) runList(ctx context.Context, in Input) ([]Document, error) {
	items, err := s.fetch(ctx, in, nil)
	if err != nil {
		return nil, err
	}
	documents := make([]Document, 0, len(items))
	for _, item := range items {
		document := s.document(item, nil, "", "")
		if document == nil {
			continue
		}
		documents = append(documents, *document)
	}
	// The limit counts the documents the filter kept, not raw rows.
	if len(documents) > s.maxItems {
		return nil, fmt.Errorf("%d documents is over limits.maxItems=%d", len(documents), s.maxItems)
	}
	return documents, nil
}

type fanoutJob struct {
	parent  Document
	field   string
	url     string
	problem string
}

// fanoutTarget is one distinct request and the fanout jobs that share it.
type fanoutTarget struct {
	request preparedRequest
	jobs    []int
}

// ErrAuth reports a 401 or 403 from a provider. The credential is wrong for
// every item, so the run fails instead of turning each item into an error.
var ErrAuth = errors.New("provider rejected the credentials")

func (s *httpJSON) runFanout(parent, ctx context.Context, in Input) ([]Document, error) {
	parents := in.Deps[s.def.Fanout.From]
	jobs := make([]fanoutJob, 0, len(parents))
	for _, parentDoc := range parents {
		for _, field := range s.def.Fanout.URLs {
			raw, ok := parentDoc.Fields[field]
			if !ok {
				continue
			}
			target := strings.TrimSpace(Text(raw))
			if target == "" {
				continue
			}
			job := fanoutJob{parent: parentDoc, field: field, url: target}
			if !isHTTPURL(target) {
				job.problem = "not an HTTP URL"
			}
			jobs = append(jobs, job)
		}
	}
	if len(jobs) > s.maxItems {
		return nil, fmt.Errorf("%d documents is over limits.maxItems=%d", len(jobs), s.maxItems)
	}
	out := make([]Document, len(jobs))
	// Jobs that render the same request share one call, so a page listed
	// twice is fetched once and every listing gets the same change status.
	targets := make([]*fanoutTarget, 0, len(jobs))
	byKey := map[string]*fanoutTarget{}
	for index := range jobs {
		job := jobs[index]
		if job.problem != "" {
			out[index] = s.errorDocument(job, job.problem)
			continue
		}
		data := s.data(in)
		data["doc"] = job.parent.Fields
		data["url"] = job.url
		data["field"] = job.field
		body, err := s.renderBody(data, false)
		if err != nil {
			out[index] = s.errorDocument(job, errorText(err))
			continue
		}
		request, err := s.prepare(data, body)
		if err != nil {
			out[index] = s.errorDocument(job, errorText(err))
			continue
		}
		key := request.key()
		if target, ok := byKey[key]; ok {
			target.jobs = append(target.jobs, index)
			continue
		}
		target := &fanoutTarget{request: request, jobs: []int{index}}
		byKey[key] = target
		targets = append(targets, target)
	}

	fanCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var authOnce sync.Once
	var authErr error
	var wg sync.WaitGroup
	sem := make(chan struct{}, s.concurrency)
	for _, target := range targets {
		wg.Add(1)
		go func(target *fanoutTarget) {
			defer wg.Done()
			fail := func(message string) {
				for _, index := range target.jobs {
					out[index] = s.errorDocument(jobs[index], message)
				}
			}
			defer s.recoverFanoutTarget(fail)
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-fanCtx.Done():
				fail(fanCtx.Err().Error())
				return
			}
			response, err := s.do(fanCtx, target.request)
			if err != nil {
				var status *statusError
				if errors.As(err, &status) && (status.Status == http.StatusUnauthorized || status.Status == http.StatusForbidden) {
					authOnce.Do(func() {
						authErr = fmt.Errorf("%w: HTTP %d from connection %q: %s", ErrAuth, status.Status, s.conn.Name, status.Message)
						cancel()
					})
				}
				fail(errorText(err))
				return
			}
			items, err := s.items(response)
			if err != nil {
				fail(errorText(err))
				return
			}
			if len(items) == 0 {
				fail("provider returned no document")
				return
			}
			for _, index := range target.jobs {
				job := jobs[index]
				if document := s.document(items[0], &job.parent, job.field, job.url); document != nil {
					out[index] = *document
				}
				// A document the filter rejects leaves out[index] zero.
			}
		}(target)
	}
	wg.Wait()
	if authErr != nil {
		return nil, authErr
	}
	// Only the job's own context aborts the run. The step deadline leaves
	// the unfinished pages as item errors and keeps the finished ones.
	if err := parent.Err(); err != nil {
		return nil, err
	}
	documents := make([]Document, 0, len(out))
	for _, document := range out {
		if document.Source == "" {
			continue
		}
		documents = append(documents, document)
	}
	return documents, nil
}

// data is the template data for one request.
func (s *httpJSON) data(in Input) map[string]any {
	return map[string]any{
		"runID":    in.RunID,
		"schedule": in.Schedule,
		"source":   s.def.Name,
		"slot":     in.Slot.UTC(),
		"now":      in.Now.UTC(),
		"cursor":   "",
	}
}

// fetch returns the items of one request, following pagination when
// configured.
func (s *httpJSON) fetch(ctx context.Context, in Input, data map[string]any) ([]any, error) {
	if data == nil {
		data = s.data(in)
	}
	var items []any
	cursor := ""
	for pageNumber := 1; ; pageNumber++ {
		// Running out of pages fails the run rather than dropping the
		// rest of the list without a word.
		if pageNumber > s.maxPages {
			return nil, fmt.Errorf("response has more than paginate.maxPages=%d pages", s.maxPages)
		}
		data["cursor"] = cursor
		body, err := s.renderBody(data, pageNumber > 1)
		if err != nil {
			return nil, err
		}
		request, err := s.prepare(data, body)
		if err != nil {
			return nil, err
		}
		response, err := s.do(ctx, request)
		if err != nil {
			return nil, err
		}
		pageItems, err := s.items(response)
		if err != nil {
			return nil, err
		}
		items = append(items, pageItems...)
		if s.def.Paginate == nil {
			break
		}
		more, _ := s.lookupBool(response, s.def.Paginate.MorePath)
		next := s.lookupString(response, s.def.Paginate.CursorPath)
		if !more || next == "" {
			break
		}
		cursor = next
	}
	return items, nil
}

func (s *httpJSON) items(response any) ([]any, error) {
	if s.def.Extract.List == "" {
		return []any{response}, nil
	}
	segments, err := parsePath(s.def.Extract.List)
	if err != nil {
		return nil, fmt.Errorf("source %q: extract.list: %w", s.def.Name, err)
	}
	value, ok := lookupSegments(response, segments)
	if !ok {
		return nil, fmt.Errorf("source %q: response has no %s", s.def.Name, s.def.Extract.List)
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("source %q: %s is not an array", s.def.Name, s.def.Extract.List)
	}
	return items, nil
}

func (s *httpJSON) renderBody(data map[string]any, withCursor bool) (map[string]any, error) {
	var body map[string]any
	if s.body != nil {
		rendered, err := s.body.render(data)
		if err != nil {
			return nil, err
		}
		body, _ = rendered.(map[string]any)
	}
	if withCursor && s.page != nil {
		rendered, err := s.page.render(data)
		if err != nil {
			return nil, err
		}
		extra, _ := rendered.(map[string]any)
		if body == nil {
			body = map[string]any{}
		}
		for key, value := range extra {
			body[key] = value
		}
	}
	return body, nil
}

// preparedRequest is one rendered request, ready to send.
type preparedRequest struct {
	url     string
	headers http.Header
	payload []byte
}

// key identifies the request for fanout dedupe.
func (r preparedRequest) key() string {
	names := make([]string, 0, len(r.headers))
	for name := range r.headers {
		names = append(names, name)
	}
	sortStrings(names)
	var b strings.Builder
	b.WriteString(r.url)
	b.WriteByte(0)
	for _, name := range names {
		b.WriteString(name)
		b.WriteByte('=')
		b.WriteString(strings.Join(r.headers[name], ","))
		b.WriteByte(0)
	}
	b.Write(r.payload)
	return b.String()
}

// prepare renders one request.
func (s *httpJSON) prepare(data map[string]any, body map[string]any) (preparedRequest, error) {
	path, err := s.path.Render(data)
	if err != nil {
		return preparedRequest{}, err
	}
	target := strings.TrimRight(s.conn.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
	parsed, err := url.Parse(target)
	if err != nil {
		return preparedRequest{}, fmt.Errorf("request URL %q: %w", target, err)
	}
	query := parsed.Query()
	for key, tmpl := range s.query {
		value, err := tmpl.Render(data)
		if err != nil {
			return preparedRequest{}, err
		}
		if value != "" {
			query.Set(key, value)
		}
	}
	parsed.RawQuery = query.Encode()

	var payload []byte
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return preparedRequest{}, fmt.Errorf("request body: %w", err)
		}
	}
	headers := make(http.Header, len(s.conn.ResolvedHeaders)+len(s.headers)+2)
	for key, value := range s.conn.ResolvedHeaders {
		headers.Set(key, value)
	}
	for key, tmpl := range s.headers {
		value, err := tmpl.Render(data)
		if err != nil {
			return preparedRequest{}, err
		}
		headers.Set(key, value)
	}
	if payload != nil && headers.Get("Content-Type") == "" {
		headers.Set("Content-Type", "application/json")
	}
	if headers.Get("Accept") == "" {
		headers.Set("Accept", "application/json")
	}

	return preparedRequest{url: parsed.String(), headers: headers, payload: payload}, nil
}

// do sends a prepared request with retries.
func (s *httpJSON) do(ctx context.Context, prepared preparedRequest) (any, error) {
	var lastErr error
	for attempt := 1; attempt <= s.attempts; attempt++ {
		request, err := http.NewRequestWithContext(ctx, s.def.Request.Method, prepared.url, bytes.NewReader(prepared.payload))
		if err != nil {
			return nil, err
		}
		request.Header = prepared.headers.Clone()
		response, err := s.client.Do(request)
		if err != nil {
			lastErr = transportError(s.def.Request.Method, err)
			if !sleepAttempt(ctx, attempt, s.attempts, s.delay) {
				return nil, lastErr
			}
			continue
		}
		status := response.StatusCode
		raw, readErr := readResponse(response, maxResponseBytes)
		_ = response.Body.Close()
		if readErr != nil {
			lastErr = readErr
			if !sleepAttempt(ctx, attempt, s.attempts, s.delay) {
				return nil, lastErr
			}
			continue
		}
		if status >= 200 && status < 300 {
			var decoded any
			if len(raw) == 0 {
				return nil, nil
			}
			if err := json.Unmarshal(raw, &decoded); err != nil {
				return nil, fmt.Errorf("decode response: %w", err)
			}
			return decoded, nil
		}
		lastErr = &statusError{Status: status, Message: s.failureMessage(raw, status)}
		if (status == http.StatusTooManyRequests || status >= 500) && attempt < s.attempts {
			sleepAttempt(ctx, attempt, s.attempts, s.delay)
			continue
		}
		return nil, lastErr
	}
	if lastErr == nil {
		lastErr = errors.New("request failed")
	}
	return nil, lastErr
}

func sleepAttempt(ctx context.Context, attempt, attempts int, delay time.Duration) bool {
	if attempt >= attempts {
		return false
	}
	timer := time.NewTimer(time.Duration(attempt) * delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func readResponse(response *http.Response, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("response is over %d bytes", limit)
	}
	return raw, nil
}

type statusError struct {
	Status  int
	Message string
}

func (e *statusError) Error() string { return e.Message }

// failureMessage mirrors the collector: the provider's error field when it
// has one, otherwise the HTTP status.
func (s *httpJSON) failureMessage(raw []byte, status int) string {
	message := ""
	if len(raw) > 0 {
		var decoded any
		if json.Unmarshal(raw, &decoded) == nil {
			if path := s.errorPath(); path != "" {
				message = s.lookupText(decoded, path)
			}
		}
	}
	if message == "" {
		message = strings.TrimSpace(string(raw))
	}
	if message == "" {
		message = "HTTP " + strconv.Itoa(status)
	}
	return truncate(message, 300)
}

func (s *httpJSON) errorPath() string {
	if s.def.Change != nil && s.def.Change.ErrorPath != "" {
		return s.def.Change.ErrorPath
	}
	return "error"
}

// document projects one response. A nil result means the filter rejected it.
func (s *httpJSON) document(response any, parent *Document, label, target string) *Document {
	fields := make(map[string]any, len(s.fields)+4)
	if parent != nil && parent.Fields != nil {
		for key, value := range parent.Fields {
			fields[key] = value
		}
	}
	for _, field := range s.fields {
		if value, ok := field.value(response); ok {
			fields[field.name] = value
		}
	}
	if target != "" {
		fields["url"] = target
	}
	if label != "" {
		fields["label"] = label
	}
	if !s.def.Filter.match(fields) {
		return nil
	}
	document := Document{
		Source: s.def.Name,
		Name:   Text(fields["name"]),
		Label:  label,
		URL:    target,
		Fields: fields,
	}
	if document.URL == "" {
		document.URL = Text(fields["url"])
	}
	if s.def.Change == nil {
		document.Status = "document"
		return &document
	}
	status := strings.ToLower(strings.TrimSpace(s.lookupText(response, s.def.Change.StatusPath)))
	switch status {
	case "new", "same":
		document.Status = status
		return &document
	case "changed", "removed":
		diff := s.lookupText(response, s.def.Change.DiffPath)
		// A removed page may come without a diff; it still counts as a
		// change. A changed page without a diff is a provider error.
		if strings.TrimSpace(diff) == "" && status == "changed" {
			document.Status = "error"
			document.Error = "change status is changed but diff is empty"
			return &document
		}
		document.Status = status
		document.Diff = truncateDiff(diff, s.changeLines(), s.changeChars())
		return &document
	default:
		document.Status = "error"
		if message := s.lookupText(response, s.errorPath()); message != "" {
			document.Error = truncate(message, 300)
		} else if status == "" {
			document.Error = "response has no change status"
		} else {
			document.Error = truncate(fmt.Sprintf("unknown change status %q", status), 300)
		}
		return &document
	}
}

// recoverFanoutTarget turns a panic in one fan-out projection into item errors
// for that target's jobs. The worker that runs the source step recovers only
// its own goroutine, so without this recover a malformed upstream page would
// end the control-plane process. The panic value is part of the error so the
// dead-letter record names the cause; the stack is not exposed in run output.
func (s *httpJSON) recoverFanoutTarget(fail func(string)) {
	if value := recover(); value != nil {
		fail(fmt.Sprintf("source %q: internal error: %v", s.def.Name, value))
	}
}

func (s *httpJSON) errorDocument(job fanoutJob, message string) Document {
	fields := make(map[string]any, len(job.parent.Fields)+2)
	for key, value := range job.parent.Fields {
		fields[key] = value
	}
	fields["url"] = job.url
	fields["label"] = job.field
	return Document{
		Source: s.def.Name,
		Name:   job.parent.Name,
		Label:  job.field,
		URL:    job.url,
		Status: "error",
		Error:  truncate(message, 300),
		Fields: fields,
	}
}

func (s *httpJSON) changeLines() int {
	if s.def.Change.MaxLines > 0 {
		return s.def.Change.MaxLines
	}
	return 40
}

func (s *httpJSON) changeChars() int {
	if s.def.Change.MaxLineChars > 0 {
		return s.def.Change.MaxLineChars
	}
	return 300
}

func (s *httpJSON) lookupText(response any, path string) string {
	segments, err := parsePath(path)
	if err != nil {
		return ""
	}
	value, ok := lookupSegments(response, segments)
	if !ok {
		return ""
	}
	return Text(value)
}

func (s *httpJSON) lookupString(response any, path string) string {
	return strings.TrimSpace(s.lookupText(response, path))
}

func (s *httpJSON) lookupBool(response any, path string) (bool, bool) {
	segments, err := parsePath(path)
	if err != nil {
		return false, false
	}
	value, ok := lookupSegments(response, segments)
	if !ok {
		return false, false
	}
	flag, ok := value.(bool)
	return flag, ok
}

func truncateDiff(diff string, lines, chars int) string {
	parts := strings.Split(diff, "\n")
	if len(parts) > lines {
		parts = parts[:lines]
	}
	for i, line := range parts {
		parts[i] = truncate(line, chars)
	}
	return strings.Join(parts, "\n")
}

// truncate cuts value to at most limit bytes without splitting a UTF-8
// sequence.
func truncate(value string, limit int) string {
	return textutil.Truncate(value, limit)
}

// transportError drops the request URL from a transport failure, because a
// credential in the query string would otherwise reach the run artifact.
func transportError(method string, err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s request failed: %w", method, urlErr.Err)
	}
	return err
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return truncate(err.Error(), 300)
}

func parseDuration(raw string, fallback time.Duration) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return 0, err
	}
	if parsed < 0 {
		return 0, fmt.Errorf("duration %q is negative", raw)
	}
	return parsed, nil
}

func isHTTPURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}
