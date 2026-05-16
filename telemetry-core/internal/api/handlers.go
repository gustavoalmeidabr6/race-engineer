package api

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"strconv"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/config"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/insights"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/intelligence"
	mcpx "github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/mcp"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/plan"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/state"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/storage"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/trackmap"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/transcript"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/voice"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/workspace"
)

// Deps bundles the dependencies handlers need. Avoids a massive constructor.
type Deps struct {
	Store          *storage.Storage
	Cfg            *config.Config
	InsightChan    <-chan models.DrivingInsight // dashboard insight channel
	StartedAt      time.Time
	PacketsRx      func() uint64 // closure to read ingester packet count
	Workspace      *workspace.Writer
	Gate           *intelligence.CommsGate      // unified insight evaluation & communication
	PiAgent        *mcpx.TriggerQueue           // pi-agent trigger queue (replaces the old strategy analyst)
	PiActivity     *mcpx.ActivityHub            // pi-agent live activity feed (Analyst Team dashboard tab)
	RawInsightChan chan<- models.DrivingInsight // strategy webhook fanout into CommsGate
	Hub            *Hub                         // WebSocket hub for live push
	VoiceClient    *intelligence.VoiceClient    // voice synthesis client (for acks)
	Transcriber    voice.Transcriber            // in-process speech-to-text (Gemini STT)
	LiveDeps       *LiveDeps                    // Phase 3.2 — bidirectional Gemini Live audio bridge
	TranslatedChan chan<- models.DrivingInsight // fanout channel (used by CommsGate internally)
	InsightLog     *insights.Log                // global insight history log
	Brain          *brain.RaceBrain             // shared blackboard (passive recorders write here)
	TrackMap       *trackmap.Registry           // hand-curated corner geometry per F1 25 track id
	Reminders      *insights.ReminderStore      // agent-set track-position reminders
	Proximity      *insights.ProximityWatcher   // per-car proximity snapshot publisher
	Transcript     *transcript.Hub              // per-session interaction log
	Plan           *plan.Store                  // collaborative per-session plan (pit stops, reminders, etc.)
}

// healthHandler returns structured health info: uptime, packet rate, DuckDB ok.
func healthHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		uptime := time.Since(deps.StartedAt).Truncate(time.Second)

		// Quick DuckDB liveness check.
		dbOK := true
		if _, err := deps.Store.Reader().Exec("SELECT 1"); err != nil {
			dbOK = false
		}

		return c.JSON(fiber.Map{
			"status":     "ok",
			"uptime":     uptime.String(),
			"packets_rx": deps.PacketsRx(),
			"duckdb_ok":  dbOK,
			"mock_mode":  deps.Cfg.MockMode.Load(),
			"talk_level": deps.Cfg.TalkLevel.Load(),
			"udp_host":   deps.Cfg.RuntimeUDPHost(),
			"udp_port":   deps.Cfg.RuntimeUDPPort(),
			"udp_mode":   deps.Cfg.RuntimeUDPMode(),
		})
	}
}

// latestTelemetryHandler returns the current RaceState from the atomic
// cache, decorated with the driver roster so callers can resolve
// car_index → surname without a second round-trip.
//
// The wrapper embeds *RaceState so every existing field marshals at the top
// level untouched (no breaking change for downstream JSON consumers); the
// new "roster" field is added alongside.
func latestTelemetryHandler(deps *Deps) fiber.Handler {
	type latestResponse struct {
		*models.RaceState
		Roster map[uint8]state.DriverInfo `json:"roster,omitempty"`
	}
	return func(c *fiber.Ctx) error {
		raceState := deps.Store.Cache().Load()
		if raceState == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "No telemetry data received yet",
			})
		}
		return c.JSON(latestResponse{
			RaceState: raceState,
			Roster:    deps.Store.Roster().Snapshot(),
		})
	}
}

