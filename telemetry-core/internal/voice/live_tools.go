package voice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"google.golang.org/genai"
)

// DefaultLiveTools is the minimal Phase 3.2 tool surface. It mirrors the
// 4 tools the original migration plan listed; the wider 17-tool surface
// the Python service grew into can be folded in incrementally as we
// validate each in production. The schemas here intentionally match the
// Python service's JSON shapes so the model behaves the same.
func DefaultLiveTools() []*genai.Tool {
	return []*genai.Tool{{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name: "get_race_state",
				Description: "Fast snapshot of current telemetry: position, lap, gaps, " +
					"tire state, weather, safety car. Sub-millisecond — call this for any " +
					"\"where am I / what's happening\" question. Includes drs (1 = open/active), " +
					"drs_allowed (1 = DRS zone, can be used), drs_fault (1 = system broken).",
				ParametersJsonSchema: emptyObject(),
			},
			{
				Name: "query_brain",
				Description: "Full race-brain markdown snapshot: telemetry + recent " +
					"observations + radio history + driver state + static context. Slower " +
					"than get_race_state but answers \"what does the team think\" or " +
					"\"have I already said this\". Includes the current DRS state " +
					"(Open / Available / Not available / FAULT) inline in the live telemetry line.",
				ParametersJsonSchema: emptyObject(),
			},
			{
				Name: "ask_data_analyst",
				Description: "FIRE-AND-FORGET. Hands a heavy DuckDB analysis question " +
					"(5–30s) to the data-analyst team member and RETURNS IMMEDIATELY. " +
					"You will NOT get the answer as the tool result — instead, when the " +
					"analyst is done, a separate 'analyst_answer' team-radio event will " +
					"arrive prompting you to relay their finding to the driver. Do NOT " +
					"wait or re-call this tool. After calling, say one short line to the " +
					"driver acknowledging the check (e.g. 'Checking the data on that') " +
					"and continue handling other comms. Use only for cross-stint " +
					"comparisons, anomaly hunts, or hypothetical 'what-if' questions. " +
					"Set urgent=true to make the eventual answer arrive at higher " +
					"priority (P4 instead of P3).",
				ParametersJsonSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"question": map[string]any{
							"type":        "string",
							"description": "The strategic question to ask, in plain English.",
						},
						"context_topic": map[string]any{
							"type": "string",
							"description": "Short topic key (e.g. 'tire_deg', 'pit_window'). " +
								"Used to dedupe identical questions within 30s.",
						},
						"urgent": map[string]any{
							"type":        "boolean",
							"description": "When true, the answer is also broadcast as a strategy insight (radio + dashboard).",
						},
					},
					"required": []string{"question", "context_topic"},
				},
			},
			{
				Name: "push_strategy_insight",
				Description: "Push a radio message into the team strategy pipeline — " +
					"broadcasts to the dashboard AND fires voice TTS. Use sparingly: only " +
					"for strategic CALLS the driver needs to act on (e.g. 'Box this lap, " +
					"target hards', 'Push now, gap is opening'). Priority 1 (info) to 5 " +
					"(critical); 4+ triggers urgent voice.",
				ParametersJsonSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"summary":        map[string]any{"type": "string", "description": "Short situation description."},
						"recommendation": map[string]any{"type": "string", "description": "Action the driver should take."},
						"priority":       map[string]any{"type": "integer", "description": "1-5; 4+ triggers urgent voice broadcast."},
					},
					"required": []string{"summary", "recommendation", "priority"},
				},
			},
			// Live coaching tools — same data the dashboard's Telemetry
			// page shows. Use when the driver asks "where am I slow", "why
			// am I losing time vs my best", "how am I driving right now",
			// or about specific corners.
			{
				Name: "get_recent_telemetry",
				Description: "Last N seconds of telemetry (default 60s) — bucketed time-series of " +
					"throttle / brake / steering / speed / g-forces / brake-temp / tyre-temp / drs " +
					"PLUS summary stats (peak g-lat, max brake, brake event count, max temps). " +
					"Use for anything about the IN-PROGRESS moment — 'how am I driving right now', " +
					"'was that stint smooth', 'am I overheating brakes'. " +
					"Also returns drs_currently_open, drs_currently_allowed, drs_currently_fault, " +
					"drs_open_seconds, drs_open_events, plus a drs_events list of recent " +
					"open/close transitions with lap + track_position_m + t_seconds_ago. " +
					"Use for 'is DRS active right now', 'did I use DRS that lap', 'when did DRS open'. " +
					"Distinct from get_lap_traces (which needs completed laps).",
				ParametersJsonSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"seconds": map[string]any{"type": "integer", "description": "Lookback window in seconds (1..300). Default 60."},
						"buckets": map[string]any{"type": "integer", "description": "Time buckets across the window (10..120). Default 30."},
					},
				},
			},
			{
				Name: "list_laps",
				Description: "List completed laps in this session with lap time, sector " +
					"splits, and valid flag. Call BEFORE get_lap_traces / get_lap_delta " +
					"to pick which lap numbers to compare.",
				ParametersJsonSchema: emptyObject(),
			},
			{
				Name: "get_lap_traces",
				Description: "Bucketed channel arrays per lap (throttle / brake / steering " +
					"/ speed / gear / rpm / g_lat / g_lon / brake_temp / tyre_temp / " +
					"tyre_pressure / fuel / ers_store etc.) indexed by track distance. " +
					"Use to overlay 1–3 laps and find where inputs differ.",
				ParametersJsonSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"laps": map[string]any{
							"type":        "string",
							"description": "CSV of lap selectors. Tokens: integer lap numbers, 'best', 'last', 'current', 'recent:N'. Example: 'best,last' or 'best,23,24'.",
						},
						"channels": map[string]any{
							"type":        "string",
							"description": "CSV of channel ids. Choose from throttle, brake, speed, gear, steering, rpm, clutch, drs, g_lat, g_lon, g_vert, brake_temp, tyre_temp, tyre_inner_temp, tyre_pressure, brake_temp_fl/fr/rl/rr, tyre_temp_fl/fr/rl/rr, tyre_press_fl/fr/rl/rr, fuel, ers_store, ers_deploy_mode. Default: throttle,brake,speed.",
						},
						"buckets": map[string]any{
							"type":        "integer",
							"description": "Number of distance buckets across the lap (20–60). Default 40.",
						},
					},
					"required": []string{"laps"},
				},
			},
			{
				Name: "get_lap_delta",
				Description: "Cumulative time delta (your_lap_elapsed_ms - reference_lap_elapsed_ms) " +
					"at every distance bucket. The single most useful tool for locating " +
					"WHERE on the lap time is being gained or lost.",
				ParametersJsonSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"lap":       map[string]any{"type": "string", "description": "Lap to analyse. 'last' or a positive integer. Default: last."},
						"reference": map[string]any{"type": "string", "description": "Reference lap to subtract. 'best' or a positive integer. Default: best."},
						"buckets":   map[string]any{"type": "integer", "description": "Distance buckets (20–60). Default 40."},
					},
				},
			},
			{
				Name: "compare_lap_corners",
				Description: "Per-corner brake-point, apex-speed, and exit-throttle deltas " +
					"between a given lap and the driver's best valid lap. Curated corner " +
					"list per track id; pass corner= to filter to one.",
				ParametersJsonSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"lap":    map[string]any{"type": "integer", "description": "Lap to compare against best. Default: most recent completed lap."},
						"corner": map[string]any{"type": "string", "description": "Optional corner id to filter (e.g. 'T3'). Default: all curated corners."},
					},
				},
			},
			// Track position + reminders. The driver can ask "what corner
			// am I near" → get_track_position, or "remind me when I'm
			// near T8" → set_corner_reminder. The store fires reminders
			// automatically as the driver approaches the configured
			// distance; the model never has to repeat itself.
			{
				Name: "get_track_position",
				Description: "Where the driver is on the lap RIGHT NOW: " +
					"lap_distance_m, current corner id (if inside one), and the next " +
					"corner ahead with its id, name, and metres-to-go. Call this when " +
					"the driver says 'this corner' / 'here' / 'where am I' or before " +
					"setting a reminder when no corner id was given.",
				ParametersJsonSchema: emptyObject(),
			},
			{
				Name: "set_reminder",
				Description: "General-purpose driver-coaching reminder. Schedules a " +
					"server-side cue that fires AUTOMATICALLY at the right moment in " +
					"the driver's lap. CALL THIS — do not just say 'OK I'll remind you' " +
					"on the radio. The radio cannot remind anyone; only this tool can.\n\n" +
					"TRIGGERS — pick what fits:\n" +
					"  • corner_id: 'T8', 'turn 3', 'Stowe' (case-insensitive). Fires " +
					"as the player approaches that corner on every lap (recurring=true " +
					"default).\n" +
					"  • lap_distance_m: raw metres into the lap. Use for non-corner " +
					"positions — pit entry, DRS detection point, sector boundaries. " +
					"PAIR with `label` so the cue identifies itself ('pit entry').\n" +
					"  • at_lap: an integer lap number. ALONE means 'fire at the start " +
					"of that lap, one-shot' (use for 'pit on lap 15', 'switch fuel mix " +
					"on lap 8'). COMBINED with corner_id or lap_distance_m it means " +
					"'fire at that position ONLY on the named lap, one-shot' — perfect " +
					"for 'remind me at T8 on lap 5' or 'tell me at pit entry on lap 22'.\n\n" +
					"Rules:\n" +
					"  - corner_id and lap_distance_m are mutually exclusive (both " +
					"resolve to one trigger distance). Either may combine with at_lap.\n" +
					"  - Combined triggers are always one-shot; the `recurring` flag " +
					"is ignored for them.\n" +
					"  - For corner cues the driver wants 'every lap', omit at_lap.\n\n" +
					"USE CASES:\n" +
					"  • 'remind me to brake earlier at T3' → corner_id=T3, message='brake earlier'\n" +
					"  • 'remind me at T8 on lap 5' → corner_id=T8, at_lap=5, message='watch turn-in'\n" +
					"  • 'tell me at pit entry on lap 22' → lap_distance_m=<pit entry m>, label='pit entry', at_lap=22\n" +
					"  • 'remind me to use DRS in zone 2' → lap_distance_m=<DRS zone 2 detect>, label='DRS zone 2'\n" +
					"  • 'remind me to pit on lap 15' → at_lap=15, message='pit this lap'\n" +
					"  • Driver locking up at T8 → corner_id=T8, message='brake earlier'\n\n" +
					"If the driver said 'this corner' / 'here' / 'where I just was', call " +
					"get_track_position first to find the nearest curated corner. If no " +
					"specific cue text was given, synthesise a SHORT one yourself " +
					"(≤60 chars). Never ask the driver to spell out the message.",
				ParametersJsonSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"message": map[string]any{
							"type":        "string",
							"description": "Short engineer cue (≤80 chars). E.g. 'brake earlier, deeper apex', 'pit this lap', 'open DRS'.",
						},
						"corner_id": map[string]any{
							"type":        "string",
							"description": "Corner id (e.g. 'T1') for corner-based triggers. Mutually exclusive with lap_distance_m and at_lap.",
						},
						"lap_distance_m": map[string]any{
							"type":        "number",
							"description": "Metres into the lap for position-based triggers (pit entry, DRS zones, sector lines). Mutually exclusive with corner_id and at_lap.",
						},
						"at_lap": map[string]any{
							"type":        "integer",
							"description": "Target lap number for lap-start triggers ('pit on lap 15'). Fires once at the start of that lap. Mutually exclusive with corner_id and lap_distance_m.",
						},
						"label": map[string]any{
							"type":        "string",
							"description": "Display label for non-corner reminders ('pit entry', 'DRS zone 2'). Surfaces in the reasoning logged when the cue fires.",
						},
						"lookahead_seconds": map[string]any{
							"type":        "number",
							"description": "Position-based only: how early to fire before the trigger point. Default 1.5s (~125m @ 300 km/h).",
						},
						"recurring": map[string]any{
							"type":        "boolean",
							"description": "Position-based only: default true — fires every lap until expiry. at_lap reminders are always one-shot.",
						},
						"expires_in_laps": map[string]any{
							"type":        "integer",
							"description": "Default 5. Auto-cancels after this many laps so stale advice doesn't haunt the stint.",
						},
						"priority": map[string]any{
							"type":        "integer",
							"description": "1-5; default 3. Use 4 only for safety-critical cues.",
						},
					},
					"required": []string{"message"},
				},
			},
			{
				Name:                 "list_reminders",
				Description:          "List currently-active corner reminders so you know what's already scheduled. Returns an array with id, corner_id, message, expires_at_lap.",
				ParametersJsonSchema: emptyObject(),
			},
			{
				Name: "cancel_reminder",
				Description: "Cancel an active reminder by id. Call when the driver says " +
					"they've fixed the issue ('OK, comfortable now') or asks to drop a cue.",
				ParametersJsonSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"reminder_id": map[string]any{"type": "string", "description": "Reminder id returned by set_corner_reminder."},
					},
					"required": []string{"reminder_id"},
				},
			},
		},
	}}
}

