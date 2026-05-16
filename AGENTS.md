# Race Engineer — Agent Guide

AI race engineer for F1 25. Ingests UDP telemetry, stores in DuckDB, serves a React
dashboard, and provides voice-driven strategy via Gemini (Live + tools).

## Architecture

```
F1 25 Game (UDP :20777) ──▶ Go Telemetry Core (:8081) ──▶ React Dashboard (:5173)
                                  │
                                  └─▶ workspace/telemetry.duckdb
```

**Phase 3: pure Go runtime.** TTS, STT, and Gemini Live all run in-binary. The
default dev flow is **2 processes**: Go telemetry-core + Vite dashboard.

```bash
make dev          # Go + Vite, no Python (recommended)
make mock         # Same as dev with TELEMETRY_MODE=mock
make start        # Legacy: also launches Python voice/Live services (A/B only)
make stop
make build        # Build every CLI under workspace/bin/
```

### Desktop app (Wails)

`desktop/RaceEngineer/` is a Wails bundle that links the same `telemetry-core`
server lib and serves an embedded copy of `dashboard/dist/`.

```bash
make app          # → desktop/RaceEngineer/build/bin/RaceEngineer.app
make install-app  # build + swap into /Applications/RaceEngineer.app
```

Note: `dashboard/`'s `npm run build` uses `tsc -b` (stricter than `tsc --noEmit`).

## Project layout

```
telemetry-core/          Go service
  cmd/                   server, racedb, insightlog, configtool, bakecenterline, buttonwatch, ...
  internal/
    api/                 Fiber REST + WS handlers
    brain/               Race brain (in-memory observations, events, snapshot)
    config/              JSON config + schema (single source of truth)
    enums/               F1 enum tables (track_id, session_type, compound, weather, ...)
    ingestion/           UDP packet parsing
    insights/            Rule engine, corner reminders, proximity watcher
    intelligence/        CommsGate, Gemini advisor, analyst
    mcp/                 MCP server (pi-agent tool surface)
    plan/                Race plan store
    state/               Atomic latest-state cache (race/tires/energy/competitors/pace/...)
    storage/             DuckDB schema + reader
    trackmap/            Curated corner geometry + centerline baking
    transcript/          Per-session JSONL transcript writer
    voice/               In-Go Gemini Live agent + Live tools
    audio/               External-music-player ducking or pausing (per-OS: macOS osascript/AppleScript, Linux pactl + playerctl/MPRIS, Windows WASAPI + media key)
dashboard/               React 19 + Vite + Tailwind v4
python/pi_agent/         Sandboxed pi-agent (only spawned when PI_AGENT_MODE=on)
workspace/               Context files + DuckDB + sessions/ + tracks/ + pi_skills/
workspace/bin/           Built CLIs
```

## Configuration

**Source of truth: `~/.race-engineer/config.json`.** Schema declared in
`telemetry-core/internal/config/schema.go` — one entry there picks up across Go,
CLI, dashboard, and Python services.

```bash
./workspace/bin/configtool show              # inspect
./workspace/bin/configtool set KEY VALUE     # edit
```

Env vars still work as fallback (one-time `WARN: env var X deprecated` per key);
set `RACE_ENGINEER_CONFIG_STRICT=1` to disable that fallback entirely.

Common knobs (see `schema.go` for the full list):