// rosterHandler returns the current driver roster as a standalone resource:
// car_index → {surname, team, race_number, is_player, is_ai, source}. The
// roster is built from F1 Participants packets and reset on session change.
// Returns an empty object (not 503) when no Participants packet has arrived
// yet so polling clients can render gracefully.
func rosterHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		snap := deps.Store.Roster().Snapshot()
		if snap == nil {
			snap = map[uint8]state.DriverInfo{}
		}
		return c.JSON(fiber.Map{
			"session_uid": deps.Store.Roster().SessionUID(),
			"drivers":     snap,
		})
	}
}

// nextInsightHandler consumes and returns the next pending insight.
func nextInsightHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		select {
		case insight := <-deps.InsightChan:
			return c.JSON(insight)
		default:
			return c.Status(fiber.StatusNoContent).Send(nil)
		}
	}
}

// queryHandler executes a read-only SQL statement against DuckDB.
func queryHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var payload models.SQLQueryPayload
		if err := c.BodyParser(&payload); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "Invalid JSON payload",
			})
		}
		if payload.SQL == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "SQL query is required",
			})
		}

		rows, err := deps.Store.Query(c.Context(), payload.SQL)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": err.Error(),
			})
		}
		return c.JSON(fiber.Map{
			"status": "success",
			"rows":   rows,
		})
	}
}

// talkLevelHandler updates the talk level from the dashboard.
func talkLevelHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var payload models.TalkLevelPayload
		if err := c.BodyParser(&payload); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "Invalid payload",
			})
		}
		deps.Cfg.SetTalkLevel(payload.TalkLevel)
		return c.JSON(fiber.Map{
			"status":     "success",
			"talk_level": deps.Cfg.TalkLevel.Load(),
		})
	}
}

// verbosityHandler updates how detailed the engineer's responses are.
func verbosityHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var payload models.VerbosityPayload
		if err := c.BodyParser(&payload); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "Invalid payload",
			})
		}
		deps.Cfg.SetVerbosity(payload.Verbosity)
		return c.JSON(fiber.Map{
			"status":    "success",
			"verbosity": deps.Cfg.Verbosity.Load(),
		})
	}
}

// telemetryModeHandler switches between mock and real UDP mode.
func telemetryModeHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var payload models.TelemetryModePayload
		if err := c.BodyParser(&payload); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "Invalid payload",
			})
		}

		switch payload.Mode {
		case "mock":
			deps.Cfg.SetMockMode(true)
		case "real":
			deps.Cfg.SetMockMode(false)
		default:
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "Mode must be 'mock' or 'real'",
			})
		}

		if payload.Host != "" || payload.Port > 0 || payload.UDPMode != "" {
			deps.Cfg.SetRuntimeUDP(payload.Host, payload.Port, payload.UDPMode)
		}

		return c.JSON(fiber.Map{
			"status":    "success",
			"mode":      payload.Mode,
			"mock_mode": deps.Cfg.MockMode.Load(),
			"udp_host":  deps.Cfg.RuntimeUDPHost(),
			"udp_port":  deps.Cfg.RuntimeUDPPort(),
			"udp_mode":  deps.Cfg.RuntimeUDPMode(),
		})
	}
}

// getMockOverridesHandler returns the current simulation overrides.
func getMockOverridesHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		return c.JSON(deps.Cfg.GetMockOverrides())
	}
}

// setMockOverridesHandler updates the simulation overrides.
func setMockOverridesHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var payload models.MockOverrides
		if err := c.BodyParser(&payload); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "Invalid JSON payload",
			})
		}
		deps.Cfg.SetMockOverrides(payload)
		return c.JSON(fiber.Map{
			"status": "success",
		})
	}
}

// settingsHandler returns all current dynamic settings.
func settingsHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"mock_mode":      deps.Cfg.MockMode.Load(),
			"talk_level":     deps.Cfg.TalkLevel.Load(),
			"verbosity":      deps.Cfg.Verbosity.Load(),
			"udp_host":       deps.Cfg.RuntimeUDPHost(),
			"udp_port":       deps.Cfg.RuntimeUDPPort(),
			"udp_mode":       deps.Cfg.RuntimeUDPMode(),
			"api_port":       deps.Cfg.APIPort,
			"db_path":        deps.Cfg.DBPath,
			"python_api":     deps.Cfg.PythonAPI,
			"mock_overrides": deps.Cfg.GetMockOverrides(),
		})
	}
}

