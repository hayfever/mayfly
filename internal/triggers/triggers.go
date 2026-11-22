// Package triggers verifies incoming webhooks and turns their payloads into
// session creation requests.
package triggers

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/session"
	"github.com/hayfever/mayfly/internal/textutil"
)

// ErrUnauthorized means webhook verification failed.
var ErrUnauthorized = errors.New("webhook signature verification failed")

// ErrBadRequest means the payload cannot be turned into a session.
var ErrBadRequest = errors.New("invalid webhook payload")

// Result is the outcome of parsing a webhook. DeliveryKey identifies the
// sender's delivery so a retry does not create a second session.
type Result struct {
	Options     session.CreateOptions
	Challenge   map[string]any
	DeliveryKey string
}

// Data is the template context for a trigger prompt.
type Data struct {
	Trigger  string         `json:"trigger"`
	Type     string         `json:"type"`
	Event    string         `json:"event"`
	Delivery string         `json:"delivery"`
	Actor    string         `json:"actor"`
	Agent    string         `json:"agent"`
	Title    string         `json:"title"`
	Body     string         `json:"body"`
	URL      string         `json:"url"`
	Payload  map[string]any `json:"payload"`
}

// Parse verifies and parses one webhook request.
func Parse(cfg config.TriggerConfig, header http.Header, body []byte) (*Result, error) {
	result, err := parse(cfg, header, body)
	if err != nil {
		return nil, err
	}
	if result.DeliveryKey == "" {
		result.DeliveryKey = strings.TrimSpace(header.Get("Idempotency-Key"))
	}
	if result.DeliveryKey != "" {
		result.Options.Trigger.Delivery = result.DeliveryKey
	}
	return result, nil
}

func parse(cfg config.TriggerConfig, header http.Header, body []byte) (*Result, error) {
	switch cfg.Type {
	case "github":
		return parseGitHub(cfg, header, body)
	case "slack":
		return parseSlack(cfg, header, body)
	case "datadog":
		return parseDatadog(cfg, header, body)
	case "generic":
		return parseGeneric(cfg, header, body)
	default:
		return nil, fmt.Errorf("%w: unsupported trigger type %s", ErrBadRequest, cfg.Type)
	}
}

func parseGeneric(cfg config.TriggerConfig, header http.Header, body []byte) (*Result, error) {
	if !verifyHMACOrToken(cfg, header, body) {
		return nil, ErrUnauthorized
	}
	payload := map[string]any{}
	text := strings.TrimSpace(string(body))
	contentType := header.Get("Content-Type")
	if strings.Contains(contentType, "application/json") || (len(body) > 0 && body[0] == '{') {
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrBadRequest, err)
		}
	}
	opts := session.CreateOptions{
		Trigger:    model.TriggerInfo{Kind: "generic", Name: cfg.Name, Event: "webhook"},
		TTL:        cfg.TTL.Std(),
		Model:      cfg.Model,
		Agent:      cfg.Agent,
		Skills:     cfg.Skills,
		MCPServers: cfg.MCPServers,
	}
	prompt := ""
	if v, ok := payload["prompt"].(string); ok {
		prompt = v
		text = v
	}
	if prompt == "" {
		prompt = text
	}
	if v, ok := payload["title"].(string); ok {
		opts.Title = truncate(v, session.MaxTitleBytes)
	}
	if v, ok := payload["model"].(string); ok && v != "" {
		opts.Model = v
	}
	if v, ok := payload["agent"].(string); ok && v != "" {
		opts.Agent = v
	}
	if v, ok := payload["ttl"].(string); ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid ttl %q", ErrBadRequest, v)
		}
		opts.TTL = d
	}
	opts.Skills = stringSlice(payload["skills"], opts.Skills)
	opts.MCPServers = stringSlice(payload["mcp_servers"], opts.MCPServers)
	prompt, err := applyTemplate(cfg, Data{
		Trigger: cfg.Name,
		Type:    cfg.Type,
		Event:   "webhook",
		Agent:   opts.Agent,
		Body:    text,
		Payload: payload,
	}, prompt)
	if err != nil {
		return nil, err
	}
	opts.Prompt = prompt
	return &Result{Options: opts}, nil
}

