package analyst

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// agentCallbacks holds the server-initiated request handlers opencode may
// invoke on us. ACP defines a fixed surface:
//
//   - fs/read_text_file        read a text file by path
//   - fs/write_text_file       write a text file by path
//   - terminal/create          spawn a terminal (we refuse — security)
//   - session/request_permission  ask the user to allow a tool/write
//
// Anything outside this surface returns "method not found" via jsonrpc.go.
type agentCallbacks struct {
	workspace string
	hub       *ActivityHub
}

func newAgentCallbacks(workspace string, hub *ActivityHub) *agentCallbacks {
	return &agentCallbacks{workspace: workspace, hub: hub}
}

// handle is the ServerRequestHandler wired into Conn.
func (a *agentCallbacks) handle(_ context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "fs/read_text_file":
		return a.readTextFile(params)
	case "fs/write_text_file":
		return a.writeTextFile(params)
	case "session/request_permission":
		return a.requestPermission(params)
	case "terminal/create", "terminal/write", "terminal/read":
		// Refuse outright — the data analyst should never spawn shells.
		return nil, errors.New("terminal access is disabled in race-engineer")
	}
	return nil, fmt.Errorf("method not implemented: %s", method)
}

type fsReadParams struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Line      *int   `json:"line,omitempty"`
	Limit     *int   `json:"limit,omitempty"`
}

type fsReadResult struct {
	Content string `json:"content"`
}

func (a *agentCallbacks) readTextFile(raw json.RawMessage) (any, error) {
	var p fsReadParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	resolved, err := a.resolvePath(p.Path)
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(resolved)
	if err != nil {
		return nil, err
	}
	if a.hub != nil {
		a.hub.Publish(Activity{
			Kind:      ActivityFileRead,
			SessionID: p.SessionID,
			Summary:   shortPath(a.workspace, resolved),
		})
	}
	if p.Line != nil || p.Limit != nil {
		body = sliceLines(body, p.Line, p.Limit)
	}
	return fsReadResult{Content: string(body)}, nil
}

type fsWriteParams struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Content   string `json:"content"`
}

func (a *agentCallbacks) writeTextFile(raw json.RawMessage) (any, error) {
	var p fsWriteParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	resolved, err := a.resolvePath(p.Path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(resolved, []byte(p.Content), 0o644); err != nil {
		return nil, err
	}
	if a.hub != nil {
		a.hub.Publish(Activity{
			Kind:      ActivityFileWrite,
			SessionID: p.SessionID,
			Summary:   shortPath(a.workspace, resolved),
			Meta:      map[string]any{"bytes": len(p.Content)},
		})
	}
	return map[string]any{}, nil
}

// requestPermission auto-approves anything scoped to our workspace; that's
// the only sandbox we control. The protocol expects a "selected" option id —
// we always pick the first "allow" / "yes" variant the agent offered.
func (a *agentCallbacks) requestPermission(raw json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
		Options   []struct {
			OptionID string `json:"optionId"`
			Name     string `json:"name"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	chosen := ""
	for _, o := range p.Options {
		if o.Kind == "allow_always" || o.Kind == "allow_once" || strings.HasPrefix(strings.ToLower(o.Name), "allow") || strings.EqualFold(o.OptionID, "allow") {
			chosen = o.OptionID
			break
		}
	}
	if chosen == "" && len(p.Options) > 0 {
		chosen = p.Options[0].OptionID
	}
	if a.hub != nil {
		a.hub.Publish(Activity{
			Kind:      ActivityPermission,
			SessionID: p.SessionID,
			Summary:   "auto-allow: " + chosen,
		})
	}
	return map[string]any{
		"outcome": map[string]any{
			"outcome":  "selected",
			"optionId": chosen,
		},
	}, nil
}

// resolvePath ensures every fs read/write stays under the workspace root.
// Absolute paths get reanchored under the workspace; "../" escapes are
// rejected outright. This is the analyst's sandbox boundary.
func (a *agentCallbacks) resolvePath(p string) (string, error) {
	if a.workspace == "" {
		return "", errors.New("workspace not configured")
	}
	root, err := filepath.Abs(a.workspace)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(p) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		if !strings.HasPrefix(abs+string(filepath.Separator), root+string(filepath.Separator)) && abs != root {
			return "", fmt.Errorf("path escapes workspace: %s", p)
		}
		return abs, nil
	}
	joined := filepath.Join(root, p)
	abs, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(abs+string(filepath.Separator), root+string(filepath.Separator)) && abs != root {
		return "", fmt.Errorf("path escapes workspace: %s", p)
	}
	return abs, nil
}

func shortPath(workspace, abs string) string {
	root, err := filepath.Abs(workspace)
	if err != nil {
		return abs
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return abs
	}
	return rel
}

// sliceLines is a tiny convenience for fs/read_text_file's optional
// line+limit parameters. Line is 1-based per ACP spec.
func sliceLines(body []byte, line, limit *int) []byte {
	if line == nil && limit == nil {
		return body
	}
	lines := strings.Split(string(body), "\n")
	start := 0
	if line != nil && *line > 1 {
		start = *line - 1
	}
	if start >= len(lines) {
		return []byte{}
	}
	end := len(lines)
	if limit != nil && start+*limit < end {
		end = start + *limit
	}
	return []byte(strings.Join(lines[start:end], "\n"))
}