// workspaceHandler returns the current insights.md content as plain text.
// LLM agents call this to get compact race context for their prompts.
func workspaceHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Workspace == nil {
			return c.Status(fiber.StatusServiceUnavailable).SendString("Workspace not initialized")
		}
		c.Set("Content-Type", "text/markdown; charset=utf-8")
		return c.SendString(deps.Workspace.Content())
	}
}

// driverQueryHandler handles POST /api/driver_query.
// Accepts {"query":"How are my tires?"} and returns a Gemini-powered response.
func driverQueryHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Gate == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "CommsGate not initialized",
			})
		}

		var payload models.DriverQuery
		if err := c.BodyParser(&payload); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "Invalid JSON payload",
			})
		}
		if payload.Query == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "Query is required",
			})
		}

		if deps.Brain != nil {
			deps.Brain.RecordDriver(payload.Query, "query")
		}

		// HandleQuery sends through the CommsGate pipeline which emits to
		// translatedChan internally (reaches fanout → WS, voice, workspace).
		insight := deps.Gate.HandleQuery(c.Context(), payload.Query)

		return c.JSON(insight)
	}
}

// strategyWebhookHandler handles POST /api/strategy.
// External agents push strategy insights here for translation and voice delivery.
// Insights with summary="analyst" (or summary="pi_agent") are treated as pre-
// translated radio sentences — CommsGate skips the LLM rephrase step.
func strategyWebhookHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.RawInsightChan == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "insight channel not initialised",
			})
		}

		var payload models.StrategyPayload
		if err := c.BodyParser(&payload); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "Invalid JSON payload",
			})
		}

		normalized := payload.Normalize()
		preTranslated := strings.EqualFold(normalized.Source, "analyst") ||
			strings.EqualFold(normalized.Source, "pi_agent") ||
			strings.HasPrefix(strings.ToLower(normalized.Source), "pi_agent.") ||
			strings.EqualFold(normalized.Summary, "analyst")
		message := strings.TrimSpace(normalized.Recommendation)
		if message == "" {
			message = strings.TrimSpace(normalized.Insight)
		}
		insight := models.DrivingInsight{
			Message:       message,
			Type:          "strategy",
			Priority:      normalized.Criticality,
			PreTranslated: preTranslated,
		}
		select {
		case deps.RawInsightChan <- insight:
		default:
			// Insight channel full — drop rather than block the HTTP handler.
		}

		if deps.Transcript != nil {
			deps.Transcript.Append(transcript.Event{
				Kind:  transcript.KindInsightPushed,
				Actor: firstNonEmpty(normalized.Source, "strategy_webhook"),
				Text:  normalized.Insight,
				Meta: map[string]any{
					"priority": normalized.Priority,
					"source":   normalized.Source,
				},
			})
		}

		return c.JSON(fiber.Map{
			"status": "accepted",
		})
	}
}

func firstNonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) != "" {
		return s
	}
	return fallback
}

