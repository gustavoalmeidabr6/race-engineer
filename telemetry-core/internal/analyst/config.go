package analyst

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// WorkspaceConfig is the subset of opencode.json the runtime owns. We
// regenerate the file on every boot so a stale model name or MCP URL from
// a previous run never persists across upgrades.
type WorkspaceConfig struct {
	Workspace string
	MCPURL    string
	Provider  string
	Model     string
}

// WriteOpencodeJSON renders an opencode.json into the workspace directory.
// The schema follows opencode's documented config — `$schema`, `mcp`, `model`,
// and an `instructions` glob pointing at our prompt templates so the agent
// auto-loads them alongside AGENTS.md.
func WriteOpencodeJSON(cfg WorkspaceConfig) error {
	if cfg.Workspace == "" {
		return fmt.Errorf("workspace path is required")
	}
	if cfg.MCPURL == "" {
		return fmt.Errorf("mcp url is required")
	}
	doc := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"mcp": map[string]any{
			"race-engineer": map[string]any{
				"type":    "remote",
				"url":     cfg.MCPURL,
				"enabled": true,
			},
		},
		"instructions": []string{
			"AGENTS.md",
			"driver/*.md",
			"tracks/*.md",
			"learnings/*.md",
		},
	}
	if cfg.Provider != "" && cfg.Model != "" {
		doc["model"] = cfg.Provider + "/" + cfg.Model
	} else if cfg.Provider != "" {
		// Provider-only — pick a sane default per provider so the user
		// can leave the model field blank in DA_MODEL.
		doc["model"] = cfg.Provider + "/" + defaultModelFor(cfg.Provider)
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	path := filepath.Join(cfg.Workspace, "opencode.json")
	return os.WriteFile(path, body, 0o644)
}

func defaultModelFor(provider string) string {
	switch provider {
	case "anthropic":
		return "claude-sonnet-4-6"
	case "openai":
		return "gpt-4.1-mini"
	case "google", "gemini":
		return "gemini-3.1-flash-lite"
	}
	return ""
}
