# Race Engineer — Agent Guide

AI-powered F1 race engineer for F1 25. Ingests real-time UDP telemetry, stores it in DuckDB, serves a React dashboard, and provides voice-driven strategic advice via Gemini.

## Architecture

```
F1 25 Game (UDP :20777)
        │
        ▼
┌─────────────────────┐     ┌─────────────────┐     ┌─────────────────┐
│  Go Telemetry Core  │────▶│  React Dashboard │     │  Python Voice   │
│  (port 8081)        │ ws  │  (port 5173)     │     │  (port 8000)    │
│                     │     └─────────────────┘     └─────────────────┘
│  - UDP ingestion    │              │                       │
│  - DuckDB storage   │◀─────── REST API ──────────────────┘
│  - Gemini advisor   │
│  - Insight engine   │
└─────────────────────┘
        │
        ▼
  workspace/telemetry.duckdb
```

**3 processes** launched via `make start`:
1. **Go Telemetry Core** — UDP listener, DuckDB writer, REST API, Gemini advisor
2. **React Dashboard** — Live telemetry visualization (Vite + React 19 + Tailwind v4)
3. **Python Voice Service** — edge-tts synthesis + Whisper STT transcription for radio comms

## Desktop app (Wails)

In addition to the dev `make start` flow, there is a **user-facing Mac
app** that ships to `/Applications/RaceEngineer.app`. It is a
[Wails](https://wails.io/) bundle whose source lives at
**`desktop/RaceEngineer/`**: a Go entrypoint (`main.go`, `app.go`) that
links the same `telemetry-core` server library and serves the React
dashboard from an embedded `frontend/dist/` (rebuilt from `dashboard/`
via `build-frontend.sh` on every Wails build).

The desktop app is independent of `make start` — you can develop with
`make start` (browser) and ship with the .app (native window) side-by-
side. Most agent work touches the Go core or the dashboard, both of
which are shared, so changes flow into the .app on the next
`make app` / `make install-app`.

Build & install targets (from repo root):

| Target | What it does |
|---|---|
| `make app` | Build the Wails bundle into `desktop/RaceEngineer/build/bin/RaceEngineer.app`. Requires the `wails` CLI at `~/go/bin/wails` (override with `WAILS=…`). |
| `make install-app` | Run `make app` then quit the running `.app` (via `osascript`) and swap the bundle into `/Applications/RaceEngineer.app`. Echoes the installed `CFBundleShortVersionString` at the end. |

`make install-app` is the canonical "ship a new version of the app"
command. See `desktop/RaceEngineer/README.md` for layout, version
bumping (`wails.json` → `info.productVersion`), and common gotchas
(notably that `dashboard/`'s `npm run build` uses `tsc -b` which is
stricter than `tsc --noEmit`).

## Querying Telemetry Data

### CLI tool: `./workspace/bin/racedb`

A read-only CLI that queries DuckDB directly. Safe to run while the server is live.

```bash
# Run a SQL query (returns JSON array)
./workspace/bin/racedb "SELECT * FROM car_damage ORDER BY timestamp DESC LIMIT 5"

# List all tables with row counts
./workspace/bin/racedb --tables

# Show full schema (all columns and types)
./workspace/bin/racedb --schema
```

Build it: `cd telemetry-core && go build -o ../workspace/bin/racedb cmd/query/main.go`

The database defaults to `workspace/telemetry.duckdb`. Override with `DB_PATH` env var.

### CLI tool: `./workspace/bin/insightlog`

Queries the running server's insight history log. Use this before publishing insights to check what's already been said.

```bash
# Show last 50 insights (default)
./workspace/bin/insightlog

# Show last 20 insights
./workspace/bin/insightlog -limit 20

# Output raw JSON (for piping to other tools)
./workspace/bin/insightlog -json

# Use a custom API URL
./workspace/bin/insightlog -api http://localhost:8081
```

Build it: `cd telemetry-core && go build -o ../workspace/bin/insightlog cmd/insightlog/main.go`

The API URL defaults to `http://localhost:8081`. Override with `-api` flag or `API_URL` env var.

### REST API query endpoint

```bash
curl -X POST http://localhost:8081/api/query \
  -H "Content-Type: application/json" \
  -d '{"sql": "SELECT COUNT(*) as n FROM lap_data"}'
```

## DuckDB Schema

The database has 10 tables. All include `timestamp DEFAULT CURRENT_TIMESTAMP`.

### telemetry
Core driving data sampled at 1Hz.
| Column | Type |
|--------|------|
| speed | DOUBLE |
| gear | INTEGER |
| throttle | DOUBLE |
| brake | DOUBLE |
| steering | DOUBLE |
| engine_rpm | INTEGER |
| tire_wear_fl/fr/rl/rr | DOUBLE |
| lap | INTEGER |
| track_position | DOUBLE |
| sector | INTEGER |

### session_data
Track conditions and session metadata.
| Column | Type |
|--------|------|
| weather | INTEGER |
| track_temperature | INTEGER |
| air_temperature | INTEGER |
| total_laps | INTEGER |
| track_length | INTEGER |
| session_type | INTEGER |
| track_id | INTEGER |
| session_time_left | INTEGER |
| safety_car_status | INTEGER |
| rain_percentage | INTEGER |
| pit_stop_window_ideal_lap | INTEGER |
| pit_stop_window_latest_lap | INTEGER |

### lap_data
Per-car lap timing and position.
| Column | Type |
|--------|------|
| car_index | INTEGER |
| last_lap_time_in_ms | INTEGER |
| current_lap_time_in_ms | INTEGER |
| sector1_time_in_ms | INTEGER |
| sector2_time_in_ms | INTEGER |
| car_position | INTEGER |
| current_lap_num | INTEGER |
| pit_status | INTEGER |
| num_pit_stops | INTEGER |
| sector | INTEGER |
| penalties | INTEGER |
| driver_status | INTEGER |
| result_status | INTEGER |
| delta_to_car_in_front_in_ms | INTEGER |
| delta_to_race_leader_in_ms | INTEGER |
| speed_trap_fastest_speed | DOUBLE |
| grid_position | INTEGER |

### car_status
Fuel, ERS, tire compound, DRS.
| Column | Type |
|--------|------|
| car_index | INTEGER |
| fuel_mix | INTEGER |
| fuel_in_tank | DOUBLE |
| fuel_remaining_laps | DOUBLE |
| ers_store_energy | DOUBLE |
| ers_deploy_mode | INTEGER |
| ers_harvested_this_lap_mguk | DOUBLE |
| ers_harvested_this_lap_mguh | DOUBLE |
| ers_deployed_this_lap | DOUBLE |
| actual_tyre_compound | INTEGER |
| visual_tyre_compound | INTEGER |
| tyres_age_laps | INTEGER |
| drs_allowed | INTEGER |
| vehicle_fia_flags | INTEGER |
| engine_power_ice | DOUBLE |
| engine_power_mguk | DOUBLE |

### car_damage
Tire wear/damage, wing damage, engine component wear.
| Column | Type |
|--------|------|
| car_index | INTEGER |
| tyres_wear_fl/fr/rl/rr | DOUBLE |
| tyres_damage_fl/fr/rl/rr | INTEGER |
| front_left_wing_damage | INTEGER |
| front_right_wing_damage | INTEGER |
| rear_wing_damage | INTEGER |
| floor_damage | INTEGER |
| diffuser_damage | INTEGER |
| sidepod_damage | INTEGER |
| gear_box_damage | INTEGER |
| engine_damage | INTEGER |
| engine_mguh/es/ce/ice/mguk/tc_wear | INTEGER |
| drs_fault | INTEGER |
| ers_fault | INTEGER |

### car_telemetry_ext
Temperatures, pressures, DRS status.
| Column | Type |
|--------|------|
| car_index | INTEGER |
| brakes_temp_fl/fr/rl/rr | INTEGER |
| tyres_surface_temp_fl/fr/rl/rr | INTEGER |
| tyres_inner_temp_fl/fr/rl/rr | INTEGER |
| engine_temperature | INTEGER |
| tyres_pressure_fl/fr/rl/rr | DOUBLE |
| drs | INTEGER |
| clutch | INTEGER |
| suggested_gear | INTEGER |

### motion_data
3D position and G-forces.
| Column | Type |
|--------|------|
| car_index | INTEGER |
| world_position_x/y/z | DOUBLE |
| g_force_lateral | DOUBLE |
| g_force_longitudinal | DOUBLE |
| g_force_vertical | DOUBLE |
| yaw | DOUBLE |
| pitch | DOUBLE |
| roll | DOUBLE |

### race_events
Discrete events (crashes, penalties, etc.).
| Column | Type |
|--------|------|
| event_code | VARCHAR |
| vehicle_idx | INTEGER |
| detail_text | VARCHAR |

### session_history
Historical lap data per car.
| Column | Type |
|--------|------|
| car_index | INTEGER |
| lap_num | INTEGER |
| lap_time_in_ms | INTEGER |
| sector1/2/3_time_in_ms | INTEGER |
| lap_valid | INTEGER |

### raw_packets
Raw JSON dump of every packet (for debugging).
| Column | Type |
|--------|------|
| packet_id | INTEGER |
| packet_name | VARCHAR |
| session_uid | UBIGINT |
| frame_id | INTEGER |
| data | JSON |

### telemetry_hifreq
Player-only ~10Hz wide row (default `HIFREQ_SAMPLE_RATE=2` from 20Hz UDP). Powers the lap-trace + brake-point coaching tools. Bundles every channel needed to bucket a lap by `track_position` so analysis SQL never has to cross-table join. `total_distance` is monotonic and disambiguates samples that straddle the start/finish line.
| Column | Type |
|--------|------|
| session_uid | UBIGINT |
| frame_id | INTEGER |
| lap | INTEGER |
| track_position | DOUBLE (metres into current lap) |
| total_distance | DOUBLE (monotonic) |
| current_lap_time_ms | INTEGER |
| pit_status | INTEGER (0=on track) |
| throttle | DOUBLE |
| brake | DOUBLE |
| steering | DOUBLE |
| speed | INTEGER (km/h) |
| gear | INTEGER |
| engine_rpm | INTEGER |
| drs | INTEGER |
| brake_temp_fl/fr/rl/rr | INTEGER |
| tyre_surf_temp_fl/fr/rl/rr | INTEGER |
| tyre_inner_temp_fl/fr/rl/rr | INTEGER |
| tyre_pressure_fl/fr/rl/rr | DOUBLE |
| world_pos_x/y/z | DOUBLE |
| g_force_lat/lon/vert | DOUBLE |
| fuel_in_tank | DOUBLE |
| ers_store_energy | DOUBLE (Joules; 4MJ max) |
| ers_deploy_mode | INTEGER |
| clutch | INTEGER |
| suggested_gear | INTEGER |

## Detecting the Player Car Index

**IMPORTANT:** The player's `car_index` changes every session. NEVER hardcode it. Always detect it dynamically:

```bash
# From the REST API (returns the full race state including player_car_index)
curl -s http://localhost:8081/api/telemetry/latest | jq '.player_car_index'

# Or via racedb — the player_car_index is in the packet headers, stored in motion_data
# The /api/telemetry/latest endpoint is the canonical source.
```

Use the returned value in all `WHERE car_index = ...` clauses below.

## Common Analysis Queries

Replace `$IDX` with the player car index from `/api/telemetry/latest`.

```sql
-- Tire wear trend (last 20 samples)
SELECT timestamp, tyres_wear_fl, tyres_wear_fr, tyres_wear_rl, tyres_wear_rr
FROM car_damage WHERE car_index = $IDX
ORDER BY timestamp DESC LIMIT 20;

-- Lap time progression
SELECT current_lap_num, last_lap_time_in_ms / 1000.0 AS lap_time_sec,
       sector1_time_in_ms, sector2_time_in_ms
FROM lap_data WHERE car_index = $IDX AND last_lap_time_in_ms > 0
ORDER BY current_lap_num;

-- Fuel pace (fuel consumed per lap)
SELECT current_lap_num, fuel_in_tank, fuel_remaining_laps
FROM (SELECT *, LAG(fuel_in_tank) OVER (ORDER BY timestamp) AS prev_fuel FROM car_status WHERE car_index = $IDX)
WHERE prev_fuel IS NOT NULL AND fuel_in_tank < prev_fuel;

-- Weather changes
SELECT timestamp, weather, track_temperature, air_temperature, rain_percentage
FROM session_data ORDER BY timestamp DESC LIMIT 10;

-- Position changes
SELECT timestamp, car_position, delta_to_car_in_front_in_ms / 1000.0 AS gap_ahead_sec
FROM lap_data WHERE car_index = $IDX ORDER BY timestamp DESC LIMIT 20;

-- G-force extremes
SELECT MAX(ABS(g_force_lateral)) AS max_lateral_g,
       MIN(g_force_longitudinal) AS max_braking_g,
       MAX(g_force_longitudinal) AS max_accel_g
FROM motion_data WHERE car_index = $IDX;
```

## REST API Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/health` | Server health (uptime, packet count, DB status) |
| GET | `/ws` | WebSocket — live push of telemetry, insights, and health |
| GET | `/api/telemetry/latest` | Latest race state from atomic cache |
| GET | `/api/settings` | Current settings (mock mode, talk level, UDP config) |
| GET | `/api/insights/next` | Consume next pending insight |
| GET | `/api/insights/history` | Full insight log as JSON (query: `?limit=N`, default 50, max 500) |
| GET | `/api/brain/snapshot` | Race brain snapshot as markdown (default) or JSON (`?format=json`) — telemetry + observations + recent radio + driver state + static context |
| POST | `/api/analyst/query` | Async strategy-analyst Q&A. Body: `{"question": "...", "context_topic": "...", "urgent": false}`. Returns `202` with `{"job_id": "anq_...", "status": "queued", "eta_seconds": 10}`. The eventual answer is written to the brain as an Observation under `analyst.<context_topic>`; if `urgent=true`, it is also broadcast via `/api/strategy`. (question, context_topic) is deduped for 30s. |
| GET | `/api/analyst/jobs` | Current in-flight analyst jobs as JSON array (`[]` when none): `[{"job_id":"...", "question":"...", "context_topic":"...", "started_at":"..."}]` |
| GET | `/api/workspace` | Returns `workspace/insights.md` as plain text |
| GET | `/api/laps/traces` | Bucketed channel traces per lap from `telemetry_hifreq`. Query: `?laps=best,current,last,recent:3,25&channels=throttle,brake,speed,gear,steering,rpm,clutch,drs,g_lat,g_lon,g_vert,brake_temp,tyre_temp,tyre_inner_temp,tyre_pressure,brake_temp_fl,tyre_press_rr,fuel,ers_store,ers_deploy_mode&buckets=80` (defaults: `laps=best,current`, `channels=throttle,brake,speed`, `buckets=80`, range 20–200). Each bucket is an AVG over `track_length / buckets` metres. Per-wheel channels (`brake_temp_fl/fr/rl/rr`, `tyre_temp_fl/fr/rl/rr`, `tyre_press_fl/fr/rl/rr`) are available for asymmetry diagnosis; the 4-wheel aliases (`brake_temp`, `tyre_temp`, `tyre_inner_temp`, `tyre_pressure`) are AVG()s for compact LLM use. |
| GET | `/api/laps/compare` | Per-corner brake-point + apex-speed + exit-throttle deltas vs the driver's best valid lap this session. Query: `?lap=25&corner=T3&window_before_m=200&window_after_m=50` (defaults: most recent completed lap, all curated corners, 200m before / 50m after). Returns `{your_..., best_..., delta_...}` triplets per corner; absent values mean the brake-point heuristic (≥10% brake sustained ≥2 hi-freq samples) didn't fire in the window. |
| GET | `/api/laps/delta` | Cumulative time-delta-vs-reference at every distance bucket. Query: `?lap=25&reference=best&buckets=80` (defaults: `lap=last`, `reference=best`, `buckets=80`). Returns `{lap, reference_lap, distance_m[], delta_ms[]}` where `delta_ms[i] = your_elapsed_ms - reference_elapsed_ms` at `distance_m[i]` (negative = ahead). Single most actionable comparison view — feeds the Telemetry Compare tab and the analyst/Live `get_lap_delta` tools. |
| GET | `/api/laps/list` | Lightweight roster of completed laps for picker UIs. Query: `?session_uid=…` (optional). Returns `{session_uid, car_index, best_lap, laps: [{lap, lap_time_ms, sector1_ms, sector2_ms, sector3_ms, valid}]}`. Cheaper than `/api/sessions/:uid/summary` so it can be polled every few seconds while the session is live. |
| GET | `/api/telemetry/recent` | Rolling-window telemetry for the last N seconds. Query: `?seconds=60&buckets=30` (`seconds` 1–300 default 60, `buckets` 10–120 default 30). Returns `{current, current_lap, summary, timeline}`: `summary` carries window-wide peak/min/max/avg stats (peak g_lat, peak g_lon braking & accel, max brake, brake event count, brake total seconds, max brake/tyre temps, samples used); `timeline` is one bucket per slot (oldest first) with bucket means of every channel. Same data the dashboard Telemetry → Live tab buffers from the WS, packaged for AI consumption — gives the LLM the *trend* (was driver smooth, stabby, overheating) not just the current instant. |
| POST | `/api/query` | Execute read-only SQL: `{"sql": "SELECT ..."}` |
| POST | `/api/strategy` | Push strategy insight: `{"source": "...", "insight": "...", "priority": 3}` |
| POST | `/api/driver_query` | Submit driver question for Gemini: `{"query": "..."}` |
| POST | `/api/voice` | Push-to-talk: multipart audio → Whisper STT → Gemini response |
| POST | `/api/settings/mode` | Switch mock/real: `{"mode": "mock"}` |
| POST | `/api/settings/talk_level` | Set verbosity: `{"level": 5}` |
| POST | `/api/transcript` | Append transcript event: `{"kind":"...","actor":"...","text":"...","meta":{...}}` |
| GET | `/api/transcript` | Recent transcript: `?scope=current&limit=50&kinds=engineer_speech,tool_call&since_minutes=10` |
| GET | `/api/transcript/sessions` | List per-session log files with line counts |

### Pushing insights

```bash
curl -X POST http://localhost:8081/api/strategy \
  -H "Content-Type: application/json" \
  -d '{
    "source": "strategy-agent",
    "insight": "Rear tire degradation accelerating. Consider boxing in 3 laps.",
    "priority": 4
  }'
```

Priority: 1 (info) to 5 (critical). Priority 4+ triggers voice alerts.

## Gemini Live conversational backend (optional)

When `GEMINI_LIVE_ENABLED=true`, `make start` launches `gemini_live_service.py`
as an additional process. The agent:

- Holds an open Gemini Live API session (audio in / audio out, low latency).
- **Streams the race brain into context every 5s** via `send_client_content`
  with `turn_complete=False` — so the model always knows current telemetry,
  the latest analyst observations, and what was just said over the radio.
- Exposes these tools that hit the Go REST API (the in-process Go Live
  agent in `telemetry-core/internal/voice/live_tools.go` exposes the same
  surface; the Python service is the legacy/standalone path):
  - `get_race_state` → `GET /api/telemetry/latest` (fast, free)
  - `query_brain` → `GET /api/brain/snapshot` (full brain markdown)
  - `ask_data_analyst` → `POST /api/analyst/query` then polls
    `/api/analyst/jobs` and `/api/brain/snapshot` until the answer lands as
    an Observation (heavy: full DuckDB-aware LLM analysis, ~5–30s, capped
    by `GEMINI_LIVE_ANALYST_TIMEOUT`). Supports `urgent=true` to also
    broadcast the answer over the team radio.
  - `push_strategy_insight` → `POST /api/strategy` (broadcasts a strategy
    radio message to the existing dashboard / voice fanout)
  - `get_recent_telemetry` → `GET /api/telemetry/recent` — bucketed
    rolling-window of the last N seconds (default 60s) of channel data
    PLUS summary stats (peak g_lat, peak braking g, max brake, brake
    event count, max temps). Use for "how am I driving right now",
    "was that stint smooth", "am I overheating brakes". Complements
    the per-instant `get_race_state` and the per-completed-lap
    `get_lap_traces`.
  - `list_laps` → `GET /api/laps/list` — completed laps with times,
    sectors, valid flag. Call this BEFORE the next three to pick lap
    numbers.
  - `get_lap_traces` → `GET /api/laps/traces` — bucketed channel arrays
    (throttle/brake/speed/gear/steering/rpm/g-forces/temps/pressures/fuel/ERS)
    for any selection of laps, indexed by distance. Use to overlay laps
    and find where inputs diverge.
  - `get_lap_delta` → `GET /api/laps/delta` — cumulative ms delta of
    one lap vs a reference at every distance bucket. The single most
    actionable tool for "where am I losing time?" coaching.
  - `compare_lap_corners` → `GET /api/laps/compare` — per-corner brake-
    point, apex-speed, exit-throttle deltas vs best.
  - `get_track_position` → `GET /api/state/track_position` — current
    lap_distance_m + nearest curated corner. Call this when the driver
    says "this corner" / "here" so you can resolve which corner_id to
    pass to set_reminder.
  - `set_reminder` → `POST /api/reminders/corner` — general-purpose
    driver-coaching reminder. Triggers mix and match:
      - `corner_id` ("T8", "Stowe") — fires as the player approaches the
        named corner (uses curated geometry). Recurring by default.
      - `lap_distance_m` + `label` ("pit entry", "DRS zone 2") — fires at
        any raw track position. Use for pit entry, DRS detection points,
        sector boundaries, anything that isn't a curated corner.
        Recurring by default.
      - `at_lap` — fires once on entering the named lap when used alone
        ("pit on lap 15"). Can ALSO combine with `corner_id` or
        `lap_distance_m` to mean "fire at that position only on lap N,
        one-shot" — e.g. "remind me at T8 on lap 5" or "tell me at pit
        entry on lap 22".
      - `corner_id` and `lap_distance_m` are mutually exclusive
        (both name a position). Combined triggers always override
        `recurring` to false.
    Back-compat: the old tool name `set_corner_reminder` still routes to
    this handler. The cue is delivered as a P3 brain event when the
    trigger condition is met; the model voices it on the team radio.
  - `list_reminders` / `cancel_reminder` → `GET /api/reminders` and
    `DELETE /api/reminders/:id`. List includes a `trigger_kind` field
    (`corner` | `distance` | `lap`) so the UI / model can render
    reminders by type.

The same five live + lap-trace tools (`get_recent_telemetry`, `list_laps`,
`get_lap_traces`, `get_lap_delta`, `compare_lap_corners`) are also
registered on the Strategy Analyst so
`POST /api/analyst/query` can fetch and reason on the same data. The
analyst's lap-trace tools run a poor-man's two-turn loop: the first LLM
call may issue tool calls, the analyst executes them, then a second
Complete() call re-asks the question with the tool results stitched in.

The existing push-to-talk pipeline (`voice_service.py` + dashboard mic
button) keeps working alongside it. They're not mutually exclusive — the
Live agent is just another conversational surface that sees the same brain.

## Workspace Files

All context files live in `workspace/`:

| File | Purpose |
|------|---------|
| `driver_profile.md` | Driver name, style, communication preferences, strengths/weaknesses |
| `track_setup.md` | Current track characteristics, setup recommendations, danger zones |
| `past_learnings.md` | Historical session debriefs, tire data, incident analysis |
| `insights.md` | Live state + accumulated findings (written by the insight engine) |
| `soul.md` | Race engineer personality, radio style, communication rules |
| `user.md` | Driver communication preferences, what they want/don't want to hear |
| `telemetry.duckdb` | DuckDB database with all telemetry tables |
| `sessions/` | Per-F1-session interaction transcripts (JSONL, one file per `session_uid`) |

Note: `SOUL.md` and `USER.md` in the project root are kept for backwards compatibility. The Go code loads from `workspace/` first, then falls back to the root.

### Per-session transcript (`workspace/sessions/`)

Every LLM-touching surface (CommsGate, analyst, voice handler, Gemini Live) writes into a shared per-session log so the agent has cross-turn — and cross-session — memory of the conversation.

- File per F1 `session_uid`: `sess_<hex_uid>.jsonl`. New uid → new file (auto-rotate). No uid yet → `sess_proc_<unix>.jsonl` until one arrives.
- Append-only JSONL, crash-safe (partial last line is silently dropped on read).
- Closed `kind` enum: `user_utterance`, `engineer_speech`, `tool_call`, `tool_result`, `analyst_query`, `analyst_answer`, `insight_pushed`, `event_dispatched`, `event_delivered`, `event_dropped`.
- Last 25 events injected into the CommsGate prompt and `/api/brain/snapshot` markdown automatically (configurable via `TRANSCRIPT_PROMPT_LINES`).
- Gemini Live tool `get_session_history(scope, limit, kinds, since_minutes)` pulls older events on demand. `scope=previous` walks one session back so the agent can reference last race.
- Retention: last `TRANSCRIPT_RETENTION_SESSIONS` files kept (default 30); oldest are deleted on rotation. Active session is never deleted.
- Files cap at 10k lines and split into `sess_<uid>-part2.jsonl`, `-part3.jsonl` …; the loader concatenates all parts of a session in order.

## Configuration

**Source of truth: `~/.race-engineer/config.json`.** Inspect with
`./workspace/bin/configtool show`, edit with `./workspace/bin/configtool
set KEY VALUE`, or via the dashboard Settings page. Schema is declared in
`telemetry-core/internal/config/schema.go` — adding a new knob means one
entry there and it picks up across Go, the CLI, the dashboard, and the
Python services. Migrate an existing `.env` with `make migrate-config`.

Env vars still work as a fallback for one-off overrides, but Go logs a
one-time `WARN: env var X deprecated` per key. Set
`RACE_ENGINEER_CONFIG_STRICT=1` to disable the env fallback entirely.

The variable table below documents the legacy env-var names — they map
1:1 to the JSON keys. New code should treat the JSON file as authoritative.

| Variable | Default | Description |
|----------|---------|-------------|
| `DB_PATH` | `workspace/telemetry.duckdb` | DuckDB file path |
| `TELEMETRY_MODE` | `real` | `real` = UDP listener, `mock` = fake data |
| `TELEMETRY_HOST` | `0.0.0.0` | UDP bind host |
| `TELEMETRY_PORT` | `20777` | UDP port (F1 25 default) |
| `UDP_MODE` | `broadcast` | `broadcast` or `unicast` |
| `API_PORT` | `8081` | REST API port |
| `LLM_PROVIDER` | `gemini` | LLM backend: `gemini`, `anthropic`, `openai` |
| `LLM_MODEL` | — | Override model name (empty = provider default) |
| `GEMINI_API_KEY` | — | Google Gemini API key |
| `ANTHROPIC_API_KEY` | — | Anthropic API key (for Claude) |
| `OPENAI_API_KEY` | — | OpenAI API key |
| `VOICE_URL` | `http://localhost:8000` | Python voice service URL |
| `TTS_ENGINE` | `edge-tts` | TTS backend: `edge-tts`, `qwen-tts` (MLX voice cloning) |
| `TTS_VOICE` | `en-GB-RyanNeural` | edge-tts voice name |
| `TTS_VOICE_REF` | `workspace/voices/reference.wav` | Voice reference WAV for Qwen TTS cloning |
| `TTS_SPEED` | `1.0` | Qwen TTS speech speed multiplier |
| `STT_ENGINE` | `whisper` | STT backend: `whisper`, `parakeet` (NeMo), `resemble` (cloud) |
| `WHISPER_MODEL` | `medium` | faster-whisper model size |
| `WORKSPACE_DIR` | `workspace` | Workspace directory path |
| `TALK_LEVEL` | `5` | Insight verbosity 1-10 |
| `LOG_LEVEL` | `info` | Log level: trace, debug, info, warn, error, fatal, panic |
| `BATCH_SIZE` | `20` | DuckDB flush threshold |
| `SAMPLE_RATE` | `20` | 20Hz to 1Hz sampling |
| `HIFREQ_SAMPLE_RATE` | `2` | Hi-freq player-only sampler stride (UDP_RATE / N). Default `2` → 10Hz from 20Hz `car_telemetry` packets. Drives writes into `telemetry_hifreq` for the `/api/laps/*` lap-trace + brake-point tools. Set to `0` to disable hi-freq capture. |
| `WS_PUSH_RATE` | `10` | WebSocket push rate in Hz (5=200ms, 10=100ms, 20=50ms) |
| `PTT_BUTTON` | `0x00000001` | F1 BUTN bitmask for push-to-talk (observed PS5 Cross/A). Set `0` to disable. **OR multiple bits to accept any of several buttons** — e.g. `0x00001800` for L2 OR R2. With `PTT_MODE=toggle`, each press of any masked bit flips the mic; with hold mode, the mic stays open while any masked button is held. **Recommended:** bind a "UDP Action 1–8" slot in F1 25 Controls > UDP (bits `0x00020000`–`0x01000000`) to a wheel button — these fire through BUTN with no in-game side-effect, combining the precision of a real button with the no-side-effect property of MFD-cycle. |
| `PTT_MODE` | `hold` | `hold` = mic open while button held (momentary). `toggle` = press flips the mic state, releases ignored. Toggle is miss-tolerant — a dropped release event can't strand the mic; the next press always recovers. |
| `PTT_TRIGGER` | `button` | Which signal drives PTT: `button` (BUTN bitmask, see `PTT_BUTTON`), `dual` (two distinct buttons — see `PTT_BUTTON_START` / `PTT_BUTTON_END`), `mfd` (any MFD-page change). |
| `PTT_BUTTON_START` | `0x00020000` | Used only when `PTT_TRIGGER=dual`. BUTN bit that OPENS the mic. Press-edge only — releases ignored, so dropped releases can't strand the mic. Most miss-tolerant config available. Default = UDP Action 1. |
| `PTT_BUTTON_END` | `0x00040000` | Used only when `PTT_TRIGGER=dual`. BUTN bit that CLOSES the mic. Default = UDP Action 2. |
| `LOG_BUTTONS` | `false` | Log every BUTN press/release with the named bitmask (Cross/A, R2, UDP Action 1, …). Use while mapping wheel buttons to PTT slots — saves running `buttonwatch` in a side terminal. Alias of `BUTN_PROBE=true`. |
| `VOICE_MODE` | `live_only` | Which voice-input path runs at boot. `live_only` skips STT init + 503s `/api/voice`. `ptt_only` skips the Gemini Live agent factory + refuses `/api/voice/live`. `both` wires both inputs; Live owns audio output. Restart required to apply. The legacy `GEMINI_LIVE_ENABLED=true` env var is honoured only when `VOICE_MODE` is empty (maps to `both`). |
| `GEMINI_LIVE_ENABLED` | (unset) | Legacy compat hint. When `VOICE_MODE` is empty and this is `true`, the effective mode becomes `both`. Prefer setting `VOICE_MODE` directly. |
| `TRANSCRIPT_PROMPT_LINES` | `25` | Recent transcript events injected into LLM prompts (CommsGate + brain snapshot) |
| `TRANSCRIPT_TOOL_LIMIT` | `200` | Default cap on `/api/transcript` and `get_session_history` responses |
| `TRANSCRIPT_RETENTION_SESSIONS` | `30` | Session files kept on disk; oldest are deleted on rotation |
| `GEMINI_LIVE_MODEL` | `gemini-3.1-flash-live-preview` | Live API model name |
| `GEMINI_LIVE_BRAIN_POLL_SEC` | `5` | How often to poll the brain snapshot and push it as conversation context |
| `GEMINI_LIVE_BRAIN_MAX_CHARS` | `8000` | Truncation cap for brain context pushes |
| `GEMINI_LIVE_ANALYST_TIMEOUT` | `120` | Timeout (s) for the `ask_data_analyst` tool round-trip |
| `RACE_API_URL` | `http://localhost:8081` | Base URL the Live agent calls for tool execution |
| `CORNER_REMINDER_DEFAULT_LOOKAHEAD_SEC` | `3.0` | Default lookahead seconds when `set_corner_reminder` omits `lookahead_seconds`. The Go side resolves seconds→metres via `insights.ComputeLookaheadM` (clamped 30..400m). |
| `RACE_ENGINEER_CONFIG_STRICT` | (unset) | When truthy, disables the env-var fallback — only `~/.race-engineer/config.json` is consulted. Recommended once you've migrated. |

## Intelligence Pipeline

### How Insights Flow

```
                 ┌──────────────┐
                 │  Rule Engine │ (Go, threshold-based)
                 └──────┬───────┘
                        │ raw insight
                        ▼
              ┌──────────────────┐
              │    CommsGate     │ (LLM translator)
              │  - dedup check   │
              │  - talk level    │
              │  - translate to  │
              │    radio comms   │
              └──────┬───────────┘
                     │ translated insight
         ┌───────────┼───────────┐
         ▼           ▼           ▼
   ┌──────────┐ ┌────────┐ ┌───────────┐
   │ WebSocket│ │ Voice  │ │ Workspace │
   │ → Dash   │ │ TTS    │ │ insights  │
   └──────────┘ └────────┘ └───────────┘
```

1. **Rule Engine** (`internal/insights/`) — fires insights based on telemetry thresholds (tire wear, fuel, damage, weather changes, safety car, etc.)
2. **Pi Agent** — sandboxed Python child process that **replaces** the old Strategy Analyst (see "Pi Agent" section below). Event-driven; reacts to questions, lap completion, and significant rule-engine events via an MCP server.
3. **CommsGate** (`internal/intelligence/comms_gate.go`) — central gate that receives all insights, deduplicates, checks talk level, and translates to radio-style speech via LLM
4. **Fanout** — translated insights go to: WebSocket (dashboard), Voice TTS (audio), and workspace/insights.md

### Pi Agent

The pi agent (`pi_agent_service.py` + `python/pi_agent/`) replaces the old
in-process Strategy Analyst. It runs only when `PI_AGENT_MODE != "off"` and
connects to telemetry-core's MCP server (default mounted at `/mcp`) — that
connection is its **only** outward channel. There is no shell access, no
arbitrary file I/O, no `subprocess`/`requests`/`httpx`. The CI test
`tests/test_pi_agent_sandbox.py` enforces the absent-imports rule.

**Event-driven, no ticker.** A planner long-polls `pull_next_trigger` over MCP
and dispatches one of seven specialist personas (`tires`, `brakes`, `pace`,
`strategy`, `weather`, `energy`, `driving`, plus a `responder` for queries).
Trigger sources, all detected Go-side and pushed onto an in-memory queue:

1. **Query trigger** — `POST /api/analyst/query` (the contract preserved from
   the old analyst; Gemini Live's `ask_data_analyst` tool keeps working).
2. **Lap-complete trigger** — fires once per player lap rollover.
3. **Significant-event trigger** — every insight the rule engine generates
   (tire-cliff, weather, damage, safety car, position change, …).

Specialists load their playbook via `get_skill(name)` from
`workspace/pi_skills/*.md` and use a focused MCP tool subset to write back:

- `write_observation(topic="<sub-topic>", …)` — internal-only, lands under
  `pi_agent.<topic>` in the brain (visible at `/api/brain/snapshot`).
- `submit_query_answer(job_id, answer, urgent)` — replies to a query trigger,
  writes under `analyst.<context_topic>` to preserve the existing contract.
- `push_insight(insight, priority, source)` — pushes onto `/api/strategy`,
  capped at `PI_AGENT_MAX_PRIORITY` (default 3).

**Adding a new skill**: drop a markdown file with the standard frontmatter
into `workspace/pi_skills/`. The registry rescans on the next `list_skills`
call — no rebuild required. See `workspace/pi_skills/_index.md` for format.

**Adding a new MCP tool**: register it in
`telemetry-core/internal/mcp/tools.go`. Tool handlers either call into
in-process services (`brain`, `storage.Reader`) directly or self-call
existing `/api/*` handlers via the injected `APIBase`.

#### What the pi agent can see — data access surface

The pi agent has **read access to every byte of telemetry the game has
ever sent**, plus the engineer↔driver dialogue from every prior session.
Specialists do not always know this; their skill markdown should remind
them when historical context is in play.

| Need | Tool | What it covers |
|---|---|---|
| Current live state | `get_race_state`, `get_state_*` | Latest atomic-cache snapshot only. Nothing historical. |
| Per-corner / per-lap coaching | `list_laps`, `get_lap_traces`, `get_lap_delta`, `compare_lap_corners`, `get_corner_brake_history`, `get_brake_balance_report`, `get_corner_coaching_report` | Hi-freq `telemetry_hifreq` table; `list_laps` accepts `session_uid` to scope to a past session. |
| Past F1 sessions (telemetry / lap_data / car_status / damage / events) | `query_sql` + `describe_schema` | **Full DuckDB read access — no session filter is applied by default.** The agent must scope queries itself. |
| Past F1 sessions — roster + metadata | `list_sessions` | Wraps `GET /api/sessions`. Returns last ~100 sessions with `{session_uid, track_name, session_type_name, first_seen, last_seen, total_laps, best_lap_ms, final_position, player_car_index}`. Start here for any "last race at X" question. |
| Past F1 sessions (engineer↔driver dialogue) | `get_session_history(scope=…)` | Reads `workspace/sessions/sess_*.jsonl`. Pass `scope='previous'` for last race, `scope='all'` for everything, or `scope=<session_uid>` from `list_sessions`. |
| What the rule engine + other agents already concluded **this session** | `get_brain_snapshot`, `recent_pi_observations`, `recent_insights` | Brain state is in-memory and reset per process. Not cross-session. |

**Scoping SQL to a specific past session.** Most telemetry tables only
carry a `timestamp` column — **NOT** `session_uid`. The only tables that
carry `session_uid` directly are `telemetry_hifreq`, `raw_packets`, and
`session_history`. The right recipe is **always**:

1. `list_sessions` → pick the matching `session_uid`, `first_seen`,
   `last_seen`, and `player_car_index` for the session you want.
2. For `telemetry_hifreq` / `session_history`: filter by
   `session_uid = <uid>` directly.
3. For every other table (`telemetry`, `lap_data`, `car_status`,
   `car_damage`, `session_data`, `motion_data`, `car_telemetry_ext`,
   `race_events`): bound by the timestamp range and the player car_index
   you got in step 1.

```sql
-- After list_sessions told you session_uid=11097…, t0='2026-03-01 14:00',
-- t1='2026-03-01 15:30', player_car_index=19:

-- Pattern A — table with session_uid (preferred when available)
SELECT lap, lap_time_in_ms, sector1_time_in_ms, sector2_time_in_ms,
       sector3_time_in_ms, lap_valid
FROM session_history
WHERE session_uid = 11097389812345678901 AND car_index = 19
ORDER BY lap_num;

-- Pattern B — table with only timestamp (bound by the session window)
SELECT timestamp, tyres_wear_fl, tyres_wear_fr, tyres_wear_rl, tyres_wear_rr
FROM car_damage
WHERE timestamp BETWEEN TIMESTAMP '2026-03-01 14:00' AND TIMESTAMP '2026-03-01 15:30'
  AND car_index = 19
ORDER BY timestamp;
```

Do **NOT** try `SELECT DISTINCT session_uid FROM session_data WHERE
track_id = N` — that column does not exist on `session_data`. Use
`list_sessions` for the discovery step; it does the join for you.

**Conversation memory across sessions.** Per-F1-session JSONL transcripts
live in `workspace/sessions/sess_<session_uid>.jsonl` (rotated to
`-partN` at 10k lines each). The pi agent reaches them via:

- `list_sessions` → roster of past session files with `line_count` /
  `modified_at`. Cheap; call it first.
- `get_session_history(scope='previous', kinds='engineer_speech,user_utterance')`
  → last race's radio. `scope='all'` walks back further; `scope=<session_id>`
  pins to one specific file.

When the driver says "what did we conclude about brakes last race?" the
correct answer path is `list_sessions` → pick the previous `session_id` →
`get_session_history(scope=<that_id>, kinds='analyst_answer,insight_pushed,engineer_speech', limit=200)`
→ summarise.

### CommsGate Translation

When CommsGate translates insights to radio speech, it receives the same rich session context via `BuildContext()`:
- Track name, session type, lap X/Y
- Position (started P-X), pit stops, gaps
- Speed, gear, RPM
- Tire compound, age, wear percentages per corner
- Fuel (kg, laps remaining, mix mode), ERS (% and deploy mode), DRS
- Damage breakdown
- Weather, temperatures, rain %, safety car
- Pit window, session time remaining

This ensures the LLM can give contextually appropriate radio messages.

### Avoiding Duplicate Insights

Before pushing an insight via `/api/strategy`, **always check recent insight history** to avoid repeating what's already been said:

```bash
# Check recent insights
./workspace/bin/insightlog -limit 20

# Or via API
curl http://localhost:8081/api/insights/history?limit=20
```

Do NOT push insights that:
- Repeat information from the last 5 minutes
- State obvious facts the driver already knows (e.g., "you are P3" when position hasn't changed)
- Contradict a more recent insight

DO push insights that:
- Alert to new developments (weather change incoming, competitor pitted, damage detected)
- Provide actionable strategy (pit window opening, tire cliff approaching, fuel target change)
- Respond to significant state changes (safety car, position change, DRS availability)

### Voice Pipeline

```
Driver speaks (push-to-talk)
        │
        ▼
   Dashboard records audio (WebM/Opus)
        │
        ▼
   POST /api/voice (multipart)
        │
        ├──→ Immediate: fire ack audio ("Copy, checking the data")
        │    (synthesized by Python voice service, broadcast via WS)
        │
        ├──→ Proxy audio to Python /transcribe (Whisper STT)
        │
        ▼
   Transcription text → CommsGate.HandleQuery()
        │
        ▼
   LLM generates response with full telemetry context
        │
        ▼
   Response → Voice TTS → WebSocket audio broadcast
```

The ack fires immediately on audio receipt (before transcription starts) so the driver hears a response within ~200ms. The LLM is instructed to NOT start with acknowledgement phrases ("Copy that", "Roger", etc.) since the system already sent one.

## Enum Value Mappings

These integer values appear in the DuckDB tables. Use them to interpret telemetry data.

### Track IDs (`session_data.track_id`)

| ID | Track | ID | Track | ID | Track |
|----|-------|----|-------|----|-------|
| 0 | Melbourne | 11 | Monza | 22 | Silverstone Short |
| 1 | Paul Ricard | 12 | Singapore | 23 | Austin Short |
| 2 | Shanghai | 13 | Suzuka | 24 | Suzuka Short |
| 3 | Bahrain | 14 | Abu Dhabi | 25 | Hanoi |
| 4 | Catalunya | 15 | Austin | 26 | Zandvoort |
| 5 | Monaco | 16 | Interlagos | 27 | Imola |
| 6 | Montreal | 17 | Red Bull Ring | 28 | Portimao |
| 7 | Silverstone | 18 | Sochi | 29 | Jeddah |
| 8 | Hockenheim | 19 | Mexico City | 30 | Miami |
| 9 | Hungaroring | 20 | Baku | 31 | Las Vegas |
| 10 | Spa | 21 | Sakhir Short | 32 | Losail |

### Session Types (`session_data.session_type`)

| Value | Type | Value | Type |
|-------|------|-------|------|
| 0 | Unknown | 7 | Q3 |
| 1 | P1 | 8 | Short Q |
| 2 | P2 | 9 | OSQ |
| 3 | P3 | 10 | Race |
| 4 | Short P | 11 | Race 2 |
| 5 | Q1 | 12 | Race 3 |
| 6 | Q2 | 13 | Time Trial |

### Tire Compounds (`car_status.actual_tyre_compound`)

| Value | Compound |
|-------|----------|
| 16 | Soft |
| 17 | Medium |
| 18 | Hard |
| 7 | Inter |
| 8 | Wet |

### Weather (`session_data.weather`)

| Value | Condition |
|-------|-----------|
| 0 | Clear |
| 1 | Light Cloud |
| 2 | Overcast |
| 3 | Light Rain |
| 4 | Heavy Rain |
| 5 | Storm |

### Fuel Mix (`car_status.fuel_mix`)

| Value | Mode |
|-------|------|
| 0 | Lean |
| 1 | Standard |
| 2 | Rich |
| 3 | Max |

### ERS Deploy Mode (`car_status.ers_deploy_mode`)

| Value | Mode |
|-------|------|
| 0 | None |
| 1 | Medium |
| 2 | Hotlap |
| 3 | Overtake |

### Safety Car (`session_data.safety_car_status`)

| Value | Status |
|-------|--------|
| 0 | None |
| 1 | Full Safety Car |
| 2 | Virtual Safety Car |
| 3 | Formation Lap |

### Pit Status (`lap_data.pit_status`)

| Value | Status |
|-------|--------|
| 0 | On Track |
| 1 | Pit Lane |
| 2 | In Pit Area |

### Driver Status (`lap_data.driver_status`)

| Value | Status |
|-------|--------|
| 0 | In Garage |
| 1 | Flying Lap |
| 2 | In Lap |
| 3 | Out Lap |
| 4 | On Track |

## Advanced Analysis Queries

Replace `$IDX` with the player car index from `/api/telemetry/latest`.

```sql
-- Session identity: what track and session are we in?
SELECT track_id, session_type, total_laps, weather, track_temperature,
       air_temperature, rain_percentage, safety_car_status,
       pit_stop_window_ideal_lap, pit_stop_window_latest_lap,
       session_time_left
FROM session_data ORDER BY timestamp DESC LIMIT 1;

-- Our car's current state
SELECT car_position, current_lap_num, last_lap_time_in_ms,
       delta_to_car_in_front_in_ms, delta_to_race_leader_in_ms,
       num_pit_stops, pit_status, grid_position
FROM lap_data WHERE car_index = $IDX ORDER BY timestamp DESC LIMIT 1;

-- Nearby competitors (within ±3 positions of us)
SELECT ld.car_index, ld.car_position, ld.current_lap_num,
       ld.last_lap_time_in_ms, ld.num_pit_stops, ld.pit_status,
       cs.actual_tyre_compound, cs.tyres_age_laps
FROM lap_data ld
JOIN car_status cs ON ld.car_index = cs.car_index
WHERE ld.car_position BETWEEN
  (SELECT car_position - 3 FROM lap_data WHERE car_index = $IDX ORDER BY timestamp DESC LIMIT 1) AND
  (SELECT car_position + 3 FROM lap_data WHERE car_index = $IDX ORDER BY timestamp DESC LIMIT 1)
AND ld.timestamp = (SELECT MAX(timestamp) FROM lap_data)
ORDER BY ld.car_position;

-- Tire degradation rate (wear change over last 5 samples)
SELECT timestamp, tyres_wear_fl, tyres_wear_fr, tyres_wear_rl, tyres_wear_rr
FROM car_damage WHERE car_index = $IDX
ORDER BY timestamp DESC LIMIT 5;

-- Fuel burn rate per lap
SELECT current_lap_num, fuel_in_tank, fuel_remaining_laps,
       LAG(fuel_in_tank) OVER (ORDER BY timestamp) - fuel_in_tank AS fuel_per_lap
FROM car_status WHERE car_index = $IDX
ORDER BY timestamp DESC LIMIT 10;

-- Brake and tire temperatures (overheating detection)
SELECT brakes_temp_fl, brakes_temp_fr, brakes_temp_rl, brakes_temp_rr,
       tyres_surface_temp_fl, tyres_surface_temp_fr, tyres_surface_temp_rl, tyres_surface_temp_rr,
       tyres_inner_temp_fl, tyres_inner_temp_fr, tyres_inner_temp_rl, tyres_inner_temp_rr
FROM car_telemetry_ext WHERE car_index = $IDX ORDER BY timestamp DESC LIMIT 1;

-- ERS energy management
SELECT ers_store_energy, ers_deploy_mode,
       ers_harvested_this_lap_mguk, ers_harvested_this_lap_mguh,
       ers_deployed_this_lap
FROM car_status WHERE car_index = $IDX ORDER BY timestamp DESC LIMIT 5;

-- Detect if a competitor just pitted (pit_status changed)
SELECT car_index, car_position, pit_status, actual_tyre_compound, tyres_age_laps
FROM (
  SELECT ld.car_index, ld.car_position, ld.pit_status, cs.actual_tyre_compound, cs.tyres_age_laps,
         LAG(ld.pit_status) OVER (PARTITION BY ld.car_index ORDER BY ld.timestamp) AS prev_pit
  FROM lap_data ld JOIN car_status cs ON ld.car_index = cs.car_index
)
WHERE pit_status != prev_pit AND pit_status IN (1, 2);
```

## Writing Effective Insights

When crafting strategy insights to push via `/api/strategy`, follow these guidelines:

1. **Be specific and actionable** — "Box this lap, switch to Mediums" not "Consider pitting soon"
2. **Include numbers** — "Gap to Hamilton is 1.2s and closing at 0.3s/lap" not "The gap is closing"
3. **Reference the current session** — Use track name, lap number, competitor positions
4. **Set appropriate priority**:
   - **1-2**: Informational (pace comparisons, tire wear updates)
   - **3**: Tactical (pit window approaching, undercut opportunity)
   - **4**: Urgent (rain incoming, safety car, tire cliff imminent)
   - **5**: Critical (mechanical failure, immediate box required)
5. **Sound like a real F1 engineer** — Concise, direct, use F1 terminology
6. **One insight per message** — Don't bundle multiple topics

## Development

```bash
make start    # Start all 3 processes (builds Go binaries first)
make stop     # Stop all processes
make build    # Build Go binaries only (telemetry-core + racedb)
```

### Project structure

```
telemetry-core/          Go service (UDP + API + DuckDB + Gemini)
  cmd/server/            Main server entry point
  cmd/query/             racedb CLI tool
  cmd/insightlog/        Insight history CLI tool
  internal/
    api/                 Fiber REST handlers + WebSocket hub
    config/              Environment config
    ingestion/           UDP packet parsing pipeline
    intelligence/        Gemini advisor, analyst, CommsGate, voice client
    insights/            Rule-based insight engine + insight history log
    models/              Data structures
    packets/             F1 telemetry packet definitions
    storage/             DuckDB schema and operations
    workspace/           Markdown context file writer
dashboard/               React frontend (Vite + React 19 + Tailwind v4)
workspace/               Agent context files + DuckDB
workspace/bin/           Compiled binaries (telemetry-core, racedb, insightlog) + publish scripts
voice_service.py         Python TTS/STT service
```