// brainSnapshotHandler returns the shared RaceBrain snapshot.
//
//	GET /api/brain/snapshot                       → text/markdown (default)
//	GET /api/brain/snapshot?format=json           → structured JSON
//	GET /api/brain/snapshot?dynamic_only=true     → omit Static block (driver
//	                                                profile, track setup, past
//	                                                learnings) — useful for
//	                                                runtime polls where static
//	                                                content is already loaded.
//	GET /api/brain/snapshot?max_chars=N           → truncate the markdown body
//	                                                to N chars (markdown only;
//	                                                JSON form ignores this).
//
// This is the canonical context surface for external conversational agents
// (e.g., Gemini Live). The markdown form is a drop-in replacement for what
// CommsGate sees at decision time.
func brainSnapshotHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Brain == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "brain not initialised",
			})
		}

		opts := brain.DefaultSnapshotOpts()
		dynamicOnly := strings.EqualFold(c.Query("dynamic_only"), "true")
		if dynamicOnly {
			opts.IncludeStatic = false
		}
		snap := deps.Brain.Snapshot(opts)

		format := strings.ToLower(c.Query("format"))
		if format == "json" {
			payload := fiber.Map{
				"now":           snap.Now,
				"hot_facts":     snap.HotFacts,
				"observations":  snap.Observations,
				"hypotheses":    snap.Hypotheses,
				"speech":        snap.Speech,
				"driver_events": snap.DriverEvents,
				"events":        snap.Events,
			}
			if !dynamicOnly {
				payload["static"] = fiber.Map{
					"soul":           snap.Static.Soul,
					"user":           snap.Static.User,
					"driver_profile": snap.Static.DriverProfile,
					"track_setup":    snap.Static.TrackSetup,
					"past_learnings": snap.Static.PastLearnings,
				}
			}
			return c.JSON(payload)
		}

		var b strings.Builder
		if snap.HotFacts != nil {
			b.WriteString("## Live Telemetry\n")
			b.WriteString(intelligence.BuildContext(snap.HotFacts))
			b.WriteString("\n\n")
		}
		if deps.Plan != nil {
			if planMarkdown := plan.RenderMarkdown(deps.Plan.Current()); planMarkdown != "" {
				b.WriteString("## Active Plan\n")
				b.WriteString(planMarkdown)
				b.WriteString("\n")
			}
		}
		if deps.Transcript != nil {
			recent := deps.Transcript.Recent(0, nil)
			if len(recent) > 0 {
				b.WriteString("## Recent Transcript\n")
				b.WriteString(transcript.RenderMarkdown(recent))
				b.WriteString("\n\n")
			}
		}
		b.WriteString(snap.Markdown())

		body := b.String()
		if maxChars, err := strconv.Atoi(c.Query("max_chars")); err == nil && maxChars > 0 && len(body) > maxChars {
			body = body[:maxChars] + "\n... [truncated]"
		}

		c.Set(fiber.HeaderContentType, "text/markdown; charset=utf-8")
		return c.SendString(body)
	}
}

// analystQueryHandler enqueues a question onto the pi-agent trigger queue
// and returns immediately with a job_id. The pi agent's planner long-polls
// pull_next_trigger, dispatches its responder specialist, and writes the
// answer back as a brain Observation under topic "analyst.<context_topic>"
// via the submit_query_answer MCP tool. When urgent=true, the answer is
// also POSTed to /api/strategy so the existing voice pipeline speaks it.
//
// Contract preserved (was the old strategy analyst's QueryAsync):
//
//	POST /api/analyst/query
//	body: {"question": "...", "context_topic": "tire_strategy", "urgent": false}
//	202   {"job_id": "anq_...", "status": "queued", "eta_seconds": 10}
func analystQueryHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.PiAgent == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "pi agent trigger queue not initialised",
			})
		}
		var payload struct {
			Question     string `json:"question"`
			ContextTopic string `json:"context_topic"`
			Urgent       bool   `json:"urgent"`
		}
		if err := c.BodyParser(&payload); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "invalid JSON payload",
			})
		}
		question := strings.TrimSpace(payload.Question)
		if question == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "question is required",
			})
		}
		topic := strings.TrimSpace(payload.ContextTopic)
		if topic == "" {
			topic = "general"
		}
		jobID, _ := deps.PiAgent.EnqueueQuery(question, topic, payload.Urgent)
		if deps.Brain != nil {
			deps.Brain.RegisterPendingJob(jobID, question, topic)
		}
		if deps.Transcript != nil {
			deps.Transcript.Append(transcript.Event{
				Kind:  transcript.KindAnalystQuery,
				Actor: "pi_agent",
				Text:  question,
				Meta: map[string]any{
					"job_id":        jobID,
					"context_topic": topic,
					"urgent":        payload.Urgent,
				},
			})
		}
		return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
			"job_id":      jobID,
			"status":      "queued",
			"eta_seconds": 10,
		})
	}
}

