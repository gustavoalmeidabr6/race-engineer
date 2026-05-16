package api

import (
	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
)

// Server wraps the Fiber HTTP server that exposes the telemetry REST API.
type Server struct {
	app  *fiber.App
	deps *Deps
	port string
}

// NewServer creates a configured Fiber server with all routes registered.
func NewServer(port string, deps *Deps) *Server {
	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ReadBufferSize:        8192,
		BodyLimit:             10 * 1024 * 1024, // 10MB for audio uploads
	})

	s := &Server{
		app:  app,
		deps: deps,
		port: port,
	}
	s.setupMiddleware()
	s.setupRoutes()
	return s
}

// noisyPollingPaths are GET endpoints that the Live agent / dashboard hits
// at sub-second cadence. They flood the terminal access log without adding
// signal — every successful poll looks the same. Suppress them here. Errors
// (status >= 400) are still logged so genuine problems surface.
//
// If diagnostic visibility is needed later, route these to a separate log
// file or drop the filter behind a debug env var rather than re-enabling
// terminal noise.
var noisyPollingPaths = map[string]struct{}{
	"/api/events/next":       {}, // fast-lane (250ms) + idle-lane (1s) dispatchers
	"/api/events/peek":       {}, // debug; not normally polled but trivially noisy if it is
	"/api/events/ack":        {}, // dispatcher confirmation per event — also chatty
	"/api/analyst/jobs":      {}, // polled by Gemini Live during ask_data_analyst rounds
	"/health":                {}, // dashboard + supervisor liveness probes
	"/api/telemetry/latest":  {}, // dashboard + Live agent's get_race_state tool, sub-second
	"/api/query":             {}, // analyst's per-tick SQL pulls — surface via insightlog instead
	"/api/debug/stream":      {}, // SSE — long-lived, would otherwise pollute the access log
	"/api/pi_agent/stream":   {}, // SSE — pi-agent activity firehose for Analyst Team tab
	"/api/pi_agent/recent":   {}, // polled on cold-start by Analyst Team tab
}

func (s *Server) setupMiddleware() {
	s.app.Use(recover.New())
	s.app.Use(logger.New(logger.Config{
		Format:     "${time} ${status} ${method} ${path} ${latency}\n",
		TimeFormat: "15:04:05",
		Next: func(c *fiber.Ctx) bool {
			// Always log non-2xx so error visibility is preserved.
			if status := c.Response().StatusCode(); status >= 400 {
				return false
			}
			_, isNoisy := noisyPollingPaths[c.Path()]
			return isNoisy
		},
	}))
	s.app.Use(cors.New(cors.Config{
		AllowOrigins: "*",
		AllowMethods: "GET,POST,PUT,DELETE,OPTIONS",
	}))
}

