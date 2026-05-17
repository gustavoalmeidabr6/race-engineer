// Package analyst hosts the in-process driver for the bundled opencode agent.
//
// opencode (https://github.com/sst/opencode) is launched once per app
// lifetime as a long-lived child process via `opencode acp`. The runtime
// speaks JSON-RPC 2.0 over the subprocess's stdin/stdout (the Agent Client
// Protocol from github.com/zed-industries/agent-client-protocol).
//
// One ACP session is created per F1 session, and triggers (lap completion,
// significant events, dashboard queries) become session/prompt calls into
// that session. The agent reaches back through MCP at /mcp for telemetry
// data and through ACP callbacks (fs/read_text_file, fs/write_text_file)
// for the self-evolving workspace at ~/.race-engineer/da-workspace.
//
// Replaces the deleted internal/mcp/{trigger_queue, skills} + python/pi_agent.
package analyst

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Options configures Start. Zero values are sensible — pass only what differs
// from the defaults.
type Options struct {
	// Binary is the opencode executable path. Empty = auto-detect via
	// OPENCODE_BIN env or PATH lookup.
	Binary string

	// Workspace is the directory opencode runs in (its cwd). AGENTS.md,
	// opencode.json, skills, and learnings all live here. Required.
	Workspace string

	// MCPURL is the http://… URL the agent connects to as its MCP server.
	// Typically "http://localhost:<api_port>/mcp". Required.
	MCPURL string

	// Provider / Model populate opencode.json's `model` field. Empty values
	// let opencode pick from its own auth state.
	Provider string
	Model    string

	// HandshakeTimeoutSec bounds the ACP session/new call (where opencode
	// reaches back through /mcp for tool-list validation). The default 90
	// covers slow boots where /mcp competes with other startup traffic;
	// clamped to [30, 300]. Initialize is bounded separately at 15s.
	HandshakeTimeoutSec int

	// Logger is used for structured runtime logs. A zero-value zerolog
	// Logger logs to stderr.
	Logger zerolog.Logger
}

// resolvedHandshakeTimeout returns the SessionNew deadline, clamped into the
// supported range. Lives on Options so both spawn() and the restart handler
// derive the same number.
func (o Options) resolvedHandshakeTimeout() time.Duration {
	sec := o.HandshakeTimeoutSec
	if sec <= 0 {
		sec = 90
	}
	if sec < 30 {
		sec = 30
	}
	if sec > 300 {
		sec = 300
	}
	return time.Duration(sec) * time.Second
}

// initializeTimeout is the deadline for the ACP `initialize` call. Fast in
// practice (a single round-trip with no MCP introspection), so we keep this
// fixed rather than expose another config key.
const initializeTimeout = 15 * time.Second

// InsightPusher is the narrow interface the analyst uses to push insights
// onto the same /api/strategy fanout the rule engine writes to. Implemented
// by intelligence.HTTPInsightPusher in the server wiring.
type InsightPusher interface {
	PushInsight(ctx context.Context, source, insight string, priority int) error
}

// Runtime is the long-lived handle the rest of the server interacts with.
// Triggers, queries, and dashboard subscribers all go through it.
type Runtime struct {
	opts Options
	log  zerolog.Logger

	hub *ActivityHub

	mu        sync.Mutex
	cmd       *exec.Cmd
	conn      *Conn
	client    *client
	version   string
	pid       int
	startedAt time.Time
	lastErr   error

	ready      atomic.Bool
	stopped    atomic.Bool
	restarting atomic.Bool

	runCtx context.Context
	cancel context.CancelFunc
	done   chan struct{}

	// pending tracks active query triggers so the HTTP /api/analyst/jobs
	// route reports something useful while a prompt is in flight.
	pendingMu sync.Mutex
	pending   map[string]*pendingQuery

	// promptMu serialises session/prompt calls so concurrent triggers don't
	// step on each other in the same ACP session, and so activeRunID
	// reliably identifies which run is producing the currently-streaming
	// chunks/tool calls in handleNotification.
	promptMu    sync.Mutex
	activeRunID atomic.Pointer[string]
}