// analystJobsHandler exposes the current pending pi-agent query jobs for
// debugging. Returns an array — empty if nothing in flight.
//
//	GET /api/analyst/jobs → [{"job_id":"...","question":"...", ...}]
func analystJobsHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.PiAgent != nil {
			return c.JSON(deps.PiAgent.PendingJobs())
		}
		if deps.Brain != nil {
			return c.JSON(deps.Brain.PendingJobs())
		}
		return c.JSON([]any{})
	}
}

// eventInterruptHandler accepts a producer-pushed Event onto the Interrupts
// Bus. Validation runs against the brevity contract (length caps, allowed
// types) and the TalkLevel floor.
//
//	POST /api/events/interrupt
//	{"type":"threat_overtake","priority":3,"summary":"Verstappen P+1, gap 1.4s","reasoning":"...","debug_data":{...},"source":"analyst","dedup_key":"threat:lap14"}
//
// 202 → {"event_id":"evt_..."} on accept
// 400 → {"error":"..."} on validation failure
// 429 → {"error":"deduped"} when DedupKey collides with a queued or recently-spoken entry
// 429 → {"error":"talk_level_filter"} when priority is below the TalkLevel floor
func eventInterruptHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Brain == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "brain not initialised",
			})
		}
		var ev brain.Event
		if err := c.BodyParser(&ev); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "invalid JSON: " + err.Error(),
			})
		}
		// Default the priority from the event type if the caller omitted it.
		if ev.Priority == 0 {
			ev.Priority = brain.DefaultEventPriority(ev.Type)
		}

		talkLevel := int32(10) // disable filter if config is missing
		if deps.Cfg != nil {
			talkLevel = deps.Cfg.TalkLevel.Load()
		}

		stored, err := deps.Brain.EnqueueEvent(ev, talkLevel)
		switch {
		case err == nil:
			return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
				"event_id": stored.ID,
				"priority": stored.Priority,
				"type":     stored.Type,
			})
		case errors.Is(err, brain.ErrEventDeduped):
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"error":  "deduped",
				"detail": err.Error(),
			})
		case errors.Is(err, brain.ErrEventTalkLevel):
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"error":  "talk_level_filter",
				"detail": err.Error(),
			})
		default:
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": err.Error(),
			})
		}
	}
}

// eventNextHandler leases the next event in the queue whose priority falls
// within [min_priority, max_priority]. The event is marked in_flight on the
// brain — the dispatcher MUST close the lease via /api/events/ack with
// outcome=delivered (success) or outcome=interrupted (user barged in,
// re-queue for retry). Returns {} (200) when the queue has nothing matching
// or another event is already in_flight (single-flight invariant), so
// polling clients can use a tight loop without status-code branching.
//
//	GET /api/events/next?min_priority=4              → fast-lane (P4–P5)
//	GET /api/events/next?min_priority=1&max_priority=3 → idle-lane (P1–P3)
func eventNextHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Brain == nil {
			return c.JSON(fiber.Map{})
		}
		minP, _ := strconv.Atoi(c.Query("min_priority", "1"))
		maxP, _ := strconv.Atoi(c.Query("max_priority", "5"))
		ev := deps.Brain.LeaseNext(minP, maxP)
		if ev == nil {
			return c.JSON(fiber.Map{})
		}
		return c.JSON(ev)
	}
}

