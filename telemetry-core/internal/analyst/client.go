package analyst

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// client wraps a Conn with the typed ACP method calls we need. Spec at
// github.com/zed-industries/agent-client-protocol — we use only the stable
// core: initialize, session/new, session/prompt, session/cancel.
type client struct {
	conn *Conn

	// protocolVersion is what the agent agreed during initialize.
	protocolVersion int

	mu        sync.Mutex
	sessionID string // single long-lived session for now (one per app lifetime)
}

func newClient(conn *Conn) *client {
	return &client{conn: conn}
}

type initializeParams struct {
	ProtocolVersion    int                    `json:"protocolVersion"`
	ClientCapabilities map[string]any         `json:"clientCapabilities,omitempty"`
}

type initializeResult struct {
	ProtocolVersion   int            `json:"protocolVersion"`
	AgentCapabilities map[string]any `json:"agentCapabilities,omitempty"`
	AuthMethods       []authMethod   `json:"authMethods,omitempty"`
}

type authMethod struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *client) Initialize(ctx context.Context) error {
	params := initializeParams{
		ProtocolVersion: 1,
		ClientCapabilities: map[string]any{
			"fs": map[string]any{
				"readTextFile":  true,
				"writeTextFile": true,
			},
			"terminal": false,
		},
	}
	raw, err := c.conn.Call(ctx, "initialize", params)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	var res initializeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("initialize result: %w", err)
	}
	c.protocolVersion = res.ProtocolVersion
	return nil
}

// mcpHeader is one HTTP header for an SSE-mode MCP server. opencode requires
// the headers field as an array (even if empty) per its zod schema.
type mcpHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// mcpServerEntry is the opencode ACP session/new schema for an MCP server.
// opencode validates this with zod and currently accepts two variants:
//
//   - type:"sse"   — remote MCP over Streamable HTTP. Needs url + headers.
//   - type:"stdio" — local subprocess. Needs command + args + env.
//
// Our /mcp endpoint is the Streamable HTTP transport (POST + SSE), so we
// use the "sse" variant. headers must be present even if empty.
type mcpServerEntry struct {
	Name    string      `json:"name"`
	Type    string      `json:"type"`
	URL     string      `json:"url,omitempty"`
	Headers []mcpHeader `json:"headers"`
}

type sessionNewParams struct {
	CWD        string           `json:"cwd"`
	MCPServers []mcpServerEntry `json:"mcpServers"`
}

type sessionNewResult struct {
	SessionID string `json:"sessionId"`
}

// SessionNew creates an ACP session rooted at `cwd` with the given MCP
// servers attached. Stores the returned id so subsequent SessionPrompt
// calls don't need it passed in.
func (c *client) SessionNew(ctx context.Context, cwd, mcpURL string) (string, error) {
	params := sessionNewParams{
		CWD: cwd,
		MCPServers: []mcpServerEntry{
			{
				Name:    "race-engineer",
				Type:    "sse",
				URL:     mcpURL,
				Headers: []mcpHeader{},
			},
		},
	}
	raw, err := c.conn.Call(ctx, "session/new", params)
	if err != nil {
		return "", fmt.Errorf("session/new: %w", err)
	}
	var res sessionNewResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("session/new result: %w", err)
	}
	if res.SessionID == "" {
		return "", errors.New("session/new returned empty sessionId")
	}
	c.mu.Lock()
	c.sessionID = res.SessionID
	c.mu.Unlock()
	return res.SessionID, nil
}

// SessionID returns the currently-active session, or "" if none has been
// created yet.
func (c *client) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

type promptContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type sessionPromptParams struct {
	SessionID string               `json:"sessionId"`
	Prompt    []promptContentBlock `json:"prompt"`
}

type sessionPromptResult struct {
	StopReason string `json:"stopReason,omitempty"`
}

// SessionPrompt sends a single user turn into the agent. Returns when the
// agent finishes the turn (or ctx fires — which the caller can use to
// cancel a long-running prompt via SessionCancel).
func (c *client) SessionPrompt(ctx context.Context, sessionID, text string) (string, error) {
	params := sessionPromptParams{
		SessionID: sessionID,
		Prompt: []promptContentBlock{
			{Type: "text", Text: text},
		},
	}
	raw, err := c.conn.Call(ctx, "session/prompt", params)
	if err != nil {
		return "", fmt.Errorf("session/prompt: %w", err)
	}
	var res sessionPromptResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", nil // empty result is acceptable
	}
	return res.StopReason, nil
}

// SessionCancel asks the agent to abort the current turn for sessionID. Best
// effort — agents ack the cancel notification but flushing in-flight tool
// calls is implementation-defined.
func (c *client) SessionCancel(sessionID string) error {
	return c.conn.Notify("session/cancel", map[string]any{"sessionId": sessionID})
}
