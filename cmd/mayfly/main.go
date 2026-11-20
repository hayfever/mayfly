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
	"sync"

	"github.com/gorilla/websocket"

	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/version"
)

// commands maps every command word to its handler. surface.go lists the same
// words, and the surface test fails when the two disagree.
var commands = map[string]func([]string) error{
	"run":        cmdRun,
	"ls":         cmdList,
	"get":        cmdGet,
	"events":     cmdEvents,
	"attach":     cmdAttach,
	"send":       cmdSend,
	"close":      cmdClose,
	"delete":     cmdDelete,
	"artifact":   cmdArtifact,
	"transcript": cmdTranscript,
	"version":    cmdVersion,
	"--version":  cmdVersion,
	"-v":         cmdVersion,
	"agents":     cmdAgents,
	"plugin":     cmdPlugin,
	"help":       helpCommand,
	"-h":         helpCommand,
	"--help":     helpCommand,
}

// helpCommand prints the usage text.
func helpCommand([]string) error {
	usage()
	return nil
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	run, ok := commands[os.Args[1]]
	if !ok {
		fmt.Fprintf(os.Stderr, "mayfly: unknown command %q\n", os.Args[1])
		os.Exit(1)
	}
	if err := run(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "mayfly:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mayfly - ephemeral agent sessions

Usage:
  mayfly COMMAND [ARGUMENTS] [FLAGS]
  mayfly help

Environment:
  MAYFLY_URL        control plane URL (default http://localhost:8080)
  MAYFLY_API_TOKEN  API token from the control plane
  MAYFLY_TOKEN      deprecated alias for MAYFLY_API_TOKEN

Commands:
  version [--json]
      print the build version, commit, and date
  run --prompt TEXT [--agent NAME] [--title T] [--ttl 30m] [--model M]
      [--skill NAME]... [--mcp NAME]... [--json]
      create a session
  agents [--json]
      list the configured agents
  ls [--json]
      list sessions
  get ID [--json]
      show one session
  events ID [--after N] [--json]
      print the events of a session
  attach ID [--name NAME] [--after N] [--once] [--exit-on-eof]
      join the session thread until Ctrl-C
  send [--json] ID TEXT
      send a message to a session
  close ID [--reason R]
      close a session gracefully
  delete ID [--yes]
      force-delete a session
  artifact ID [--output FILE]
      download the session bundle (-o is an alias)
  transcript ID
      print the session transcript
  plugin init [--lang go|python|ts] DIR
      write a plugin scaffold
  plugin check DIR
      validate a manifest and start the plugin
  plugin test DIR TOOL JSON [--workspace DIR] [--key KEY] [--timeout 2m]
      check a plugin, then call one tool
  help
      print this message

Commands that print data accept --json. With --json, the command writes one
JSON document to stdout and nothing else. Errors go to stderr.
`)
}

const (
	// envURL names the control plane URL.
	envURL = "MAYFLY_URL"
	// defaultURL is the control plane URL that envURL overrides.
	defaultURL = "http://localhost:8080"
	// envToken names the API token.
	envToken = "MAYFLY_API_TOKEN"
	// envTokenAlias is the deprecated alias of envToken.
	envTokenAlias = "MAYFLY_TOKEN"
)

func baseURL() string {
	u := os.Getenv(envURL)
	if u == "" {
		u = defaultURL
	}
	return strings.TrimRight(u, "/")
}

// resolveToken reads the control-plane token. MAYFLY_TOKEN is a deprecated
// alias for MAYFLY_API_TOKEN; the alias prints one warning per process.
func resolveToken(getenv func(string) string, warn io.Writer) string {
	if value := strings.TrimSpace(getenv(envToken)); value != "" {
		return value
	}
	if value := strings.TrimSpace(getenv(envTokenAlias)); value != "" {
		fmt.Fprintf(warn, "mayfly: %s is deprecated; set %s instead\n", envTokenAlias, envToken)
		return value
	}
	return ""
}

var (
	tokenOnce  sync.Once
	tokenValue string
)

func token() string {
	tokenOnce.Do(func() { tokenValue = resolveToken(os.Getenv, os.Stderr) })
	return tokenValue
}

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

// writeJSONDocument writes one indented JSON document and one newline. The
// bytes are the API value unchanged, so a field this CLI build does not know
// still reaches the caller.
func writeJSONDocument(raw []byte) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, bytes.TrimSpace(raw), "", "  "); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	buf.WriteByte('\n')
	_, err := os.Stdout.Write(buf.Bytes())
	return err
}

// printJSON writes one constructed value in the same form as an API object.
func printJSON(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeJSONDocument(raw)
}

// printJSONResponse writes a successful response body as one JSON document.
func printJSONResponse(resp *http.Response) error {
	if resp.StatusCode >= 300 {
		return responseError(resp)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	return writeJSONDocument(raw)
}

// sessionResponse is the body of a session create or read response.
type sessionResponse struct {
	Session model.Session `json:"session"`
	URL     string        `json:"url"`
}

// versionFlags holds the flags of mayfly version.
type versionFlags struct {
	*flag.FlagSet
	asJSON *bool
}

// newVersionFlags builds the flag set that mayfly version parses.
func newVersionFlags() *versionFlags {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	return &versionFlags{FlagSet: fs, asJSON: fs.Bool("json", false, "print the version as JSON")}
}

func cmdVersion(args []string) error {
	f := newVersionFlags()
	if _, err := parseArgs(f.FlagSet, args); err != nil {
		return err
	}
	if *f.asJSON {
		return printJSON(struct {
			Version string `json:"version"`
			Commit  string `json:"commit"`
			Date    string `json:"date"`
		}{version.Version, version.Commit, version.Date})
	}
	fmt.Println(version.String())
	return nil
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

// runFlags holds the flags of mayfly run.
type runFlags struct {
	*flag.FlagSet
	prompt, agent, title, ttl, model *string
	asJSON                           *bool
	skills, mcps                     stringList
}

// newRunFlags builds the flag set that mayfly run parses.
func newRunFlags() *runFlags {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	f := &runFlags{FlagSet: fs}
	f.prompt = fs.String("prompt", "", "initial prompt")
	f.agent = fs.String("agent", "", "agent name")
	f.title = fs.String("title", "", "session title")
	f.ttl = fs.String("ttl", "", "session TTL, e.g. 30m")
	f.model = fs.String("model", "", "model name")
	f.asJSON = fs.Bool("json", false, "print the session as JSON")
	fs.Var(&f.skills, "skill", "skill name (repeatable)")
	fs.Var(&f.mcps, "mcp", "MCP server name (repeatable)")
	return f
}

func cmdRun(args []string) error {
	f := newRunFlags()
	if _, err := parseArgs(f.FlagSet, args, "prompt", "agent", "title", "ttl", "model", "skill", "mcp"); err != nil {
		return err
	}
	if *f.prompt == "" {
		return fmt.Errorf("--prompt is required")
	}
	body, _ := json.Marshal(map[string]any{
		"prompt":      *f.prompt,
		"agent":       *f.agent,
		"title":       *f.title,
		"ttl":         *f.ttl,
		"model":       *f.model,
		"skills":      f.skills,
		"mcp_servers": f.mcps,
	})
	resp, err := request(http.MethodPost, "/api/v1/sessions", body)
	if err != nil {
		return err
	}
	if *f.asJSON {
		return printJSONResponse(resp)
	}
	var out sessionResponse
	if err := decodeResponse(resp, &out); err != nil {
		return err
	}
	fmt.Printf("%s  %s  %s\n", out.Session.ID, out.Session.Status, out.URL)
	return nil
}

// agentsFlags holds the flags of mayfly agents.
type agentsFlags struct {
	*flag.FlagSet
	asJSON *bool
}

// newAgentsFlags builds the flag set that mayfly agents parses.
func newAgentsFlags() *agentsFlags {
	fs := flag.NewFlagSet("agents", flag.ContinueOnError)
	return &agentsFlags{FlagSet: fs, asJSON: fs.Bool("json", false, "print the agents as JSON")}
}

func cmdAgents(args []string) error {
	f := newAgentsFlags()
	if _, err := parseArgs(f.FlagSet, args); err != nil {
		return err
	}
	resp, err := request(http.MethodGet, "/api/v1/config", nil)
	if err != nil {
		return err
	}
	var out struct {
		Agents []json.RawMessage `json:"agents"`
	}
	if err := decodeResponse(resp, &out); err != nil {
		return err
	}
	if *f.asJSON {
		return printJSON(out.Agents)
	}
	fmt.Printf("%-16s  %-8s  %-10s  %-24s  %s\n", "NAME", "DEFAULT", "HARNESS", "MODEL", "DESCRIPTION")
	for _, raw := range out.Agents {
		var agent struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Harness     string `json:"harness"`
			Model       string `json:"model"`
			Default     bool   `json:"default"`
		}
		if err := json.Unmarshal(raw, &agent); err != nil {
			return err
		}
		marker := ""
		if agent.Default {
			marker = "yes"
		}
		fmt.Printf("%-16s  %-8s  %-10s  %-24s  %s\n", agent.Name, marker, agent.Harness, agent.Model, truncate(agent.Description, 60))
	}
	return nil
}

// listFlags holds the flags of mayfly ls.
type listFlags struct {
	*flag.FlagSet
	asJSON *bool
}

// newListFlags builds the flag set that mayfly ls parses.
func newListFlags() *listFlags {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	return &listFlags{FlagSet: fs, asJSON: fs.Bool("json", false, "print the sessions as JSON")}
}

func cmdList(args []string) error {
	f := newListFlags()
	if _, err := parseArgs(f.FlagSet, args); err != nil {
		return err
	}
	sessions, err := allSessions()
	if err != nil {
		return err
	}
	if *f.asJSON {
		return printJSON(sessions)
	}
	fmt.Printf("%-12s  %-10s  %-20s  %s\n", "ID", "STATUS", "CREATED", "TITLE")
	for _, raw := range sessions {
		var s model.Session
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		fmt.Printf("%-12s  %-10s  %-20s  %s\n", s.ID, s.Status, s.CreatedAt.Local().Format("2006-01-02 15:04:05"), truncate(s.Title, 48))
	}
	return nil
}

// allSessions reads every page of the session list and returns each session
// object as the API sent it.
func allSessions() ([]json.RawMessage, error) {
	var out []json.RawMessage
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
			Items      []json.RawMessage `json:"items"`
			NextCursor string            `json:"next_cursor"`
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

// printSession writes a readable summary of one session. The text is for
// people; use --json for a stable shape.
func printSession(out sessionResponse) {
	s := out.Session
	fmt.Printf("id:        %s\n", s.ID)
	fmt.Printf("title:     %s\n", orDash(s.Title))
	fmt.Printf("status:    %s\n", s.Status)
	fmt.Printf("agent:     %s\n", orDash(s.Agent))
	fmt.Printf("harness:   %s\n", orDash(s.Harness))
	fmt.Printf("model:     %s\n", orDash(s.Model))
	fmt.Printf("trigger:   %s\n", orDash(s.Trigger.Kind))
	fmt.Printf("created:   %s\n", s.CreatedAt.Local().Format("2006-01-02 15:04:05"))
	fmt.Printf("expires:   %s\n", s.ExpiresAt.Local().Format("2006-01-02 15:04:05"))
	fmt.Printf("messages:  %d\n", s.MessageCount)
	if s.Error != "" {
		fmt.Printf("error:     %s\n", s.Error)
	}
	fmt.Printf("url:       %s\n", out.URL)
	if s.Prompt != "" {
		fmt.Printf("prompt:\n%s\n", s.Prompt)
	}
}

// getFlags holds the flags of mayfly get.
type getFlags struct {
	*flag.FlagSet
	asJSON *bool
}

// newGetFlags builds the flag set that mayfly get parses.
func newGetFlags() *getFlags {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	return &getFlags{FlagSet: fs, asJSON: fs.Bool("json", false, "print the session as JSON")}
}

func cmdGet(args []string) error {
	f := newGetFlags()
	positional, err := parseArgs(f.FlagSet, args)
	if err != nil {
		return err
	}
	if len(positional) < 1 {
		return fmt.Errorf("usage: mayfly get ID")
	}
	resp, err := request(http.MethodGet, "/api/v1/sessions/"+url.PathEscape(positional[0]), nil)
	if err != nil {
		return err
	}
	if *f.asJSON {
		return printJSONResponse(resp)
	}
	var out sessionResponse
	if err := decodeResponse(resp, &out); err != nil {
		return err
	}
	printSession(out)
	return nil
}

// eventsFlags holds the flags of mayfly events.
type eventsFlags struct {
	*flag.FlagSet
	after  *int64
	asJSON *bool
}

// newEventsFlags builds the flag set that mayfly events parses.
func newEventsFlags() *eventsFlags {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	f := &eventsFlags{FlagSet: fs}
	f.after = fs.Int64("after", 0, "start after sequence")
	f.asJSON = fs.Bool("json", false, "print the events as JSON")
	return f
}

func cmdEvents(args []string) error {
	f := newEventsFlags()
	positional, err := parseArgs(f.FlagSet, args, "after")
	if err != nil {
		return err
	}
	if len(positional) < 1 {
		return fmt.Errorf("usage: mayfly events ID")
	}
	path := fmt.Sprintf("/api/v1/sessions/%s/events?after=%d", url.PathEscape(positional[0]), *f.after)
	resp, err := request(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	var out struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := decodeResponse(resp, &out); err != nil {
		return err
	}
	if *f.asJSON {
		return printJSON(out.Events)
	}
	for _, raw := range out.Events {
		var ev model.Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			return err
		}
		printEvent(ev)
	}
	return nil
}

// sendFlags holds the flags of mayfly send.
type sendFlags struct {
	*flag.FlagSet
	asJSON *bool
}

// newSendFlags builds the flag set that mayfly send parses.
func newSendFlags() *sendFlags {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	return &sendFlags{FlagSet: fs, asJSON: fs.Bool("json", false, "print the event as JSON")}
}

func cmdSend(args []string) error {
	f := newSendFlags()
	// Flags go before TEXT, either before or after ID. Everything from the
	// first TEXT word on is the message, sent as typed, so a message such as
	// "rerun with --force" keeps its dashes.
	if err := f.Parse(args); err != nil {
		return err
	}
	rest := f.Args()
	if len(rest) < 1 {
		return fmt.Errorf("usage: mayfly send [--json] ID TEXT")
	}
	id := rest[0]
	if err := f.Parse(rest[1:]); err != nil {
		return err
	}
	text := f.Args()
	if len(text) < 1 {
		return fmt.Errorf("usage: mayfly send [--json] ID TEXT")
	}
	body, _ := json.Marshal(map[string]string{"text": strings.Join(text, " ")})
	resp, err := request(http.MethodPost, "/api/v1/sessions/"+url.PathEscape(id)+"/messages", body)
	if err != nil {
		return err
	}
	if *f.asJSON {
		return printJSONResponse(resp)
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

// closeFlags holds the flags of mayfly close.
type closeFlags struct {
	*flag.FlagSet
	reason *string
}

// newCloseFlags builds the flag set that mayfly close parses.
func newCloseFlags() *closeFlags {
	fs := flag.NewFlagSet("close", flag.ContinueOnError)
	return &closeFlags{FlagSet: fs, reason: fs.String("reason", "closed", "close reason")}
}

func cmdClose(args []string) error {
	f := newCloseFlags()
	positional, err := parseArgs(f.FlagSet, args, "reason")
	if err != nil {
		return err
	}
	if len(positional) < 1 {
		return fmt.Errorf("usage: mayfly close ID")
	}
	body, _ := json.Marshal(map[string]string{"reason": *f.reason})
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

// deleteFlags holds the flags of mayfly delete.
type deleteFlags struct {
	*flag.FlagSet
	yes *bool
}

// newDeleteFlags builds the flag set that mayfly delete parses.
func newDeleteFlags() *deleteFlags {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	return &deleteFlags{FlagSet: fs, yes: fs.Bool("yes", false, "do not ask for confirmation")}
}

func cmdDelete(args []string) error {
	f := newDeleteFlags()
	positional, err := parseArgs(f.FlagSet, args)
	if err != nil {
		return err
	}
	if len(positional) < 1 {
		return fmt.Errorf("usage: mayfly delete ID")
	}
	id := positional[0]
	if !*f.yes {
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

// artifactFlags holds the flags of mayfly artifact. The o flag is a second
// name for output and shares its value.
type artifactFlags struct {
	*flag.FlagSet
	output *string
}

// newArtifactFlags builds the flag set that mayfly artifact parses.
func newArtifactFlags() *artifactFlags {
	fs := flag.NewFlagSet("artifact", flag.ContinueOnError)
	f := &artifactFlags{FlagSet: fs}
	f.output = fs.String("output", "", "output file (default stdout)")
	fs.StringVar(f.output, "o", "", "alias for --output")
	return f
}

func cmdArtifact(args []string) error {
	f := newArtifactFlags()
	positional, err := parseArgs(f.FlagSet, args, "output", "o")
	if err != nil {
		return err
	}
	if len(positional) < 1 {
		return fmt.Errorf("usage: mayfly artifact ID [--output FILE]")
	}
	body, err := openArtifact(positional[0])
	if err != nil {
		return err
	}
	defer body.Close()
	if *f.output == "" {
		_, err = io.Copy(os.Stdout, body)
		return err
	}
	file, err := os.Create(*f.output)
	if err != nil {
		return err
	}
	defer file.Close()
	n, err := io.Copy(file, body)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %d bytes to %s\n", n, *f.output)
	return nil
}

// attachFlags holds the flags of mayfly attach.
type attachFlags struct {
	*flag.FlagSet
	name      *string
	after     *int64
	once      *bool
	exitOnEOF *bool
}

// newAttachFlags builds the flag set that mayfly attach parses.
func newAttachFlags() *attachFlags {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	f := &attachFlags{FlagSet: fs}
	f.name = fs.String("name", "cli", "display name")
	f.after = fs.Int64("after", 0, "replay events after sequence")
	f.once = fs.Bool("once", false, "exit after the first assistant reply")
	f.exitOnEOF = fs.Bool("exit-on-eof", false, "exit when stdin closes")
	return f
}

func cmdAttach(args []string) error {
	f := newAttachFlags()
	positional, err := parseArgs(f.FlagSet, args, "name", "after")
	if err != nil {
		return err
	}
	if len(positional) < 1 {
		return fmt.Errorf("usage: mayfly attach ID")
	}
	id := positional[0]
	body, _ := json.Marshal(map[string]string{"actor": *f.name})
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
	q.Set("after", fmt.Sprint(*f.after))
	u.RawQuery = q.Encode()
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	fmt.Printf("attached to %s as %s; Ctrl-C to detach\n", id, *f.name)
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
		if *f.exitOnEOF {
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
			if *f.once && msg.Event.Type == model.EventAssistantMessage {
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
		call, _ := model.DataAs[model.ToolCallData](ev.Data)
		fmt.Printf("[tool] %s %s\n", call.Tool, call.Arguments)
	case model.EventToolResult:
		result, _ := model.DataAs[model.ToolResultData](ev.Data)
		fmt.Printf("[result] %s\n%s\n", result.Tool, ev.Text)
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