// eventAckHandler closes a lease opened by /api/events/next.
//
//	POST /api/events/ack
//	{"event_id":"evt_...","outcome":"delivered","spoken_text":"...","priority":4}
//	{"event_id":"evt_...","outcome":"interrupted","reason":"user_barge_in"}
//	{"event_id":"evt_...","outcome":"errored","reason":"dispatcher_send_failed"}
//	{"event_id":"evt_...","outcome":"dropped","reason":"spurious_interrupt"}
//
// outcome=delivered: removes the event and records spoken_text into the
// recent-radio buffer. spoken_text defaults to event.Summary if empty.
// outcome=interrupted / errored: nacks the event so it returns to queued
// status for retry. After MaxAttemptsForPriority interrupts the event is
// dropped with a "delivery" Observation logged.
// outcome=dropped: removes the event without retry. Used when the
// dispatcher knows the event is no longer worth saying — e.g. a spurious
// server-side `interrupted` that produced no audio.
func eventAckHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Brain == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "brain not initialised",
			})
		}
		var payload struct {
			EventID    string `json:"event_id"`
			Outcome    string `json:"outcome"`
			Reason     string `json:"reason"`
			SpokenText string `json:"spoken_text"`
			Priority   int    `json:"priority"`
		}
		if err := c.BodyParser(&payload); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "invalid JSON",
			})
		}
		if payload.EventID == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "event_id is required",
			})
		}

		switch strings.ToLower(strings.TrimSpace(payload.Outcome)) {
		case "delivered":
			err := deps.Brain.AckDelivered(payload.EventID, strings.TrimSpace(payload.SpokenText), payload.Priority)
			if errors.Is(err, brain.ErrEventNotFound) {
				return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
			}
			return c.JSON(fiber.Map{"status": "delivered"})
		case "interrupted", "errored":
			reason := strings.TrimSpace(payload.Reason)
			if reason == "" {
				reason = payload.Outcome
			}
			err := deps.Brain.NackInterrupted(payload.EventID, reason)
			if errors.Is(err, brain.ErrEventNotFound) {
				return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
			}
			return c.JSON(fiber.Map{"status": "requeued"})
		case "dropped":
			reason := strings.TrimSpace(payload.Reason)
			if reason == "" {
				reason = "dropped"
			}
			err := deps.Brain.DropDelivered(payload.EventID, reason)
			if errors.Is(err, brain.ErrEventNotFound) {
				return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
			}
			return c.JSON(fiber.Map{"status": "dropped"})
		default:
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "outcome must be one of: delivered, interrupted, errored, dropped",
			})
		}
	}
}

// eventPeekHandler returns the current queue contents without consuming.
// For the dashboard and `racedb`-style debugging.
//
//	GET /api/events/peek
func eventPeekHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Brain == nil {
			return c.JSON([]brain.Event{})
		}
		evs := deps.Brain.PeekEvents()
		if evs == nil {
			evs = []brain.Event{}
		}
		return c.JSON(evs)
	}
}

// eventSpokenHandler closes the speech-log loop. Compatibility shim for
// older Live-agent builds: if event_id matches an in_flight lease this
// behaves like /api/events/ack {outcome:"delivered"}; otherwise it falls
// back to a free-form RecordSpeech (the legacy contract).
//
//	POST /api/events/spoken
//	{"event_id":"evt_...","spoken_text":"Verstappen one back, give it up."}
func eventSpokenHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Brain == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "brain not initialised",
			})
		}
		var payload struct {
			EventID    string `json:"event_id"`
			SpokenText string `json:"spoken_text"`
			Priority   int    `json:"priority"`
		}
		if err := c.BodyParser(&payload); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "invalid JSON",
			})
		}
		text := strings.TrimSpace(payload.SpokenText)
		if text == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "spoken_text is required",
			})
		}
		priority := payload.Priority
		if priority < 1 || priority > 5 {
			priority = 3
		}
		// New path: if the event still exists on the bus, ack-as-delivered
		// so the lease lifecycle closes properly. ErrEventNotFound is the
		// expected case for free-form speech entries that aren't tied to
		// a queued event — fall back to the legacy free-form record.
		if payload.EventID != "" {
			if err := deps.Brain.AckDelivered(payload.EventID, text, priority); err == nil {
				return c.JSON(fiber.Map{"status": "delivered"})
			}
		}
		deps.Brain.RecordSpeech(text, priority)
		return c.JSON(fiber.Map{"status": "logged"})
	}
}

// insightHistoryHandler returns the insight log as JSON.
// Optional query param: ?limit=N (default 50, max 500).
func insightHistoryHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.InsightLog == nil {
			return c.JSON([]insights.LogEntry{})
		}
		limit := 50
		if l := c.Query("limit"); l != "" {
			if n, err := strconv.Atoi(l); err == nil && n > 0 {
				if n > 500 {
					n = 500
				}
				limit = n
			}
		}
		return c.JSON(deps.InsightLog.Recent(limit))
	}
}