type pendingQuery struct {
	JobID     string
	Question  string
	Topic     string
	Urgent    bool
	StartedAt time.Time
	RunID     string
}

// Start validates the configuration and kicks off the opencode subprocess
// + ACP handshake in a background goroutine, returning the Runtime
// IMMEDIATELY. The runtime reports Ready()=true only after the subprocess
// has spawned and `initialize`+`session/new` have completed.
//
// Returning immediately matters because the handshake races with the API
// server boot: opencode's session/new validation hits our /mcp endpoint to
// list MCP tools, and that endpoint comes up only after main.go's
// server.Start() runs. Blocking the caller here would deadlock the server.
//
// Errors here are pre-flight only (workspace mkdir, binary not found,
// opencode.json write). Runtime failures during spawn / handshake are
// reported via Status().reason and the activity hub.
func Start(ctx context.Context, opts Options) (*Runtime, error) {
	if opts.Workspace == "" {
		return nil, errors.New("analyst: workspace is required")
	}
	if opts.MCPURL == "" {
		return nil, errors.New("analyst: mcp url is required")
	}
	binary, err := LocateBinary(opts.Binary)
	if err != nil {
		return nil, fmt.Errorf("analyst: locate binary: %w", err)
	}
	opts.Binary = binary

	if err := os.MkdirAll(opts.Workspace, 0o755); err != nil {
		return nil, fmt.Errorf("analyst: workspace mkdir: %w", err)
	}
	if err := WriteOpencodeJSON(WorkspaceConfig{
		Workspace: opts.Workspace,
		MCPURL:    opts.MCPURL,
		Provider:  opts.Provider,
		Model:     opts.Model,
	}); err != nil {
		return nil, fmt.Errorf("analyst: write opencode.json: %w", err)
	}

	logger := opts.Logger
	if logger.GetLevel() == zerolog.Disabled {
		logger = log.Logger
	}

	rt := &Runtime{
		opts:    opts,
		log:     logger,
		hub:     NewActivityHub(512),
		pending: map[string]*pendingQuery{},
		done:    make(chan struct{}),
	}
	rt.hub.Publish(Activity{
		Kind:    ActivityRuntime,
		Summary: "queued opencode start; waiting for MCP endpoint",
		Meta:    map[string]any{"binary": binary, "workspace": opts.Workspace},
	})

	runCtx, cancel := context.WithCancel(context.Background())
	rt.runCtx = runCtx
	rt.cancel = cancel

	go rt.bootAsync(runCtx)

	return rt, nil
}

// Restart tears down the current opencode subprocess (if any) and re-runs
// the boot sequence on the same Runtime. Used by POST /api/analyst/restart
// so the user can recover a failed handshake without bouncing the whole
// server. Returns an error when:
//   - the runtime has been Stop()'d
//   - a restart is already in flight
//   - the boot sequence is still running and Ready hasn't flipped yet
//     (caller should wait or hit /api/analyst/status to see progress)
func (rt *Runtime) Restart() error {
	if rt.stopped.Load() {
		return errors.New("analyst is stopped; cannot restart")
	}
	if !rt.restarting.CompareAndSwap(false, true) {
		return errors.New("analyst restart already in flight")
	}
	defer rt.restarting.Store(false)

	rt.log.Info().Msg("analyst: restart requested")
	rt.hub.Publish(Activity{
		Kind:    ActivityRuntime,
		Summary: "restart requested; tearing down opencode",
	})
	rt.ready.Store(false)
	rt.mu.Lock()
	rt.lastErr = nil
	rt.pid = 0
	rt.startedAt = time.Time{}
	rt.mu.Unlock()
	rt.teardownChild()

	// Spawn a fresh boot goroutine on the same runtime context. We
	// intentionally do NOT cancel rt.cancel here — that would kill the
	// runtime entirely. The new bootAsync inherits the existing rt.runCtx,
	// which only fires when Stop() is called.
	go rt.bootAsync(rt.runCtx)
	return nil
}