func emptyObject() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

// HTTPToolHandler builds a ToolHandler that translates the 4 default
// tool names into REST calls against the local Go server (typically
// http://localhost:<API_PORT>). This matches the Python service's
// architecture — the model invokes a tool, we hit our own REST API.
//
// Why HTTP and not direct function calls? Three reasons:
//  1. The REST handlers already encode every shape decision (e.g. which
//     fields the model gets to see, secret masking) and we don't want a
//     parallel "Live mode" copy of those rules.
//  2. Tools may need to wait on a background job (ask_data_analyst polls
//     /api/analyst/jobs + /api/brain/snapshot until an answer lands).
//     Doing that via HTTP is identical to how Python does it today.
//  3. Wiring the Live agent into every internal package would expand
//     its dependency surface dramatically. HTTP-self-call keeps the
//     surface narrow and matches the existing pattern.
//
// HTTPEventsPoller returns a poller that leases one brain Interrupts
// Bus event from /api/events/next per call. The bus' own dedupe + lease
// semantics make repeated calls cheap and idempotent — we never get the
// same event leased to two callers.
//
// min_priority filters how loud an event has to be before the Live
// agent voices it. The Python service ran at min_priority=1 (everything
// reaches the engineer) and let the model decide what to skip; we
// match that here. Tighten via the optional knob if it gets noisy.
func HTTPEventsPoller(apiBase string, minPriority int) EventsPoller {
	if minPriority < 1 {
		minPriority = 1
	}
	if minPriority > 5 {
		minPriority = 5
	}
	client := &http.Client{Timeout: 10 * time.Second}
	url := fmt.Sprintf("%s/api/events/next?min_priority=%d", strings.TrimRight(apiBase, "/"), minPriority)
	return func(ctx context.Context) (*LiveEvent, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("events/next: %d", resp.StatusCode)
		}
		var raw struct {
			ID        string         `json:"id"`
			Type      string         `json:"type"`
			Priority  int            `json:"priority"`
			Summary   string         `json:"summary"`
			Reasoning string         `json:"reasoning"`
			DebugData map[string]any `json:"debug_data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			return nil, fmt.Errorf("events/next decode: %w", err)
		}
		// The brain returns {} (no ID) when the queue is empty — surface
		// that as nil so the pump skips the tick without logging.
		if raw.ID == "" {
			return nil, nil
		}
		return &LiveEvent{
			ID:        raw.ID,
			Type:      raw.Type,
			Priority:  raw.Priority,
			Summary:   raw.Summary,
			Reasoning: raw.Reasoning,
			DebugData: raw.DebugData,
		}, nil
	}
}

// HTTPEventsAcker closes the lease the bus opened in /api/events/next.
// Mirrors the Python service's _post_ack helper. Failure is logged but
// non-fatal — the brain's lease reaper re-queues unacked events after
// LeaseTimeout (~8s), so a missed ack doesn't strand an event.
func HTTPEventsAcker(apiBase string) EventsAcker {
	client := &http.Client{Timeout: 10 * time.Second}
	url := strings.TrimRight(apiBase, "/") + "/api/events/ack"
	return func(ctx context.Context, eventID string, outcome EventOutcome, spokenText string) error {
		body, _ := json.Marshal(map[string]any{
			"event_id":    eventID,
			"outcome":     string(outcome),
			"spoken_text": spokenText,
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("events/ack: %d", resp.StatusCode)
		}
		return nil
	}
}

// apiBase should be the same address the dashboard hits, e.g.
// "http://localhost:8081" — without trailing slash.
//
// analystTimeout is retained for callers' backwards compatibility but is
// no longer consulted: ask_data_analyst is now fire-and-forget (the
// answer arrives as a brain EventAnalystAnswer, not as a synchronous
// tool result).
func HTTPToolHandler(apiBase string, analystTimeout time.Duration) ToolHandler {
	_ = analystTimeout
	client := &http.Client{Timeout: 60 * time.Second}
	base := strings.TrimRight(apiBase, "/")
	return func(ctx context.Context, name string, args map[string]any) (any, error) {
		switch name {
		case "get_race_state":
			return httpGetJSON(ctx, client, base+"/api/state/race")
		case "query_brain":
			return httpGetText(ctx, client, base+"/api/brain/snapshot")
		case "ask_data_analyst":
			return askDataAnalyst(ctx, client, base, args)
		case "push_strategy_insight":
			return pushStrategyInsight(ctx, client, base, args)
		case "get_recent_telemetry":
			return httpGetJSON(ctx, client, base+"/api/telemetry/recent"+buildRecentTelemetryQuery(args))
		case "list_laps":
			return httpGetJSON(ctx, client, base+"/api/laps/list")
		case "get_lap_traces":
			return httpGetJSON(ctx, client, base+"/api/laps/traces"+buildLapTraceQuery(args, 40))
		case "get_lap_delta":
			return httpGetJSON(ctx, client, base+"/api/laps/delta"+buildLapDeltaQuery(args, 40))
		case "compare_lap_corners":
			return httpGetJSON(ctx, client, base+"/api/laps/compare"+buildLapCompareQuery(args))
		case "get_track_position":
			return httpGetJSON(ctx, client, base+"/api/state/track_position")
		case "set_reminder", "set_corner_reminder": // back-compat: old name still routes here
			return setReminder(ctx, client, base, args)
		case "list_reminders":
			return httpGetJSON(ctx, client, base+"/api/reminders")
		case "cancel_reminder":
			return cancelReminder(ctx, client, base, args)
		}
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}

// setReminder POSTs to /api/reminders/corner (kept as the legacy path; it
// now handles all three trigger kinds — corner, lap_distance_m, at_lap).
// The Go REST handler enforces "message required + exactly one trigger"
// and returns its own error string, which we surface to the model.
func setReminder(ctx context.Context, client *http.Client, base string, args map[string]any) (any, error) {
	message, _ := args["message"].(string)
	if strings.TrimSpace(message) == "" {
		return nil, errors.New("message is required")
	}
	body := map[string]any{"message": strings.TrimSpace(message)}
	if v, ok := args["corner_id"].(string); ok && strings.TrimSpace(v) != "" {
		body["corner_id"] = strings.TrimSpace(v)
	}
	if v, ok := args["lap_distance_m"]; ok && v != nil {
		switch x := v.(type) {
		case float64:
			body["lap_distance_m"] = x
		case int:
			body["lap_distance_m"] = float64(x)
		case string:
			if n, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
				body["lap_distance_m"] = n
			}
		}
	}
	if n := intArg(args, "at_lap", 0); n > 0 {
		body["at_lap"] = n
	}
	if v, ok := args["label"].(string); ok && strings.TrimSpace(v) != "" {
		body["label"] = strings.TrimSpace(v)
	}
	// Local sanity-check so the model gets a fast, specific error instead
	// of generic 400 noise. The server re-validates anyway.
	triggers := 0
	for _, k := range []string{"corner_id", "lap_distance_m", "at_lap"} {
		if _, ok := body[k]; ok {
			triggers++
		}
	}
	if triggers == 0 {
		return nil, errors.New("supply exactly one of corner_id, lap_distance_m, at_lap — call get_track_position first if you don't know which corner the driver means")
	}
	if triggers > 1 {
		return nil, errors.New("supply exactly one of corner_id, lap_distance_m, at_lap (not multiple)")
	}
	if v, ok := args["lookahead_seconds"].(float64); ok {
		body["lookahead_seconds"] = v
	}
	if v, ok := args["recurring"].(bool); ok {
		body["recurring"] = v
	}
	if n := intArg(args, "expires_in_laps", 0); n > 0 {
		body["expires_in_laps"] = n
	}
	if n := intArg(args, "priority", 0); n > 0 {
		body["priority"] = n
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/reminders/corner", strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("set_reminder: %w", err)
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("set_reminder: %d %s", resp.StatusCode, string(bodyBytes))
	}
	var out any
	if err := json.Unmarshal(bodyBytes, &out); err != nil {
		return nil, fmt.Errorf("set_reminder decode: %w", err)
	}
	return out, nil
}

// cancelReminder DELETEs /api/reminders/:id.
func cancelReminder(ctx context.Context, client *http.Client, base string, args map[string]any) (any, error) {
	id, _ := args["reminder_id"].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("reminder_id is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, base+"/api/reminders/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cancel_reminder: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("cancel_reminder: %d %s", resp.StatusCode, string(body))
	}
	return map[string]any{"reminder_id": id, "status": "cancelled"}, nil
}

// httpGetJSON GETs a JSON endpoint and returns the parsed object. Used
// by tools whose REST endpoint returns a structured response we can
// pass straight back to the model.
func httpGetJSON(ctx context.Context, client *http.Client, url string) (any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("GET %s: %d %s", url, resp.StatusCode, string(body))
	}
	var out any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode %s: %w", url, err)
	}
	return out, nil
}

// httpGetText GETs an endpoint that returns plain text (e.g. the brain
// snapshot markdown) and wraps it in {"text": ...} so the model sees a
// consistent map shape across every tool.
func httpGetText(ctx context.Context, client *http.Client, url string) (any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, err
	}
	return map[string]any{"text": string(body)}, nil
}

// askDataAnalyst submits an /api/analyst/query and returns immediately —
// fire-and-forget. The actual analyst answer is published asynchronously
// by pi_agent's submit_query_answer MCP tool as a brain
// EventAnalystAnswer, which the Live agent's event pump injects as its
// own synthetic team-radio turn. This frees the model to keep talking to
// the driver instead of blocking the Live session for 5–30s while a
// heavy DuckDB analysis runs.
//
// The tool's response is just an ack: the model is expected to relay a
// short "checking with the data analyst" line on the radio and carry
// on. The system prompt teaches it to expect the team-radio event later.
func askDataAnalyst(ctx context.Context, client *http.Client, base string, args map[string]any) (any, error) {
	question, _ := args["question"].(string)
	topic, _ := args["context_topic"].(string)
	urgent, _ := args["urgent"].(bool)
	if strings.TrimSpace(question) == "" {
		return nil, errors.New("question is required")
	}
	if strings.TrimSpace(topic) == "" {
		return nil, errors.New("context_topic is required")
	}

	body, _ := json.Marshal(map[string]any{
		"question":      question,
		"context_topic": topic,
		"urgent":        urgent,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/analyst/query", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("analyst submit: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("analyst submit: %d", resp.StatusCode)
	}
	var submit struct {
		JobID      string `json:"job_id"`
		Status     string `json:"status"`
		EtaSeconds int    `json:"eta_seconds"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&submit); err != nil {
		return nil, fmt.Errorf("analyst submit decode: %w", err)
	}

	return map[string]any{
		"status":        "submitted",
		"job_id":        submit.JobID,
		"eta_seconds":   submit.EtaSeconds,
		"context_topic": topic,
		"urgent":        urgent,
		"message":       "Query is with the data analyst — you'll hear them on team radio as an analyst_answer event when they're done. Acknowledge to the driver briefly and carry on.",
	}, nil
}

