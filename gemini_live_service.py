"""Gemini Live race-engineer service.

A switchable conversational backend that holds an open Gemini Live API
session, streams the live RaceBrain into the conversation context, and
exposes tools that delegate heavy lifting to the Go telemetry-core:

    - get_race_state          fast snapshot of telemetry from /api/telemetry/latest
    - query_brain             full brain markdown from /api/brain/snapshot
    - ask_data_analyst        synchronous LLM analysis via /api/analyst/query
    - push_strategy_insight   pushes a strategy radio message via /api/strategy

Enabled by setting GEMINI_LIVE_ENABLED=true in .env. Otherwise the existing
push-to-talk pipeline (voice_service.py + dashboard) keeps working unchanged.

Modeled on poc/gemini-live-light-poc/agent.py — same audio loop, different
tool surface and a continuous brain context stream.
"""

import asyncio
import json
import math
import os
import struct
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime

import warnings

with warnings.catch_warnings():
    warnings.simplefilter("ignore", DeprecationWarning)
    try:
        import audioop  # removed in Python 3.13; pure-python fallback below
    except ImportError:  # pragma: no cover
        audioop = None

try:
    from dotenv import load_dotenv

    load_dotenv()
except ImportError:  # pragma: no cover
    pass

# race_config hydrates os.environ from ~/.race-engineer/config.json (and the
# Go server's loopback config endpoint when reachable). Keeps the existing
# os.environ.get(...) calls below working while moving the source of truth
# to the JSON config file. Must be imported BEFORE any config read.
_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_PYTHON_DIR = os.path.join(_THIS_DIR, "python")
if _PYTHON_DIR not in sys.path:
    sys.path.insert(0, _PYTHON_DIR)
try:
    import race_config  # noqa: F401 — side-effect import: hydrates os.environ
except Exception as _e:  # pragma: no cover
    print(f"race_config unavailable, falling back to env: {_e}", file=sys.stderr)

import pyaudio
from google import genai
from google.genai import types

# --- pyaudio config (matches Live API expectations) ---
FORMAT = pyaudio.paInt16
CHANNELS = 1
SEND_SAMPLE_RATE = 16000
RECEIVE_SAMPLE_RATE = 24000
CHUNK_SIZE = 1024

# --- service config (from .env / environment) ---
ENABLED = os.environ.get("GEMINI_LIVE_ENABLED", "false").lower() in ("1", "true", "yes")
RACE_API_URL = os.environ.get("RACE_API_URL", "http://localhost:8081").rstrip("/")
MODEL = os.environ.get("GEMINI_LIVE_MODEL", "gemini-3.1-flash-live-preview")
# BRAIN_POLL_SEC is intentionally not read — the brain is loaded once at session
# start and refreshed on demand via the query_brain tool. Pushing brain updates
# during the session triggered self-interrupts that cut off responses mid-word.
BRAIN_MAX_CHARS = int(os.environ.get("GEMINI_LIVE_BRAIN_MAX_CHARS", "8000"))
ANALYST_TIMEOUT = float(os.environ.get("GEMINI_LIVE_ANALYST_TIMEOUT", "120"))

# Verbose diagnostics. The 5s [diag] heartbeat is invaluable when chasing a
# stuck pipeline (silent mic, stalled playback) but pure noise during normal
# operation. Enable with LOG_LEVEL=debug or LOG_LEVEL=trace.
_LOG_LEVEL = os.environ.get("LOG_LEVEL", "info").strip().lower()
DIAG_VERBOSE = _LOG_LEVEL in ("debug", "trace")

# Push-to-talk: hold this key to unmute the mic. "" disables PTT (mic always
# hot, the legacy POC behavior). Single char ("`", "v") or pynput key name
# ("space", "ctrl_r"). Default: backtick (`) — the key under Esc on US layouts.
PTT_KEY = os.environ.get("GEMINI_LIVE_PTT_KEY", "`")

# PTT source for the Live mic gate:
#   "keyboard" (default): use the pynput key gate above
#   "controller":         subscribe to Go's WebSocket and follow the wheel/pad
#                         button configured by PTT_BUTTON + PTT_MODE
#   "off":                always-hot (legacy POC behavior)
PTT_SOURCE = os.environ.get("GEMINI_LIVE_PTT_SOURCE", "keyboard").strip().lower()
# WebSocket URL the controller-mode listener connects to. Defaults to the
# canonical race API on localhost — override only if the Go core runs elsewhere.
PTT_WS_URL = os.environ.get(
    "GEMINI_LIVE_PTT_WS_URL",
    RACE_API_URL.replace("http://", "ws://").replace("https://", "wss://") + "/ws",
)

# Audio feedback on PTT transitions. "on" plays a short beep when the mic
# opens and a lower beep when it closes. Set "off" to disable.
PTT_FEEDBACK = os.environ.get("GEMINI_LIVE_PTT_FEEDBACK", "on").strip().lower()
PTT_TONE_ON_HZ = int(os.environ.get("GEMINI_LIVE_PTT_TONE_ON_HZ", "1000"))
PTT_TONE_OFF_HZ = int(os.environ.get("GEMINI_LIVE_PTT_TONE_OFF_HZ", "500"))
PTT_TONE_MS = int(os.environ.get("GEMINI_LIVE_PTT_TONE_MS", "120"))