// agentStatusHandler returns a snapshot of the pi-agent state. Vestigial
// route — kept so the dashboard's existing /api/agent/status poll keeps
// returning a sensible payload.
func agentStatusHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		enabled := deps.PiAgent != nil
		depth := 0
		if enabled {
			depth = deps.PiAgent.Depth()
		}
		return c.JSON(fiber.Map{
			"enabled":       enabled,
			"trigger_depth": depth,
			"mode":          deps.Cfg.PiAgentMode,
			"provider":      deps.Cfg.PiAgentProvider,
			"logs":          []any{},
		})
	}
}

// voiceHandler handles POST /api/voice.
//
// Accepts a multipart audio upload, transcribes it via the in-process
// voice.Transcriber (Phase 3.1 — was Python /transcribe), feeds the
// transcription to CommsGate, and returns the combined result.
//
// When GEMINI_LIVE_ENABLED=true the Live agent owns the voice surface,
// so we accept-and-drop incoming audio instead of running a parallel
// transcription path that would race with Live's mic.
func voiceHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		// Disabled at boot — voice_mode=live_only means STT isn't even
		// initialised. Return 503 so the dashboard's mic button knows
		// to fall back to the Live Voice tab.
		if deps.Cfg != nil && !deps.Cfg.PTTPathEnabled() {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error":      "push-to-talk disabled (voice_mode=live_only)",
				"voice_mode": deps.Cfg.VoiceMode,
			})
		}
		if deps.Cfg != nil && deps.Cfg.GeminiLiveEnabled {
			// voice_mode=both — Live owns the voice surface but the PTT
			// path is intentionally kept up as an input fallback. Tell
			// callers the request was ignored so they don't re-try.
			return c.Status(fiber.StatusOK).JSON(fiber.Map{
				"status":     "ignored",
				"message":    "Gemini Live owns the voice surface — /api/voice is disabled",
				"voice_mode": deps.Cfg.VoiceMode,
			})
		}
		if deps.Transcriber == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "STT not configured (missing GEMINI_API_KEY?)",
			})
		}
		if avail, ok := deps.Transcriber.(voice.Available); ok && !avail.Available() {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "STT provider unavailable (missing GEMINI_API_KEY?)",
			})
		}

		// 1. Get the audio file from multipart form.
		file, err := c.FormFile("audio")
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "Audio file required (field: audio)",
			})
		}
		src, err := file.Open()
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "Failed to read audio file",
			})
		}
		defer src.Close()
		audioBytes, err := io.ReadAll(src)
		if err != nil || len(audioBytes) == 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "Empty or unreadable audio",
			})
		}

		// 2. Fire acknowledgement audio immediately (non-blocking) so the
		// driver hears "Copy, checking the data" within ~50ms while
		// transcription + LLM run in parallel.
		if deps.VoiceClient != nil {
			go deps.VoiceClient.SynthesizeAck(context.Background())
		}

		// 3. Transcribe in-process. The MIME hint comes from the
		// multipart Content-Type; normalisation lives in the voice
		// package so the handler stays format-agnostic.
		mime := file.Header.Get("Content-Type")
		text, confidence, err := deps.Transcriber.Transcribe(c.Context(), audioBytes, mime)
		if err != nil {
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
				"error": "Transcription failed: " + err.Error(),
			})
		}
		if text == "" {
			return c.JSON(fiber.Map{
				"transcription": "",
				"confidence":    0,
				"insight":       nil,
			})
		}

		if deps.Brain != nil {
			deps.Brain.RecordDriver(text, "query")
		}

		// 4. Feed transcription to CommsGate (handles fanout internally).
		var insight *models.DrivingInsight
		if deps.Gate != nil {
			result := deps.Gate.HandleQuery(c.Context(), text)
			insight = &result
		}

		return c.JSON(fiber.Map{
			"transcription": text,
			"confidence":    confidence,
			"insight":       insight,
		})
	}
}
