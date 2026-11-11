package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/hayden/mayfly/internal/sessioncfg"
)

// Manager owns every MCP client for one session.
type Manager struct {
	clients map[string]*Client
	tools   []Tool
	byName  map[string]Tool
	errs    []error
	closed  bool
}

// StartServer connects to one configured MCP server and lists its tools.
func StartServer(ctx context.Context, def sessioncfg.MCPServer, token string, timeout time.Duration, logger *slog.Logger) (*Client, []Tool, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var client *Client
	switch def.Transport {
	case "stdio":
		c, err := NewStdio(ctx, def.Name, def.Command, def.Args, def.Env)
		if err != nil {
			return nil, nil, err
		}
		client = c
	case "http":
		client = NewHTTP(def.Name, def.URL, token, def.Headers)
	default:
		return nil, nil, fmt.Errorf("unsupported transport %q", def.Transport)
	}
	if err := client.Initialize(ctx); err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	logger.Info("mcp server ready", "server", def.Name, "transport", def.Transport, "tools", len(tools))
	return client, tools, nil
}

// Start connects all servers sequentially. Individual failures do not abort
// the session; they are reported through Errors.
func Start(ctx context.Context, defs []sessioncfg.MCPServer, token string, logger *slog.Logger) *Manager {
	m := &Manager{
		clients: map[string]*Client{},
		byName:  map[string]Tool{},
	}
	used := map[string]int{}
	for _, def := range defs {
		timeout := 30 * time.Second
		if def.InitTimeoutSeconds > 0 {
			timeout = time.Duration(def.InitTimeoutSeconds) * time.Second
		}
		client, tools, err := StartServer(ctx, def, token, timeout, logger)
		if err != nil {
			logger.Warn("mcp server unavailable", "server", def.Name, "error", err)
			m.errs = append(m.errs, fmt.Errorf("%s: %w", def.Name, err))
			continue
		}
		m.clients[def.Name] = client
		for _, tool := range tools {
			remote := tool.Name
			name := toolName(def.Name, remote)
			if n, ok := used[name]; ok {
				used[name] = n + 1
				name = fmt.Sprintf("%s_%d", name, n+1)
			} else {
				used[name] = 0
			}
			tool.Name = name
			tool.RemoteName = remote
			tool.Server = def.Name
			m.byName[name] = tool
			m.tools = append(m.tools, tool)
		}
	}
	sort.Slice(m.tools, func(i, j int) bool { return m.tools[i].Name < m.tools[j].Name })
	return m
}

// Tools returns all tools with globally unique names.
func (m *Manager) Tools() []Tool {
	out := make([]Tool, len(m.tools))
	copy(out, m.tools)
	return out
}

// Errors returns startup failures.
func (m *Manager) Errors() []error { return m.errs }

// Call invokes a tool by its namespaced name.
func (m *Manager) Call(ctx context.Context, name string, args json.RawMessage) (string, bool, error) {
	tool, ok := m.byName[name]
	if !ok {
		return "", false, fmt.Errorf("unknown MCP tool %q", name)
	}
	client, ok := m.clients[tool.Server]
	if !ok {
		return "", false, fmt.Errorf("MCP server %q is not connected", tool.Server)
	}
	return client.CallTool(ctx, tool.RemoteName, args)
}

// Close shuts down every client.
func (m *Manager) Close() {
	if m.closed {
		return
	}
	m.closed = true
	for _, client := range m.clients {
		_ = client.Close()
	}
}

func toolName(server, tool string) string {
	return "mcp_" + sanitize(server) + "_" + sanitize(tool)
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "unnamed"
	}
	return out
}