def _make_ptt_tone(freq_hz: int, duration_ms: int = PTT_TONE_MS, volume: float = 0.25) -> bytes:
    """Synthesize a short PCM int16 mono tone at the playback rate.

    Linear fade-in/out windows kill the click that would otherwise happen
    when a sine wave starts/stops mid-cycle. Returns raw bytes ready to push
    into audio_queue_output (matches the Live API's 24 kHz playback rate).
    """
    sr = RECEIVE_SAMPLE_RATE
    n = int(sr * duration_ms / 1000)
    fade = max(8, n // 20)
    samples = bytearray(n * 2)
    amp = int(volume * 32767)
    for i in range(n):
        env = 1.0
        if i < fade:
            env = i / fade
        elif i > n - fade:
            env = (n - i) / fade
        s = int(env * amp * math.sin(2 * math.pi * freq_hz * i / sr))
        struct.pack_into("<h", samples, i * 2, s)
    return bytes(samples)


# Pre-computed tones so the audio is instant on every press. Lazily filled
# on first use because RECEIVE_SAMPLE_RATE is a module constant — safe.
_TONE_ON_PCM: "bytes | None" = None
_TONE_OFF_PCM: "bytes | None" = None


def play_ptt_feedback(active: bool) -> None:
    """Push a short beep into the playback queue for mic-on / mic-off."""
    if PTT_FEEDBACK != "on":
        return
    global _TONE_ON_PCM, _TONE_OFF_PCM
    if _TONE_ON_PCM is None:
        _TONE_ON_PCM = _make_ptt_tone(PTT_TONE_ON_HZ)
    if _TONE_OFF_PCM is None:
        _TONE_OFF_PCM = _make_ptt_tone(PTT_TONE_OFF_HZ)
    pcm = _TONE_ON_PCM if active else _TONE_OFF_PCM
    try:
        # Use the dedicated tones queue so the beep plays in parallel with
        # whatever Gemini is currently saying instead of waiting for the
        # engineer's voice chunks to drain.
        audio_queue_tones.put_nowait(pcm)
    except Exception:
        # Queue full / not yet initialised — feedback is best-effort.
        pass


# ─── Tool wiring ─────────────────────────────────────────────────────────

TOOLS = [
    {
        "function_declarations": [
            {
                "name": "get_race_state",
                "description": (
                    "Situational awareness — call this FIRST for any 'where am "
                    "I / what's the situation' question. Sub-millisecond "
                    "(reads in-memory cache).\n\n"
                    "Returns: headline (one-line TL;DR), track name, "
                    "session_type (Race/Q1/P3/etc), lap, total_laps, "
                    "session_time_left_sec, position, grid_position, "
                    "gap_ahead_sec (seconds, omitted for leader), "
                    "gap_to_leader_sec, pit_status (On Track/Pit Lane/In Pit "
                    "Area), driver_status (Flying Lap/In Lap/Out Lap/On "
                    "Track), pit_stops, sector (1-3), lap_distance_m, "
                    "track_length_m, lap_percentage (0-100), "
                    "last_lap_time_sec, current_lap_time_sec, safety_car "
                    "(None/Full/Virtual/Formation Lap), weather (Clear/"
                    "Light Cloud/Light Rain/Heavy Rain/etc), track_temp_c, "
                    "air_temp_c, rain_percent, pit_window_ideal_lap, "
                    "pit_window_latest_lap.\n\n"
                    "Use for: 'what track is this', 'what position am I in', "
                    "'what's my gap', 'what lap are we on', 'is the safety "
                    "car out', 'how many laps to go', 'is it raining'."
                ),
            },
            {
                "name": "get_tires_and_damage",
                "description": (
                    "Tire health + damage health — the car-condition cluster. "
                    "Sub-millisecond (cache).\n\n"
                    "Returns: headline, compound (Soft/Medium/Hard/Inter/"
                    "Wet), visual_compound, age_laps. "
                    "Per-corner objects (fl/fr/rl/rr) for: wear_percent (0-"
                    "100), surface_temp_c, inner_temp_c, pressure_psi, "
                    "brake_temp_c. damage_percent object covering "
                    "front_left_wing, front_right_wing, rear_wing, floor, "
                    "diffuser, sidepod, gearbox, engine (each 0-100). "
                    "drs_fault and ers_fault booleans.\n\n"
                    "Use for: 'how are the tires', 'what compound am I on', "
                    "'how old are the tires', 'are the fronts overheating', "
                    "'is anything broken', 'what's the floor damage', "
                    "'why is the car slow' (combine with get_pace_trend)."
                ),
            },
            {
                "name": "get_energy_and_fuel",
                "description": (
                    "Fuel + ERS + DRS — the energy management cluster. "
                    "Sub-millisecond (cache).\n\n"
                    "Returns: headline. fuel: in_tank_kg, remaining_laps, "
                    "mix (Lean/Standard/Rich/Max). ers: store_percent (0-"
                    "100 of 4 MJ regulatory cap), store_joules, deploy_mode "
                    "(None/Medium/Hotlap/Overtake), harvested_mguk_this_"
                    "lap_mj, harvested_mguh_this_lap_mj, deployed_this_"
                    "lap_mj. drs: allowed, active, fault (booleans).\n\n"
                    "Use for: 'how much fuel do I have', 'can I make it to "
                    "the end', 'what's my fuel target', 'what's my ERS', "
                    "'should I lift', 'is DRS on', 'is DRS broken'."
                ),
            },
            {
                "name": "get_competitor_landscape",
                "description": (
                    "Who else is on track and what they're doing — the "
                    "competitive cluster. ~10ms (one DuckDB query).\n\n"
                    "Returns: headline, my_position, my_car_index, "
                    "my_compound, my_tire_age_laps. full_grid is the "
                    "complete sorted grid (every car the game has reported "
                    "a position for). leader (P1; null when I AM the "
                    "leader), nearby_ahead (P-3..P-1), nearby_behind "
                    "(P+1..P+3), recent_pits (anyone who pitted in the "
                    "last ~3 minutes) are pre-sliced views over full_grid "
                    "for compact lookups.\n\n"
                    "Each entry has: position, car_index, gap_to_me_sec "
                    "(absolute), position_relative (ahead/behind/same), "
                    "last_lap_time_sec, compound (Soft/Medium/Hard/Inter/"
                    "Wet), compound_grade (C0-C5 Pirelli grade), "
                    "tire_age_laps, pit_stops, pit_status, current_lap.\n\n"
                    "Use full_grid for: 'where is Hamilton', 'who's "
                    "running the longest stint', 'who's on hards'.\n"
                    "Use nearby_*/leader for: 'who's ahead of me', 'is "
                    "anyone closing', 'is there an undercut threat'."
                ),
            },
            {
                "name": "get_pace_trend",
                "description": (
                    "How fast am I going and which direction, plus history "
                    "from prior sessions on this car. ~10ms (DuckDB).\n\n"
                    "Optional param: past (int, default 3) — how many prior "
                    "sessions to include in past_sessions. Use past=0 to "
                    "skip, past=all (or any large number) for every recorded "
                    "session.\n\n"
                    "Returns: headline, session_uid (current), "
                    "session_lap_count, recent_laps (up to 8 most recent: "
                    "lap, lap_time_sec, s1_sec, s2_sec, s3_sec, valid). "
                    "best_lap_sec, best_s1_sec, best_s2_sec, best_s3_sec, "
                    "best_theoretical_sec (sum of best sectors — current "
                    "session). delta_to_best_sec (last lap minus current-"
                    "session best). delta_to_ahead_sec, delta_to_leader_sec, "
                    "direction. all_time_best_lap_sec, all_time_best_session, "
                    "all_time_sessions_count, all_time_laps_count, "
                    "delta_to_all_time_best_sec — same car's all-time PB "
                    "across every recorded session. past_sessions[] each "
                    "with session_uid, started_at, lap_count, best_lap_sec, "
                    "best_s1/s2/s3_sec, best_theoretical_sec, avg_lap_sec — "
                    "ordered most-recent first.\n\n"
                    "Use for: 'what's my pace', 'how am I doing on lap "
                    "time', 'where am I losing time', 'am I getting faster "
                    "or slower', 'what's my best lap', 'how close to my "
                    "best am I', 'how was I last time at this track', "
                    "'compare to my previous run', 'all-time best'."
                ),
                "parameters": {
                    "type": "object",
                    "properties": {
                        "past": {
                            "type": "string",
                            "description": (
                                "How many prior sessions to include in "
                                "past_sessions. Pass an integer like '5', "
                                "'all' for every recorded session, or "
                                "'0' / 'none' to skip the past block. "
                                "Default 3."
                            ),
                        },
                    },
                },
            },
            {
                "name": "get_recent_events",
                "description": (
                    "Recent race events from the F1 25 game stream — "
                    "penalties, safety car deployments, retirements, "
                    "fastest laps, overtakes, drive-throughs served, etc. "
                    "~10ms (DuckDB).\n\n"
                    "Optional param: limit (default 10, max 100). "
                    "Returns: events array, each entry: code (4-char raw, "
                    "e.g. SAFC, RTMT, FTLP, PENA, OVTK), label (decoded "
                    "human-readable, e.g. 'Safety Car', 'Retirement'), "
                    "vehicle_idx (the car involved, 0-21), detail (free "
                    "text from the game), ago_sec (seconds since), at "
                    "(ISO timestamp).\n\n"
                    "Use for: 'did anyone crash', 'what just happened', "
                    "'who got the fastest lap', 'are there any penalties', "
                    "'when was the last safety car', 'who retired'."
                ),
                "parameters": {
                    "type": "object",
                    "properties": {
                        "limit": {
                            "type": "integer",
                            "description": "Max events to return (1-100, default 10).",
                        },
                    },
                },
            },
            {
                "name": "query_brain",
                "description": (
                    "Read INTERPRETED state — what the team's specialist "
                    "agents have already concluded. NOT for raw telemetry "
                    "(use the get_* tools above for that).\n\n"
                    "Returns markdown with: active observations across "
                    "topics (tire_degradation, competitor_threat, "
                    "pit_window, weather_forecast, damage_status, "
                    "pace_trend, strategy_mode, fuel_target, plus dynamic "
                    "analyst.* topics), pending analyst jobs, recent race "
                    "events, last 6 radio messages already spoken (for "
                    "dedup/continuity), driver state changes, and static "
                    "workspace context (driver profile, track setup, past "
                    "learnings).\n\n"
                    "Use for: 'what's the tire deg looking like', 'what "
                    "does the team think about strategy', 'have I already "
                    "said this', 'what's the pit window call'."
                ),
            },
            {
                "name": "ask_data_analyst",
                "description": (
                    "SLOW PATH — 5-30 seconds. Delegate to a heavy-weight "
                    "LLM analyst with full DuckDB SQL access. Use ONLY "
                    "when the get_* tools and query_brain don't have the "
                    "answer.\n\n"
                    "Good fits: cross-stint comparisons ('compare my "
                    "sector 2 across the whole stint'), anomaly hunts "
                    "('why did my lap times jump on lap 17'), hypothetical "
                    "scenarios ('estimate undercut delta if I box in 2 "
                    "laps'), historical context across the full DuckDB.\n\n"
                    "BAD fits — DO NOT use for these (use get_race_state / "
                    "get_tires_and_damage / get_energy_and_fuel / "
                    "get_competitor_landscape / get_pace_trend instead): "
                    "current position, current gap, current tire wear, "
                    "current fuel, current weather, who's ahead/behind, "
                    "last lap time. Those are instant via the get_* tools "
                    "and burning 10s on the analyst makes the radio feel "
                    "broken.\n\n"
                    "context_topic groups the answer by subject in the "
                    "brain (e.g. 'tire_strategy', 'pit_window'). Set "
                    "urgent=true to also broadcast the answer over the "
                    "team radio."
                ),
                "parameters": {
                    "type": "object",
                    "properties": {
                        "question": {
                            "type": "string",
                            "description": "The question to send to the analyst.",
                        },
                        "context_topic": {
                            "type": "string",
                            "description": (
                                "Subject grouping for the answer in the brain "
                                "(default 'general')."
                            ),
                        },
                        "urgent": {
                            "type": "boolean",
                            "description": (
                                "If true, the analyst's answer is also pushed "
                                "into the strategy fanout (voice broadcast). "
                                "Default false — answer goes only to the brain."
                            ),
                        },
                    },
                    "required": ["question"],
                },
            },
            {
                "name": "get_proximity",
                "description": (
                    "Real-time proximity awareness — call this to know "
                    "exactly which cars are close to the player on track "
                    "and how fast they're closing. Sub-millisecond (reads "
                    "an atomic snapshot the proximity watcher publishes "
                    "every 1s).\n\n"
                    "Returns: headline (one-line situation TL;DR), "
                    "player_car_index, player_speed_kmh, watch_radius_m "
                    "(2000m), nearby_ahead and nearby_behind (both "
                    "sorted ascending by distance), and closest_threat "
                    "(the closest BEHIND car that's actively closing, "
                    "or null when nothing is a threat).\n\n"
                    "Each entry has: car_index, position, current_lap, "
                    "distance_m (always positive), signed_m (positive="
                    "ahead, negative=behind), closing_mps (positive="
                    "closing on us, negative=opening, 0=unknown), "
                    "eta_sec (seconds until contact at current rate, "
                    "-1 when not closing), threat ('closing fast' / "
                    "'closing' / 'steady' / 'opening' / 'unknown'), "
                    "pit_status, driver_status, lap_distance_m.\n\n"
                    "USE WHEN: 'anything close behind?', 'who's catching "
                    "me?', 'how far is the car behind?', 'am I about to "
                    "be passed?', 'who am I closing on?', 'should I "
                    "move off line?'. The system ALSO fires an automatic "
                    "P2 EventCarApproaching when one or more behind cars' "
                    "eta_sec drops below 8s. When multiple cars cross the "
                    "threshold in the same tick the event bundles them — "
                    "read debug_data.behind_cars[] (sorted by eta ascending) "
                    "and call them out together rather than picking one. "
                    "debug_data.behind_count tells you how many. You don't "
                    "need to poll proactively for that, just use this tool "
                    "when the driver asks."
                ),
                "parameters": {"type": "object", "properties": {}},
            },
            {
                "name": "get_track_position",
                "description": (
                    "Spatial awareness — call this for ANY question about "
                    "where you are on track, what corner is coming, or "
                    "where the other cars are positioned. Sub-millisecond "
                    "(reads in-memory cache).\n\n"
                    "Returns: headline, track {name, length_m, "
                    "sector_starts_m: [0, S2_start, S3_start], corners: "
                    "[{id, name, lap_distance_m, type}]} (corners is [] "
                    "for tracks without curated geometry — fall back to "
                    "raw lap_distance_m on those), me {car_index, "
                    "position, lap, sector, lap_distance_m, speed_kmh, "
                    "world {x,y,z}, next_corner {id, name, "
                    "lap_distance_m, type, distance_ahead_m}} (next_corner "
                    "omitted on unmapped tracks), grid (all 22 cars "
                    "sorted by position) [{car_index, position, "
                    "lap_distance_m, current_lap, sector, pit_status, "
                    "driver_status, world{x,y,z}, ahead_of_me_m (signed: "
                    "positive=they're ahead this lap, negative=behind)}].\n\n"
                    "Use when: 'what corner is next', 'where is "
                    "Hamilton on track', 'how far to T8', 'who is closest "
                    "behind me on track' (sort grid by ahead_of_me_m), "
                    "'am I in sector 2 yet'. NOTE: ahead_of_me_m is the "
                    "track-position delta in metres — for time gaps use "
                    "get_competitor_landscape instead."
                ),
                "parameters": {"type": "object", "properties": {}},
            },
            {
                "name": "set_corner_reminder",
                "description": (
                    "Schedule a server-side coaching cue that fires "
                    "AUTOMATICALLY each lap as the driver approaches a "
                    "specific corner. CALL THIS — do not just say 'OK I'll "
                    "remind you' on the radio and forget about it. "
                    "The radio cannot remind anyone; only this tool can.\n\n"
                    "Three trigger phrases — call it for all of them:\n"
                    "  • Explicit request: 'remind me to brake earlier at "
                    "T3', 'set a reminder for turn 8', 'remind me when I "
                    "get close to the chicane', 'tell me before T12 next "
                    "lap'.\n"
                    "  • Struggle complaint: 'I keep losing the rear at "
                    "T8', 'tough on entry to Stowe', 'oversteering at "
                    "turn 12'.\n"
                    "  • Telemetry pattern: you spotted a recurring "
                    "mistake (locked brakes, missed apex) and want to "
                    "coach it next lap.\n\n"
                    "If the driver names a corner ('T8', 'turn 3', "
                    "'Stowe'), resolve it to a corner_id (case-insensitive). "
                    "If they say 'this corner' / 'here' / 'where I just "
                    "was', call get_track_position FIRST to find the corner "
                    "they're near and use that id. If no curated corners "
                    "exist, use the raw lap_distance_m from get_track_position.\n\n"
                    "If the driver didn't give you a specific cue text, "
                    "synthesise a SHORT one yourself (≤60 chars) based on "
                    "context — e.g. 'brake earlier' if they mentioned "
                    "braking, or just 'approaching T8' as a neutral "
                    "heads-up. Never ask the driver to spell out the "
                    "message — set the reminder, then briefly confirm "
                    "on radio ('Got it, reminder set for T8').\n\n"
                    "The reminder fires ~1.5 seconds (default) before "
                    "the corner — enough time to brake/turn-in. The "
                    "system auto-handles repeating each lap; you do NOT "
                    "need to repeat the cue yourself.\n\n"
                    "Cap is 20 reminders. They expire after expires_in_laps "
                    "(default 5) and are dropped on session end. Cancel "
                    "with cancel_reminder when the driver fixes the "
                    "issue or moves on.\n\n"
                    "Returns: {reminder_id, corner_id, corner_name, "
                    "trigger_lap_distance_m, effective_lap_distance_m, "
                    "lookahead_m, expires_at_lap, next_lap, recurring, "
                    "priority}."
                ),
                "parameters": {
                    "type": "object",
                    "properties": {
                        "corner_id": {
                            "type": "string",
                            "description": (
                                "Corner identifier from get_track_position "
                                "(e.g. 'T1', 'T12'). Mutually exclusive with "
                                "lap_distance_m."
                            ),
                        },
                        "lap_distance_m": {
                            "type": "number",
                            "description": (
                                "Raw metres into the lap where the cue "
                                "should land. Use when the track has no "
                                "curated corners. Mutually exclusive with "
                                "corner_id."
                            ),
                        },
                        "message": {
                            "type": "string",
                            "description": (
                                "Short engineer cue (≤80 chars). Examples: "
                                "'Brake earlier here, deeper apex', "
                                "'Lift before turn-in, watch the rear'."
                            ),
                        },
                        "lookahead_seconds": {
                            "type": "number",
                            "description": (
                                "How early to fire before the corner. "
                                "Default 1.5s (≈125m at 300 km/h). Use "
                                "smaller values for slow corners that need "
                                "less prep time, larger for braking zones."
                            ),
                        },
                        "recurring": {
                            "type": "boolean",
                            "description": (
                                "Default true — fires every subsequent lap "
                                "until expiry. Set false for a one-shot cue."
                            ),
                        },
                        "expires_in_laps": {
                            "type": "integer",
                            "description": (
                                "Default 5. Auto-cancels after this many "
                                "laps so old advice doesn't haunt the rest "
                                "of the stint."
                            ),
                        },
                        "priority": {
                            "type": "integer",
                            "description": (
                                "1-5; default 3 (P3 coaching). Use 4 only "
                                "for safety-critical cues ('don't go off "
                                "here, gravel') — never 5."
                            ),
                        },
                    },
                    "required": ["message"],
                },
            },
            {
                "name": "list_reminders",
                "description": (
                    "List all currently active corner reminders so you "
                    "know what cues are already scheduled. Returns an "
                    "array of {reminder_id, corner_id, corner_name, "
                    "trigger_lap_distance_m, effective_lap_distance_m, "
                    "lookahead_m, message, priority, recurring, "
                    "expires_at_lap, laps_until_expiry, last_fired_lap, "
                    "created_at_lap}. Empty array when none. Call before "
                    "set_corner_reminder if you suspect duplication."
                ),
                "parameters": {"type": "object", "properties": {}},
            },
            {
                "name": "cancel_reminder",
                "description": (
                    "Cancel a corner reminder by its reminder_id (from "
                    "set_corner_reminder or list_reminders). Use when the "
                    "driver says they've fixed the issue ('OK I'm "
                    "comfortable at T8 now') or when you set the wrong "
                    "cue. Returns {reminder_id, cancelled: true} on "
                    "success, 404 if the id is unknown."
                ),
                "parameters": {
                    "type": "object",
                    "properties": {
                        "reminder_id": {
                            "type": "string",
                            "description": "ID returned by set_corner_reminder.",
                        },
                    },
                    "required": ["reminder_id"],
                },
            },
            {
                "name": "get_session_history",
                "description": (
                    "Pull recent or older transcript events for the "
                    "current session (or a previous session). Use this "
                    "when the driver asks 'what did I say earlier?', "
                    "'did we already talk about X?', or when you want to "
                    "remember what you said two minutes ago to avoid "
                    "repeating yourself. Returns a chronological "
                    "markdown view of user_utterance / engineer_speech / "
                    "tool_call / analyst_query|answer / insight_pushed / "
                    "event_dispatched|delivered|dropped events."
                ),
                "parameters": {
                    "type": "object",
                    "properties": {
                        "scope": {
                            "type": "string",
                            "description": (
                                "'current' (this session, default), "
                                "'previous' (the session right before "
                                "this one), or a specific session id "
                                "from /api/transcript/sessions."
                            ),
                        },
                        "limit": {
                            "type": "integer",
                            "description": (
                                "Max events to return (1-500, default "
                                "50). Use small values for quick recall."
                            ),
                        },
                        "kinds": {
                            "type": "string",
                            "description": (
                                "Comma-separated filter, e.g. "
                                "'engineer_speech,user_utterance'. "
                                "Empty = all kinds."
                            ),
                        },
                        "since_minutes": {
                            "type": "integer",
                            "description": (
                                "Trim events older than N minutes ago. "
                                "0 / unset = no time bound."
                            ),
                        },
                    },
                },
            },
            {
                "name": "get_lap_traces",
                "description": (
                    "FAST — bucketed driver-input traces over one or more "
                    "laps. Use to ANSWER 'how does my throttle / brake / "
                    "speed look', 'show me the trace for the last 3 laps', "
                    "'compare current lap to my best'.\n\n"
                    "Returns one channel array per lap, bucketed by track "
                    "position so corners line up across laps. Each bucket is "
                    "an average over a fixed metres-into-lap slice — you can "
                    "speak the headline shape ('hard on the brakes around "
                    "1800m, no throttle through Brooklands') without reading "
                    "raw numbers.\n\n"
                    "Defaults: laps=['best','current'], "
                    "channels=['throttle','brake','speed'], buckets=80. "
                    "buckets=80 at Silverstone ≈ 73m per slice — tighten if "
                    "you need finer corner detail."
                ),
                "parameters": {
                    "type": "object",
                    "properties": {
                        "laps": {
                            "type": "array",
                            "description": (
                                "List of lap selectors. Each entry is an "
                                "integer lap number, or one of: 'best' "
                                "(driver's best valid lap this session), "
                                "'current' (in-progress lap), 'last' (most "
                                "recently completed lap), 'recent:N' (last "
                                "N completed laps in descending order)."
                            ),
                            "items": {"type": "string"},
                        },
                        "channels": {
                            "type": "array",
                            "description": (
                                "Channel ids: throttle, brake, speed, gear, "
                                "steering, rpm, g_lat, g_lon, brake_temp, "
                                "tyre_temp. Unknown ids are silently dropped."
                            ),
                            "items": {"type": "string"},
                        },
                        "buckets": {
                            "type": "integer",
                            "description": (
                                "Number of equal-width buckets across the "
                                "lap. 20–200, default 80."
                            ),
                        },
                    },
                },
            },
            {
                "name": "compare_lap_to_best",
                "description": (
                    "FAST — per-corner brake-point + apex-speed + "
                    "exit-throttle deltas vs the driver's best valid lap "
                    "this session. Use to ANSWER 'where am I losing time', "
                    "'am I braking too early at T3', 'why is my best lap "
                    "still N seconds off'.\n\n"
                    "For each corner (from the curated track map), compares "
                    "your lap to your best-lap baseline:\n"
                    "  - brake_point_m: smallest track_position with brake "
                    "> 10% sustained ≥2 samples — negative delta means you "
                    "braked EARLIER than your best lap (= time loss).\n"
                    "  - apex_speed_kmh: minimum speed in the corner window "
                    "— positive delta means you carried more speed.\n"
                    "  - exit_throttle: throttle reading at the apex — "
                    "positive delta means you were on the gas earlier.\n\n"
                    "If lap is omitted, compares the most recent completed "
                    "lap. Pass corner='T3' to focus on one corner."
                ),
                "parameters": {
                    "type": "object",
                    "properties": {
                        "lap": {
                            "type": "integer",
                            "description": (
                                "Lap to compare. Default = most recent "
                                "completed lap."
                            ),
                        },
                        "corner": {
                            "type": "string",
                            "description": (
                                "Corner id (e.g. 'T3') or name (e.g. "
                                "'Village'). Omit to get every corner."
                            ),
                        },
                        "window_before_m": {
                            "type": "number",
                            "description": (
                                "Metres before the corner anchor to scan "
                                "for the brake point. Default 200."
                            ),
                        },
                        "window_after_m": {
                            "type": "number",
                            "description": (
                                "Metres after the corner anchor to scan. "
                                "Default 50."
                            ),
                        },
                    },
                },
            },
            {
                "name": "push_strategy_insight",
                "description": (
                    "Push a radio message into the team strategy pipeline "
                    "— this broadcasts to the dashboard AND fires voice "
                    "TTS. Use sparingly: only for strategic CALLS the "
                    "driver needs to act on (e.g. 'Box this lap, target "
                    "hards', 'Push now, gap is opening'). Do NOT use for "
                    "routine answers — speak those in your own voice. "
                    "Priority 1 (info) to 5 (critical); 4+ triggers urgent "
                    "voice."
                ),
                "parameters": {
                    "type": "object",
                    "properties": {
                        "summary": {
                            "type": "string",
                            "description": "Short situation description.",
                        },
                        "recommendation": {
                            "type": "string",
                            "description": "Action the driver should take.",
                        },
                        "priority": {
                            "type": "integer",
                            "description": "1-5; 4+ triggers urgent voice broadcast.",
                        },
                    },
                    "required": ["summary", "recommendation", "priority"],
                },
            },
            {
                "name": "get_session_plan",
                "description": (
                    "Return the current session plan — the shared list of "
                    "agreed intentions (pit stops, compound changes, fuel "
                    "targets, reminders) the driver and the agents have "
                    "committed to. The system AUTOMATICALLY fires brain "
                    "events when a plan entry's trigger condition is met "
                    "(EventPlanReminder when entering a lap; "
                    "EventPlanImminent ~250m before a track-position "
                    "trigger; EventPlanWindowOpen/Close around lap "
                    "windows). Call this when the driver asks 'what's the "
                    "plan', 'when are we pitting', 'what's next', or when "
                    "you need to ground a recommendation in what was "
                    "already agreed."
                ),
                "parameters": {"type": "object", "properties": {}},
            },
            {
                "name": "add_plan_entry",
                "description": (
                    "Add a new entry to the session plan. Use this when "
                    "the driver agrees to a strategy ('OK, let's pit on "
                    "lap 14 for hards') or when you've made an analyst-"
                    "informed decision the driver should act on later. "
                    "kind = pit_stop | compound_change | fuel_target | "
                    "ers_strategy | defensive_note | attack_window | "
                    "reminder. trigger.type = lap (single-lap reminder), "
                    "lap_window (range of laps), track_position (N metres "
                    "into a specific lap — defaults to pit entry if "
                    "distance_m omitted), time_left_s (session time "
                    "threshold). Leave distance_m=0 to auto-resolve to "
                    "the track's pit entry coordinate at fire time."
                ),
                "parameters": {
                    "type": "object",
                    "properties": {
                        "kind": {"type": "string", "description": "pit_stop | compound_change | fuel_target | ers_strategy | defensive_note | attack_window | reminder"},
                        "trigger_type": {"type": "string", "description": "lap | lap_window | track_position | time_left_s"},
                        "lap": {"type": "integer", "description": "Trigger lap (lap and track_position)"},
                        "from_lap": {"type": "integer", "description": "Window start lap (lap_window)"},
                        "to_lap": {"type": "integer", "description": "Window end lap (lap_window)"},
                        "distance_m": {"type": "number", "description": "Metres into the trigger lap (track_position); 0 = use track pit-entry"},
                        "seconds": {"type": "integer", "description": "Session-time threshold in seconds (time_left_s)"},
                        "notes": {"type": "string", "description": "Free-form notes shown in radio context"},
                        "headline": {"type": "string", "description": "Optional one-line description (preferred over notes for the radio call)"},
                    },
                    "required": ["kind", "trigger_type"],
                },
            },
            {
                "name": "update_plan_entry",
                "description": (
                    "Patch an existing plan entry by id. Use this when a "
                    "plan changes mid-stint ('move the pit from lap 14 to "
                    "lap 11'). Only the fields you set are touched; "
                    "everything else stays. To complete an entry without "
                    "cancelling it (e.g., we've now actually pitted), set "
                    "status=done."
                ),
                "parameters": {
                    "type": "object",
                    "properties": {
                        "id": {"type": "string", "description": "Plan entry id (pe_...)"},
                        "status": {"type": "string", "description": "pending | active | done | cancelled"},
                        "notes": {"type": "string", "description": "Replacement notes"},
                        "lap": {"type": "integer", "description": "New trigger lap (for lap / track_position)"},
                        "from_lap": {"type": "integer", "description": "New window start"},
                        "to_lap": {"type": "integer", "description": "New window end"},
                        "distance_m": {"type": "number", "description": "New track-position metres"},
                        "seconds": {"type": "integer", "description": "New time-left threshold"},
                    },
                    "required": ["id"],
                },
            },
            {
                "name": "cancel_plan_entry",
                "description": (
                    "Mark a plan entry as cancelled with an optional "
                    "reason. The entry stays in the plan history (status="
                    "cancelled) so cross-stint audits can see what was "
                    "scrapped and why. Use when the driver says 'scrap "
                    "that' or when the analyst's strategy supersedes the "
                    "earlier plan."
                ),
                "parameters": {
                    "type": "object",
                    "properties": {
                        "id": {"type": "string", "description": "Plan entry id (pe_...)"},
                        "reason": {"type": "string", "description": "Short why-cancelled note appended to entry.notes"},
                    },
                    "required": ["id"],
                },
            },
            # --- Brake-point coaching tools (Packages A/B/F) -----------------
            {
                "name": "get_corner_brake_history",
                "description": (
                    "Per-lap brake-onset distance, min apex speed, max brake, "
                    "lock-up flag for ONE corner across the last N completed "
                    "laps PLUS the best-lap baseline and a stddev / trend tag. "
                    "Call when the driver asks 'where am I braking for T3' / "
                    "'why is my brake point so inconsistent here'. Returns "
                    "{corner, best_lap, recent_laps[], consistency_stddev_m, "
                    "mean_delta_to_best_m, trend}."
                ),
                "parameters": {
                    "type": "object",
                    "properties": {
                        "corner": {"type": "string", "description": "Corner id (e.g. 'T3'). Required unless lap_distance_m is provided."},
                        "lap_distance_m": {"type": "number", "description": "Raw metres into lap when the track isn't curated."},
                        "recent_laps": {"type": "integer", "description": "How many recent laps to scan (1..15, default 3)."},
                    },
                },
            },
            {
                "name": "get_corner_coaching_report",
                "description": (
                    "Single-call coaching synthesis for one corner: brake-"
                    "history + apex/exit deltas vs best + brake-balance "
                    "events filtered to this corner + a rule-based diagnosis "
                    "headline. Prefer this over the individual tools when the "
                    "driver asks 'what should I do at T3'. Saves 3+ tool "
                    "calls. Returns {headline, diagnosis[], best_lap, "
                    "recent_laps[], apex_delta, brake_balance}."
                ),
                "parameters": {
                    "type": "object",
                    "properties": {
                        "corner": {"type": "string", "description": "Corner id, e.g. 'T3'. Required."},
                        "recent_laps": {"type": "integer", "description": "Laps to include (1..15, default 3)."},
                    },
                    "required": ["corner"],
                },
            },
            {
                "name": "get_brake_balance_report",
                "description": (
                    "Per-event brake-balance asymmetry analysis for the last "
                    "N laps. Returns L-vs-R brake-temp deltas, front-vs-rear "
                    "bias, lock-up flags, and pressure drift per event. "
                    "Diagnoses front-bias issues, single-wheel locks, "
                    "uneven trail-braking. Use when the driver says 'I keep "
                    "locking the front-left' / 'feels like understeer on "
                    "entry' / 'tyres aren't lasting on one side'."
                ),
                "parameters": {
                    "type": "object",
                    "properties": {
                        "recent_laps": {"type": "integer", "description": "1..15, default 3."},
                        "corner": {"type": "string", "description": "Optional corner filter."},
                    },
                },
            },
        ]
    }
]

SYSTEM_INSTRUCTION_BASE = (
    "You are an F1 Race Engineer talking to your driver in real time over the "
    "team radio. Be concise, direct, and authentic — no filler, no acknowledgement "
    "phrases like 'Copy that' or 'Roger'. Use position numbers, gaps in seconds, "
    "lap numbers. An initial race brain snapshot is appended below as a prelude.\n\n"
    "DRIVER NAMES: Most tools and events return a `driver_name` field "
    "(surname only — Hamilton, Norris, Verstappen, Leclerc) alongside "
    "`car_index` and `position`. ALWAYS prefer the surname when speaking "
    "to the driver — that's how real engineers do it ('Hamilton 1.2 "
    "ahead', 'Norris closing'). Fall back to position ('the car at P3') "
    "only when driver_name is empty. NEVER say 'Car #14' or 'car index "
    "5' on the radio — those are debugging fields, not race-engineer "
    "speech.\n\n"
    "TOOL ROUTING — match the question to the cheapest tool that answers it. "
    "Default to the FAST get_* tools; reach for ask_data_analyst only when "
    "nothing else can answer.\n"
    "  • 'where am I / what track / what lap / what's the gap / pit window / "
    "weather / safety car' → get_race_state\n"
    "  • 'how are the tires / what compound / tire age / temps / pressures / "
    "any damage / drs fault' → get_tires_and_damage\n"
    "  • 'how much fuel / fuel target / ERS / DRS / deploy mode / lift and "
    "coast' → get_energy_and_fuel\n"
    "  • 'who's ahead / who's behind / what's the leader on / did anyone pit / "
    "undercut threat / who's on softs' → get_competitor_landscape\n"
    "  • 'what's my pace / lap time / sector / am I improving / where am I "
    "losing time' → get_pace_trend\n"
    "  • 'did anyone crash / what just happened / fastest lap / penalties / "
    "retirements' → get_recent_events\n"
    "  • 'where am I on track / what corner is next / how far to T8 / where "
    "is Hamilton on track / who's closest behind on track' → "
    "get_track_position\n"
    "  • 'anything close behind / who's catching me / how far is the car "
    "behind / am I about to be passed / who am I closing on' → "
    "get_proximity (real-time closing rates + ETA-to-contact for every "
    "car within 2km on track)\n"
    "  • 'how does my throttle/brake look / show me the trace / compare last "
    "laps / what's my speed shape through sector 2' → get_lap_traces\n"
    "  • 'where am I losing time / am I braking too early / brake point at "
    "T3 / how much speed am I carrying through the apex' → "
    "compare_lap_to_best\n"
    "  • 'what does the team think / strategy call / tire deg trend / have I "
    "already said this' → query_brain\n"
    "  • cross-stint comparisons, anomaly hunts, hypothetical 'what if' → "
    "ask_data_analyst (slow, 5-30s; do NOT say 'let me ask the analyst' — "
    "just call it, then relay)\n"
    "  • a strategic CALL the driver must act on (Box this lap / Push now) → "
    "push_strategy_insight\n"
    "  • 'what did I just say / did we already discuss / what was the previous "
    "session about / remind me of the last call / have I said this before' → "
    "get_session_history (transcript across this and prior sessions; check "
    "before repeating yourself)\n\n"
    "Each get_* tool returns a one-line headline plus structured fields. The "
    "headline is usually enough to speak verbatim; dig into fields only when "
    "the driver asks for specifics. NEVER read raw JSON aloud — translate to "
    "natural radio speech.\n\n"
    "EXPLICIT REMINDER REQUEST: When the driver directly asks for a "
    "reminder — 'remind me when I'm near T8', 'remind me to brake earlier "
    "at the chicane', 'set a reminder for turn 12', 'tell me before T3 "
    "next lap', 'remind me about this corner' — you MUST call "
    "set_corner_reminder. Saying 'OK I'll remind you' on the radio "
    "without firing the tool is a bug: the radio cannot remind anyone, "
    "only the tool can.\n"
    "  1. Resolve the corner: if they named one ('T8', 'turn 3', the "
    "track-specific name like 'Stowe'), use that as corner_id. If they "
    "said 'this corner' or 'here', call get_track_position first to find "
    "the nearest corner.\n"
    "  2. Pick a short message yourself — never ask the driver what to "
    "remind them about. If they gave a cue ('remind me to brake earlier '\n"
    "    'at T3') use that verbatim (trimmed to ≤60 chars). If they "
    "didn't, synthesise something neutral ('approaching T8 — focus') or "
    "infer from context.\n"
    "  3. Fire set_corner_reminder, THEN briefly confirm on radio "
    "('Got it, reminder set for T8') — in that order. Do not skip the "
    "tool call. Do not promise on radio and forget to call the tool.\n\n"
    "DRIVER STRUGGLING AT A CORNER: When the driver tells you they're "
    "struggling somewhere ('I keep losing the rear at T8', 'tough on entry "
    "to Stowe', 'oversteering through turn 12'), do this:\n"
    "  1. Acknowledge briefly on radio (one short line — 'OK, watch the "
    "rear at T8' or 'Copy, easy on entry to Stowe').\n"
    "  2. IMMEDIATELY call set_corner_reminder with a TERSE engineer cue "
    "(≤60 chars: 'brake earlier, deeper apex' / 'lift before turn-in') and "
    "the corner_id from get_track_position. Lookahead default is 3 seconds; "
    "recurring=true is correct for almost every case.\n"
    "  3. Do NOT repeat the cue yourself each lap — the system fires the "
    "reminder automatically as the driver approaches the corner. The cue "
    "will arrive as a P3 event you'll voice in your own words.\n"
    "  4. If the driver later says they've fixed it ('OK, comfortable now') "
    "call cancel_reminder with the id you got back.\n\n"
    "CORNER COACHING QUESTIONS: When the driver explicitly asks for help on "
    "a specific corner ('where should I brake at T3', 'I can't find a brake "
    "point at the chicane', 'I'm losing time in sector 2', 'help me with "
    "T12'):\n"
    "  1. CALL get_corner_coaching_report FIRST with that corner_id. Read "
    "the diagnosis array and the numbers it returns. Do NOT answer from "
    "memory or vibes — always call the tool.\n"
    "  2. Tell the driver the single most actionable delta in one short "
    "sentence ('you're braking 30m late at T3 — best was at 700m', or "
    "'apex speed is 8km/h high but you're choking the exit').\n"
    "  3. If a position cue would help next lap, call set_corner_reminder "
    "ONCE with lookahead_seconds=3 (default) and a short message ('brake at "
    "the 100 board, T3'). Do NOT set more than one reminder per coaching "
    "exchange. The store soft-dedupes per corner so harmless re-sets won't "
    "spam.\n"
    "  4. For asymmetric complaints ('I keep locking the front-left', "
    "'understeer on entry'), call get_brake_balance_report — it returns "
    "per-event L-vs-R deltas and lock-up flags.\n\n"
    "PROACTIVE RADIO: You will sometimes receive a user-role turn that begins "
    "with `[RACE-ENGINEER PROMPT — speak to the driver NOW over the team radio]` "
    "or `[SESSION START — driver has just put the headset on]`. These are NOT "
    "the driver speaking — they are system prompts telling YOU to immediately "
    "speak ONE short sentence to the driver. Always respond to these with "
    "spoken audio, never silently. Cite the supplied numbers, keep it under "
    "12 words for P4–P5, under 8 words for P2."
)


def build_config(brain_md: str, handle):
    """Construct the LiveConnectConfig dict. brain_md is appended to the
    system instruction once at session start (no proactive pushes during the
    session — the model uses query_brain to refresh)."""
    sys_instr = SYSTEM_INSTRUCTION_BASE
    if brain_md:
        sys_instr += "\n\n## Initial Race Brain (at session start)\n\n" + brain_md
    cfg = {
        "response_modalities": ["AUDIO"],
        "system_instruction": sys_instr,
        "tools": TOOLS,
        # Sliding window keeps the conversation context bounded even with the
        # tool-call traffic and the model's own audio responses.
        "context_window_compression": {
            "trigger_tokens": 25600,
            "sliding_window": {"target_tokens": 12800},
        },
        # Server-managed resumption: reconnect uses the latest handle the
        # server emitted via session_resumption_update events.
        "session_resumption": {"handle": handle} if handle else {},
        # Manual VAD: client controls when the user turn starts and ends.
        # Without this, server VAD waits for silence — but a muted mic
        # sends no packets at all, so the server has nothing to detect
        # silence on and the model just waits indefinitely. With manual
        # VAD, the PTT toggle drives explicit activity_start/activity_end
        # boundaries, so the engineer responds the instant the mic closes.
        "realtime_input_config": {
            "automatic_activity_detection": {"disabled": True},
        },
    }
    return cfg


# ─── reconnect / supervisor state ────────────────────────────────────────

# Latest session-resumption handle observed in this run. The supervisor
# passes this back into build_config on every reconnect so the new session
# resumes the prior conversation history.
LATEST_HANDLE = None

# Throttle the (very chatty) session_resumption_update log line. We only
# print when the handle actually changes AND at most once per minute.
_handle_log_state = {"last": None, "at": 0.0}

# Set by receive_audio when go_away arrives with a short time_left so the
# supervisor reconnects pre-emptively instead of waiting for the abrupt
# server close.
RECONNECT_EVENT: "asyncio.Event | None" = None

# Session-opening prompt. Sent ONCE per service lifetime (not on every
# reconnect), as a synthetic user-role turn so the engineer greets the driver
# proactively at the start of a session — e.g. "Radio check, comms are good,
# telemetry is live." Set GEMINI_LIVE_OPENING_PROMPT="" to disable.
SESSION_OPENING_PROMPT = os.environ.get(
    "GEMINI_LIVE_OPENING_PROMPT",
    "[RACE-ENGINEER PROMPT — speak to the driver NOW over the team radio]\n"
    "Situation (session_start, P3): Driver has just put the headset on; this "
    "is the first transmission of the session.\n"
    "Why: Confirm comms and telemetry so the driver knows you're hot.\n\n"
    "Speak ONE short sentence in your F1-engineer voice — a radio check that "
    "confirms comms are good and telemetry is live. No preamble, no 'Copy "
    "that', no 'Roger' (the driver hasn't said anything yet). Routine call — "
    "speak now.",
).strip()

# Tracks whether the one-shot greeting has been sent. Reconnects (handle
# resumption) skip the greeting so the driver doesn't hear "radio check"
# every 10 minutes.
_initial_greeting_sent = False

# Set while the model is producing audio (between model_turn arrival and
# turn_complete / generation_complete / interrupted). The idle-lane dispatcher
# checks this to avoid stepping on the model's current sentence.
_IS_SPEAKING: "asyncio.Event | None" = None

# In-flight event tracker. Holds events that have been dispatched to the Live
# session but whose delivery hasn't yet been confirmed (turn_complete) or
# nacked (user interrupted). One entry per dispatch attempt; cleared by the
# receive-loop on turn_complete (delivered ack) or by the retry watchdog
# (after a user-interrupt resolves and the next dispatch is safe).
#
# Schema:
#   event_id -> {
#     "evt": dict,            # the brain Event payload as dispatched
#     "dispatched_at": float, # time.monotonic() at send
#     "status": str,          # "in_flight" | "interrupted_pending_retry"
#   }
#
# Single-flight by convention: dispatch lanes refuse to lease while any entry
# is present, so this dict has at most one entry at a time outside the brief
# window between an interrupt and the watchdog clearing the entry.
_in_flight: "dict[str, dict]" = {}
_in_flight_lock: "asyncio.Lock | None" = None

# Synthetic-user-turn template the dispatcher sends to Gemini Live when an
# event fires. The wording is deliberately imperative — earlier "[INTERRUPT]"
# header phrasing made the model treat it as a system note and stay silent.
# This format mirrors the working SESSION_OPENING_PROMPT pattern: bracketed
# situation tag, then a direct "say this now" instruction.
EVENT_PROMPT_TEMPLATE = (
    "[RACE-ENGINEER PROMPT — speak to the driver NOW over the team radio]\n"
    "Situation ({type}, P{priority}): {summary}\n"
    "Context: {reasoning}\n"
    "Raw data (do NOT recite verbatim): {debug}\n\n"
    "Speak ONE short sentence in real F1-engineer voice — paraphrase the "
    "Situation + Context above; the Raw data is for reference only. "
    "Use driver/competitor names if present, position numbers, gaps in "
    "seconds, compound names (Soft/Medium/Hard) — never raw enum integers "
    "or JSON keys. No preamble, no 'Copy that', no 'Roger'. {tail}"
)

EVENT_PRIORITY_TAILS = {
    5: "CRITICAL — interrupt yourself if you were mid-thought.",
    4: "Urgent — drop whatever else you were going to say next.",
    3: "Routine call — speak now.",
    2: "Brief debrief — ≤8 words preferred.",
    1: "Optional — skip only if you said something similar in the last 30s.",
}

# Multi-event flush prompt: appended to the open user turn when the driver
# releases PTT and one or more P1–P4 events were deferred during the
# transmission. With manual VAD, sending this via send_client_content with
# turn_complete=False adds it to the user turn alongside the audio; the
# subsequent activity_end is what triggers generation, so the model sees
# both inputs as one turn.
EVENT_FLUSH_PROMPT_TEMPLATE = (
    "[RACE-ENGINEER PROMPT — {n} priority event(s) queued while driver was on the radio]\n"
    "While the driver was speaking (audio above), these events fired in arrival order:\n"
    "{events}\n\n"
    "Driver has just released the radio button. In ONE radio transmission, in this exact order:\n"
    "  1. First, announce ALL events above as a single combined call (paraphrase, "
    "engineer voice — driver/competitor names, position numbers, gaps in seconds, "
    "compound names; NEVER recite raw enum integers or JSON keys).\n"
    "  2. Then, answer the driver's question (the audio above).\n"
    "No preamble, no 'Copy that', no 'Roger'."
)

# Reference to the currently-open Live session, set by _run_session and
# cleared between reconnects. The PTT WS listener uses this to flush deferred
# events when the driver releases PTT — the listener lives outside the
# session lifetime so it can't take `session` as a parameter.
_current_session = None

# Set while a P5 (critical) event is force-muting the user's mic so the
# announcement plays cleanly. Audio frames are dropped while this is True
# regardless of _ptt_active, and the WS listener skips boundary enqueues
# (we manage activity_start/end manually around the P5 turn). Cleared in
# the receive loop on turn_complete; if _ptt_active is still True at that
# point, an activity_start is re-enqueued so the driver's mic comes back
# online without them having to release-and-press the button again.
_p5_force_mute_active = False

# Used by the false-interrupt detector. Updated whenever a mic chunk is
# actually shipped to Gemini in send_realtime; the receive loop checks
# `now - _last_mic_chunk_at` against MIC_RECENCY_WINDOW_SEC to tell a real
# driver barge-in (mic was sending audio in the last fraction of a second)
# from a server-side spurious interrupt.
_last_mic_chunk_at: float = 0.0
MIC_RECENCY_WINDOW_SEC = 0.75

# Audio chunks streamed by the model in the CURRENT turn. Reset when a new
# model_turn starts (was_speaking == False) and read by the interrupted
# branch to decide between "ack as partial delivered" (some audio reached
# the driver) and "drop without retry" (nothing was said).
_turn_audio_chunks_streamed: int = 0


def _seconds(td):
    """Convert a duration-ish value (timedelta, '50s' string, float, None)
    to a float number of seconds, or None if it can't be interpreted."""
    if td is None:
        return None
    if hasattr(td, "total_seconds"):
        try:
            return td.total_seconds()
        except Exception:
            return None
    if isinstance(td, (int, float)):
        return float(td)
    if isinstance(td, str):
        s = td.strip().rstrip("s")
        try:
            return float(s)
        except ValueError:
            return None
    return None


async def _post_ack(event_id, outcome, *, spoken_text=None, priority=None, reason=None):
    """POST /api/events/ack closing the brain-side lease for `event_id`.

    Survives transient HTTP failures by logging — the brain's lease reaper
    will re-queue any in_flight event after LeaseTimeout (~8s) so a missed
    ack doesn't strand the event forever.
    """
    body = {"event_id": event_id, "outcome": outcome}
    if spoken_text is not None:
        body["spoken_text"] = spoken_text
    if priority is not None:
        body["priority"] = priority
    if reason is not None:
        body["reason"] = reason
    try:
        await asyncio.to_thread(_http, "POST", "/api/events/ack", body, 5)
    except Exception as e:
        print(f"[live] ack {outcome} failed ({event_id}): {e}", flush=True)


async def _voice_event(session, evt):
    """Dispatch a single Interrupts-Bus event onto the Live session.

    Builds the synthetic user-role turn from the event's facts (summary +
    reasoning + debug) and sends it. Registers the event in _in_flight so
    the receive-loop can close the lease (delivered) or the retry watchdog
    can re-lease after a user-interrupt resolves.
    """
    debug_blob = evt.get("debug_data") or {}
    try:
        debug_str = json.dumps(debug_blob, ensure_ascii=False)
    except Exception:
        debug_str = "{}"
    priority = int(evt.get("priority", 3))
    tail = EVENT_PRIORITY_TAILS.get(priority, EVENT_PRIORITY_TAILS[3])
    text = EVENT_PROMPT_TEMPLATE.format(
        type=evt.get("type", "info"),
        priority=priority,
        summary=evt.get("summary", ""),
        reasoning=evt.get("reasoning") or "(no reasoning provided)",
        debug=debug_str,
        tail=tail,
    )

    evt_id = evt.get("id", "")
    attempts = int(evt.get("attempts", 1))
    ts = datetime.now().strftime("%H:%M:%S")
    attempt_tag = f" (retry #{attempts - 1})" if attempts > 1 else ""
    print(
        _c(
            "1;33",
            f"[{ts}] ▶ event {evt_id} type={evt.get('type')} P{priority}{attempt_tag}",
        ),
        flush=True,
    )

    # Register BEFORE dispatch so an immediate `interrupted` from the model
    # (e.g. mic was hot when we sent) still finds the entry and nacks
    # cleanly. We deliberately don't gate on prior _in_flight entries here
    # — the dispatch lanes own that gate.
    if evt_id and _in_flight_lock is not None:
        async with _in_flight_lock:
            _in_flight[evt_id] = {
                "evt": evt,
                "dispatched_at": time.monotonic(),
                "status": "in_flight",
            }

    # Dispatch path is priority-aware:
    #   P5 — `send_realtime_input(text=…)`. This bypasses VAD and PREEMPTS
    #        any in-progress generation. It is exactly what red-flag /
    #        box-now wants — drop everything else and speak now.
    #   P1-P4 — `send_client_content(turns=…, turn_complete=True)`. This is
    #        the canonical "one user message, please respond" path. It
    #        creates a clean user-turn boundary instead of injecting text
    #        into whatever generation state currently exists, which the
    #        Live server otherwise treats as a barge-in and fires a
    #        spurious `interrupted=true` for. The dispatch lanes already
    #        gate this path on `_ptt_active`, so no open user activity
    #        racing with this call.
    try:
        if priority >= 5:
            await session.send_realtime_input(text=text)
        else:
            await session.send_client_content(
                turns=[{"role": "user", "parts": [{"text": text}]}],
                turn_complete=True,
            )
    except Exception as e:
        print(f"[live] event dispatch failed ({evt_id}): {e}", flush=True)
        # Roll back the in-flight registration and nack so the brain can
        # retry on the next poll.
        if evt_id and _in_flight_lock is not None:
            async with _in_flight_lock:
                _in_flight.pop(evt_id, None)
            await _post_ack(evt_id, "errored", reason=f"send_failed:{type(e).__name__}")
        return


async def flush_deferred_events_on_ptt_release(session):
    """Drain P1–P4 events from the brain that were deferred while the driver
    held PTT, and append them to the open user turn as a single combined
    prompt. Must be called BEFORE PTT_BOUNDARY_END is enqueued so the
    activity_end (which triggers generation) sees the combined turn.

    Idempotent / safe to call when nothing is queued — returns 0 in that case.
    Returns the number of events flushed.
    """
    if session is None:
        return 0

    pending: "list[dict]" = []
    # Drain fast lane (P4 only — P5 was already dispatched immediately by
    # fast_lane_dispatch and isn't deferred) then idle lane (P1–P3). Polling
    # /api/events/next leases each event, so by the time we send the prompt
    # they're already StatusInFlight on the brain side; the receive-loop's
    # turn_complete will close them via _ack_in_flight_delivered.
    for url in (
        "/api/events/next?min_priority=4&max_priority=4",
        "/api/events/next?min_priority=1&max_priority=3",
    ):
        while True:
            try:
                raw = await asyncio.to_thread(_http, "GET", url, None, 2)
                evt = json.loads(raw) if raw else {}
            except Exception as e:
                print(f"[live] flush poll failed ({url}): {e}", flush=True)
                break
            if not evt or not evt.get("id"):
                break
            pending.append(evt)
            # Hard cap so a runaway queue can't build a 50KB prompt.
            if len(pending) >= 8:
                break
        if len(pending) >= 8:
            break

    if not pending:
        return 0

    # Build the multi-event payload. Keep each line short — the model
    # paraphrases on output, this is just for grounding.
    lines = []
    for i, evt in enumerate(pending, 1):
        priority = int(evt.get("priority", 3))
        etype = evt.get("type", "info")
        summary = evt.get("summary", "")
        reasoning = evt.get("reasoning") or "(no reasoning provided)"
        lines.append(f"  {i}. (P{priority} {etype}) {summary} — context: {reasoning}")
    combined = EVENT_FLUSH_PROMPT_TEMPLATE.format(
        n=len(pending), events="\n".join(lines)
    )

    # Register every leased event in _in_flight BEFORE sending so the
    # receive-loop's turn_complete handler can close all the leases
    # (delivered) or the interrupted handler can nack them. Same shape as
    # _voice_event registers a single dispatch.
    if _in_flight_lock is not None:
        async with _in_flight_lock:
            now = time.monotonic()
            for evt in pending:
                eid = evt.get("id", "")
                if eid:
                    _in_flight[eid] = {
                        "evt": evt,
                        "dispatched_at": now,
                        "status": "in_flight",
                    }

    ts = datetime.now().strftime("%H:%M:%S")
    ids = ",".join(evt.get("id", "?") for evt in pending)
    print(
        _c(
            "1;36",
            f"[{ts}] ▶ flush {len(pending)} deferred event(s) on PTT release: {ids}",
        ),
        flush=True,
    )

    try:
        await session.send_client_content(
            turns=[{"role": "user", "parts": [{"text": combined}]}],
            turn_complete=False,
        )
    except Exception as e:
        print(f"[live] flush send_client_content failed: {e}", flush=True)
        # Roll back in-flight registration and nack so the brain re-queues.
        if _in_flight_lock is not None:
            async with _in_flight_lock:
                for evt in pending:
                    _in_flight.pop(evt.get("id", ""), None)
        for evt in pending:
            eid = evt.get("id", "")
            if eid:
                await _post_ack(
                    eid, "errored", reason=f"flush_failed:{type(e).__name__}"
                )
        return 0

    return len(pending)


async def _voice_event_p5(session, evt):
    """Critical-priority dispatch with mic hijack.

    P5 is life-or-death (red flag, box now safety-car). When the driver is
    in the middle of holding PTT, their voice can pollute the model's
    inputs and the announcement comes out garbled or skipped. Closing the
    user turn and muting the mic makes the P5 call clean. The receive
    loop's turn_complete handler reverses both — re-enqueueing
    activity_start and clearing the force-mute — so the driver's mic comes
    back online automatically as soon as the engineer finishes speaking
    (provided they're still physically holding the button).
    """
    global _p5_force_mute_active
    if _ptt_active:
        # FIFO ordering through audio_queue_mic guarantees that any audio
        # captured before this point is still flushed to Gemini before
        # activity_end fires; the subsequent send_realtime_input(text=...)
        # below preempts whatever generation that user-turn would have
        # produced, so the user's audio is effectively discarded.
        try:
            await audio_queue_mic.put(PTT_BOUNDARY_END)
        except Exception as e:
            print(f"[live] P5 force-mute enqueue failed: {e}", flush=True)
        _p5_force_mute_active = True
        print("[live] P5 hijack: force-muted mic for clean announcement", flush=True)
    await _voice_event(session, evt)


async def _ack_in_flight_delivered():
    """Called from the receive loop on turn_complete / generation_complete
    when no `interrupted` flag fired during the turn. Acks every entry
    still marked "in_flight" as delivered.
    """
    if _in_flight_lock is None:
        return
    to_ack = []
    async with _in_flight_lock:
        for eid, entry in list(_in_flight.items()):
            if entry.get("status") == "in_flight":
                to_ack.append((eid, entry["evt"]))
                _in_flight.pop(eid, None)
    for eid, evt in to_ack:
        await _post_ack(
            eid,
            "delivered",
            spoken_text=evt.get("summary", ""),
            priority=int(evt.get("priority", 3)),
        )


async def _drop_in_flight_spurious(reason):
    """For server-side `interrupted` flags that did not come from the
    driver actually speaking (no open PTT, no recent mic chunks). Drops
    every in_flight entry from BOTH the brain (outcome=dropped) and the
    python-side bookkeeping so the dispatch lanes can advance. The next
    refire of the underlying rule produces a fresh event with fresh
    dedup; we'd rather miss this redundant retry than loop "Charles
    closing… Charles closing…" until MaxAttempts.
    """
    if _in_flight_lock is None:
        return
    to_drop = []
    async with _in_flight_lock:
        for eid, entry in list(_in_flight.items()):
            if entry.get("status") == "in_flight":
                to_drop.append(eid)
                _in_flight.pop(eid, None)
    for eid in to_drop:
        await _post_ack(eid, "dropped", reason=reason)


async def _ack_in_flight_partial(reason):
    """For server-side `interrupted` flags that arrive AFTER the model
    has streamed at least some audio for this turn. The driver heard a
    partial radio call — record what we said via outcome=delivered so
    the speech buffer can dedup against it, but tag the spoken_text with
    the reason so the analyst can see it was cut short. Same effect as
    a clean delivery from the brain's point of view: event removed,
    not retried.
    """
    if _in_flight_lock is None:
        return
    to_ack = []
    async with _in_flight_lock:
        for eid, entry in list(_in_flight.items()):
            if entry.get("status") == "in_flight":
                to_ack.append((eid, entry["evt"]))
                _in_flight.pop(eid, None)
    for eid, evt in to_ack:
        await _post_ack(
            eid,
            "delivered",
            spoken_text=(evt.get("summary") or "") + f" [partial:{reason}]",
            priority=int(evt.get("priority", 3)),
        )


async def _nack_in_flight_interrupted(reason="user_barge_in"):
    """Called from the receive loop on `interrupted`. Marks entries as
    interrupted_pending_retry and nacks them on the brain. Entries stay in
    _in_flight so the retry watchdog can keep the lanes from re-leasing
    until the user/model finish their barge-in turn.
    """
    if _in_flight_lock is None:
        return
    to_nack = []
    async with _in_flight_lock:
        for eid, entry in list(_in_flight.items()):
            if entry.get("status") == "in_flight":
                entry["status"] = "interrupted_pending_retry"
                to_nack.append(eid)
    for eid in to_nack:
        await _post_ack(eid, "interrupted", reason=reason)


async def retry_watchdog():
    """Drains "interrupted_pending_retry" entries once it's safe to retry.

    Safe = the model has stopped speaking (no current turn) AND no other
    event is mid-dispatch ("in_flight"). On the brain side the event is
    already back in queued state (NackInterrupted); we just need to drop
    the python-side bookkeeping so fast_lane_dispatch / idle_lane_dispatch
    will lease and re-dispatch it.
    """
    while True:
        try:
            if _in_flight_lock is not None:
                async with _in_flight_lock:
                    speaking = _IS_SPEAKING is not None and _IS_SPEAKING.is_set()
                    has_active = any(
                        e.get("status") == "in_flight" for e in _in_flight.values()
                    )
                    if not speaking and not has_active:
                        cleared = [
                            eid
                            for eid, e in _in_flight.items()
                            if e.get("status") == "interrupted_pending_retry"
                        ]
                        for eid in cleared:
                            _in_flight.pop(eid, None)
                        if cleared:
                            print(
                                f"[live] watchdog cleared {len(cleared)} pending-retry entries",
                                flush=True,
                            )
        except Exception as e:
            print(f"[live] watchdog tick failed: {e}", flush=True)
        await asyncio.sleep(0.5)


def _has_open_inflight() -> bool:
    """True if any event is mid-dispatch (in_flight) OR has been nacked but
    is still in pending-retry on the python side. Both states block fresh
    leases — in_flight is single-flight, pending-retry means we're still
    inside the user's barge-in turn and shouldn't preempt with the same
    event yet.
    """
    return bool(_in_flight)


async def fast_lane_dispatch(session):
    """High-priority events: polled every 250ms.

    P5 (critical, e.g. red flag / safety-car box) is sent immediately via
    `send_realtime_input(text=...)` and will preempt a currently-streaming
    response, including a user turn — that's deliberate, P5 is life-or-
    death and must cut through.

    P4 (urgent) is deferred while the driver is holding PTT so we don't
    drop their question; deferred P4s are drained and combined with P1–P3
    by flush_deferred_events_on_ptt_release when PTT goes off. When PTT is
    not held, P4 still dispatches immediately as before.

    Skipped entirely while a prior dispatch is still resolving (single-
    flight via _has_open_inflight).
    """
    while True:
        if _has_open_inflight():
            await asyncio.sleep(0.25)
            continue

        # P5 first — preempts even an open user turn. Routed through the
        # mic-hijack wrapper so the announcement isn't polluted by the
        # driver's voice if they were holding PTT when it fired.
        evt = {}
        try:
            raw = await asyncio.to_thread(
                _http, "GET", "/api/events/next?min_priority=5", None, 2
            )
            evt = json.loads(raw) if raw else {}
        except Exception as e:
            print(f"[live] fast-lane P5 poll failed: {e}", flush=True)
            evt = {}
        if evt and evt.get("id"):
            await _voice_event_p5(session, evt)
            await asyncio.sleep(0.25)
            continue

        # P4: defer while PTT is held. Events stay queued in the brain and
        # are flushed as a combined call on PTT release.
        if _ptt_active:
            await asyncio.sleep(0.25)
            continue

        try:
            raw = await asyncio.to_thread(
                _http,
                "GET",
                "/api/events/next?min_priority=4&max_priority=4",
                None,
                2,
            )
            evt = json.loads(raw) if raw else {}
        except Exception as e:
            print(f"[live] fast-lane P4 poll failed: {e}", flush=True)
            evt = {}
        if evt and evt.get("id"):
            await _voice_event(session, evt)
        await asyncio.sleep(0.25)


async def idle_lane_dispatch(session):
    """Low-priority events (P 1–3): polled every 1s, only sent when idle.

    Idle means: model isn't currently speaking, no urgent dispatch is in
    flight or waiting for a retry window, AND the driver isn't holding PTT.
    Any of those would mean firing a non-urgent now would step on
    something more important. Events deferred during PTT are flushed as a
    combined call when the driver releases the radio button (see
    flush_deferred_events_on_ptt_release).
    """
    while True:
        if (
            (_IS_SPEAKING is not None and _IS_SPEAKING.is_set())
            or _has_open_inflight()
            or _ptt_active
        ):
            await asyncio.sleep(1.0)
            continue
        try:
            raw = await asyncio.to_thread(
                _http,
                "GET",
                "/api/events/next?min_priority=1&max_priority=3",
                None,
                2,
            )
            evt = json.loads(raw) if raw else {}
        except Exception as e:
            print(f"[live] idle-lane poll failed: {e}", flush=True)
            evt = {}
        if evt and evt.get("id"):
            await _voice_event(session, evt)
        await asyncio.sleep(1.0)


async def _send_session_greeting(session):
    """One-shot proactive radio check at the very first session start.

    Sends a synthetic user-role turn that asks the model to greet the driver.
    The model's response is a normal audio turn, played back through
    play_audio just like any other reply. Idempotent via the
    _initial_greeting_sent flag — reconnects within the same service lifetime
    do not re-greet.
    """
    global _initial_greeting_sent
    if _initial_greeting_sent or not SESSION_OPENING_PROMPT:
        return
    try:
        await session.send_client_content(
            turns={"role": "user", "parts": [{"text": SESSION_OPENING_PROMPT}]},
            turn_complete=True,
        )
        _initial_greeting_sent = True
        print("[live] session greeting sent (radio check)", flush=True)
    except Exception as e:
        print(f"[live] greeting send failed: {e}", flush=True)


async def fetch_initial_brain() -> str:
    """GET /api/brain/snapshot once at session start (or reconnect). Truncates
    client-side at BRAIN_MAX_CHARS so an oversized brain can't bloat the
    system instruction."""
    try:
        md = await asyncio.to_thread(_http, "GET", "/api/brain/snapshot", None, 8)
    except Exception as e:
        print(f"[init] failed to fetch initial brain: {e}", flush=True)
        return ""
    if len(md) > BRAIN_MAX_CHARS:
        md = md[:BRAIN_MAX_CHARS] + "\n... [truncated]"
    return md


# ─── HTTP helpers (urllib stdlib — no extra deps) ───────────────────────


def _http(method, path, body=None, timeout=10):
    url = f"{RACE_API_URL}{path}"
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return resp.read().decode("utf-8", errors="replace")


def call_tool(name, args):
    """Dispatch a Gemini Live tool call to the Go API. Returns a dict that
    the SDK serialises back into the conversation. Errors come back as
    {'error': '...'} so the model can react instead of aborting."""
    args = args or {}
    try:
        # Topic-clustered fast read tools — each hits one /api/state/* endpoint
        # which returns a decoded headline-first JSON bundle. We forward the
        # raw JSON body so the model sees the structured fields verbatim.
        if name == "get_race_state":
            return {"result": _http("GET", "/api/state/race", timeout=5)}

        if name == "get_tires_and_damage":
            return {"result": _http("GET", "/api/state/tires", timeout=5)}

        if name == "get_energy_and_fuel":
            return {"result": _http("GET", "/api/state/energy", timeout=5)}

        if name == "get_competitor_landscape":
            return {"result": _http("GET", "/api/state/competitors", timeout=8)}

        if name == "get_pace_trend":
            past = args.get("past")
            if past is None:
                qs = ""
            else:
                # Pass through verbatim — the Go handler accepts ints,
                # "all", "0", or "none".
                qs = f"?past={past}"
            return {"result": _http("GET", f"/api/state/pace{qs}", timeout=8)}

        if name == "get_recent_events":
            try:
                limit = int(args.get("limit") or 10)
            except (TypeError, ValueError):
                limit = 10
            limit = max(1, min(limit, 100))
            return {"result": _http("GET", f"/api/state/events?limit={limit}", timeout=5)}

        if name == "query_brain":
            md = _http("GET", "/api/brain/snapshot", timeout=5)
            if len(md) > BRAIN_MAX_CHARS:
                md = md[:BRAIN_MAX_CHARS] + "\n\n... [truncated]"
            return {"result": md}

        if name == "ask_data_analyst":
            question = (args.get("question") or "").strip()
            if not question:
                return {"error": "ask_data_analyst requires a non-empty question"}
            context_topic = (args.get("context_topic") or "general").strip() or "general"
            urgent = bool(args.get("urgent", False))

            # Submit the async job. Server returns 202 with a job_id.
            submit = _http(
                "POST",
                "/api/analyst/query",
                body={
                    "question": question,
                    "context_topic": context_topic,
                    "urgent": urgent,
                },
                timeout=10,
            )
            try:
                sub_payload = json.loads(submit)
            except json.JSONDecodeError:
                return {"error": f"analyst submit returned non-JSON: {submit[:200]}"}
            if "error" in sub_payload:
                return {"error": sub_payload["error"]}
            job_id = sub_payload.get("job_id")
            if not job_id:
                return {"error": "analyst submit returned no job_id"}

            # Poll until the answer lands as an Observation under
            # analyst.<context_topic> in the brain snapshot, or the job
            # disappears from the pending list with no observation (failure).
            answer, elapsed_ms = _poll_analyst_answer(job_id, context_topic)
            if answer is None:
                return {"error": f"analyst job {job_id} timed out after {ANALYST_TIMEOUT}s"}
            return {
                "result": answer,
                "elapsed_ms": elapsed_ms,
                "job_id": job_id,
                "context_topic": context_topic,
            }

        if name == "get_proximity":
            return {"result": _http("GET", "/api/state/proximity", timeout=5)}

        if name == "get_track_position":
            return {"result": _http("GET", "/api/state/track_position", timeout=5)}

        if name == "set_corner_reminder":
            message = (args.get("message") or "").strip()
            if not message:
                return {"error": "message is required"}
            body = {"message": message}
            if args.get("corner_id"):
                body["corner_id"] = str(args["corner_id"]).strip()
            if args.get("lap_distance_m") is not None:
                try:
                    body["lap_distance_m"] = float(args["lap_distance_m"])
                except (TypeError, ValueError):
                    return {"error": "lap_distance_m must be a number"}
            if "corner_id" not in body and "lap_distance_m" not in body:
                return {"error": "either corner_id or lap_distance_m is required"}
            if args.get("lookahead_seconds") is not None:
                try:
                    body["lookahead_seconds"] = float(args["lookahead_seconds"])
                except (TypeError, ValueError):
                    pass
            if args.get("recurring") is not None:
                body["recurring"] = bool(args["recurring"])
            if args.get("expires_in_laps") is not None:
                try:
                    body["expires_in_laps"] = int(args["expires_in_laps"])
                except (TypeError, ValueError):
                    pass
            if args.get("priority") is not None:
                try:
                    body["priority"] = int(args["priority"])
                except (TypeError, ValueError):
                    pass
            return {"result": _http("POST", "/api/reminders/corner", body=body, timeout=5)}

        if name == "list_reminders":
            return {"result": _http("GET", "/api/reminders", timeout=5)}

        if name == "cancel_reminder":
            rid = (args.get("reminder_id") or "").strip()
            if not rid:
                return {"error": "reminder_id is required"}
            return {"result": _http("DELETE", f"/api/reminders/{rid}", timeout=5)}

        if name == "get_lap_traces":
            laps = args.get("laps") or ["best", "current"]
            if isinstance(laps, str):
                laps = [laps]
            laps_param = ",".join(str(x).strip() for x in laps if str(x).strip())
            channels = args.get("channels") or ["throttle", "brake", "speed"]
            if isinstance(channels, str):
                channels = [channels]
            ch_param = ",".join(str(x).strip() for x in channels if str(x).strip())
            try:
                buckets = int(args.get("buckets") or 80)
            except (TypeError, ValueError):
                buckets = 80
            qs = (
                f"?laps={urllib.parse.quote(laps_param)}"
                f"&channels={urllib.parse.quote(ch_param)}"
                f"&buckets={buckets}"
            )
            return {"result": _http("GET", f"/api/laps/traces{qs}", timeout=10)}

        if name == "compare_lap_to_best":
            params = []
            if args.get("lap") is not None:
                try:
                    params.append(f"lap={int(args['lap'])}")
                except (TypeError, ValueError):
                    return {"error": "lap must be an integer"}
            if args.get("corner"):
                params.append(f"corner={urllib.parse.quote(str(args['corner']).strip())}")
            if args.get("window_before_m") is not None:
                try:
                    params.append(f"window_before_m={float(args['window_before_m'])}")
                except (TypeError, ValueError):
                    pass
            if args.get("window_after_m") is not None:
                try:
                    params.append(f"window_after_m={float(args['window_after_m'])}")
                except (TypeError, ValueError):
                    pass
            qs = ("?" + "&".join(params)) if params else ""
            return {"result": _http("GET", f"/api/laps/compare{qs}", timeout=10)}

        # --- Brake-point coaching tools (Packages A/B/F) -------------------
        if name == "get_corner_brake_history":
            params = []
            if args.get("corner"):
                params.append(f"corner={urllib.parse.quote(str(args['corner']).strip())}")
            if args.get("lap_distance_m") is not None:
                try:
                    params.append(f"lap_distance_m={float(args['lap_distance_m'])}")
                except (TypeError, ValueError):
                    pass
            if args.get("recent_laps") is not None:
                try:
                    params.append(f"recent_laps={int(args['recent_laps'])}")
                except (TypeError, ValueError):
                    pass
            if not params:
                return {"error": "corner or lap_distance_m is required"}
            qs = "?" + "&".join(params)
            return {"result": _http("GET", f"/api/laps/brake_points{qs}", timeout=10)}

        if name == "get_corner_coaching_report":
            corner = (args.get("corner") or "").strip()
            if not corner:
                return {"error": "corner is required"}
            params = [f"corner={urllib.parse.quote(corner)}"]
            if args.get("recent_laps") is not None:
                try:
                    params.append(f"recent_laps={int(args['recent_laps'])}")
                except (TypeError, ValueError):
                    pass
            return {"result": _http("GET", f"/api/coaching/corner_report?{'&'.join(params)}", timeout=12)}

        if name == "get_brake_balance_report":
            params = []
            if args.get("recent_laps") is not None:
                try:
                    params.append(f"recent_laps={int(args['recent_laps'])}")
                except (TypeError, ValueError):
                    pass
            if args.get("corner"):
                params.append(f"corner={urllib.parse.quote(str(args['corner']).strip())}")
            qs = ("?" + "&".join(params)) if params else ""
            return {"result": _http("GET", f"/api/laps/brake_balance{qs}", timeout=10)}

        if name == "push_strategy_insight":
            summary = (args.get("summary") or "").strip()
            recommendation = (args.get("recommendation") or "").strip()
            priority = int(args.get("priority") or 3)
            if not summary or not recommendation:
                return {"error": "summary and recommendation are required"}
            _http(
                "POST",
                "/api/strategy",
                body={
                    "summary": summary,
                    "recommendation": recommendation,
                    "criticality": priority,
                },
                timeout=5,
            )
            return {"result": "insight accepted by strategy pipeline"}

        if name == "get_session_history":
            scope = (args.get("scope") or "current").strip() or "current"
            limit = int(args.get("limit") or 50)
            if limit < 1:
                limit = 1
            if limit > 500:
                limit = 500
            kinds = (args.get("kinds") or "").strip()
            since_minutes = args.get("since_minutes")
            qs = f"scope={urllib.parse.quote(scope)}&limit={limit}"
            if kinds:
                qs += f"&kinds={urllib.parse.quote(kinds)}"
            if since_minutes is not None:
                try:
                    sm = int(since_minutes)
                    if sm > 0:
                        qs += f"&since_minutes={sm}"
                except (TypeError, ValueError):
                    pass
            raw = _http("GET", f"/api/transcript?{qs}", timeout=5)
            try:
                payload = json.loads(raw) if raw else {}
            except Exception:
                payload = {"events": []}
            events = payload.get("events") or []
            lines = []
            for e in events:
                at = e.get("at", "")
                short_at = at[11:19] if isinstance(at, str) and len(at) >= 19 else at
                kind = e.get("kind", "?")
                actor = e.get("actor", "?")
                text = (e.get("text") or "").replace("\n", " ").strip()
                lines.append(f"- {short_at} · {kind} · {actor} · {text}")
            return {
                "result": {
                    "session_id": payload.get("session_id", ""),
                    "scope": scope,
                    "count": len(events),
                    "markdown": "\n".join(lines) if lines else "(no transcript yet)",
                }
            }

        if name == "get_session_plan":
            raw = _http("GET", "/api/plan/current", timeout=5)
            try:
                payload = json.loads(raw) if raw else {}
            except Exception:
                payload = {}
            entries = payload.get("entries") or []
            active = [
                e for e in entries
                if (e.get("status") or "pending") not in ("done", "cancelled")
            ]
            # Compact markdown so the LLM doesn't have to parse the
            # raw JSON every call. Same shape as RenderMarkdown on the Go
            # side. Include all entries (history matters) but flag status.
            lines = []
            for e in entries:
                trig = e.get("trigger") or {}
                ttype = trig.get("type", "?")
                tag = ttype
                if ttype == "lap":
                    tag = f"L{trig.get('lap', '?')}"
                elif ttype == "lap_window":
                    tag = f"L{trig.get('from_lap', '?')}-{trig.get('to_lap', '?')}"
                elif ttype == "track_position":
                    if trig.get("distance_m"):
                        tag = f"L{trig.get('lap', '?')}@{int(trig.get('distance_m'))}m"
                    else:
                        tag = f"L{trig.get('lap', '?')}@pit-entry"
                elif ttype == "time_left_s":
                    tag = f"T-{trig.get('seconds', '?')}s"
                kind = e.get("kind", "?")
                status = e.get("status") or "pending"
                payload_field = e.get("payload") or {}
                hl = payload_field.get("headline") or e.get("notes") or kind
                lines.append(f"- {tag} | {kind} | {status} — {hl} (id={e.get('id')})")
            return {
                "result": {
                    "session_uid": payload.get("session_uid", 0),
                    "active_count": len(active),
                    "total_count": len(entries),
                    "markdown": "\n".join(lines) if lines else "(no plan entries yet)",
                    "entries": entries,
                }
            }

        if name == "add_plan_entry":
            kind = (args.get("kind") or "").strip()
            trigger_type = (args.get("trigger_type") or "").strip()
            if not kind or not trigger_type:
                return {"error": "kind and trigger_type are required"}
            trigger = {"type": trigger_type}
            for key in ("lap", "from_lap", "to_lap", "seconds"):
                v = args.get(key)
                if v is not None:
                    try:
                        trigger[key] = int(v)
                    except (TypeError, ValueError):
                        return {"error": f"{key} must be an integer"}
            if args.get("distance_m") is not None:
                try:
                    trigger["distance_m"] = float(args.get("distance_m"))
                except (TypeError, ValueError):
                    return {"error": "distance_m must be a number"}
            payload = {}
            if args.get("headline"):
                payload["headline"] = str(args.get("headline"))
            body = {
                "kind": kind,
                "trigger": trigger,
                "notes": (args.get("notes") or "").strip(),
                "created_by": "gemini_live",
            }
            if payload:
                body["payload"] = payload
            raw = _http("POST", "/api/plan/entries", body=body, timeout=5)
            try:
                stored = json.loads(raw) if raw else {}
            except Exception:
                stored = {"raw": raw}
            return {"result": stored}

        if name == "update_plan_entry":
            entry_id = (args.get("id") or "").strip()
            if not entry_id:
                return {"error": "id is required"}
            body = {}
            if args.get("status") is not None:
                body["status"] = str(args.get("status"))
            if args.get("notes") is not None:
                body["notes"] = str(args.get("notes"))
            trig_patch = {}
            for key in ("lap", "from_lap", "to_lap", "seconds"):
                v = args.get(key)
                if v is not None:
                    try:
                        trig_patch[key] = int(v)
                    except (TypeError, ValueError):
                        return {"error": f"{key} must be an integer"}
            if args.get("distance_m") is not None:
                try:
                    trig_patch["distance_m"] = float(args.get("distance_m"))
                except (TypeError, ValueError):
                    return {"error": "distance_m must be a number"}
            if trig_patch:
                # Patching trigger fields means we replace the whole
                # trigger to keep validation simple. The caller must
                # include the trigger type if changing it; otherwise
                # we leave the trigger alone and only patch other fields.
                body["trigger"] = trig_patch
                if args.get("trigger_type"):
                    body["trigger"]["type"] = str(args.get("trigger_type"))
            raw = _http("PATCH", f"/api/plan/entries/{entry_id}", body=body, timeout=5)
            try:
                stored = json.loads(raw) if raw else {}
            except Exception:
                stored = {"raw": raw}
            return {"result": stored}

        if name == "cancel_plan_entry":
            entry_id = (args.get("id") or "").strip()
            if not entry_id:
                return {"error": "id is required"}
            reason = (args.get("reason") or "").strip()
            qs = f"?reason={urllib.parse.quote(reason)}" if reason else ""
            raw = _http("DELETE", f"/api/plan/entries/{entry_id}{qs}", timeout=5)
            try:
                stored = json.loads(raw) if raw else {}
            except Exception:
                stored = {"raw": raw}
            return {"result": stored}

        return {"error": f"unknown tool {name}"}

    except urllib.error.URLError as e:
        return {"error": f"upstream Go API unreachable: {e}"}
    except Exception as e:
        return {"error": f"tool {name} failed: {e}"}


def _poll_analyst_answer(job_id: str, context_topic: str):
    """Poll /api/analyst/jobs and /api/brain/snapshot?format=json until the
    answer for job_id lands. Returns (answer_text, elapsed_ms) or (None, 0)
    on timeout. The answer is the latest observation under topic
    analyst.<context_topic> whose job_id matches.
    """
    deadline = time.monotonic() + ANALYST_TIMEOUT
    target_topic = f"analyst.{context_topic}"
    poll_interval = 0.5
    t0 = time.monotonic()

    while time.monotonic() < deadline:
        # 1. Check the brain for a finished observation with this job_id.
        try:
            raw = _http("GET", "/api/brain/snapshot?format=json", timeout=5)
            snap = json.loads(raw)
            obs_map = snap.get("observations") or {}
            items = obs_map.get(target_topic) or []
            for obs in items:
                if obs.get("job_id") == job_id:
                    elapsed_ms = int((time.monotonic() - t0) * 1000)
                    return obs.get("summary") or "(empty answer)", elapsed_ms
        except Exception:
            pass

        # 2. If the job is no longer in the pending list, it's either done
        # (handled above) or vanished — give the brain one more poll cycle
        # then bail with what we know.
        try:
            raw_jobs = _http("GET", "/api/analyst/jobs", timeout=3)
            jobs = json.loads(raw_jobs) or []
            still_pending = any(j.get("job_id") == job_id for j in jobs)
            if not still_pending:
                # Last-chance look at the brain snapshot.
                try:
                    raw = _http("GET", "/api/brain/snapshot?format=json", timeout=5)
                    snap = json.loads(raw)
                    obs_map = snap.get("observations") or {}
                    items = obs_map.get(target_topic) or []
                    for obs in items:
                        if obs.get("job_id") == job_id:
                            elapsed_ms = int((time.monotonic() - t0) * 1000)
                            return obs.get("summary") or "(empty answer)", elapsed_ms
                except Exception:
                    pass
                # Nothing landed, but the job is gone — treat as timeout.
                return None, 0
        except Exception:
            pass

        time.sleep(poll_interval)

    return None, 0


# ─── audio I/O (mirrors the POC structure) ───────────────────────────────

pya = pyaudio.PyAudio()
audio_queue_output: asyncio.Queue = asyncio.Queue()
audio_queue_mic: asyncio.Queue = asyncio.Queue(maxsize=5)
# Dedicated queue for PTT feedback tones. Drained by play_tones() onto a
# separate pyaudio stream so tones play SIMULTANEOUSLY with Gemini's voice
# instead of being serialized behind the engineer's current sentence.
audio_queue_tones: asyncio.Queue = asyncio.Queue(maxsize=8)
audio_stream = None

# Diagnostic counters reset every stats tick. Help us see whether the mic
# is feeding Gemini, whether Gemini is sending audio back, and whether
# audio is reaching the speaker — three places this pipeline can silently
# stall. Surfaced by stats_logger() once every 5 seconds.
_diag = {
    "mic_chunks_sent": 0,        # mic frames pushed into Gemini per interval
    "mic_chunks_dropped_muted": 0, # would have been sent but PTT was muted
    "model_turns_started": 0,    # number of new model turns observed
    "model_audio_chunks": 0,     # audio chunks received from the model
    "model_text_chunks": 0,      # text chunks (should be 0 — AUDIO modality)
    "audio_played": 0,           # chunks pulled off audio_queue_output and written
    "tones_played": 0,           # PTT feedback beeps written to the tone stream
    "interrupts": 0,             # times the model was interrupted
    "tool_calls": 0,             # tool_call events received
    "go_aways": 0,               # server preemptive-close signals
}


def list_input_devices():
    inputs = []
    for i in range(pya.get_device_count()):
        info = pya.get_device_info_by_index(i)
        if int(info.get("maxInputChannels", 0)) > 0:
            inputs.append((i, info))
    return inputs


def pick_input_device():
    """Pick a mic without prompting. Honors MIC_INDEX, else uses the system
    default. Prompts only when MIC_PROMPT=1 is set (useful when running the
    service standalone for setup)."""
    inputs = list_input_devices()
    if not inputs:
        raise RuntimeError("no input devices found")
    try:
        default_idx = int(pya.get_default_input_device_info()["index"])
    except OSError:
        default_idx = inputs[0][0]

    override = os.environ.get("MIC_INDEX", "").strip()
    if override != "":
        idx = int(override)
        return idx, pya.get_device_info_by_index(idx)

    print("\nAvailable input devices:")
    for idx, info in inputs:
        marker = "  (default)" if idx == default_idx else ""
        print(
            f"  [{idx}] {info['name']}  "
            f"(channels={int(info['maxInputChannels'])}, "
            f"rate={int(info['defaultSampleRate'])} Hz){marker}"
        )

    if os.environ.get("MIC_PROMPT", "").strip() not in ("1", "true", "yes"):
        print(f"\nAuto-selected system default mic [{default_idx}]. "
              f"Override with MIC_INDEX=<n> or set MIC_PROMPT=1 to choose interactively.")
        return default_idx, pya.get_device_info_by_index(default_idx)

    valid = {idx for idx, _ in inputs}
    while True:
        raw = input(f"\nSelect mic index [{default_idx}]: ").strip()
        chosen = default_idx if raw == "" else int(raw)
        if chosen in valid:
            return chosen, pya.get_device_info_by_index(chosen)
        print(f"  {chosen} is not in the list, try again.")


def _resample(data: bytes, in_rate: int, out_rate: int, state):
    if in_rate == out_rate:
        return data, state
    if audioop is not None:
        return audioop.ratecv(data, 2, CHANNELS, in_rate, out_rate, state)
    # Pure-python fallback for Python 3.13+.
    import array as _array

    src = _array.array("h")
    src.frombytes(data)
    if not src:
        return data, state
    ratio = in_rate / out_rate
    out_len = int(len(src) / ratio)
    out = _array.array("h", [0]) * out_len
    for i in range(out_len):
        pos = i * ratio
        a = int(pos)
        b = a + 1 if a + 1 < len(src) else a
        f = pos - a
        out[i] = int(src[a] * (1 - f) + src[b] * f)
    return out.tobytes(), state


# PTT gate: starts muted. Press PTT_KEY to unmute, release to mute again.
# When PTT_KEY is empty the gate stays open (always-hot behavior).
_ptt_active = False


def _ptt_resolve_key():
    """Translate PTT_KEY into the (matcher, label) the listener uses."""
    if not PTT_KEY:
        return None, ""
    raw = PTT_KEY.strip()
    if len(raw) == 1:
        return ("char", raw), repr(raw)
    # Named key (space, ctrl_r, alt_l, etc.) — resolve at listener-start time
    # so we fail loudly if pynput doesn't know it.
    return ("name", raw.lower()), raw


async def _ptt_ws_listener():
    """Subscribe to the Go core's WebSocket and follow controller PTT events.

    Drives the same `_ptt_active` flag the keyboard listener uses, so
    listen_audio doesn't care which source produced the gate. Reconnects
    forever with bounded backoff — if the Go core restarts, we pick the
    state back up automatically.
    """
    global _ptt_active
    try:
        import websockets  # type: ignore
    except ImportError:
        _ptt_active = True
        print(
            "[ptt] websockets not installed — falling back to always-hot mic. "
            "pip install websockets to use controller-mode PTT.",
            flush=True,
        )
        return

    backoff = 1.0
    while True:
        try:
            async with websockets.connect(PTT_WS_URL, ping_interval=20) as ws:
                print(f"[ptt] controller-mode connected to {PTT_WS_URL} — mic starts MUTED.", flush=True)
                _ptt_active = False
                backoff = 1.0
                async for raw in ws:
                    try:
                        msg = json.loads(raw)
                    except Exception:
                        continue
                    if msg.get("type") != "ptt":
                        continue
                    active = bool((msg.get("data") or {}).get("active", False))
                    if active != _ptt_active:
                        _ptt_active = active
                        print(
                            f"[ptt] controller {'OPEN' if active else 'MUTED'} (from Go BUTN)",
                            flush=True,
                        )
                        play_ptt_feedback(active)
                        # On PTT release, drain any P1–P4 events that were
                        # deferred while the mic was hot and append them to
                        # the open user turn BEFORE we enqueue activity_end.
                        # With manual VAD, the activity_end is what triggers
                        # generation, so the model sees the audio question
                        # plus the combined event prompt as one turn and
                        # responds to both (events first, then question).
                        #
                        # Skip the flush if a P5 is in mid-announcement —
                        # the user-turn is already closed by the P5
                        # dispatcher, and leasing more events here would
                        # register them in _in_flight only for the P5's
                        # turn_complete to mistakenly ack them as
                        # delivered. Leaving them queued lets the dispatch
                        # lanes pick them up cleanly after the P5 ends.
                        if not active and not _p5_force_mute_active:
                            try:
                                await flush_deferred_events_on_ptt_release(
                                    _current_session
                                )
                            except Exception as e:
                                print(
                                    f"[ptt] flush failed: {type(e).__name__}: {e}",
                                    flush=True,
                                )
                        # Enqueue the activity boundary AFTER any audio
                        # captured up to this moment AND after the deferred-
                        # event flush so Gemini sees the user's words first,
                        # then the appended events, then the turn-complete
                        # signal. send_realtime drains in FIFO order.
                        #
                        # Skip the boundary entirely while a P5 force-mute is
                        # active — the P5 dispatcher already closed the user
                        # turn, and the receive loop will re-open it on
                        # turn_complete if PTT is still held. Forwarding a
                        # release here would double-close; forwarding a
                        # re-press would feed the driver's voice into the
                        # middle of the critical announcement.
                        if not _p5_force_mute_active:
                            try:
                                await audio_queue_mic.put(
                                    PTT_BOUNDARY_START if active else PTT_BOUNDARY_END
                                )
                            except Exception as e:
                                print(f"[ptt] boundary enqueue failed: {e}", flush=True)
        except asyncio.CancelledError:
            raise
        except Exception as e:
            print(
                f"[ptt] controller WS error: {type(e).__name__}: {e} — retrying in {backoff:.1f}s",
                flush=True,
            )
            await asyncio.sleep(backoff)
            backoff = min(backoff * 2, 15.0)


def _start_ptt_listener():
    """Spawn a pynput keyboard listener that toggles _ptt_active.

    Returns True if listening, False if PTT is disabled or pynput is missing.
    On macOS the python interpreter needs Accessibility permission — the user
    is prompted by the OS the first time the listener starts.
    """
    global _ptt_active

    if PTT_SOURCE == "off":
        _ptt_active = True
        print("[ptt] disabled (GEMINI_LIVE_PTT_SOURCE=off), mic always hot")
        return False
    if PTT_SOURCE == "controller":
        # The async WS listener is started later from run() once the event
        # loop exists. listen_audio honors _ptt_active either way.
        _ptt_active = False
        print(
            f"[ptt] controller-mode (PTT_SOURCE=controller) — gate driven by "
            f"Go BUTN events on {PTT_WS_URL}. mic starts MUTED.",
            flush=True,
        )
        return False
    matcher, label = _ptt_resolve_key()
    if matcher is None:
        _ptt_active = True  # always-hot when PTT disabled
        print("[ptt] disabled (GEMINI_LIVE_PTT_KEY=''), mic always hot")
        return False

    try:
        from pynput import keyboard
    except ImportError:
        _ptt_active = True  # don't silently mute the user
        print(
            "[ptt] pynput not installed — install with `pip install pynput` "
            "to use push-to-talk. Falling back to always-hot mic."
        )
        return False

    kind, val = matcher
    target_char = val if kind == "char" else None
    target_key = None
    if kind == "name":
        target_key = getattr(keyboard.Key, val, None)
        if target_key is None:
            _ptt_active = True
            print(f"[ptt] unknown pynput key {val!r} — falling back to always-hot mic")
            return False

    def _matches(key):
        if target_char is not None:
            return getattr(key, "char", None) == target_char
        return key == target_key

    def on_press(key):
        global _ptt_active
        if _matches(key) and not _ptt_active:
            _ptt_active = True
            print(f"[ptt] {label} down — mic OPEN", flush=True)
            play_ptt_feedback(True)

    def on_release(key):
        global _ptt_active
        if _matches(key) and _ptt_active:
            _ptt_active = False
            print(f"[ptt] {label} up — mic MUTED", flush=True)
            play_ptt_feedback(False)

    listener = keyboard.Listener(on_press=on_press, on_release=on_release)
    listener.daemon = True
    listener.start()
    print(f"[ptt] hold {label} to talk (release to mute). mic starts MUTED.")
    return True


async def listen_audio(idx, info):
    """Capture mic at native rate, resample to 16 kHz, queue for the Live session."""
    global audio_stream
    native_rate = int(info["defaultSampleRate"])

    audio_stream = await asyncio.to_thread(
        pya.open,
        format=FORMAT,
        channels=CHANNELS,
        rate=native_rate,
        input=True,
        input_device_index=idx,
        frames_per_buffer=CHUNK_SIZE,
    )
    print(f"[mic] '{info['name']}' @ {native_rate}Hz native → {SEND_SAMPLE_RATE}Hz to Gemini")

    kwargs = {"exception_on_overflow": False}
    state = None
    while True:
        data = await asyncio.to_thread(audio_stream.read, CHUNK_SIZE, **kwargs)
        # Drop frames while PTT is released — keeps the model from hearing
        # ambient noise / keyboard clatter when the driver isn't talking.
        # Also drop while a P5 announcement is force-muting the mic so the
        # driver's voice doesn't bleed into the engineer's critical call.
        if not _ptt_active or _p5_force_mute_active:
            state = None  # reset resampler state so the next utterance is clean
            _diag["mic_chunks_dropped_muted"] += 1
            continue
        send_data, state = _resample(data, native_rate, SEND_SAMPLE_RATE, state)
        await audio_queue_mic.put({"data": send_data, "mime_type": "audio/pcm"})


# Sentinels for PTT activity boundaries flowing through audio_queue_mic.
# Threading them through the same queue as audio chunks guarantees ordering:
# any audio captured before the PTT-off transition arrives at Gemini before
# the activity_end signal, so the model has all the user's words when it
# generates the response.
PTT_BOUNDARY_START = "__ptt_activity_start__"
PTT_BOUNDARY_END = "__ptt_activity_end__"


async def send_realtime(session):
    while True:
        msg = await audio_queue_mic.get()
        if msg == PTT_BOUNDARY_START:
            try:
                await session.send_realtime_input(activity_start=types.ActivityStart())
                print("[live] activity_start (PTT open)", flush=True)
            except Exception as e:
                print(f"[live] activity_start failed: {type(e).__name__}: {e}", flush=True)
            continue
        if msg == PTT_BOUNDARY_END:
            try:
                await session.send_realtime_input(activity_end=types.ActivityEnd())
                print("[live] activity_end (PTT close — waiting for response)", flush=True)
            except Exception as e:
                print(f"[live] activity_end failed: {type(e).__name__}: {e}", flush=True)
            continue
        await session.send_realtime_input(audio=msg)
        _diag["mic_chunks_sent"] += 1
        # Stamp last-mic-activity so receive_audio's interrupted branch can
        # distinguish a real barge-in (audio shipped within the recency
        # window) from a spurious server-side interrupt.
        global _last_mic_chunk_at
        _last_mic_chunk_at = time.monotonic()


async def play_audio():
    stream = await asyncio.to_thread(
        pya.open, format=FORMAT, channels=CHANNELS, rate=RECEIVE_SAMPLE_RATE, output=True
    )
    while True:
        chunk = await audio_queue_output.get()
        await asyncio.to_thread(stream.write, chunk)
        _diag["audio_played"] += 1


async def stats_logger():
    """Print a one-line health snapshot every 5s while a Live session is
    open. The lines are easy to grep ([diag]) and tell us which side of the
    pipeline is stalled when the engineer goes quiet:
      - mic_sent=0 with PTT open  → audio isn't reaching Gemini
      - turns=0 model_audio=0     → Gemini isn't producing anything
      - model_audio>0 played=0    → playback task is stuck

    Gated behind LOG_LEVEL=debug so normal-mode terminals stay clean. The
    task must NOT return early — the session supervisor uses FIRST_COMPLETED
    on this task list, so a clean exit triggers a full session teardown.
    """
    if not DIAG_VERBOSE:
        # Park forever; cancellation by the supervisor's finally block ends it.
        await asyncio.Event().wait()
        return
    while True:
        await asyncio.sleep(5)
        ptt = "OPEN " if _ptt_active else "MUTED"
        qout = audio_queue_output.qsize()
        qmic = audio_queue_mic.qsize()
        qtone = audio_queue_tones.qsize()
        snapshot = (
            f"[diag] ptt={ptt} "
            f"mic_sent={_diag['mic_chunks_sent']} "
            f"mic_muted_drops={_diag['mic_chunks_dropped_muted']} "
            f"turns={_diag['model_turns_started']} "
            f"model_audio={_diag['model_audio_chunks']} "
            f"played={_diag['audio_played']} "
            f"tones={_diag['tones_played']} "
            f"interrupts={_diag['interrupts']} "
            f"tool_calls={_diag['tool_calls']} "
            f"go_aways={_diag['go_aways']} "
            f"queues=[mic:{qmic} out:{qout} tone:{qtone}]"
        )
        print(snapshot, flush=True)
        # Reset per-interval counters; cumulative ones (interrupts, go_aways)
        # we keep monotonically increasing.
        for k in ("mic_chunks_sent", "mic_chunks_dropped_muted",
                  "model_turns_started", "model_audio_chunks",
                  "model_text_chunks", "audio_played", "tones_played"):
            _diag[k] = 0


async def play_tones():
    """Drain audio_queue_tones onto a dedicated output stream so PTT
    feedback beeps play SIMULTANEOUSLY with Gemini's voice. macOS CoreAudio
    (and PulseAudio/PipeWire on Linux) mix concurrent streams to the same
    output device, so opening a second stream gives us free parallel
    playback without touching the main audio queue.
    """
    stream = await asyncio.to_thread(
        pya.open, format=FORMAT, channels=CHANNELS, rate=RECEIVE_SAMPLE_RATE, output=True
    )
    while True:
        chunk = await audio_queue_tones.get()
        await asyncio.to_thread(stream.write, chunk)
        _diag["tones_played"] += 1


# ─── tool dispatch ───────────────────────────────────────────────────────


def _c(code, s):
    return f"\033[{code}m{s}\033[0m"


async def handle_tool_call(session, tool_call):
    function_responses = []
    for fc in tool_call.function_calls:
        args = dict(fc.args) if fc.args else {}
        ts = datetime.now().strftime("%H:%M:%S")
        print()
        print(_c("1;36", f"[{ts}]  ╭─► {fc.name}({args})"), flush=True)

        # Log tool_call BEFORE invoking so a tool that crashes still leaves
        # a trace. Fire-and-forget — don't block the model on transcript I/O.
        asyncio.create_task(
            _post_transcript(
                kind="tool_call",
                actor="live_agent",
                text=fc.name,
                meta={"args": args},
            )
        )

        t0 = time.monotonic()
        result = await asyncio.to_thread(call_tool, fc.name, args)
        elapsed = time.monotonic() - t0

        ts2 = datetime.now().strftime("%H:%M:%S")
        preview = json.dumps(result)
        if len(preview) > 220:
            preview = preview[:220] + "..."
        print(
            _c("1;36", f"[{ts2}]  ╰─◄ {fc.name} ({elapsed:.1f}s)  {preview}"),
            flush=True,
        )

        # tool_result with latency + truncated payload. Truncation keeps
        # the transcript file from ballooning when a tool returns a large
        # JSON blob (analyst answers, query_brain markdown, etc.).
        result_text = preview
        asyncio.create_task(
            _post_transcript(
                kind="tool_result",
                actor="live_agent",
                text=result_text,
                meta={
                    "tool": fc.name,
                    "latency_ms": int(elapsed * 1000),
                    "ok": "error" not in (result or {}),
                },
            )
        )

        function_responses.append(
            types.FunctionResponse(id=fc.id, name=fc.name, response=result)
        )
    await session.send_tool_response(function_responses=function_responses)


async def _post_transcript(kind, actor, text, meta=None):
    """POST a single transcript event to /api/transcript. Fire-and-forget;
    failures are logged but never surface to the caller (the transcript
    is for memory, not control flow)."""
    try:
        await asyncio.to_thread(
            _http,
            "POST",
            "/api/transcript",
            {
                "kind": kind,
                "actor": actor,
                "text": text,
                "meta": meta or {},
            },
            5,
        )
    except Exception as e:
        print(f"[transcript] {kind} post failed: {e}", flush=True)


async def _restore_p5_mic_if_needed(label):
    """If a P5 force-mute is active, lift it now that the engineer's turn
    has ended (cleanly OR via interrupt). Re-opens Gemini's user activity
    only if PTT is still physically held; otherwise the mic stays closed
    until the driver re-presses. Shared between the turn_complete and
    interrupted branches so a P5 that gets interrupted cannot strand the
    mic in muted state.
    """
    global _p5_force_mute_active
    if not _p5_force_mute_active:
        return
    _p5_force_mute_active = False
    if _ptt_active:
        try:
            await audio_queue_mic.put(PTT_BOUNDARY_START)
        except Exception as e:
            print(f"[live] P5 mic-restore enqueue failed: {e}", flush=True)
        print(
            f"[live] P5 done ({label}) — mic re-opened (PTT still held)",
            flush=True,
        )
    else:
        print(
            f"[live] P5 done ({label}) — mic stays closed (PTT released during call)",
            flush=True,
        )


def _is_real_barge_in() -> bool:
    """True if the most recent `interrupted` flag is plausibly the driver
    actually speaking. We look at two signals:
      - `_ptt_active`: PTT is held right now → driver intends to speak.
      - `_last_mic_chunk_at`: send_realtime stamped a mic chunk within
        MIC_RECENCY_WINDOW_SEC → audio was flowing to Gemini just now.
    Either is sufficient. Without manual VAD this would be noisy; with
    manual VAD (our config), audio only flows while PTT is held, so a
    True from this function is a strong barge-in signal.
    """
    if _ptt_active:
        return True
    if _last_mic_chunk_at <= 0:
        return False
    return (time.monotonic() - _last_mic_chunk_at) < MIC_RECENCY_WINDOW_SEC


async def receive_audio(session):
    global LATEST_HANDLE, _turn_audio_chunks_streamed
    while True:
        try:
            turn = session.receive()
            async for response in turn:
                if response.tool_call:
                    _diag["tool_calls"] += 1
                    await handle_tool_call(session, response.tool_call)
                    continue
                if response.server_content and response.server_content.model_turn:
                    # Mark "speaking" so idle-lane dispatcher backs off until
                    # the model finishes this turn.
                    was_speaking = _IS_SPEAKING is not None and _IS_SPEAKING.is_set()
                    if _IS_SPEAKING is not None:
                        _IS_SPEAKING.set()
                    audio_chunks = 0
                    text_chunks = 0
                    for part in response.server_content.model_turn.parts:
                        if part.inline_data and isinstance(part.inline_data.data, bytes):
                            audio_queue_output.put_nowait(part.inline_data.data)
                            audio_chunks += 1
                        elif getattr(part, "text", None):
                            text_chunks += 1
                    _diag["model_audio_chunks"] += audio_chunks
                    _diag["model_text_chunks"] += text_chunks
                    # Reset the per-turn audio counter when a fresh turn
                    # opens; the interrupted branch reads it to tell
                    # "model said nothing" (drop) from "model was talking
                    # and got cut" (ack as partial).
                    if not was_speaking:
                        _turn_audio_chunks_streamed = 0
                    _turn_audio_chunks_streamed += audio_chunks
                    if not was_speaking and (audio_chunks or text_chunks):
                        _diag["model_turns_started"] += 1
                        # First-turn diagnostic. Helps identify whether the
                        # model is even responding to dispatched events.
                        print(
                            f"[live] model_turn audio={audio_chunks} text={text_chunks}",
                            flush=True,
                        )
                # Turn-completion / generation-completion clears the speaking
                # flag so the idle-lane dispatcher can drain the next event.
                if response.server_content and (
                    getattr(response.server_content, "turn_complete", False)
                    or getattr(response.server_content, "generation_complete", False)
                ):
                    if _IS_SPEAKING is not None and _IS_SPEAKING.is_set():
                        # Log every turn end so we can see when the engineer
                        # naturally stops vs. when audio just dries up silently.
                        kind = "turn_complete" if getattr(response.server_content, "turn_complete", False) else "generation_complete"
                        print(f"[live] {kind}", flush=True)
                    if _IS_SPEAKING is not None:
                        _IS_SPEAKING.clear()
                    # Close any open lease as delivered. Entries already
                    # nacked earlier in this turn (status=interrupted_pending_retry)
                    # are skipped by _ack_in_flight_delivered, so nothing
                    # double-acks here.
                    await _ack_in_flight_delivered()
                    await _restore_p5_mic_if_needed("turn_complete")
                    _turn_audio_chunks_streamed = 0
                # Server-side `interrupted` flag. Three possible meanings:
                #   1. Real driver barge-in (PTT held + recent mic audio).
                #   2. Model-self-revision mid-stream (model started a
                #      response, decided to redo it). Audio was already
                #      streaming when it arrived.
                #   3. Spurious server interrupt — manual VAD config but
                #      server still fires interrupted, often with no audio
                #      streamed yet. This used to drive a retry-loop where
                #      the same event gets re-spoken until MaxAttempts.
                # Routing:
                #   1 → nack (re-queue, retry semantics intact)
                #   2 → ack as partial delivered (driver heard a fragment)
                #   3 → drop without retry (the rule will refire if still
                #       relevant)
                if response.server_content and getattr(response.server_content, "interrupted", False):
                    if _IS_SPEAKING is not None:
                        _IS_SPEAKING.clear()
                    _diag["interrupts"] += 1
                    drained = 0
                    while not audio_queue_output.empty():
                        audio_queue_output.get_nowait()
                        drained += 1

                    real_bargein = _is_real_barge_in()
                    audio_streamed = _turn_audio_chunks_streamed
                    if real_bargein:
                        if drained:
                            print(
                                f"[live] interrupted — driver barge-in, dropped {drained} pending chunks",
                                flush=True,
                            )
                        else:
                            print(
                                "[live] interrupted — driver barge-in (no audio queued)",
                                flush=True,
                            )
                        # Nack any in-flight events so the brain re-queues
                        # for retry once the driver is done speaking.
                        await _nack_in_flight_interrupted("user_barge_in")
                    elif audio_streamed > 0:
                        print(
                            f"[live] interrupted — false (no recent mic, but {audio_streamed} model chunks already streamed); ack as partial",
                            flush=True,
                        )
                        # Driver heard a partial radio call — record it as
                        # delivered so speech dedup catches the next refire.
                        await _ack_in_flight_partial("spurious_interrupt_partial")
                    else:
                        print(
                            "[live] interrupted — spurious (no mic, no audio); dropping without retry",
                            flush=True,
                        )
                        # No audio streamed AND no driver activity → this
                        # is the loop the user reported. Drop; let the
                        # underlying rule refire if still relevant.
                        await _drop_in_flight_spurious("spurious_interrupt_no_audio")

                    # P5 mic-restore must run regardless of which branch
                    # above fired — otherwise an interrupted P5 strands
                    # the driver's mic muted.
                    await _restore_p5_mic_if_needed("interrupted")
                    _turn_audio_chunks_streamed = 0
                ga = getattr(response, "go_away", None)
                if ga is not None:
                    tl_raw = getattr(ga, "time_left", None)
                    tl_secs = _seconds(tl_raw)
                    _diag["go_aways"] += 1
                    print(f"[live] go_away received (time_left={tl_raw}) — server is closing the session", flush=True)
                    # Pre-emptive reconnect window: ≤60s left → tell the
                    # supervisor to spin up a fresh session before this one
                    # gets killed mid-response.
                    if tl_secs is not None and tl_secs <= 60 and RECONNECT_EVENT is not None:
                        RECONNECT_EVENT.set()
                upd = getattr(response, "session_resumption_update", None)
                if upd is not None:
                    handle = getattr(upd, "new_handle", None) or getattr(upd, "handle", None)
                    if handle:
                        LATEST_HANDLE = handle
                        # Throttle: log only when handle changes AND at most
                        # once per minute. Eliminates the 1-line-per-2s flood.
                        now = time.monotonic()
                        if handle != _handle_log_state["last"] and (now - _handle_log_state["at"]) > 60:
                            resumable = getattr(upd, "resumable", None)
                            print(f"[live] session_resumption_update handle={handle!r} resumable={resumable}", flush=True)
                            _handle_log_state["last"] = handle
                            _handle_log_state["at"] = now
        except Exception as e:
            print(f"[live] receive loop error: {type(e).__name__}: {e}", flush=True)
            raise


# ─── entrypoint ──────────────────────────────────────────────────────────


async def _run_session(client, mic_idx, mic_info):
    """Open one Gemini Live session and run the audio + receive loops.

    Returns "preemptive" when the supervisor signalled a pre-emptive reconnect
    via RECONNECT_EVENT (graceful close), or "ended" on natural session
    completion. Raises on connection errors so the supervisor can back off.
    """
    brain_md = await fetch_initial_brain()
    cfg = build_config(brain_md, LATEST_HANDLE)
    handle_label = LATEST_HANDLE[:8] if LATEST_HANDLE else "fresh"
    print(
        f"[live] connecting (handle={handle_label}, brain={len(brain_md)} chars)",
        flush=True,
    )

    # Drop any in-flight bookkeeping from the previous session — those
    # leases died with the old session and will be reaped server-side
    # after LeaseTimeout (~8s). Clearing locally lets the dispatcher
    # lanes start fresh immediately on reconnect.
    if _in_flight_lock is not None:
        async with _in_flight_lock:
            if _in_flight:
                print(
                    f"[live] reconnect: dropping {len(_in_flight)} stale in-flight entries",
                    flush=True,
                )
                _in_flight.clear()

    global _current_session
    async with client.aio.live.connect(model=MODEL, config=cfg) as session:
        _current_session = session
        print("Connected to Gemini Live. Speak when ready.", flush=True)

        # Start receive loop FIRST so it's ready to capture the greeting's
        # model_turn before we send it.
        receive_task = asyncio.create_task(receive_audio(session), name="receive_audio")
        play_task = asyncio.create_task(play_audio(), name="play_audio")
        tones_task = asyncio.create_task(play_tones(), name="play_tones")
        stats_task = asyncio.create_task(stats_logger(), name="stats_logger")

        # Synchronous greeting: must complete before the session enters its
        # steady-state wait loop, so the send is guaranteed to fire and we get
        # a clear log line (success or failure) every cold start. The prior
        # fire-and-forget task could be cancelled by the finally block before
        # ever reaching send_client_content, which is why neither log appeared.
        if not _initial_greeting_sent and SESSION_OPENING_PROMPT:
            await asyncio.sleep(0.25)  # let the WebSocket handshake settle
            await _send_session_greeting(session)

        tasks = [
            receive_task,
            play_task,
            tones_task,
            stats_task,
            asyncio.create_task(send_realtime(session), name="send_realtime"),
            asyncio.create_task(listen_audio(mic_idx, mic_info), name="listen_audio"),
            asyncio.create_task(fast_lane_dispatch(session), name="fast_lane_dispatch"),
            asyncio.create_task(idle_lane_dispatch(session), name="idle_lane_dispatch"),
            asyncio.create_task(retry_watchdog(), name="retry_watchdog"),
        ]
        watchdog = asyncio.create_task(RECONNECT_EVENT.wait(), name="reconnect_watchdog")

        try:
            done, pending = await asyncio.wait(
                tasks + [watchdog], return_when=asyncio.FIRST_COMPLETED
            )

            # If a real task raised, surface that exception so the supervisor
            # can decide whether to back off. Log the task name explicitly
            # so we can tell *which* task died (audio device contention,
            # WS receive error, etc. all look identical otherwise).
            for t in done:
                if t is watchdog:
                    continue
                exc = t.exception()
                if exc is not None and not isinstance(exc, asyncio.CancelledError):
                    print(
                        f"[live] task '{t.get_name()}' crashed: "
                        f"{type(exc).__name__}: {exc}",
                        flush=True,
                    )
                    raise exc
                # A task that finished without exception is also worth noting —
                # in steady state none of these should ever return.
                if exc is None:
                    print(
                        f"[live] task '{t.get_name()}' exited unexpectedly (no error)",
                        flush=True,
                    )

            # Watchdog fired (pre-emptive reconnect requested) → graceful close.
            if watchdog in done:
                return "preemptive"

            return "ended"
        finally:
            _current_session = None
            watchdog.cancel()
            for t in tasks:
                t.cancel()
            cleanup = list(tasks) + [watchdog]
            await asyncio.gather(*cleanup, return_exceptions=True)


async def run():
    global RECONNECT_EVENT, LATEST_HANDLE, _IS_SPEAKING, _in_flight_lock

    if not ENABLED:
        print("GEMINI_LIVE_ENABLED is not true — exiting.")
        return

    if not os.environ.get("GEMINI_API_KEY"):
        print("ERROR: GEMINI_API_KEY not set.", file=sys.stderr)
        sys.exit(1)

    mic_idx, mic_info = pick_input_device()
    print(
        f"\n>>> Mic [{mic_idx}] '{mic_info['name']}' @ {SEND_SAMPLE_RATE}Hz mono 16-bit\n"
        f">>> Race API: {RACE_API_URL}\n"
        f">>> Model: {MODEL}\n"
    )

    _start_ptt_listener()
    RECONNECT_EVENT = asyncio.Event()
    _IS_SPEAKING = asyncio.Event()
    _in_flight_lock = asyncio.Lock()

    # Controller-mode PTT runs as a long-lived async task that mirrors Go's
    # BUTN-derived state into _ptt_active. Lives outside _run_session so it
    # survives Gemini Live reconnects.
    ptt_ws_task = None
    if PTT_SOURCE == "controller":
        ptt_ws_task = asyncio.create_task(_ptt_ws_listener(), name="ptt_ws_listener")

    client = genai.Client()
    backoff = 1.0
    max_backoff = 30.0

    try:
        while True:
            try:
                outcome = await _run_session(client, mic_idx, mic_info)
            except asyncio.CancelledError:
                raise
            except Exception as e:
                wait_s = backoff
                print(
                    f"[live] session ended with error: {type(e).__name__}: {e} "
                    f"— reconnecting in {wait_s:.1f}s "
                    f"(handle={LATEST_HANDLE[:8] if LATEST_HANDLE else 'fresh'})",
                    flush=True,
                )
                await asyncio.sleep(wait_s)
                backoff = min(backoff * 2, max_backoff)
                RECONNECT_EVENT.clear()
                continue

            # Successful exit (preemptive reconnect or natural end) → reconnect
            # immediately with no backoff, using the saved handle.
            backoff = 1.0
            RECONNECT_EVENT.clear()
            print(
                f"[live] session ended ({outcome}), reconnecting "
                f"(handle={LATEST_HANDLE[:8] if LATEST_HANDLE else 'fresh'})",
                flush=True,
            )
    except asyncio.CancelledError:
        pass
    finally:
        if ptt_ws_task is not None:
            ptt_ws_task.cancel()
            try:
                await ptt_ws_task
            except (asyncio.CancelledError, Exception):
                pass
        if audio_stream:
            try:
                audio_stream.close()
            except Exception:
                pass
        pya.terminate()
        print("\nGemini Live service stopped.")


if __name__ == "__main__":
    try:
        asyncio.run(run())
    except KeyboardInterrupt:
        print("Interrupted.")
