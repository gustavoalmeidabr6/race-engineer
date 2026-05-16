#!/bin/bash
# ---------------------------------------------------------------------------
# Race Engineer — Concurrent launcher
# Starts Go core, React dashboard, Python voice service, and (optionally)
# the Gemini Live conversational backend.
# ---------------------------------------------------------------------------

set -e
cd "$(dirname "$0")"

# ── Clean-shutdown trap ─────────────────────────────────────────────────
# Ctrl+C used to leak orphan processes (telemetry-core holding the DuckDB
# lock, vite servers, Python procs) because:
#   1. The output pipeline `| tee >(awk ...)` put `tee` in the foreground,
#      so SIGINT landed on `tee` instead of `concurrently`.
#   2. Each entry in PROCS ran via `sh -c "VAR=... ./binary"`, and the
#      shell wrapper sometimes outlived its child after concurrently sent
#      SIGTERM.
# This trap fires on Ctrl+C / kill / normal exit and force-tears-down
# everything by name. Belt-and-suspenders with the `exec` prefixes below
# — either alone is usually enough, both together are bulletproof.
cleanup() {
    local rc=$?
    echo
    echo "Stopping race-engineer processes…"
    # Graceful first.
    pkill -TERM -f "workspace/bin/telemetry-core" 2>/dev/null || true
    pkill -TERM -f "dashboard/node_modules/.bin/vite" 2>/dev/null || true
    pkill -TERM -f "voice_service.py" 2>/dev/null || true
    pkill -TERM -f "gemini_live_service.py" 2>/dev/null || true
    pkill -TERM -f "pi_agent_service.py" 2>/dev/null || true
    # Kill any direct children of this script (concurrently, npm wrappers, …).
    pkill -TERM -P $$ 2>/dev/null || true
    # Force-kill survivors after a short grace period.
    sleep 0.4
    pkill -KILL -f "workspace/bin/telemetry-core" 2>/dev/null || true
    pkill -KILL -f "dashboard/node_modules/.bin/vite" 2>/dev/null || true
    pkill -KILL -f "voice_service.py" 2>/dev/null || true
    pkill -KILL -f "gemini_live_service.py" 2>/dev/null || true
    pkill -KILL -f "pi_agent_service.py" 2>/dev/null || true
    pkill -KILL -P $$ 2>/dev/null || true
    exit $rc
}
trap cleanup INT TERM

# ensure dependencies exist
if ! command -v concurrently &> /dev/null
then
    echo "concurrently is not installed. Installing it globally now..."
    npm install -g concurrently
fi

# Configuration source of truth: ~/.race-engineer/config.json. The Go
# core auto-seeds it with every schema default on first boot, and the
# dashboard's Settings page persists changes there. .env is no longer
# read by default — set RACE_ENGINEER_ALLOW_ENV=1 to opt back into the
# legacy fallback (and surface deprecation warnings per-key).
if [ -f .env ]; then
    if [ "${RACE_ENGINEER_ALLOW_ENV:-}" = "1" ] || [ "${RACE_ENGINEER_ALLOW_ENV:-}" = "true" ]; then
        echo "RACE_ENGINEER_ALLOW_ENV set — sourcing .env (legacy fallback)."
        set -a
        # shellcheck disable=SC1091
        source .env
        set +a
    else
        echo "Note: .env present but ignored. Move values into the Settings page or run"
        echo "      'make migrate-config' to import them. Set RACE_ENGINEER_ALLOW_ENV=1"
        echo "      to keep the legacy fallback during migration."
    fi
fi