// bootAsync runs the version probe → wait-for-MCP → spawn → handshake
// sequence in the background so Start() never blocks the caller. Any
// failure is published to the activity hub and recorded in lastErr so
// /api/analyst/status can surface it.
func (rt *Runtime) bootAsync(ctx context.Context) {
	version, vErr := ProbeVersion(ctx, rt.opts.Binary)
	if vErr != nil {
		rt.log.Warn().Err(vErr).Str("binary", rt.opts.Binary).Msg("analyst: --version probe failed; continuing")
	}
	rt.mu.Lock()
	rt.version = version
	rt.mu.Unlock()

	// Wait until /mcp answers — opencode's session/new will introspect our
	// MCP server during validation, and that endpoint comes up only after
	// the Fiber server.Start() in main.go is listening. 30s budget; if the
	// API never comes up the analyst stays disabled, which is fine.
	if err := waitForMCP(ctx, rt.opts.MCPURL, 30*time.Second); err != nil {
		rt.log.Warn().Err(err).Str("mcp_url", rt.opts.MCPURL).Msg("analyst: MCP endpoint never came up; analyst disabled")
		rt.recordBootErr(err)
		return
	}

	err := rt.spawn(ctx)
	if err != nil && isRetryableHandshakeErr(err) && ctx.Err() == nil {
		rt.log.Warn().Err(err).Msg("analyst: handshake timed out; retrying once")
		rt.hub.Publish(Activity{
			Kind:    ActivityRuntime,
			Summary: "handshake retry 1/1 after deadline",
			Error:   err.Error(),
		})
		// Brief settle so the just-killed PID and its /mcp HTTP socket fully
		// close before we spawn again — avoids the retry inheriting a flaky
		// transport.
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			rt.recordBootErr(ctx.Err())
			return
		}
		err = rt.spawn(ctx)
	}
	if err != nil {
		rt.log.Warn().Err(err).Msg("analyst: spawn / handshake failed; analyst disabled")
		rt.recordBootErr(err)
		return
	}
	go func() {
		<-rt.conn.Done()
		close(rt.done)
	}()
}

// isRetryableHandshakeErr is true for errors worth a second attempt — today
// just context deadlines from initialize/session/new. Pipe/binary/start
// errors are not retried (the second attempt would fail the same way).
func isRetryableHandshakeErr(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}

// recordBootErr stores a human-readable failure reason for /api/analyst/status
// and publishes a corresponding activity for the dashboard live feed.
func (rt *Runtime) recordBootErr(err error) {
	rt.mu.Lock()
	rt.lastErr = err
	rt.mu.Unlock()
	rt.hub.Publish(Activity{
		Kind:    ActivityRuntime,
		Summary: "analyst disabled",
		Error:   err.Error(),
	})
}