// pushStrategyInsight POSTs to /api/strategy so the dashboard + voice
// fanout broadcasts the model's strategic call to the driver.
func pushStrategyInsight(ctx context.Context, client *http.Client, base string, args map[string]any) (any, error) {
	summary, _ := args["summary"].(string)
	rec, _ := args["recommendation"].(string)
	priority := intArg(args, "priority", 3)
	if strings.TrimSpace(summary) == "" {
		return nil, errors.New("summary is required")
	}
	if strings.TrimSpace(rec) == "" {
		return nil, errors.New("recommendation is required")
	}
	body, _ := json.Marshal(map[string]any{
		"source":   "gemini_live",
		"insight":  summary + ". " + rec,
		"priority": priority,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/strategy", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("strategy push: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("strategy push: %d %s", resp.StatusCode, string(body))
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out, nil
}

// intArg coerces a tool argument into an int. The model can send
// numbers as float64 (default JSON number type), strings ("3"), or the
// expected int — handle all three so we don't reject valid calls.
func intArg(args map[string]any, key string, fallback int) int {
	v, ok := args[key]
	if !ok {
		return fallback
	}
	switch x := v.(type) {
	case int:
		return x
	case int32:
		return int(x)
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
			return n
		}
	}
	return fallback
}

// Ensure url is imported even when only used in tests (linter quietener).
var _ = url.Parse

// buildLapTraceQuery serialises a Gemini Live tool-call's args map into the
// query-string format /api/laps/traces expects. Empty query is fine; the
// handler falls back to its own defaults.
func buildLapTraceQuery(args map[string]any, defaultBuckets int) string {
	q := url.Values{}
	if v, ok := args["laps"].(string); ok && strings.TrimSpace(v) != "" {
		q.Set("laps", v)
	}
	if v, ok := args["channels"].(string); ok && strings.TrimSpace(v) != "" {
		q.Set("channels", v)
	}
	if b := clampBuckets(intArg(args, "buckets", defaultBuckets)); b > 0 {
		q.Set("buckets", strconv.Itoa(b))
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

func buildLapDeltaQuery(args map[string]any, defaultBuckets int) string {
	q := url.Values{}
	if v, ok := args["lap"].(string); ok && strings.TrimSpace(v) != "" {
		q.Set("lap", v)
	}
	if v, ok := args["reference"].(string); ok && strings.TrimSpace(v) != "" {
		q.Set("reference", v)
	}
	if b := clampBuckets(intArg(args, "buckets", defaultBuckets)); b > 0 {
		q.Set("buckets", strconv.Itoa(b))
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

func buildRecentTelemetryQuery(args map[string]any) string {
	q := url.Values{}
	if n := intArg(args, "seconds", 0); n > 0 {
		q.Set("seconds", strconv.Itoa(n))
	}
	if n := intArg(args, "buckets", 0); n > 0 {
		q.Set("buckets", strconv.Itoa(n))
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

func buildLapCompareQuery(args map[string]any) string {
	q := url.Values{}
	if n := intArg(args, "lap", 0); n > 0 {
		q.Set("lap", strconv.Itoa(n))
	}
	if v, ok := args["corner"].(string); ok && strings.TrimSpace(v) != "" {
		q.Set("corner", v)
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

// clampBuckets keeps LLM-readable response sizes by squeezing the bucket
// count into 20..60 (the handler permits up to 200 for the dashboard but
// that's noisy in chat context).
func clampBuckets(n int) int {
	if n <= 0 {
		return 0
	}
	if n < 20 {
		return 20
	}
	if n > 60 {
		return 60
	}
	return n
}