func parseDatadog(cfg config.TriggerConfig, header http.Header, body []byte) (*Result, error) {
	if !verifyHMACOrToken(cfg, header, body) {
		return nil, ErrUnauthorized
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	title := str(payload, "title")
	title = truncate(title, session.MaxTitleBytes)
	text := str(payload, "text")
	alertType := str(payload, "alert_type")
	link := str(payload, "url")
	var b strings.Builder
	if title != "" {
		b.WriteString("Datadog alert: ")
		b.WriteString(title)
		b.WriteString("\n")
	}
	if alertType != "" {
		fmt.Fprintf(&b, "Type: %s\n", alertType)
	}
	if text != "" {
		b.WriteString("\n")
		b.WriteString(text)
		b.WriteString("\n")
	}
	if link != "" {
		fmt.Fprintf(&b, "\nDashboard: %s\n", link)
	}
	prompt, err := applyTemplate(cfg, Data{
		Trigger: cfg.Name,
		Type:    cfg.Type,
		Event:   alertType,
		Agent:   cfg.Agent,
		Title:   title,
		Body:    text,
		URL:     link,
		Payload: payload,
	}, strings.TrimSpace(b.String()))
	if err != nil {
		return nil, err
	}
	return &Result{Options: session.CreateOptions{
		Prompt:     prompt,
		Title:      title,
		TTL:        cfg.TTL.Std(),
		Model:      cfg.Model,
		Agent:      cfg.Agent,
		Skills:     cfg.Skills,
		MCPServers: cfg.MCPServers,
		Trigger:    model.TriggerInfo{Kind: "datadog", Name: cfg.Name, Event: alertType},
	}}, nil
}

func parseGitHub(cfg config.TriggerConfig, header http.Header, body []byte) (*Result, error) {
	if !verifyGitHub(cfg, header, body) {
		return nil, ErrUnauthorized
	}
	event := textutil.Valid(header.Get("X-GitHub-Event"))
	delivery := textutil.Valid(header.Get("X-GitHub-Delivery"))
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	summary, title, actor, link := summarizeGitHub(event, payload)
	summary = truncate(summary, 12<<10)
	title = truncate(title, session.MaxTitleBytes)
	bodyText := summary
	prompt, err := applyTemplate(cfg, Data{
		Trigger:  cfg.Name,
		Type:     cfg.Type,
		Event:    event,
		Delivery: delivery,
		Actor:    actor,
		Agent:    cfg.Agent,
		Title:    title,
		Body:     bodyText,
		URL:      link,
		Payload:  payload,
	}, summary)
	if err != nil {
		return nil, err
	}
	return &Result{Options: session.CreateOptions{
		Prompt:     prompt,
		Title:      title,
		TTL:        cfg.TTL.Std(),
		Model:      cfg.Model,
		Agent:      cfg.Agent,
		Skills:     cfg.Skills,
		MCPServers: cfg.MCPServers,
		Trigger: model.TriggerInfo{
			Kind:     "github",
			Name:     cfg.Name,
			Event:    event,
			Delivery: delivery,
			Actor:    actor,
		},
	}, DeliveryKey: delivery}, nil
}

func parseSlack(cfg config.TriggerConfig, header http.Header, body []byte) (*Result, error) {
	if !verifySlack(cfg, header, body) {
		return nil, ErrUnauthorized
	}
	contentType := header.Get("Content-Type")
	if strings.Contains(contentType, "application/json") || len(body) > 0 && body[0] == '{' {
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrBadRequest, err)
		}
		if payload["type"] == "url_verification" {
			challenge := str(payload, "challenge")
			return &Result{Challenge: map[string]any{"challenge": challenge}}, nil
		}
		return nil, fmt.Errorf("%w: unsupported Slack JSON event", ErrBadRequest)
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	text := strings.TrimSpace(values.Get("text"))
	if text == "" {
		text = values.Get("command")
	}
	user := values.Get("user_name")
	channel := values.Get("channel_name")
	summary := text
	if user != "" || channel != "" {
		summary = fmt.Sprintf("Slack request from %s in #%s: %s", user, channel, text)
	}
	prompt, err := applyTemplate(cfg, Data{
		Trigger: cfg.Name,
		Type:    cfg.Type,
		Event:   "slash_command",
		Actor:   user,
		Agent:   cfg.Agent,
		Body:    text,
		Payload: map[string]any{
			"command": values.Get("command"),
			"channel": channel,
			"user":    user,
		},
	}, summary)
	if err != nil {
		return nil, err
	}
	return &Result{Options: session.CreateOptions{
		Prompt:     prompt,
		Title:      "Slack: " + truncate(text, 60),
		TTL:        cfg.TTL.Std(),
		Model:      cfg.Model,
		Agent:      cfg.Agent,
		Skills:     cfg.Skills,
		MCPServers: cfg.MCPServers,
		Trigger:    model.TriggerInfo{Kind: "slack", Name: cfg.Name, Event: "slash_command", Actor: user},
	}, DeliveryKey: strings.TrimSpace(values.Get("trigger_id"))}, nil
}