# ── Hydrate env from ~/.race-engineer/config.json ────────────────────────
# The Go core auto-seeds this file with all schema defaults and the
# Settings dashboard persists changes there. start.sh used to read mode
# gates (PI_AGENT_MODE, VOICE_MODE, …) from the shell env only, so a
# value set via the dashboard never reached the launcher (e.g. PI-AGENT
# child never started even though the dashboard showed "mode on").
# This block mirrors python/race_config.py's _hydrate_environ: explicit
# env wins, JSON fills the gaps.
CONFIG_JSON="${HOME}/.race-engineer/config.json"
if [ -f "$CONFIG_JSON" ] && command -v python3 >/dev/null 2>&1; then
    while IFS='=' read -r k v; do
        [ -z "$k" ] && continue
        if [ -z "${!k+x}" ] || [ -z "${!k}" ]; then
            export "$k=$v"
        fi
    done < <(python3 -c '
import json, sys
try:
    with open(sys.argv[1]) as f:
        data = json.load(f)
except Exception:
    sys.exit(0)
if not isinstance(data, dict):
    sys.exit(0)
for k, v in data.items():
    if v is None or v == "":
        continue
    if isinstance(v, bool):
        v = "true" if v else "false"
    print(f"{k}={v}")
' "$CONFIG_JSON")
fi

# VOICE_MODE is the source of truth for which voice path runs at boot.
#   live_only — only Gemini Live (default). voice_service.py NOT needed.
#   ptt_only  — only legacy push-to-talk. Gemini Live agent NOT started.
#   both      — both paths active.
# Legacy GEMINI_LIVE_ENABLED is honoured only when VOICE_MODE is empty,
# matching the precedence rules inside the Go config loader.
VOICE_MODE="${VOICE_MODE:-}"
if [ -z "$VOICE_MODE" ]; then
    if [ "${GEMINI_LIVE_ENABLED:-false}" = "true" ]; then
        VOICE_MODE="both"
    else
        VOICE_MODE="live_only"
    fi
fi
case "$VOICE_MODE" in
    live_only|ptt_only|both) ;;
    *) echo "WARN: unknown VOICE_MODE='$VOICE_MODE', falling back to live_only"; VOICE_MODE="live_only" ;;
esac
# Derived booleans used downstream for the conditional process list.
LIVE_PATH_ENABLED="false"
PTT_PATH_ENABLED="false"
[ "$VOICE_MODE" = "live_only" ] || [ "$VOICE_MODE" = "both" ] && LIVE_PATH_ENABLED="true"
[ "$VOICE_MODE" = "ptt_only" ]  || [ "$VOICE_MODE" = "both" ] && PTT_PATH_ENABLED="true"
# Keep GEMINI_LIVE_ENABLED in sync for the Python deps installer + child env.
# Must be exported — gemini_live_service.py reads it from os.environ and
# bails out if unset, and a bare assignment is shell-local only.
export GEMINI_LIVE_ENABLED="$LIVE_PATH_ENABLED"

# ── Log capture ──────────────────────────────────────────────────────────
# All concurrently output is tee'd to logs/race-engineer-<ts>.log (ANSI
# colors stripped for grep-friendliness). The terminal still gets colored
# output. Override the path with LOG_FILE=path/to/file.log.
LOG_DIR="${LOG_DIR:-logs}"
mkdir -p "$LOG_DIR"
LOG_FILE="${LOG_FILE:-$LOG_DIR/race-engineer-$(date +%Y%m%d-%H%M%S).log}"
ln -sf "$(basename "$LOG_FILE")" "$LOG_DIR/latest.log" 2>/dev/null || true
echo "Logging to: $LOG_FILE  (symlink: $LOG_DIR/latest.log)"

# ── Python venv setup ────────────────────────────────────────────────────
if [ ! -d venv ]; then
    echo "Creating Python virtual environment..."
    python3 -m venv venv
fi

# Only install Python deps on first run or when explicitly requested (REINSTALL_DEPS=1)
DEPS_MARKER="venv/.deps_installed"
NEED_GEMINI_DEPS=0
if [ "$GEMINI_LIVE_ENABLED" = "true" ] && [ ! -f "venv/.gemini_live_installed" ]; then
    NEED_GEMINI_DEPS=1
fi

if [ ! -f "$DEPS_MARKER" ] || [ "${REINSTALL_DEPS:-0}" = "1" ]; then
    echo "Installing Python voice service dependencies..."
    source venv/bin/activate

    # Core deps (always needed)
    pip install -q edge-tts faster-whisper httpx fastapi python-multipart uvicorn pydantic python-dotenv

    # Optional deps based on configured engines
    if [ "$TTS_ENGINE" = "qwen-tts" ]; then
        echo "  Installing Qwen TTS deps (mlx-audio, soundfile, numpy)..."
        pip install -q mlx-audio soundfile numpy
    fi

    if [ "$STT_ENGINE" = "parakeet" ]; then
        echo "  Installing Parakeet STT deps (nemo_toolkit[asr], soundfile, librosa)..."
        pip install -q "nemo_toolkit[asr]" soundfile librosa
    fi

    deactivate
    touch "$DEPS_MARKER"
else
    echo "Python deps already installed (run with REINSTALL_DEPS=1 to force reinstall)"
fi

if [ "$NEED_GEMINI_DEPS" = "1" ]; then
    echo "Installing Gemini Live deps (google-genai, pyaudio, pynput)..."
    source venv/bin/activate
    pip install -q google-genai pyaudio pynput
    deactivate
    touch venv/.gemini_live_installed
fi