| Key | Default | Notes |
|---|---|---|
| `TELEMETRY_MODE` | `real` | `real` or `mock` |
| `TELEMETRY_PORT` | `20777` | F1 25 UDP port |
| `UDP_MODE` | `broadcast` | or `unicast` |
| `API_PORT` | `8081` | REST + WS |
| `LLM_PROVIDER` | `gemini` | `gemini`, `anthropic`, `openai` |
| `VOICE_MODE` | `live_only` | `live_only` \| `ptt_only` \| `both` (Live owns audio out) |
| `PI_AGENT_MODE` | `off` | `on` spawns `pi_agent_service.py` |
| `HIFREQ_SAMPLE_RATE` | `2` | 20Hz UDP / N → hi-freq sampler stride (`0` to disable) |
| `WS_PUSH_RATE` | `10` | Hz |
| `TALK_LEVEL` | `5` | Insight verbosity 1–10 |
| `PTT_TRIGGER` / `PTT_BUTTON*` / `PTT_MODE` | — | Push-to-talk button mapping (see schema) |
| `AUDIO_DUCKING_ENABLED` | `false` | Lower (or pause) external music players (Spotify, Apple Music, browsers, VLC) while the engineer speaks. Per-OS code in `internal/audio/`. Surfaced in dashboard Settings → **Audio** section. |
| `AUDIO_DUCKING_MODE` | `duck` | `duck` (lower volume) or `pause` (pause/resume the player so the song doesn't progress). Pause uses AppleScript on macOS, MPRIS `playerctl` on Linux, and the global media key on Windows. |
| `AUDIO_DUCKING_LEVEL` | `0.3` | Target volume (0.0–1.0) for ducked players (ignored in pause mode) |
| `AUDIO_DUCKING_TARGETS` | — | Comma-separated app names; empty = OS defaults |
| `AUDIO_DUCKING_TAIL_MS` | `800` | Tail (ms) after last Live audio chunk before un-ducking |

> **Adding a new config key — full checklist.** A new key isn't user-visible until *all* of these are done:
> 1. `telemetry-core/internal/config/schema.go` — append `Field` to `Schema` (drives configtool + `/api/config/schema`).
> 2. `telemetry-core/internal/config/config.go` — add struct field + `src.foo("KEY", default)` in `Load()`.
> 3. `telemetry-core/internal/api/config_handlers.go` — add entry to `keyRegistry` (POST `/api/config` rejects unknown keys).
> 4. `dashboard/src/pages/Settings.tsx` — add field (or new section) to `SECTIONS`; Settings UI is hardcoded, not auto-rendered.
> 5. Wire the actual behaviour wherever the key is consumed.
>
> Forgetting (3) makes the dashboard save look successful but silently rejected; forgetting (4) means the key is only reachable via `configtool`.

## DuckDB

Tables: `telemetry`, `session_data`, `lap_data`, `car_status`, `car_damage`,
`car_telemetry_ext`, `motion_data`, `race_events`, `session_history`,
`raw_packets`, `telemetry_hifreq`. All carry `timestamp DEFAULT CURRENT_TIMESTAMP`.

**`telemetry_hifreq` is special** — player-only ~30Hz wide row (stride=2 on the
60Hz CarTelemetry packet at default `HIFREQ_SAMPLE_RATE`) keyed by
`(session_uid, total_distance)` with every channel needed to bucket a lap by
`track_position` without cross-table joins. Powers all `/api/laps/*` lap-trace
and brake-point coaching tools.

Wide-row columns: driver inputs (throttle, brake, steering, clutch),
powertrain (speed, gear, rpm, drs, drs_allowed, ers_*), per-wheel temps /
pressures, G-forces (lat/lon/vert), world position, fuel, driver-setting
mirrors (brake_bias, diff_on_throttle, engine_braking), and **the "is the car
sliding?" channels**: `wheel_slip_ratio_{fl,fr,rl,rr}` and
`wheel_slip_angle_{fl,fr,rl,rr}` (from `MotionEx` packet 13) plus per-wheel
`surface_type_{fl,fr,rl,rr}` (0=tarmac, 1=kerb, 4=gravel, 7=grass, …).
Exposed as channels in `/api/laps/traces` (`slip_ratio`, `slip_angle`,
`surface` plus per-wheel breakouts).

Three tables carry `session_uid` directly: `telemetry_hifreq`, `raw_packets`,
`session_history`. Every other table only has `timestamp` — scope past-session
queries by `list_sessions` → bound by the `[first_seen, last_seen]` window for
that uid, and filter by the `player_car_index` it returns.

For the full schema use `./workspace/bin/racedb --schema` and `--tables`.
For enum integer→name mappings see `telemetry-core/internal/enums/enums.go`.

### Player car index

Changes every session. Never hardcode it.

```bash
curl -s http://localhost:8081/api/telemetry/latest | jq '.player_car_index'
```

Use it in every `WHERE car_index = ...` clause.

## CLIs (`./workspace/bin/`)

- **`racedb "SQL"`** — read-only DuckDB query, JSON output. `--tables`, `--schema`.
- **`insightlog`** — read the running server's insight history. `-limit`, `-json`, `-api`. Run before pushing a new insight to avoid repeating yourself.
- **`configtool show | set KEY VALUE`** — manage `~/.race-engineer/config.json`.
- **`bakecenterline -track <id> [-lap best|last|N]`** — bake one driven lap into `workspace/tracks/<id>.json` (curated racing line).
- **`buttonwatch`** — log BUTN bitmask presses while mapping wheel buttons to PTT.

## REST API

### Telemetry / state

- `GET /api/telemetry/latest` — atomic race-state snapshot (includes `player_car_index`).
- `GET /api/telemetry/recent?seconds=60&buckets=30` — rolling-window bucketed channels + summary stats (peak g_lat/g_lon, brake events, max temps). "Was that stint smooth/stabby/overheating".
- `GET /api/state/{race,tires,energy,competitors,pace,events,track_position,proximity,nearby_cars}` — facet-specific snapshots from the in-memory state cache.
- `GET /api/state/nearby_cars?ahead=5&behind=5` — top N ahead + behind on track, each with `signed_m`, `closing_mps`, `eta_sec`, `threat`. Symmetric companion to the closing-only proximity watcher.
- `GET /api/track/outline`, `GET /api/roster`.
- `GET /ws` — live push of telemetry / insights / health.

### Laps & coaching (powered by `telemetry_hifreq`)

- `GET /api/laps/list?session_uid=…` — lightweight roster of completed laps. Call first to pick lap numbers.
- `GET /api/laps/traces?laps=best,current,last,recent:3,25&channels=…&buckets=80&session_uid=…` — bucketed channel arrays (throttle/brake/speed/gear/steering/rpm/g-forces/temps/pressures/fuel/ERS/`slip_ratio`/`slip_angle`/`surface` plus per-wheel breakouts). Range 20–200 buckets. Each `laps=` token can carry an `@<session_uid>` suffix to stack laps from different sessions on one chart.
- `GET /api/laps/delta?lap=25&reference=best&buckets=80&session_uid=…&lap_session_uid=…&reference_session_uid=…` — cumulative ms delta vs reference at every distance. Most actionable "where am I losing time?" view. Cross-session via `lap_session_uid` + `reference_session_uid` (same-track only).
- `GET /api/laps/compare?lap=25&corner=T3&reference_session_uid=…&reference_lap=…` — per-corner brake-point + apex-speed + exit-throttle deltas vs best valid lap. `reference_session_uid` pulls the baseline from a different same-track session ("vs my best ever at Monza").
- `GET /api/laps/best_at_track?track_id=…&session_type=race|quali|practice|all` — all-time best valid lap at a track (defaults to the live session's track). Returns `session_uid` + `lap` + `lap_time_ms` — chain into `delta`/`compare` via the `reference_session_uid` knobs.
- `GET /api/laps/snapshot?lap=…&reference=best&width=80&channels=…` — text/plain ASCII strip-chart snapshot of one lap (throttle/brake/speed/gear/slip/DRS/surface + delta-vs-reference). Designed for LLM glance-at-the-lap consumption; no image pipeline.
- `GET /api/laps/{brake_points,brake_balance}`, `GET /api/coaching/corner_report`.

### Sessions / history

- `GET /api/sessions` — last ~100 sessions: `{session_uid, track_name, session_type_name, first_seen, last_seen, total_laps, best_lap_ms, final_position, player_car_index}`. Start here for "last race at X" questions.
- `GET /api/sessions/:uid/summary`, `GET /api/stats/career`.
- `GET /api/transcript?scope=current&limit=50&kinds=…&since_minutes=10` — JSONL conversation log.
- `GET /api/transcript/sessions`.

### Brain / analyst / pi-agent

- `GET /api/brain/snapshot[?format=json]` — telemetry + observations + recent radio + driver state + static context (markdown by default).
- `POST /api/analyst/query` — async strategy Q&A: `{"question": "...", "context_topic": "...", "urgent": false}` → `202` with `job_id`. Answer lands as Observation under `analyst.<context_topic>`; `urgent=true` also broadcasts via `/api/strategy`. Deduped 30s on `(question, context_topic)`. When `PI_AGENT_MODE=on`, routes through pi-agent.
- `GET /api/analyst/jobs` — in-flight jobs.
- `GET /api/pi_agent/recent`, `GET /api/pi_agent/stream`.
- `GET /api/agent/status`.

### Insights / strategy / driver query

- `POST /api/strategy` — push insight: `{"source", "insight", "priority": 1-5}`. Priority 4+ triggers voice. **Check `insightlog -limit 20` first** to avoid repeating yourself.
- `POST /api/driver_query` — `{"query": "..."}`.
- `GET /api/insights/{next,history}`.
- `GET /api/workspace` — `workspace/insights.md` plaintext.

### Events / reminders / plan

- `GET /api/events/{next,peek,pending}`, `POST /api/events/{interrupt,spoken,ack,:id/ack,:id/nag}`.
- `POST /api/reminders/corner` — general-purpose driver-coaching reminder. Triggers: `corner_id`, or `lap_distance_m`+`label`, or `at_lap` (alone = one-shot lap entry; combined with a position = one-shot at that position on that lap). `corner_id` and `lap_distance_m` are mutually exclusive.
- `GET /api/reminders` (each entry has `trigger_kind`: `corner|distance|lap`), `DELETE /api/reminders/:id`.
- `GET /api/plan/current`, `POST /api/plan/entries`, `PATCH /api/plan/entries/:id`, `DELETE /api/plan/entries/:id`.

### Voice / settings / admin

- `POST /api/voice` — PTT multipart audio → STT → CommsGate query.
- `GET /api/voice/live` — Gemini Live WebSocket.
- `POST /api/settings/{mode,talk_level,verbosity}`, `GET/POST /api/settings/mock/overrides`.
- `GET /api/config`, `POST /api/config`, `GET /api/config/schema`, `POST /api/server/restart`.
- `POST /api/query` — read-only SQL: `{"sql": "..."}`.

## Intelligence pipeline

```
Rule Engine (Go thresholds) ─▶ CommsGate (LLM: dedup + talk-level + radio translate) ─▶ {WS, TTS, insights.md}
```

- **Rule engine** (`internal/insights/`) — fires on tire wear, fuel, damage, weather, safety car, proximity, etc. Proximity emits a single unified `traffic_update` (P2) event carrying the top 5 ahead + top 5 behind with `signed_m`, `closing_mps`, `eta_sec`, `threat`, `speed_kmh` whenever any car closes within ~5s TTI in either direction — no separate "approaching"/"catching" events.
- **CommsGate** (`internal/intelligence/comms_gate.go`) — central gate. `BuildContext()` injects track, lap, position, gaps, tire compound/age/wear, fuel/ERS/DRS, damage, weather, pit window. Last `TRANSCRIPT_PROMPT_LINES` transcript events are also injected so the LLM has cross-turn memory.
- **Fanout** — WebSocket → dashboard, in-Go TTS → audio, `workspace/insights.md` append.

### Pi Agent (`PI_AGENT_MODE=on`)

Sandboxed Python child (`pi_agent_service.py` + `python/pi_agent/`). Connects to
the in-Go MCP server (mounted at `/mcp`) as its **only** outward channel — no
shell, no `subprocess`/`requests`/`httpx`. CI test `tests/test_pi_agent_sandbox.py`
enforces the absent-imports rule.

Event-driven, no ticker. A planner long-polls `pull_next_trigger` and dispatches
one of: `tires`, `brakes`, `pace`, `strategy`, `weather`, `energy`, `driving`,
or `responder` (queries). Trigger sources, Go-side:

1. `POST /api/analyst/query` (preserves the old analyst contract).
2. Lap-complete rollover (once per player lap).
3. Every rule-engine insight.

Specialists load playbooks from `workspace/pi_skills/*.md` via `get_skill(name)`
and write back through a focused MCP subset:

- `write_observation(topic, …)` → brain at `pi_agent.<topic>`.
- `submit_query_answer(job_id, answer, urgent)` → brain at `analyst.<context_topic>`.
- `push_insight(insight, priority, source)` → `/api/strategy`, capped at `PI_AGENT_MAX_PRIORITY` (default 3).

Add a skill: drop a markdown file into `workspace/pi_skills/` (see `_index.md` for
frontmatter). Add an MCP tool: register in `telemetry-core/internal/mcp/tools.go`.

#### Data access surface

| Need | Tool |
|---|---|
| Live state | `get_race_state`, `get_state_*` |
| Per-lap / per-corner coaching | `list_laps`, `get_lap_traces`, `get_lap_delta`, `compare_lap_corners`, `get_corner_brake_history`, `get_brake_balance_report`, `get_corner_coaching_report`, `get_lap_snapshot` (ASCII strip-chart) |
| Cross-session "best ever at this track" | `get_best_lap_at_track` → chain into `get_lap_delta` / `compare_lap_corners` / `get_lap_snapshot` via `reference_session_uid` |
| Past F1 sessions roster | `list_sessions` (start here for "last race at X") |
| Arbitrary historical SQL | `query_sql` + `describe_schema` (no session filter is applied — scope it yourself per the timestamp recipe above) |
| Cross-session dialogue | `get_session_history(scope='previous'` \| `'all'` \| `<session_uid>)` |
| What this session already concluded | `get_brain_snapshot`, `recent_pi_observations`, `recent_insights` |

## Gemini Live (in-Go)

`telemetry-core/internal/voice/` holds the Live agent. It streams the brain
snapshot into context every `GEMINI_LIVE_BRAIN_POLL_SEC` seconds via
`send_client_content(turn_complete=False)` so the model always sees current
telemetry + analyst observations + recent radio.

Tools registered on the Live agent (mirrored on the Strategy Analyst):
`get_race_state`, `query_brain`, `ask_data_analyst`, `push_strategy_insight`,
`get_recent_telemetry`, `list_sessions`, `list_laps`, `get_lap_traces`,
`get_lap_delta`, `compare_lap_corners`, `get_lap_snapshot`,
`get_best_lap_at_track`, `get_track_position`, `get_nearby_cars`,
`set_reminder` (back-compat: `set_corner_reminder`), `list_reminders`,
`cancel_reminder`, `get_session_history`. The lap-data tools all accept
`session_uid` (and `get_lap_delta` / `get_lap_snapshot` accept
`lap_session_uid` + `reference_session_uid`) so the voice agent can compare
the current lap against any past session on the same track.

`gemini_live_service.py` is the legacy standalone Python path — only used when
`VOICE_MODE=both` (or `GEMINI_LIVE_ENABLED=true` with `VOICE_MODE` empty).

## Workspace files

| Path | Purpose |
|---|---|
| `workspace/driver_profile.md` | Driver style, comm prefs, strengths |
| `workspace/track_setup.md` | Track characteristics + setup notes |
| `workspace/past_learnings.md` | Historical debriefs |
| `workspace/insights.md` | Live state + accumulated findings (written by insight engine) |
| `workspace/soul.md` | Race engineer personality / radio style |
| `workspace/user.md` | Driver communication preferences |
| `workspace/sessions/sess_<uid>.jsonl` | Per-F1-session transcript (append-only, splits at 10k lines into `-partN`) |
| `workspace/pi_skills/*.md` | Pi-agent specialist playbooks |
| `workspace/tracks/<id>.json` | Baked centerlines (output of `bakecenterline`) |
| `workspace/telemetry.duckdb` | Database |

Root-level `SOUL.md` / `USER.md` are back-compat fallbacks — code loads
`workspace/` first.

### Per-session transcript

Every LLM surface (CommsGate, analyst, voice, Live) writes one shared JSONL per
F1 `session_uid`. Closed `kind` enum: `user_utterance`, `engineer_speech`,
`tool_call`, `tool_result`, `analyst_query`, `analyst_answer`, `insight_pushed`,
`event_dispatched`, `event_delivered`, `event_dropped`. Last
`TRANSCRIPT_PROMPT_LINES` (default 25) auto-injected into CommsGate prompts and
brain snapshot. Older events via `get_session_history(scope, limit, kinds, since_minutes)`.
Retention: last `TRANSCRIPT_RETENTION_SESSIONS` (default 30) files kept.

## Writing effective insights

Before `POST /api/strategy`, check recent history (`insightlog -limit 20`).
**Don't push** if it repeats anything from the last ~5 min, restates an obvious
fact (e.g. position unchanged), or contradicts a more recent insight. **Do push**
for new developments (weather, competitor pit, damage), actionable strategy
(pit window, undercut, tire cliff), or significant state changes (safety car,
position change, DRS gained).

Priorities: **1–2** info · **3** tactical · **4** urgent (triggers voice) · **5** critical.
Be specific and numerical: "Gap to Hamilton 1.2s closing 0.3s/lap" beats "gap closing".
One topic per message. Sound like a real F1 engineer.