func applyTemplate(cfg config.TriggerConfig, data Data, fallback string) (string, error) {
	if strings.TrimSpace(cfg.Template) == "" {
		return fallback, nil
	}
	tmpl, err := template.New(cfg.Name).Option("missingkey=zero").Parse(cfg.Template)
	if err != nil {
		return "", fmt.Errorf("%w: trigger template: %w", ErrBadRequest, err)
	}
	var out strings.Builder
	if err := tmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("%w: trigger template: %w", ErrBadRequest, err)
	}
	return strings.TrimSpace(out.String()), nil
}

// verifyHMACOrToken accepts a request signature or a static shared token.
func verifyHMACOrToken(cfg config.TriggerConfig, header http.Header, body []byte) bool {
	if cfg.Secret == "" {
		return cfg.AllowUnsigned
	}
	return verifySignature(cfg.Secret, header, body) || verifyToken(cfg.Secret, header)
}

// verifyGitHub accepts only a request signature. GitHub never sends the
// static token header, and a token can be replayed, so it is not accepted here.
func verifyGitHub(cfg config.TriggerConfig, header http.Header, body []byte) bool {
	if cfg.Secret == "" {
		return cfg.AllowUnsigned
	}
	return verifySignature(cfg.Secret, header, body)
}

func verifySignature(secret string, header http.Header, body []byte) bool {
	return verifyHMAC(secret, body, header.Get("X-Mayfly-Signature-256")) ||
		verifyHMAC(secret, body, header.Get("X-Hub-Signature-256"))
}

func verifyToken(secret string, header http.Header) bool {
	token := header.Get("X-Mayfly-Token")
	return token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1
}

func verifyHMAC(secret string, body []byte, header string) bool {
	if header == "" {
		return false
	}
	value := strings.TrimPrefix(header, "sha256=")
	expected, err := hex.DecodeString(value)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(expected, mac.Sum(nil))
}

func verifySlack(cfg config.TriggerConfig, header http.Header, body []byte) bool {
	if cfg.Secret == "" {
		return cfg.AllowUnsigned
	}
	ts := header.Get("X-Slack-Request-Timestamp")
	sig := header.Get("X-Slack-Signature")
	if ts == "" || sig == "" {
		return false
	}
	seconds, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if delta := time.Since(time.Unix(seconds, 0)); delta > 5*time.Minute || delta < -5*time.Minute {
		return false
	}
	mac := hmac.New(sha256.New, []byte(cfg.Secret))
	_, _ = fmt.Fprintf(mac, "v0:%s:%s", ts, body)
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(sig))
}