# Pi-agent deps (mcp + LLM SDK). Lazy install on first run with PI_AGENT_MODE
# set to anything other than "off" / empty. The marker is per-provider so
# switching PI_AGENT_PROVIDER between runs installs the new SDK instead of
# silently reusing the previously-installed one.
PI_AGENT_MODE="${PI_AGENT_MODE:-off}"
PI_AGENT_PROVIDER="${PI_AGENT_PROVIDER:-anthropic}"
PI_AGENT_INSTALL_MARKER="venv/.pi_agent_installed_${PI_AGENT_PROVIDER}"
if [ "$PI_AGENT_MODE" != "off" ] && [ -n "$PI_AGENT_MODE" ] && [ ! -f "$PI_AGENT_INSTALL_MARKER" ]; then
    echo "Installing pi-agent deps (mcp + provider SDK for $PI_AGENT_PROVIDER)..."
    source venv/bin/activate
    pip install -q "mcp>=1.0"
    case "$PI_AGENT_PROVIDER" in
        anthropic|claude) pip install -q anthropic ;;
        gemini)           pip install -q google-genai ;;
        # "custom" speaks the OpenAI chat-completions protocol (Ollama,
        # vLLM, LiteLLM, OpenRouter, Together, …) — same SDK.
        openai|custom)    pip install -q openai ;;
    esac
    deactivate
    touch "$PI_AGENT_INSTALL_MARKER"
fi

# ── Pre-launch cleanup ───────────────────────────────────────────────────
# Kill any orphans from a previous run BEFORE we build/launch. The
# cleanup() trap above only fires on graceful Ctrl+C of this script; if a
# previous run was killed via terminal-close, kill -9, crash, or OOM, the
# trap never ran and the old processes still hold their ports (especially
# :8081 / the DuckDB lock on telemetry-core). Without this pre-pass, the
# new go-core hits "bind: address already in use" but doesn't exit, and
# the whole stack silently runs against the stale old process — pi-agent
# 404 loops, vite ws proxy EPIPEs, dead dashboard.
echo "Killing any orphaned processes from previous runs…"
pkill -TERM -f "workspace/bin/telemetry-core" 2>/dev/null || true
pkill -TERM -f "dashboard/node_modules/.bin/vite" 2>/dev/null || true
pkill -TERM -f "voice_service.py" 2>/dev/null || true
pkill -TERM -f "gemini_live_service.py" 2>/dev/null || true
pkill -TERM -f "pi_agent_service.py" 2>/dev/null || true
sleep 0.5
pkill -KILL -f "workspace/bin/telemetry-core" 2>/dev/null || true
pkill -KILL -f "dashboard/node_modules/.bin/vite" 2>/dev/null || true
pkill -KILL -f "voice_service.py" 2>/dev/null || true
pkill -KILL -f "gemini_live_service.py" 2>/dev/null || true
pkill -KILL -f "pi_agent_service.py" 2>/dev/null || true

# Belt-and-suspenders: anything else still bound to :8081 / :8000 / :8092
# (e.g. the desktop Wails .app's embedded telemetry-core under a different
# binary path, a stray `go run`, a manually-started vite). Without this,
# the pattern-based pkill above misses non-checkout binaries that still
# occupy our ports.
if command -v lsof >/dev/null 2>&1; then
    for port in "${API_PORT:-8081}" 8000 8092; do
        stragglers=$(lsof -ti tcp:"$port" 2>/dev/null || true)
        if [ -n "$stragglers" ]; then
            echo "  Force-killing stragglers on :$port: $stragglers"
            kill -KILL $stragglers 2>/dev/null || true
        fi
    done
    sleep 0.2
fi

# ── Go builds ────────────────────────────────────────────────────────────
echo "Building telemetry-core Go service..."
(cd telemetry-core && go build -o ../workspace/bin/telemetry-core cmd/server/main.go)
echo "Building racedb query tool..."
(cd telemetry-core && go build -o ../workspace/bin/racedb cmd/query/main.go)
echo "Go builds complete!"

echo "Installing dashboard dependencies..."
(cd dashboard && npm install)

# ── Build process list dynamically ───────────────────────────────────────
PROCS=()
NAMES=()
COLORS=()

# `exec env …` makes the Go binary REPLACE the shell wrapper, so
# concurrently's SIGTERM goes straight to telemetry-core (which has its
# own graceful-shutdown handler in cmd/server/main.go). Without exec the
# wrapping `sh` would catch the signal and the Go binary could outlive
# its parent — exactly the orphan that holds the DuckDB lock.
PROCS+=("exec env DB_PATH=\"${DB_PATH:-workspace/telemetry.duckdb}\" ./workspace/bin/telemetry-core")
NAMES+=("GO-CORE")
COLORS+=("blue")

