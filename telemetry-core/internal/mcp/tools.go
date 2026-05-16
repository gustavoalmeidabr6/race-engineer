package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog/log"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
)

// registerTools wires the full pi-agent tool surface onto the MCP server.
// Tools are grouped by category in the source for readability; each tool's
// description block is what the LLM sees when picking what to call.
func (s *Server) registerTools() {
	s.registerStateTools()
	s.registerLapTools()
	s.registerDBTools()
	s.registerInsightTools()
	s.registerTranscriptTools()
	s.registerTriggerTools()
	s.registerWriteTools()
	s.registerSkillTools()
}

// ── 1. Read race state ──────────────────────────────────────────────────────

func (s *Server) registerStateTools() {
	s.mcp.AddTool(
		mcp.NewTool("get_race_state",
			mcp.WithDescription("Snapshot of the live race state (player position, lap, gaps, fuel, tire compound, damage, weather). Cheap — call freely. Identical payload to GET /api/telemetry/latest."),
		),
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			body, err := s.httpGet(ctx, "/api/telemetry/latest", nil)
			if err != nil {
				return errResult(err), nil
			}
			return textResult(body), nil
		},
	)

	s.mcp.AddTool(
		mcp.NewTool("get_brain_snapshot",
			mcp.WithDescription("Full race brain markdown — telemetry + observations + recent radio + driver state + active session plan + static context. Use this to see what other agents (rule engine, prior pi-agent runs, the engineer) have already concluded."),
			mcp.WithString("format", mcp.Description("'markdown' (default) or 'json'")),
			mcp.WithBoolean("dynamic_only", mcp.Description("Skip static driver/track/learnings blocks")),
			mcp.WithNumber("max_chars", mcp.Description("Truncate the markdown body to this many chars (markdown only)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			q := map[string]string{}
			if v, ok := args["format"].(string); ok {
				q["format"] = v
			}
			if v, ok := args["dynamic_only"].(bool); ok && v {
				q["dynamic_only"] = "true"
			}
			if v, ok := args["max_chars"].(float64); ok && v > 0 {
				q["max_chars"] = fmt.Sprintf("%d", int(v))
			}
			body, err := s.httpGet(ctx, "/api/brain/snapshot", q)
			if err != nil {
				return errResult(err), nil
			}
			return textResult(body), nil
		},
	)

	s.mcp.AddTool(
		mcp.NewTool("get_recent_telemetry",
			mcp.WithDescription("Bucketed rolling-window telemetry for the last N seconds (throttle/brake/speed/g-forces/temps) plus summary stats (peaks, brake events, max temps). Use to evaluate driving smoothness, recent stints, or whether the driver is overheating brakes."),
			mcp.WithNumber("seconds", mcp.Description("Window length in seconds (1-300, default 60)")),
			mcp.WithNumber("buckets", mcp.Description("Number of time buckets (10-120, default 30)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			q := map[string]string{}
			if v, ok := args["seconds"].(float64); ok && v > 0 {
				q["seconds"] = fmt.Sprintf("%d", int(v))
			}
			if v, ok := args["buckets"].(float64); ok && v > 0 {
				q["buckets"] = fmt.Sprintf("%d", int(v))
			}
			body, err := s.httpGet(ctx, "/api/telemetry/recent", q)
			if err != nil {
				return errResult(err), nil
			}
			return textResult(body), nil
		},
	)

	for _, route := range []struct {
		name, path, desc string
	}{
		{"get_state_race", "/api/state/race", "Race-topic state bundle: position, lap, gaps to leader and ahead/behind, pit count."},
		{"get_state_tires", "/api/state/tires", "Tire-topic state bundle: compound, age, wear per corner, surface/inner temps."},
		{"get_state_energy", "/api/state/energy", "Energy-topic state bundle: fuel in tank, fuel mix, ERS store + deploy mode, harvested-this-lap."},
		{"get_state_competitors", "/api/state/competitors", "Competitors bundle: nearby cars (±3 positions) with compound, tire age, pit count."},
		{"get_state_pace", "/api/state/pace", "Pace bundle: recent lap times, deltas, sector breakdown."},
		{"get_state_events", "/api/state/events", "Recent race events (yellow flags, SC, weather changes, etc.)"},
		{"get_state_track_position", "/api/state/track_position", "All cars' on-track lap_distance — useful with /state/proximity."},
		{"get_state_proximity", "/api/state/proximity", "Closing-from-behind / catching-ahead bundle."},
	} {
		route := route
		s.mcp.AddTool(
			mcp.NewTool(route.name, mcp.WithDescription(route.desc)),
			func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				body, err := s.httpGet(ctx, route.path, nil)
				if err != nil {
					return errResult(err), nil
				}
				return textResult(body), nil
			},
		)
	}
}

// ── 2. Lap analysis ─────────────────────────────────────────────────────────

func (s *Server) registerLapTools() {
	s.mcp.AddTool(
		mcp.NewTool("list_laps",
			mcp.WithDescription("List completed laps with time/sector/valid info. Always call this BEFORE get_lap_traces / get_lap_delta / compare_lap_corners so the lap numbers you pass are real."),
		),
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			body, err := s.httpGet(ctx, "/api/laps/list", nil)
			if err != nil {
				return errResult(err), nil
			}
			return textResult(body), nil
		},
	)

	s.mcp.AddTool(
		mcp.NewTool("get_lap_traces",
			mcp.WithDescription("Bucketed channel arrays per lap, indexed by track distance. Channels: throttle, brake, speed, gear, steering, rpm, g_lat, g_lon, g_vert, brake_temp{,_fl,_fr,_rl,_rr}, tyre_temp{,_fl,_fr,_rl,_rr}, tyre_inner_temp, tyre_pressure{,_fl,_fr,_rl,_rr}, fuel, ers_store, ers_deploy_mode, clutch, drs."),
			mcp.WithString("laps", mcp.Description("Comma-separated: lap numbers, or 'best','current','last','recent:N'. Default 'best,current'.")),
			mcp.WithString("channels", mcp.Description("Comma-separated channel names. Default 'throttle,brake,speed'.")),
			mcp.WithNumber("buckets", mcp.Description("Number of distance buckets (20-200, default 80).")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			q := map[string]string{}
			if v, ok := args["laps"].(string); ok {
				q["laps"] = v
			}
			if v, ok := args["channels"].(string); ok {
				q["channels"] = v
			}
			if v, ok := args["buckets"].(float64); ok && v > 0 {
				q["buckets"] = fmt.Sprintf("%d", int(v))
			}
			body, err := s.httpGet(ctx, "/api/laps/traces", q)
			if err != nil {
				return errResult(err), nil
			}
			return textResult(body), nil
		},
	)

	s.mcp.AddTool(
		mcp.NewTool("get_lap_delta",
			mcp.WithDescription("Cumulative time-delta-vs-reference at every distance bucket. The single most actionable view for 'where am I losing time?' coaching."),
			mcp.WithString("lap", mcp.Description("Lap number, or 'last' (default).")),
			mcp.WithString("reference", mcp.Description("Reference lap: number, or 'best' (default).")),
			mcp.WithNumber("buckets", mcp.Description("Distance buckets (20-200, default 80).")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			q := map[string]string{}
			if v, ok := args["lap"].(string); ok {
				q["lap"] = v
			}
			if v, ok := args["reference"].(string); ok {
				q["reference"] = v
			}
			if v, ok := args["buckets"].(float64); ok && v > 0 {
				q["buckets"] = fmt.Sprintf("%d", int(v))
			}
			body, err := s.httpGet(ctx, "/api/laps/delta", q)
			if err != nil {
				return errResult(err), nil
			}
			return textResult(body), nil
		},
	)

	s.mcp.AddTool(
		mcp.NewTool("compare_lap_corners",
			mcp.WithDescription("Per-corner brake-point + apex-speed + exit-throttle deltas vs the driver's best valid lap this session."),
			mcp.WithString("lap", mcp.Description("Lap number to compare; default = most recent completed lap.")),
			mcp.WithString("corner", mcp.Description("Optional corner id (e.g. 'T3'); default = all curated corners.")),
			mcp.WithNumber("window_before_m", mcp.Description("Metres before the corner to inspect (default 200).")),
			mcp.WithNumber("window_after_m", mcp.Description("Metres after the corner to inspect (default 50).")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			q := map[string]string{}
			if v, ok := args["lap"].(string); ok {
				q["lap"] = v
			}
			if v, ok := args["corner"].(string); ok {
				q["corner"] = v
			}
			if v, ok := args["window_before_m"].(float64); ok && v > 0 {
				q["window_before_m"] = fmt.Sprintf("%d", int(v))
			}
			if v, ok := args["window_after_m"].(float64); ok && v > 0 {
				q["window_after_m"] = fmt.Sprintf("%d", int(v))
			}
			body, err := s.httpGet(ctx, "/api/laps/compare", q)
			if err != nil {
				return errResult(err), nil
			}
			return textResult(body), nil
		},
	)

	for _, route := range []struct {
		name, path, desc string
		params           []mcp.ToolOption
	}{
		{"get_corner_brake_history", "/api/laps/brake_points", "Brake-onset distance, min-speed, max-brake, lock-up signatures per corner across recent laps + trend (braking-later / earlier / consistent).", []mcp.ToolOption{
			mcp.WithString("corner", mcp.Description("Corner id; required.")),
			mcp.WithNumber("laps", mcp.Description("How many recent laps to include (default 10).")),
		}},
		{"get_brake_balance_report", "/api/laps/brake_balance", "Per-event left-vs-right + front-vs-rear brake-temp asymmetry + lock-up flags.", []mcp.ToolOption{
			mcp.WithString("corner", mcp.Description("Optional corner id filter.")),
			mcp.WithNumber("laps", mcp.Description("Recent laps to include (default 5).")),
		}},
		{"get_corner_coaching_report", "/api/coaching/corner_report", "Single-call synthesis: brake history + apex/exit deltas + brake-balance + rule-based diagnosis.", []mcp.ToolOption{
			mcp.WithString("corner", mcp.Description("Corner id; required.")),
		}},
	} {
		route := route
		opts := append([]mcp.ToolOption{mcp.WithDescription(route.desc)}, route.params...)
		s.mcp.AddTool(
			mcp.NewTool(route.name, opts...),
			func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				args := req.GetArguments()
				q := map[string]string{}
				if v, ok := args["corner"].(string); ok {
					q["corner"] = v
				}
				if v, ok := args["laps"].(float64); ok && v > 0 {
					q["laps"] = fmt.Sprintf("%d", int(v))
				}
				body, err := s.httpGet(ctx, route.path, q)
				if err != nil {
					return errResult(err), nil
				}
				return textResult(body), nil
			},
		)
	}
}

// ── 3. DuckDB read access ───────────────────────────────────────────────────

func (s *Server) registerDBTools() {
	s.mcp.AddTool(
		mcp.NewTool("query_sql",
			mcp.WithDescription("Run a read-only SQL query against the live DuckDB. Returns JSON rows. Use describe_schema first if you don't know the columns. NEVER hardcode car_index — derive it from get_race_state.player_car_index."),
			mcp.WithString("sql", mcp.Required(), mcp.Description("Read-only SQL (SELECT only). LIMIT recommended.")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			sqlStr, _ := args["sql"].(string)
			if strings.TrimSpace(sqlStr) == "" {
				return errResult(fmt.Errorf("sql is required")), nil
			}
			body, err := s.httpPostJSON(ctx, "/api/query", map[string]any{"sql": sqlStr})
			if err != nil {
				return errResult(err), nil
			}
			return textResult(body), nil
		},
	)

	s.mcp.AddTool(
		mcp.NewTool("describe_schema",
			mcp.WithDescription("Returns table definitions for the DuckDB telemetry database — table name, columns, types. Use to plan SQL queries."),
		),
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if s.deps.Reader == nil {
				return errResult(fmt.Errorf("db reader not configured")), nil
			}
			rows, err := s.deps.Reader.QueryContext(ctx, `
				SELECT table_name, column_name, data_type
				FROM information_schema.columns
				WHERE table_schema = 'main'
				ORDER BY table_name, ordinal_position`)
			if err != nil {
				return errResult(err), nil
			}
			defer rows.Close()
			schema := map[string][][2]string{}
			order := []string{}
			for rows.Next() {
				var t, c, ty string
				if err := rows.Scan(&t, &c, &ty); err != nil {
					continue
				}
				if _, ok := schema[t]; !ok {
					order = append(order, t)
				}
				schema[t] = append(schema[t], [2]string{c, ty})
			}
			var b strings.Builder
			for _, t := range order {
				fmt.Fprintf(&b, "## %s\n", t)
				for _, col := range schema[t] {
					fmt.Fprintf(&b, "  %-30s %s\n", col[0], col[1])
				}
				b.WriteString("\n")
			}
			return textResult(b.String()), nil
		},
	)
}

// ── 4. Insights & prior reasoning ───────────────────────────────────────────

func (s *Server) registerInsightTools() {
	s.mcp.AddTool(
		mcp.NewTool("recent_insights",
			mcp.WithDescription("The last N insights actually pushed to the driver (from any source). Use to AVOID telling the driver something they were just told."),
			mcp.WithNumber("limit", mcp.Description("Max items to return (1-200, default 25).")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			q := map[string]string{}
			if v, ok := args["limit"].(float64); ok && v > 0 {
				q["limit"] = fmt.Sprintf("%d", int(v))
			}
			body, err := s.httpGet(ctx, "/api/insights/history", q)
			if err != nil {
				return errResult(err), nil
			}
			return textResult(body), nil
		},
	)

	s.mcp.AddTool(
		mcp.NewTool("recent_pi_observations",
			mcp.WithDescription("Recent observations the pi agent itself wrote (filtered by topic prefix). Critical for self-deduplication: read this before pushing a new insight."),
			mcp.WithString("topic_prefix", mcp.Description("Filter prefix; default 'pi_agent.' (everything the pi agent has written).")),
			mcp.WithNumber("limit", mcp.Description("Max items per topic (default 5).")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if s.deps.Brain == nil {
				return errResult(fmt.Errorf("brain not configured")), nil
			}
			args := req.GetArguments()
			prefix := "pi_agent."
			if v, ok := args["topic_prefix"].(string); ok && v != "" {
				prefix = v
			}
			limit := 5
			if v, ok := args["limit"].(float64); ok && v > 0 {
				limit = int(v)
			}
			snap := s.deps.Brain.Snapshot(brain.DefaultSnapshotOpts())
			out := map[string][]brain.Observation{}
			for topic, obs := range snap.Observations {
				if !strings.HasPrefix(string(topic), prefix) {
					continue
				}
				if len(obs) > limit {
					obs = obs[len(obs)-limit:]
				}
				out[string(topic)] = obs
			}
			body, err := json.Marshal(out)
			if err != nil {
				return errResult(err), nil
			}
			return textResult(string(body)), nil
		},
	)
}

// ── 4b. Cross-session transcript access ─────────────────────────────────────
//
// Gives the pi agent the same per-session-conversation memory Gemini Live
// has. Without these tools the agent has no way to ask "what did we say
// about T8 last race?" — it only sees the current session via the brain
// snapshot. DuckDB telemetry is already cross-session via query_sql, but
// dialogue lives in workspace/sessions/*.jsonl which only the transcript
// hub knows how to read.

func (s *Server) registerTranscriptTools() {
	s.mcp.AddTool(
		mcp.NewTool("list_sessions",
			mcp.WithDescription("Roster of past F1 sessions (last ~100) with track + session-type metadata. Each entry: session_uid, track_id, track_name, session_type, session_type_name, first_seen, last_seen, total_laps, player_car_index, best_lap_ms, final_position. Use this for ANY 'last race at X' / 'past session' question — find the matching session_uid here, then pass it to query_sql (WHERE session_uid = …) against telemetry_hifreq / raw_packets, or to get_session_history(scope=<session_uid>) for dialogue."),
		),
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			body, err := s.httpGet(ctx, "/api/sessions", nil)
			if err != nil {
				return errResult(err), nil
			}
			return textResult(body), nil
		},
	)

	s.mcp.AddTool(
		mcp.NewTool("get_session_history",
			mcp.WithDescription("Recent transcript events (user_utterance, engineer_speech, tool_call, analyst_query, analyst_answer, insight_pushed, …) for the chosen scope. Use scope='previous' to read the last F1 session, scope='current' for live, scope=<session_id> for a specific past file. Critical for cross-session memory: e.g. 'what did we conclude about brakes last race?'."),
			mcp.WithString("scope", mcp.Description("'current' (default) | 'previous' | 'all' | <session_id from list_sessions>")),
			mcp.WithNumber("limit", mcp.Description("Cap on events returned (default = server TRANSCRIPT_TOOL_LIMIT, usually 200).")),
			mcp.WithString("kinds", mcp.Description("Comma-separated kinds filter: user_utterance, engineer_speech, tool_call, tool_result, analyst_query, analyst_answer, insight_pushed, event_dispatched, event_delivered, event_dropped.")),
			mcp.WithNumber("since_minutes", mcp.Description("Trim events older than now - N minutes (optional).")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			q := map[string]string{}
			if v, ok := args["scope"].(string); ok && v != "" {
				q["scope"] = v
			}
			if v, ok := args["limit"].(float64); ok && v > 0 {
				q["limit"] = fmt.Sprintf("%d", int(v))
			}
			if v, ok := args["kinds"].(string); ok && v != "" {
				q["kinds"] = v
			}
			if v, ok := args["since_minutes"].(float64); ok && v > 0 {
				q["since_minutes"] = fmt.Sprintf("%d", int(v))
			}
			body, err := s.httpGet(ctx, "/api/transcript", q)
			if err != nil {
				return errResult(err), nil
			}
			return textResult(body), nil
		},
	)
}

// ── 5. Trigger queue (the heart of the event-driven model) ──────────────────

func (s *Server) registerTriggerTools() {
	s.mcp.AddTool(
		mcp.NewTool("pull_next_trigger",
			mcp.WithDescription("Long-poll for the next trigger to react to. Returns one of: a queued query (kind='query'), a freshly-completed lap (kind='lap_complete'), or a significant rule-engine event (kind='significant_event'). When kind='query', call submit_query_answer with the same job_id when done. Returns kind='none' on timeout — call again immediately to keep waiting."),
			mcp.WithNumber("timeout_seconds", mcp.Description("Max time to block (default = server PI_AGENT_TRIGGER_TIMEOUT_SEC).")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if s.deps.Triggers == nil {
				return errResult(fmt.Errorf("trigger queue not configured")), nil
			}
			timeout := s.deps.TriggerTimeout
			if v, ok := req.GetArguments()["timeout_seconds"].(float64); ok && v > 0 {
				timeout = secsToDuration(v)
			}
			t, ok := s.deps.Triggers.Pull(ctx, timeout)
			if !ok {
				return textResult(`{"kind":"none"}`), nil
			}
			body, err := json.Marshal(t)
			if err != nil {
				return errResult(err), nil
			}
			return textResult(string(body)), nil
		},
	)

	s.mcp.AddTool(
		mcp.NewTool("submit_query_answer",
			mcp.WithDescription("Reply to a queued query trigger. Writes the answer as a brain Observation under 'analyst.<context_topic>' AND enqueues an analyst_answer brain event so the Live engineer relays it on team radio (the model decides phrasing). Urgent=true bumps the event from P3 to P4."),
			mcp.WithString("job_id", mcp.Required(), mcp.Description("The job_id from the pull_next_trigger response.")),
			mcp.WithString("answer", mcp.Required(), mcp.Description("The answer text. Conversational, ≤3 complete sentences.")),
			mcp.WithBoolean("urgent", mcp.Description("Override the trigger's urgent flag (default = inherit from trigger).")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if s.deps.Triggers == nil || s.deps.Brain == nil {
				return errResult(fmt.Errorf("trigger queue / brain not configured")), nil
			}
			args := req.GetArguments()
			jobID, _ := args["job_id"].(string)
			answer, _ := args["answer"].(string)
			if strings.TrimSpace(jobID) == "" || strings.TrimSpace(answer) == "" {
				return errResult(fmt.Errorf("job_id and answer are required")), nil
			}
			question, topic, urgent, ok := s.deps.Triggers.CompleteQuery(jobID)
			if !ok {
				return errResult(fmt.Errorf("unknown job_id %q", jobID)), nil
			}
			if v, ok := args["urgent"].(bool); ok {
				urgent = v
			}
			s.deps.Brain.Write(brain.Observation{
				Topic:   brain.Topic(brain.AnalystTopicPrefix + topic),
				Agent:   "pi_agent",
				At:      nowFn(),
				Summary: answer,
				JobID:   jobID,
				Urgent:  urgent,
			})
			s.deps.Brain.ClearPendingJob(jobID)

			priority := 3
			if urgent {
				priority = 4
			}
			ev := brain.Event{
				Type:         brain.EventAnalystAnswer,
				Priority:     priority,
				Summary:      truncate(answer, brain.MaxEventSummaryChars),
				Reasoning:    truncate(answer, brain.MaxAnalystReasoningChars),
				Source:       "pi_agent",
				DedupKey:     "analyst_answer:" + topic,
				DedupSubject: "analyst:" + topic,
				DebugData: map[string]any{
					"job_id":        jobID,
					"context_topic": topic,
					"question":      truncate(question, 200),
				},
			}
			// talkLevel=10 → bypass the per-config talk-level floor. The
			// driver explicitly asked the analyst; the answer should
			// always reach the radio regardless of the noise setting.
			if _, err := s.deps.Brain.EnqueueEvent(ev, 10); err != nil {
				log.Warn().Err(err).Str("job_id", jobID).Str("topic", topic).Int("summary_len", len(ev.Summary)).Msg("pi_agent: analyst_answer enqueue failed — observation still written")
				// Surface the failure to the caller so a model retry can fix it.
				return errResult(fmt.Errorf("analyst_answer enqueue failed: %w", err)), nil
			}
			log.Info().Str("job_id", jobID).Str("topic", topic).Bool("urgent", urgent).Int("priority", priority).Int("answer_len", len(answer)).Str("question", truncate(question, 80)).Msg("pi_agent: query answered → analyst_answer event enqueued")
			return textResult(`{"status":"ok"}`), nil
		},
	)
}

// ── 6. Write tools (gated) ──────────────────────────────────────────────────

func (s *Server) registerWriteTools() {
	s.mcp.AddTool(
		mcp.NewTool("write_observation",
			mcp.WithDescription("Record a finding under 'pi_agent.<topic>' for future ticks (and the dashboard) to see. Internal-only — does NOT speak to the driver. Use push_insight when you want the driver to hear it."),
			mcp.WithString("topic", mcp.Required(), mcp.Description("Sub-topic, e.g. 'tires', 'pace', 'strategy'. Stored as 'pi_agent.<topic>'.")),
			mcp.WithString("summary", mcp.Required(), mcp.Description("One-line headline.")),
			mcp.WithString("body", mcp.Description("Optional structured details (any JSON-encodable string).")),
			mcp.WithBoolean("hypothesis", mcp.Description("True = latest-wins per topic; false (default) = ring buffer.")),
			mcp.WithNumber("confidence", mcp.Description("0.0–1.0; default 0.7.")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if s.deps.Brain == nil {
				return errResult(fmt.Errorf("brain not configured")), nil
			}
			args := req.GetArguments()
			topic, _ := args["topic"].(string)
			summary, _ := args["summary"].(string)
			topic = strings.TrimSpace(topic)
			summary = strings.TrimSpace(summary)
			if topic == "" || summary == "" {
				return errResult(fmt.Errorf("topic and summary are required")), nil
			}
			body, _ := args["body"].(string)
			hypothesis, _ := args["hypothesis"].(bool)
			confidence := 0.7
			if v, ok := args["confidence"].(float64); ok && v > 0 {
				confidence = v
			}
			s.deps.Brain.Write(brain.Observation{
				Topic:      brain.Topic("pi_agent." + topic),
				Agent:      "pi_agent",
				At:         nowFn(),
				Summary:    summary,
				Body:       body,
				Hypothesis: hypothesis,
				Confidence: confidence,
			})
			return textResult(`{"status":"ok"}`), nil
		},
	)

	s.mcp.AddTool(
		mcp.NewTool("push_insight",
			mcp.WithDescription("Push a strategy insight onto the radio fanout. THIS REACHES THE DRIVER'S EARS. Always call recent_insights and recent_pi_observations first to avoid repeating yourself. Priority is capped server-side at PI_AGENT_MAX_PRIORITY."),
			mcp.WithString("insight", mcp.Required(), mcp.Description("Radio-style sentence, ≤2 sentences.")),
			mcp.WithNumber("priority", mcp.Description("1=info, 3=tactical, 5=critical. Default 2. Capped at PI_AGENT_MAX_PRIORITY.")),
			mcp.WithString("source", mcp.Description("Sub-source for the insight log, e.g. 'pi_agent.tires'.")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			insight, _ := args["insight"].(string)
			insight = strings.TrimSpace(insight)
			if insight == "" {
				return errResult(fmt.Errorf("insight is required")), nil
			}
			priority := 2
			if v, ok := args["priority"].(float64); ok && v > 0 {
				priority = int(v)
			}
			priority = clampPriority(priority, s.deps.MaxPriority)
			source, _ := args["source"].(string)
			if source == "" {
				source = "pi_agent"
			}
			payload := map[string]any{
				"source":      source,
				"summary":     "analyst",
				"insight":     insight,
				"recommendation": insight,
				"priority":    priority,
				"criticality": priority,
			}
			body, err := s.httpPostJSON(ctx, "/api/strategy", payload)
			if err != nil {
				return errResult(err), nil
			}
			return textResult(body), nil
		},
	)

	s.mcp.AddTool(
		mcp.NewTool("set_corner_reminder",
			mcp.WithDescription("Schedule a track-position coaching cue (≤80 chars, fires every lap or once, expires after N laps). Identical to POST /api/reminders/corner."),
			mcp.WithString("corner_id", mcp.Description("Curated corner id (e.g. 'T3'). Either corner_id or lap_distance_m is required.")),
			mcp.WithNumber("lap_distance_m", mcp.Description("Trigger at this metric distance into the lap.")),
			mcp.WithString("message", mcp.Required(), mcp.Description("Reminder text the engineer will say.")),
			mcp.WithNumber("lookahead_seconds", mcp.Description("Fire this many seconds before the trigger point (default = CORNER_REMINDER_DEFAULT_LOOKAHEAD_SEC).")),
			mcp.WithNumber("priority", mcp.Description("1-5; default 3.")),
			mcp.WithBoolean("recurring", mcp.Description("Fire every lap (default true).")),
			mcp.WithNumber("expires_in_laps", mcp.Description("Auto-cancel after N laps (default 5).")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			payload := map[string]any{}
			for _, k := range []string{"corner_id", "lap_distance_m", "message", "lookahead_seconds", "priority", "recurring", "expires_in_laps"} {
				if v, ok := args[k]; ok {
					payload[k] = v
				}
			}
			body, err := s.httpPostJSON(ctx, "/api/reminders/corner", payload)
			if err != nil {
				return errResult(err), nil
			}
			return textResult(body), nil
		},
	)

	s.mcp.AddTool(
		mcp.NewTool("cancel_corner_reminder",
			mcp.WithDescription("Cancel a previously-scheduled corner reminder by id."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Reminder id from set_corner_reminder.")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id, _ := req.GetArguments()["id"].(string)
			if strings.TrimSpace(id) == "" {
				return errResult(fmt.Errorf("id is required")), nil
			}
			body, err := s.httpDelete(ctx, "/api/reminders/"+id)
			if err != nil {
				return errResult(err), nil
			}
			return textResult(body), nil
		},
	)
}

// ── 7. Skills ───────────────────────────────────────────────────────────────

func (s *Server) registerSkillTools() {
	s.mcp.AddTool(
		mcp.NewTool("list_skills",
			mcp.WithDescription("List the agent's loadable skills (markdown playbooks in workspace/pi_skills/). Returns metadata only — call get_skill(name) to fetch a body. Always call this at the start of a tick to know what specialised guidance is available."),
			mcp.WithString("filter", mcp.Description("Substring filter (case-insensitive) on name+description+when_to_use.")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if s.deps.Skills == nil {
				return errResult(fmt.Errorf("skill registry not configured")), nil
			}
			filter, _ := req.GetArguments()["filter"].(string)
			body, err := json.Marshal(s.deps.Skills.List(filter))
			if err != nil {
				return errResult(err), nil
			}
			return textResult(string(body)), nil
		},
	)

	s.mcp.AddTool(
		mcp.NewTool("get_skill",
			mcp.WithDescription("Return the full markdown body of a skill — instructions, heuristics, sample SQL, push-criteria. Cheap; load lazily when picking a specialist."),
			mcp.WithString("name", mcp.Required(), mcp.Description("Skill name as returned by list_skills.")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if s.deps.Skills == nil {
				return errResult(fmt.Errorf("skill registry not configured")), nil
			}
			name, _ := req.GetArguments()["name"].(string)
			s, err := s.deps.Skills.Get(name)
			if err != nil {
				return errResult(err), nil
			}
			body, err := json.Marshal(s)
			if err != nil {
				return errResult(err), nil
			}
			return textResult(string(body)), nil
		},
	)
}