func summarizeGitHub(event string, payload map[string]any) (summary, title, actor, link string) {
	repo := nestedString(payload, "repository", "full_name")
	actor = nestedString(payload, "sender", "login")
	link = nestedString(payload, "repository", "html_url")
	action := str(payload, "action")
	switch event {
	case "push":
		ref := str(payload, "ref")
		summary = fmt.Sprintf("Push to %s (%s) by %s.", repo, ref, actor)
		link = str(payload, "compare")
		if commits, ok := payload["commits"].([]any); ok && len(commits) > 0 {
			summary += fmt.Sprintf("\n\nCommits: %d\n", len(commits))
			for i, c := range commits {
				if i >= 10 {
					break
				}
				commit, _ := c.(map[string]any)
				summary += fmt.Sprintf("- %s\n", str(commit, "message"))
			}
		}
		title = "GitHub push: " + repo
	case "pull_request":
		pr, _ := payload["pull_request"].(map[string]any)
		number := int(nestedNumber(payload, "number"))
		summary = fmt.Sprintf("Pull request #%d %s in %s: %s\n%s", number, action, repo, str(pr, "title"), str(pr, "html_url"))
		if body := str(pr, "body"); body != "" {
			summary += "\n\n" + truncate(body, 4000)
		}
		link = str(pr, "html_url")
		title = fmt.Sprintf("GitHub PR %s#%d", repo, number)
	case "issues":
		issue, _ := payload["issue"].(map[string]any)
		number := int(nestedNumber(payload, "issue", "number"))
		summary = fmt.Sprintf("Issue #%d %s in %s: %s\n%s", number, action, repo, str(issue, "title"), str(issue, "html_url"))
		if body := str(issue, "body"); body != "" {
			summary += "\n\n" + truncate(body, 4000)
		}
		link = str(issue, "html_url")
		title = fmt.Sprintf("GitHub issue %s#%d", repo, number)
	case "issue_comment":
		issue, _ := payload["issue"].(map[string]any)
		comment, _ := payload["comment"].(map[string]any)
		number := int(nestedNumber(payload, "issue", "number"))
		summary = fmt.Sprintf("New comment by %s on %s#%d: %s\n%s", actor, repo, number, str(issue, "title"), str(comment, "html_url"))
		if body := str(comment, "body"); body != "" {
			summary += "\n\n" + truncate(body, 4000)
		}
		link = str(comment, "html_url")
		title = fmt.Sprintf("GitHub comment %s#%d", repo, number)
	case "check_run", "workflow_run":
		target, _ := payload[event].(map[string]any)
		name := str(target, "name")
		conclusion := str(target, "conclusion")
		summary = fmt.Sprintf("%s %s completed with %s in %s.", event, name, conclusion, repo)
		link = str(target, "html_url")
		title = fmt.Sprintf("GitHub %s: %s", event, name)
	default:
		raw, _ := json.Marshal(payload)
		summary = fmt.Sprintf("GitHub event %s in %s by %s.\n\n%s", event, repo, actor, truncate(string(raw), 8000))
		title = "GitHub event: " + event
	}
	return summary, title, actor, link
}

func str(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

func nestedString(m map[string]any, path ...string) string {
	cur := m
	for i, key := range path {
		v, ok := cur[key]
		if !ok {
			return ""
		}
		if i == len(path)-1 {
			s, _ := v.(string)
			return s
		}
		next, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		cur = next
	}
	return ""
}

func nestedNumber(m map[string]any, path ...string) float64 {
	cur := m
	for i, key := range path {
		v, ok := cur[key]
		if !ok {
			return 0
		}
		if i == len(path)-1 {
			n, _ := v.(float64)
			return n
		}
		next, ok := v.(map[string]any)
		if !ok {
			return 0
		}
		cur = next
	}
	return 0
}

func stringSlice(v any, fallback []string) []string {
	list, ok := v.([]any)
	if !ok {
		return fallback
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

// truncate returns s with at most max bytes, backing up to a UTF-8 sequence
// boundary and ending a shortened value with an ellipsis. An invalid UTF-8
// byte is replaced with U+FFFD, so the result is always valid text.
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	s = textutil.Valid(s)
	if len(s) <= max {
		return s
	}
	const marker = "…"
	if max <= len(marker) {
		return textutil.Truncate(s, max)
	}
	return textutil.Truncate(s, max-len(marker)) + marker
}