PROCS+=("cd dashboard && exec npm run dev")
NAMES+=("REACT")
COLORS+=("green")

# Launch voice_service.py only when the PTT path is wired (ptt_only or
# both). In live_only mode the Gemini Live agent owns the entire voice
# channel, so loading Whisper + edge-tts here is wasted RAM, wasted boot
# time, and a port-8000 hazard if a previous run leaked a process.
# VOICE_SERVICE_ENABLED=true forces it on regardless of mode (e.g. when
# debugging the legacy pipeline alongside Live).
START_VOICE_SERVICE=false
if [ "$PTT_PATH_ENABLED" = "true" ] || [ "${VOICE_SERVICE_ENABLED:-auto}" = "true" ]; then
    START_VOICE_SERVICE=true
fi

if [ "$START_VOICE_SERVICE" = "true" ]; then
    PROCS+=("source venv/bin/activate && exec python voice_service.py")
    NAMES+=("WHISPER")
    COLORS+=("yellow")
    echo "Voice service ENABLED (VOICE_MODE=$VOICE_MODE)."
else
    echo "Voice service skipped (VOICE_MODE=$VOICE_MODE — Live owns the voice channel)."
fi

# The Python Gemini Live service (gemini_live_service.py) is the LEGACY
# Live backend. The Go in-process Live agent at /api/voice/live is the
# supported path. Running both at once opens two independent Gemini Live
# sessions — the Python one plays through pyaudio (laptop speakers) and
# the Go one streams PCM to the dashboard browser — so the user hears
# two voices simultaneously.
#
# Default: only the Go in-process agent runs.
# Set GEMINI_LIVE_PYTHON_BACKEND=true to force the legacy Python path;
# the Go server then disables its in-process Live factory (it reads the
# same env var) so the two paths stay mutually exclusive.
if [ "$LIVE_PATH_ENABLED" = "true" ] && [ "${GEMINI_LIVE_PYTHON_BACKEND:-false}" = "true" ]; then
    export GEMINI_LIVE_PYTHON_BACKEND="true"
    # PYTHONUNBUFFERED=1 ensures prints flush immediately so concurrently shows
    # logs (mic picker, [ptt] state, errors) in real time instead of buffering.
    PROCS+=("source venv/bin/activate && exec env PYTHONUNBUFFERED=1 python gemini_live_service.py")
    NAMES+=("GEMINI-LIVE-PY")
    COLORS+=("cyan")
    echo "Gemini Live PYTHON backend ENABLED (legacy). Go in-process Live will stand down."
elif [ "$LIVE_PATH_ENABLED" = "true" ]; then
    echo "Gemini Live ENABLED via Go in-process agent at /api/voice/live."
fi

if [ "$PI_AGENT_MODE" != "off" ] && [ -n "$PI_AGENT_MODE" ]; then
    # Sandboxed background data-analyst team. Connects to the Go server's
    # MCP endpoint (default /mcp) and reacts to query / lap_complete /
    # significant_event triggers. No shell, no arbitrary HTTP — see
    # tests/test_pi_agent_sandbox.py for the enforced banned-imports list.
    PROCS+=("source venv/bin/activate && exec env PYTHONUNBUFFERED=1 python pi_agent_service.py")
    NAMES+=("PI-AGENT")
    COLORS+=("magenta")
    echo "Pi agent ENABLED (mode=$PI_AGENT_MODE provider=$PI_AGENT_PROVIDER)."
fi

NAMES_CSV=$(IFS=, ; echo "${NAMES[*]}")
COLORS_CSV=$(IFS=, ; echo "${COLORS[*]}")

echo "Starting ${#PROCS[@]} processes: $NAMES_CSV"
set -o pipefail

# Run concurrently as the foreground process so Ctrl+C (SIGINT) reaches
# it directly — `concurrently --kill-others` then SIGTERMs every child.
# The previous `| tee >(awk ...)` setup put `tee` in the foreground and
# left concurrently to be reaped by happenstance, which is why Ctrl+C
# leaked orphan processes. We redirect output via a background process
# substitution that strips ANSI for the log file but keeps the terminal
# stream live and colourful.
concurrently \
    --names "$NAMES_CSV" \
    --prefix-colors "$COLORS_CSV" \
    --kill-others \
    --kill-signal SIGTERM \
    "${PROCS[@]}" \
    > >(tee >(awk '{ gsub(/\033\[[0-9;]*[mK]/, ""); print; fflush() }' >> "$LOG_FILE")) 2>&1
