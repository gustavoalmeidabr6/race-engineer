package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/adaptor/v2"
	"github.com/gofiber/fiber/v2"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog/log"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/insights"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// Deps bundles the in-process services the MCP tools call into. All fields
// are optional — tools degrade with a clear error to the LLM when a
// dependency is missing.
type Deps struct {
	Brain          *brain.RaceBrain
	Reader         *sql.DB
	InsightLog     *insights.Log
	Cache          func() *models.RaceState
	Triggers       *TriggerQueue
	Skills         *SkillRegistry
	Activity       *ActivityHub // optional — when set, every tool call is broadcast to /api/pi_agent/stream
	APIBase        string       // e.g. http://localhost:8081 — used for tools that self-call existing handlers
	MaxPriority    int          // cap on push_insight priority
	TriggerTimeout time.Duration
}

// Server wraps mcp-go's MCPServer + StreamableHTTPServer so it can be
// mounted on the existing Fiber app.
type Server struct {
	deps   *Deps
	mcp    *mcpserver.MCPServer
	stream *mcpserver.StreamableHTTPServer
	http   *http.Client
}

// NewServer builds the MCP server with every pi-agent tool registered.
func NewServer(deps *Deps) *Server {
	if deps.MaxPriority == 0 {
		deps.MaxPriority = 3
	}
	if deps.TriggerTimeout == 0 {
		deps.TriggerTimeout = 10 * time.Second
	}

	hooks := &mcpserver.Hooks{}
	if deps.Activity != nil {
		installActivityHooks(hooks, deps.Activity)
	}
	mcpSrv := mcpserver.NewMCPServer(
		"race-engineer",
		"0.1.0",
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithLogging(),
		mcpserver.WithHooks(hooks),
	)

	s := &Server{
		deps: deps,
		mcp:  mcpSrv,
		http: &http.Client{Timeout: 25 * time.Second},
	}
	s.registerTools()

	s.stream = mcpserver.NewStreamableHTTPServer(mcpSrv,
		mcpserver.WithStateLess(true),
	)
	return s
}

// Mount wires the MCP transport onto a Fiber router at path. The MCP
// streamable HTTP transport handles GET (SSE), POST (JSON-RPC), and DELETE.
func (s *Server) Mount(router fiber.Router, path string) {
	if path == "" {
		path = "/mcp"
	}
	handler := adaptor.HTTPHandler(s.stream)
	router.All(path, handler)
	log.Info().Str("path", path).Msg("MCP server mounted")
}

// httpGet fetches an existing internal API endpoint and returns the response
// body. Used by the lap/state tools to reuse the same handlers the dashboard
// hits, instead of re-implementing their bucketing/SQL logic in the MCP layer.
func (s *Server) httpGet(ctx context.Context, path string, query map[string]string) (string, error) {
	if s.deps.APIBase == "" {
		return "", fmt.Errorf("APIBase not configured")
	}
	u, err := url.Parse(s.deps.APIBase + path)
	if err != nil {
		return "", err
	}
	if len(query) > 0 {
		q := u.Query()
		for k, v := range query {
			if v == "" {
				continue
			}
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("upstream %s returned %d: %s", path, resp.StatusCode, truncate(string(body), 400))
	}
	return string(body), nil
}

func (s *Server) httpPostJSON(ctx context.Context, path string, payload any) (string, error) {
	if s.deps.APIBase == "" {
		return "", fmt.Errorf("APIBase not configured")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.deps.APIBase+path, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("upstream %s returned %d: %s", path, resp.StatusCode, truncate(string(respBody), 400))
	}
	return string(respBody), nil
}

// truncate returns s clamped to at most n bytes (NOT runes), with the
// ellipsis "…" appended when truncation actually fires. The whole result
// honors the byte budget — callers can pass a hard cap like
// brain.MaxEventSummaryChars and trust the output won't exceed it. UTF-8
// rune boundaries are respected so we never produce malformed text.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	const suffix = "…" // 3 bytes in UTF-8
	if n < len(suffix) {
		return safeByteCut(s, n)
	}
	return safeByteCut(s, n-len(suffix)) + suffix
}

// safeByteCut clips s to at most n bytes, backing off any UTF-8
// continuation bytes so we don't split a multi-byte rune.
func safeByteCut(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func textResult(s string) *mcp.CallToolResult {
	return mcp.NewToolResultText(s)
}

func errResult(err error) *mcp.CallToolResult {
	return mcp.NewToolResultError(err.Error())
}