// waitForMCP probes the MCP endpoint with a minimal JSON-RPC `initialize`
// POST until it returns a 2xx, or the deadline fires. Returns nil on
// success.
//
// The old HEAD probe accepted any response, so it returned the moment the
// Fiber router was up — well before the MCP transport could actually serve
// tool-list calls. opencode's session/new validation then competed with
// other startup traffic and blew its handshake budget (the cause of the
// failure that motivated this rewrite). POSTing an initialize confirms the
// MCP server is functionally answering, not just reachable.
func waitForMCP(ctx context.Context, mcpURL string, deadline time.Duration) error {
	u, err := url.Parse(mcpURL)
	if err != nil {
		return fmt.Errorf("invalid mcp url: %w", err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	client := &http.Client{Timeout: 2 * time.Second}
	// Minimal MCP initialize payload — protocol version matches mcp-go's
	// default. The server is mounted with WithStateLess(true), so each POST
	// is self-contained and no session handshake is required.
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize",` +
		`"params":{"protocolVersion":"2024-11-05","capabilities":{},` +
		`"clientInfo":{"name":"race-engineer-probe","version":"0"}}}`)
	for {
		req, _ := http.NewRequestWithContext(probeCtx, http.MethodPost, u.String(), bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := client.Do(req)
		if err == nil {
			status := resp.StatusCode
			resp.Body.Close()
			if status >= 200 && status < 300 {
				return nil
			}
		}
		select {
		case <-probeCtx.Done():
			return fmt.Errorf("mcp endpoint %s did not respond within %s", mcpURL, deadline)
		case <-tick.C:
		}
	}
}

// spawn launches the subprocess + handshake. Idempotent on subsequent
// restarts (Stop must be called between spawns).
func (rt *Runtime) spawn(ctx context.Context) error {
	cmd := exec.Command(rt.opts.Binary, "acp")
	cmd.Dir = rt.opts.Workspace
	// Forward host environment but force CI-style output so the agent
	// doesn't try to render ANSI cursor sequences on our pipe.
	cmd.Env = append(os.Environ(),
		"CI=1",
		"TERM=dumb",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start opencode: %w", err)
	}
	rt.mu.Lock()
	rt.cmd = cmd
	rt.pid = cmd.Process.Pid
	rt.startedAt = time.Now()
	rt.mu.Unlock()

	rt.log.Info().Int("pid", cmd.Process.Pid).Str("workspace", rt.opts.Workspace).Msg("analyst: opencode acp spawned")

	// Pipe stderr into the host log at debug — surfaces opencode panics
	// without flooding the terminal during normal operation.
	go forwardStderr(stderr, rt.log)

	callbacks := newAgentCallbacks(rt.opts.Workspace, rt.hub)
	conn := NewConn(
		readCloser(stdout),
		writeCloser(stdin),
		callbacks.handle,
		rt.handleNotification,
	)
	rt.conn = conn
	rt.client = newClient(conn)

	go conn.Loop(ctx)
	go rt.waitChild(cmd)

	// Initialize and session/new get separate deadlines. Initialize is a
	// pure stdio round-trip (fast), so a tight 15s ceiling catches a hung
	// child early. session/new is the expensive one — opencode does a live
	// /mcp tool-list round-trip during validation, which under boot load can
	// take many seconds. Sharing one deadline was today's bug.
	initCtx, initCancel := context.WithTimeout(ctx, initializeTimeout)
	if err := rt.client.Initialize(initCtx); err != nil {
		initCancel()
		rt.teardownChild()
		return fmt.Errorf("acp initialize: %w", err)
	}
	initCancel()

	sessionTimeout := rt.opts.resolvedHandshakeTimeout()
	sessCtx, sessCancel := context.WithTimeout(ctx, sessionTimeout)
	sessionID, err := rt.client.SessionNew(sessCtx, rt.opts.Workspace, rt.opts.MCPURL)
	sessCancel()
	if err != nil {
		rt.teardownChild()
		return fmt.Errorf("acp session/new: %w", err)
	}
	rt.ready.Store(true)
	rt.hub.Publish(Activity{
		Kind:      ActivitySessionStart,
		SessionID: sessionID,
		Summary:   "ACP session ready",
		Meta:      map[string]any{"version": rt.version},
	})
	rt.log.Info().Str("session_id", sessionID).Str("version", rt.version).Msg("analyst: ACP session ready")
	return nil
}

func (rt *Runtime) waitChild(cmd *exec.Cmd) {
	err := cmd.Wait()
	rt.ready.Store(false)
	if rt.stopped.Load() {
		rt.log.Info().Int("pid", cmd.Process.Pid).Msg("analyst: opencode exited (stopped)")
		return
	}
	rt.log.Warn().Err(err).Int("pid", cmd.Process.Pid).Msg("analyst: opencode exited unexpectedly")
	rt.hub.Publish(Activity{
		Kind:    ActivityRuntime,
		Summary: "opencode exited",
		Error:   safeError(err),
	})
}

// Stop terminates the subprocess and tears down the conn. Idempotent.
func (rt *Runtime) Stop() {
	if !rt.stopped.CompareAndSwap(false, true) {
		return
	}
	_ = rt.shutdown()
}

func (rt *Runtime) shutdown() error {
	if rt.cancel != nil {
		rt.cancel()
	}
	rt.teardownChild()
	return nil
}

// teardownChild closes the JSON-RPC conn and SIGINTs (then SIGKILLs after 5s)
// the opencode subprocess. Does NOT cancel rt.cancel, so callers that need
// to retry the handshake (e.g. bootAsync after a deadline-exceeded spawn)
// can rebuild conn + cmd on top of the same runtime context.
func (rt *Runtime) teardownChild() {
	if rt.conn != nil {
		_ = rt.conn.Close()
	}
	rt.mu.Lock()
	cmd := rt.cmd
	rt.cmd = nil
	rt.conn = nil
	rt.client = nil
	rt.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
	}
}

// Ready reports whether the subprocess has completed initialize+session/new
// and is accepting prompts.
func (rt *Runtime) Ready() bool { return rt.ready.Load() }

// Hub exposes the live activity feed for the HTTP layer.
func (rt *Runtime) Hub() *ActivityHub { return rt.hub }

// Status snapshots the runtime for /api/analyst/status.
func (rt *Runtime) Status() map[string]any {
	rt.mu.Lock()
	pid := rt.pid
	startedAt := rt.startedAt
	version := rt.version
	lastErr := rt.lastErr
	rt.mu.Unlock()
	sessionID := ""
	if rt.client != nil {
		sessionID = rt.client.SessionID()
	}
	out := map[string]any{
		"installed":   true,
		"ready":       rt.ready.Load(),
		"binary":      rt.opts.Binary,
		"version":     version,
		"workspace":   rt.opts.Workspace,
		"mcp_url":     rt.opts.MCPURL,
		"child_pid":   pid,
		"started_at":  startedAt,
		"session_id":  sessionID,
		"subscribers": rt.hub.SubscriberCount(),
	}
	if lastErr != nil {
		out["reason"] = lastErr.Error()
	}
	return out
}

// PendingJobs returns currently in-flight query triggers (the contract the
// legacy /api/analyst/jobs route reports).
func (rt *Runtime) PendingJobs() []map[string]any {
	rt.pendingMu.Lock()
	defer rt.pendingMu.Unlock()
	out := make([]map[string]any, 0, len(rt.pending))
	for _, j := range rt.pending {
		out = append(out, map[string]any{
			"job_id":        j.JobID,
			"question":      j.Question,
			"context_topic": j.Topic,
			"urgent":        j.Urgent,
			"started_at":    j.StartedAt,
			"run_id":        j.RunID,
		})
	}
	return out
}

// handleNotification routes ACP notifications (session/update etc.) into the
// activity hub for the dashboard's live feed. Stamps each emitted Activity
// with the active run_id (set by the in-flight dispatch) so message_chunk /
// tool_call rows group under the correct run in the UI sidebar — previously
// these rows landed without a run_id and the dashboard collapsed them into
// the "All runs" bucket only.
func (rt *Runtime) handleNotification(method string, params json.RawMessage) {
	switch method {
	case "session/update":
		sessionID := ""
		if rt.client != nil {
			sessionID = rt.client.SessionID()
		}
		acts := renderSessionUpdate(sessionID, params)
		runID := ""
		if p := rt.activeRunID.Load(); p != nil {
			runID = *p
		}
		for _, a := range acts {
			if a.RunID == "" {
				a.RunID = runID
			}
			rt.hub.Publish(a)
		}
	}
}

// safeError reduces an error to its message, returning "" for nil.
func safeError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// forwardStderr drains opencode's stderr into the host logger at debug.
// Keeps the terminal quiet during normal operation but surfaces panics.
func forwardStderr(r io.ReadCloser, logger zerolog.Logger) {
	defer r.Close()
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			logger.Debug().Str("stream", "opencode.stderr").Msg(string(buf[:n]))
		}
		if err != nil {
			return
		}
	}
}

// readCloser / writeCloser adapt io.ReadCloser / io.WriteCloser shapes when
// the underlying pipe is already a Closer (exec.Cmd's StdoutPipe etc. are).
func readCloser(r io.ReadCloser) io.ReadCloser { return r }
func writeCloser(w io.WriteCloser) io.WriteCloser { return w }

// resolveDefaultWorkspace picks a sensible workspace directory when the user
// hasn't set DA_WORKSPACE_DIR. Lives here (not in callers) so the .app and
// the dev binary share one rule.
func ResolveDefaultWorkspace(override string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".race-engineer", "da-workspace"), nil
}
