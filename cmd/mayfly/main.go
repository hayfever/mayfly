// Command mayfly is a small CLI for the Mayfly control plane.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gorilla/websocket"

	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	command := os.Args[1]
	args := os.Args[2:]
	var err error
	switch command {
	case "run":
		err = cmdRun(args)
	case "ls":
		err = cmdList(args)
	case "get":
		err = cmdGet(args)
	case "events":
		err = cmdEvents(args)
	case "attach":
		err = cmdAttach(args)
	case "send":
		err = cmdSend(args)
	case "close":
		err = cmdClose(args)
	case "delete":
		err = cmdDelete(args)
	case "artifact":
		err = cmdArtifact(args)
	case "transcript":
		err = cmdTranscript(args)
	case "version", "--version", "-v":
		fmt.Println(version.String())
		return
	case "agents":
		err = cmdAgents(args)
	case "plugin":
		err = cmdPlugin(args)
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", command)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mayfly:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mayfly - ephemeral agent sessions

Environment:
  MAYFLY_URL    control plane URL (default http://localhost:8080)
  MAYFLY_TOKEN  admin API token

Commands:
  version
  run --prompt TEXT [--agent NAME] [--title T] [--ttl 30m] [--model M] [--skill NAME]... [--mcp NAME]...
  agents
  ls
  get ID
  events ID [--after N] [--json]
  attach ID [--name NAME] [--after N] [--once] [--exit-on-eof]
  send ID TEXT
  close ID [--reason R]
  delete ID [--yes]
  artifact ID [-o FILE]
  transcript ID
  plugin init --lang go|python|ts DIR
  plugin check DIR
  plugin test DIR TOOL JSON [--workspace DIR] [--key KEY] [--timeout 2m]
`)
}

func baseURL() string {
	u := os.Getenv("MAYFLY_URL")
	if u == "" {
		u = "http://localhost:8080"
	}
	return strings.TrimRight(u, "/")
}

func token() string { return os.Getenv("MAYFLY_TOKEN") }

func request(method, path string, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, baseURL()+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token() != "" {
		req.Header.Set("Authorization", "Bearer "+token())
	}
	return http.DefaultClient.Do(req)
}

// responseError reads the API error envelope. It still accepts a plain string
// or text body so an older control plane gives a readable message.
func responseError(resp *http.Response) error {
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &env) == nil && len(env.Error) > 0 {
		var body struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(env.Error, &body) == nil && body.Message != "" {
			if body.Code != "" {
				return fmt.Errorf("%s: %s", body.Code, body.Message)
			}
			return fmt.Errorf("%s", body.Message)
		}
		var text string
		if json.Unmarshal(env.Error, &text) == nil && text != "" {
			return fmt.Errorf("%s", text)
		}
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		text = resp.Status
	}
	return fmt.Errorf("%s", text)
}

func decodeResponse(resp *http.Response, dst any) error {
	if resp.StatusCode >= 300 {
		return responseError(resp)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if dst == nil {
		return nil
	}
	return json.Unmarshal(raw, dst)
}

// parseArgs accepts flags before or after positional arguments.
func parseArgs(fs *flag.FlagSet, args []string, valueFlags ...string) ([]string, error) {
	needsValue := map[string]bool{}
	for _, name := range valueFlags {
		needsValue["-"+name] = true
		needsValue["--"+name] = true
	}
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			flags = append(flags, arg)
			if !strings.Contains(arg, "=") && needsValue[arg] && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		positional = append(positional, arg)
	}
	if err := fs.Parse(flags); err != nil {
		return nil, err
	}
	return positional, nil
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	prompt := fs.String("prompt", "", "initial prompt")
	agent := fs.String("agent", "", "agent name")
	title := fs.String("title", "", "session title")
	ttl := fs.String("ttl", "", "session TTL, e.g. 30m")
	modelName := fs.String("model", "", "model name")
	asJSON := fs.Bool("json", false, "print the session as JSON")
	var skillList, mcpList stringList
	fs.Var(&skillList, "skill", "skill name (repeatable)")
	fs.Var(&mcpList, "mcp", "MCP server name (repeatable)")
	if _, err := parseArgs(fs, args, "prompt", "agent", "title", "ttl", "model", "skill", "mcp"); err != nil {
		return err
	}
	if *prompt == "" {
		return fmt.Errorf("--prompt is required")
	}
	body, _ := json.Marshal(map[string]any{
		"prompt":      *prompt,
		"agent":       *agent,
		"title":       *title,
		"ttl":         *ttl,
		"model":       *modelName,
		"skills":      skillList,
		"mcp_servers": mcpList,
	})
	resp, err := request(http.MethodPost, "/api/v1/sessions", body)
	if err != nil {
		return err
	}
	var out struct {
		Session model.Session `json:"session"`
		URL     string        `json:"url"`
	}
	if err := decodeResponse(resp, &out); err != nil {
		return err
	}
	if *asJSON {
		raw, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(raw))
		return nil
	}
	fmt.Printf("%s  %s  %s\n", out.Session.ID, out.Session.Status, out.URL)
	return nil
}

func cmdAgents(args []string) error {
	fs := flag.NewFlagSet("agents", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	resp, err := request(http.MethodGet, "/api/v1/config", nil)
	if err != nil {
		return err
	}
	var out struct {
		Agents []struct {
			Name        string   `json:"name"`
			Description string   `json:"description"`
			Harness     string   `json:"harness"`
			Source      string   `json:"source"`
			Model       string   `json:"model"`
			Image       string   `json:"image"`
			Skills      []string `json:"skills"`
			MCPServers  []string `json:"mcp_servers"`
			Default     bool     `json:"default"`
			Builtin     bool     `json:"builtin"`
		} `json:"agents"`
	}
	if err := decodeResponse(resp, &out); err != nil {
		return err
	}
	if *asJSON {
		raw, _ := json.MarshalIndent(out.Agents, "", "  ")
		fmt.Println(string(raw))
		return nil
	}
	fmt.Printf("%-16s  %-8s  %-10s  %-24s  %s\n", "NAME", "DEFAULT", "HARNESS", "MODEL", "DESCRIPTION")
	for _, agent := range out.Agents {
		marker := ""
		if agent.Default {
			marker = "yes"
		}
		fmt.Printf("%-16s  %-8s  %-10s  %-24s  %s\n", agent.Name, marker, agent.Harness, agent.Model, truncate(agent.Description, 60))
	}
	return nil
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	sessions, err := allSessions()
	if err != nil {
		return err
	}
	if *asJSON {
		raw, _ := json.MarshalIndent(sessions, "", "  ")
		fmt.Println(string(raw))
		return nil
	}
	fmt.Printf("%-12s  %-10s  %-20s  %s\n", "ID", "STATUS", "CREATED", "TITLE")
	for _, s := range sessions {
		fmt.Printf("%-12s  %-10s  %-20s  %s\n", s.ID, s.Status, s.CreatedAt.Local().Format("2006-01-02 15:04:05"), truncate(s.Title, 48))
	}
	return nil
}

// allSessions reads every page of the session list.
func allSessions() ([]model.Session, error) {
	var out []model.Session
	cursor := ""
	for {
		path := "/api/v1/sessions"
		if cursor != "" {
			path += "?cursor=" + url.QueryEscape(cursor)
		}
		resp, err := request(http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Items      []model.Session `json:"items"`
			NextCursor string          `json:"next_cursor"`
		}
		if err := decodeResponse(resp, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

func cmdGet(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: mayfly get ID")
	}
	resp, err := request(http.MethodGet, "/api/v1/sessions/"+url.PathEscape(args[0]), nil)
	if err != nil {
		return err
	}
	var out struct {
		Session model.Session `json:"session"`
		URL     string        `json:"url"`
	}
	if err := decodeResponse(resp, &out); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(map[string]any{"session": out.Session, "url": out.URL}, "", "  ")
	fmt.Println(string(raw))
	return nil
}

func cmdEvents(args []string) error {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	after := fs.Int64("after", 0, "start after sequence")
	asJSON := fs.Bool("json", false, "print JSON")
	positional, err := parseArgs(fs, args, "after")
	if err != nil {
		return err
	}
	if len(positional) < 1 {
		return fmt.Errorf("usage: mayfly events ID")
	}
	path := fmt.Sprintf("/api/v1/sessions/%s/events?after=%d", url.PathEscape(positional[0]), *after)
	resp, err := request(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	var out struct {
		Events []model.Event `json:"events"`
	}
	if err := decodeResponse(resp, &out); err != nil {
		return err
	}
	if *asJSON {
		raw, _ := json.MarshalIndent(out.Events, "", "  ")
		fmt.Println(string(raw))
		return nil
	}
	for _, ev := range out.Events {
		printEvent(ev)
	}
	return nil
}

func cmdSend(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: mayfly send ID TEXT")
	}
	body, _ := json.Marshal(map[string]string{"text": strings.Join(args[1:], " ")})
	resp, err := request(http.MethodPost, "/api/v1/sessions/"+url.PathEscape(args[0])+"/messages", body)
	if err != nil {
		return err
	}
	var out struct {
		Event model.Event `json:"event"`
	}
	if err := decodeResponse(resp, &out); err != nil {
		return err
	}
	fmt.Printf("sent seq=%d\n", out.Event.Seq)
	return nil
}

func cmdClose(args []string) error {
	fs := flag.NewFlagSet("close", flag.ContinueOnError)
	reason := fs.String("reason", "closed", "close reason")
	positional, err := parseArgs(fs, args, "reason")
	if err != nil {
		return err
	}
	if len(positional) < 1 {
		return fmt.Errorf("usage: mayfly close ID")
	}
	body, _ := json.Marshal(map[string]string{"reason": *reason})
	resp, err := request(http.MethodPost, "/api/v1/sessions/"+url.PathEscape(positional[0])+"/close", body)
	if err != nil {
		return err
	}
	if err := decodeResponse(resp, nil); err != nil {
		return err
	}
	fmt.Println("closing")
	return nil
}

func cmdDelete(args []string) error {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 1 {
		return fmt.Errorf("usage: mayfly delete ID")
	}
	id := positional[0]
	if !*yes {
		fmt.Printf("force-delete session %s? [y/N] ", id)
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y") {
			return fmt.Errorf("aborted")
		}
	}
	resp, err := request(http.MethodDelete, "/api/v1/sessions/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	if err := decodeResponse(resp, nil); err != nil {
		return err
	}
	fmt.Println("deleted")
	return nil
}

// openArtifact starts a download of the session bundle. The caller closes it.
func openArtifact(id string) (io.ReadCloser, error) {
	resp, err := request(http.MethodGet, "/api/v1/sessions/"+url.PathEscape(id)+"/artifact", nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, responseError(resp)
	}
	return resp.Body, nil
}

func cmdArtifact(args []string) error {
	fs := flag.NewFlagSet("artifact", flag.ContinueOnError)
	out := fs.String("o", "", "output file (default stdout)")
	positional, err := parseArgs(fs, args, "o")
	if err != nil {
		return err
	}
	if len(positional) < 1 {
		return fmt.Errorf("usage: mayfly artifact ID [-o FILE]")
	}
	body, err := openArtifact(positional[0])
	if err != nil {
		return err
	}
	defer body.Close()
	if *out == "" {
		_, err = io.Copy(os.Stdout, body)
		return err
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(f, body)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %d bytes to %s\n", n, *out)
	return nil
}

func cmdAttach(args []string) error {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	name := fs.String("name", "cli", "display name")
	after := fs.Int64("after", 0, "replay events after sequence")
	once := fs.Bool("once", false, "exit after the first assistant reply")
	exitOnEOF := fs.Bool("exit-on-eof", false, "exit when stdin closes")
	positional, err := parseArgs(fs, args, "name", "after")
	if err != nil {
		return err
	}
	if len(positional) < 1 {
		return fmt.Errorf("usage: mayfly attach ID")
	}
	id := positional[0]
	body, _ := json.Marshal(map[string]string{"actor": *name})
	resp, err := request(http.MethodPost, "/api/v1/sessions/"+url.PathEscape(id)+"/attach-ticket", body)
	if err != nil {
		return err
	}
	var ticket struct {
		WSURL string `json:"ws_url"`
	}
	if err := decodeResponse(resp, &ticket); err != nil {
		return err
	}
	u, err := url.Parse(ticket.WSURL)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("after", fmt.Sprint(*after))
	u.RawQuery = q.Encode()
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	fmt.Printf("attached to %s as %s; Ctrl-C to detach\n", id, *name)
	done := make(chan struct{})
	reader := bufio.NewScanner(os.Stdin)
	go func() {
		defer close(done)
		for reader.Scan() {
			text := strings.TrimSpace(reader.Text())
			if text == "" {
				continue
			}
			_ = conn.WriteJSON(map[string]string{"type": "user_message", "text": text})
		}
		if *exitOnEOF {
			_ = conn.Close()
		}
	}()

	for {
		var msg struct {
			Type    string        `json:"type"`
			Message string        `json:"message"`
			Event   model.Event   `json:"event"`
			Session model.Session `json:"session"`
		}
		if err := conn.ReadJSON(&msg); err != nil {
			return nil
		}
		switch msg.Type {
		case "hello":
			fmt.Printf("session %s [%s] %s\n", msg.Session.ID, msg.Session.Status, msg.Session.Title)
		case "error":
			fmt.Fprintln(os.Stderr, "error:", msg.Message)
		case "event":
			if msg.Event.Type == model.EventAssistantDelta {
				continue
			}
			printEvent(msg.Event)
			if *once && msg.Event.Type == model.EventAssistantMessage {
				return nil
			}
		}
	}
}

func printEvent(ev model.Event) {
	switch ev.Type {
	case model.EventUserMessage:
		fmt.Printf("%s: %s\n", ev.Actor, ev.Text)
	case model.EventAssistantMessage:
		if ev.Text != "" {
			fmt.Println(ev.Text)
		}
	case model.EventToolCall:
		tool, _ := ev.Data["tool"].(string)
		args, _ := ev.Data["arguments"].(string)
		fmt.Printf("[tool] %s %s\n", tool, args)
	case model.EventToolResult:
		tool, _ := ev.Data["tool"].(string)
		fmt.Printf("[result] %s\n%s\n", tool, ev.Text)
	case model.EventStatus, model.EventSystem:
		fmt.Printf("-- %s\n", ev.Text)
	case model.EventError:
		fmt.Fprintf(os.Stderr, "!! %s\n", ev.Text)
	case model.EventArtifact:
		fmt.Printf("[artifact] %s\n", ev.Text)
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