func (s *Server) setupRoutes() {
	// Health & status
	s.app.Get("/health", healthHandler(s.deps))
	s.app.Get("/api/settings", settingsHandler(s.deps))

	// Live telemetry from atomic cache
	s.app.Get("/api/telemetry/latest", latestTelemetryHandler(s.deps))

	// Topic-clustered state endpoints powering the Gemini Live tool surface.
	// Each returns a decoded, headline-first bundle scoped to one radio topic.
	// See state_handlers.go for the per-tool field reference.
	s.app.Get("/api/state/race", raceStateHandler(s.deps))
	s.app.Get("/api/state/tires", tiresHandler(s.deps))
	s.app.Get("/api/state/energy", energyHandler(s.deps))
	s.app.Get("/api/state/competitors", competitorsHandler(s.deps))
	s.app.Get("/api/state/pace", paceHandler(s.deps))
	s.app.Get("/api/state/events", eventsHandler(s.deps))
	s.app.Get("/api/state/track_position", trackPositionHandler(s.deps))
	s.app.Get("/api/state/proximity", proximityHandler(s.deps))

	// Driver roster (car_index → surname/team/race-number) from Participants packets
	s.app.Get("/api/roster", rosterHandler(s.deps))

	// Track-position reminder API — driver coaching cues set by the Live agent
	s.app.Post("/api/reminders/corner", setReminderHandler(s.deps))
	s.app.Get("/api/reminders", listRemindersHandler(s.deps))
	s.app.Delete("/api/reminders/:id", cancelReminderHandler(s.deps))

	// Insight consumption
	s.app.Get("/api/insights/next", nextInsightHandler(s.deps))
	s.app.Get("/api/insights/history", insightHistoryHandler(s.deps))

	// SQL query (read-only, via DuckDB reader connection)
	s.app.Post("/api/query", queryHandler(s.deps))

	// Dashboard-controllable settings
	s.app.Post("/api/settings/talk_level", talkLevelHandler(s.deps))
	s.app.Post("/api/settings/verbosity", verbosityHandler(s.deps))
	s.app.Post("/api/settings/mode", telemetryModeHandler(s.deps))
	s.app.Get("/api/settings/mock/overrides", getMockOverridesHandler(s.deps))
	s.app.Post("/api/settings/mock/overrides", setMockOverridesHandler(s.deps))

	// Lap analysis — bucketed channel traces and per-corner brake-point deltas
	// derived from telemetry_hifreq. Both read-only.
	s.app.Get("/api/laps/traces", lapTracesHandler(s.deps))
	s.app.Get("/api/laps/compare", lapCompareHandler(s.deps))
	s.app.Get("/api/laps/delta", lapDeltaHandler(s.deps))
	s.app.Get("/api/laps/list", lapListHandler(s.deps))
	// Coaching tools — Packages A, B, F of the brake-point plan.
	s.app.Get("/api/laps/brake_points", brakePointsHandler(s.deps))
	s.app.Get("/api/laps/brake_balance", brakeBalanceHandler(s.deps))
	s.app.Get("/api/coaching/corner_report", cornerReportHandler(s.deps))
	s.app.Get("/api/telemetry/recent", recentTelemetryHandler(s.deps))

	// Career stats — aggregated lifetime stats for the Home page.
	s.app.Get("/api/stats/career", careerStatsHandler(s.deps))

	// Deep Insights — historical session list + per-session bundle.
	s.app.Get("/api/sessions", sessionsListHandler(s.deps))
	s.app.Get("/api/sessions/:uid/summary", sessionSummaryHandler(s.deps))

	// Persisted UI config (~/.race-engineer/config.json) — read/write surface
	// for the Settings page. Layered on top of env at boot time.
	s.app.Get("/api/config", getConfigHandler(s.deps))
	s.app.Post("/api/config", postConfigHandler(s.deps))
	s.app.Get("/api/config/schema", getConfigSchemaHandler())

	// Lifecycle — restart the entire process group. The Go server drops a
	// .restart sentinel into WorkspaceDir, returns 202, and exits cleanly
	// after a short flush delay. The Node supervisor (Phase 2) watches the
	// sentinel and respawns every child when it sees one alongside a clean
	// exit. See lifecycle_handlers.go for the full contract.
	s.app.Post("/api/server/restart", postRestartHandler(s.deps, s.app.Shutdown, nil))

	// Workspace — compact insights context for LLM agents
	s.app.Get("/api/workspace", workspaceHandler(s.deps))

	// OpenCode agent status + logs
	s.app.Get("/api/agent/status", agentStatusHandler(s.deps))

	// Intelligence — driver queries, strategy webhooks, and voice proxy
	s.app.Post("/api/driver_query", driverQueryHandler(s.deps))
	s.app.Post("/api/strategy", strategyWebhookHandler(s.deps))
	s.app.Post("/api/voice", voiceHandler(s.deps))

	// Brain — shared blackboard snapshot (consumed by external agents)
	s.app.Get("/api/brain/snapshot", brainSnapshotHandler(s.deps))

	// Analyst — synchronous Q&A for external tools (e.g., Gemini Live)
	s.app.Post("/api/analyst/query", analystQueryHandler(s.deps))
	s.app.Get("/api/analyst/jobs", analystJobsHandler(s.deps))

	// Pi Agent — live activity feed for the dashboard's "Analyst Team" tab.
	// /recent is the cold-start payload; /stream is the SSE firehose.
	s.app.Get("/api/pi_agent/recent", piAgentRecentHandler(s.deps))
	s.app.Get("/api/pi_agent/stream", piAgentStreamHandler(s.deps))

	// Interrupts Bus — typed proactive events flowing into Gemini Live.
	s.app.Post("/api/events/interrupt", eventInterruptHandler(s.deps))
	s.app.Get("/api/events/next", eventNextHandler(s.deps))
	s.app.Get("/api/events/peek", eventPeekHandler(s.deps))
	s.app.Post("/api/events/spoken", eventSpokenHandler(s.deps))
	s.app.Post("/api/events/ack", eventAckHandler(s.deps))

	// Transcript — per-session interaction history (voice / engineer / tools).
	s.app.Post("/api/transcript", transcriptIngestHandler(s.deps))
	s.app.Get("/api/transcript", transcriptListHandler(s.deps))
	s.app.Get("/api/transcript/sessions", transcriptSessionsHandler(s.deps))

	// Live Debug — cold-start snapshot + SSE firehose powering the dashboard's
	// Live Debug tab. Reuses the transcript hub's ring + Subscribe API.
	s.app.Get("/api/debug/recent", debugRecentHandler(s.deps))
	s.app.Get("/api/debug/stream", debugStreamHandler(s.deps))

	// Session plan — collaborative store of pit stops, reminders, compound
	// changes etc. Drives the plan ticker's lap-keyed brain events.
	s.app.Get("/api/plan/current", planCurrentHandler(s.deps))
	s.app.Post("/api/plan/entries", planAddHandler(s.deps))
	s.app.Patch("/api/plan/entries/:id", planUpdateHandler(s.deps))
	s.app.Delete("/api/plan/entries/:id", planCancelHandler(s.deps))

	// Mock-only test surface for the dashboard's "Event Simulator" buttons.
	// Gated on cfg.MockMode inside the handler — refuses 403 otherwise.
	s.app.Post("/api/mock/event", mockEventHandler(s.deps))

	// WebSocket — live telemetry push
	if s.deps.Hub != nil {
		s.app.Use("/ws", func(c *fiber.Ctx) error {
			if websocket.IsWebSocketUpgrade(c) {
				return c.Next()
			}
			return fiber.ErrUpgradeRequired
		})
		s.app.Get("/ws", websocket.New(wsHandler(s.deps.Hub)))
	}

	// WebSocket — Gemini Live bidirectional audio bridge (Phase 3.2).
	// Distinct route from /ws because /ws is broadcast-fan-out to all
	// dashboards and the Live stream is point-to-point per conversation.
	if s.deps.LiveDeps != nil {
		s.app.Use("/api/voice/live", func(c *fiber.Ctx) error {
			if websocket.IsWebSocketUpgrade(c) {
				return c.Next()
			}
			return fiber.ErrUpgradeRequired
		})
		s.app.Get("/api/voice/live", websocket.New(liveWSHandler(s.deps.LiveDeps)))
	}
}

// App returns the underlying Fiber app so external packages (e.g. the MCP
// server) can mount additional routes before Start is called.
func (s *Server) App() *fiber.App { return s.app }

// Start begins listening on the configured port. Blocks until shutdown.
func (s *Server) Start() error {
	return s.app.Listen(":" + s.port)
}

// Shutdown gracefully shuts down the HTTP server.
func (s *Server) Shutdown() error {
	return s.app.Shutdown()
}
