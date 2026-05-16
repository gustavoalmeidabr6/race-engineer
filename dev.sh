#!/bin/bash
# ---------------------------------------------------------------------------
# Race Engineer — developer launcher
#
# One command to: build the Go telemetry-core, ensure dashboard deps are
# installed, then run both via `concurrently` with proper Ctrl+C
# propagation. No Python, no venv, no edge-tts — Phase 3 moved everything
# into the Go binary so the developer path is a 2-process world (Go +
# Vite).
#
# Use `./dev.sh` (or `make dev`). The legacy `./start.sh` is kept for the
# Python TTS/Live path if you want to A/B compare; it should be retired
# once the Go Live path is fully production-hardened.
# ---------------------------------------------------------------------------

set -e
cd "$(dirname "$0")"

# ── Clean-shutdown trap ─────────────────────────────────────────────────
# Mirrors the start.sh trap. Ctrl+C fires this; we TERM the known process
# names first, give them 400ms to flush, then KILL any survivors. This
# is the safety net for cases where concurrently's --kill-others doesn't
# reach a grandchild (npm wrappers can hide vite from signal propagation
# on macOS).
cleanup() {
    local rc=$?
    echo
    echo "Stopping race-engineer…"
    pkill -TERM -f "workspace/bin/telemetry-core" 2>/dev/null || true
    pkill -TERM -f "dashboard/node_modules/.bin/vite" 2>/dev/null || true
    pkill -TERM -P $$ 2>/dev/null || true
    sleep 0.4
    pkill -KILL -f "workspace/bin/telemetry-core" 2>/dev/null || true
    pkill -KILL -f "dashboard/node_modules/.bin/vite" 2>/dev/null || true
    pkill -KILL -P $$ 2>/dev/null || true
    exit $rc
}
trap cleanup INT TERM

# ── Concurrently ─────────────────────────────────────────────────────────
if ! command -v concurrently &> /dev/null; then
    echo "concurrently not installed — installing globally via npm..."
    npm install -g concurrently
fi

# ── Logs ─────────────────────────────────────────────────────────────────
LOG_DIR="${LOG_DIR:-logs}"
mkdir -p "$LOG_DIR"
LOG_FILE="${LOG_FILE:-$LOG_DIR/race-engineer-$(date +%Y%m%d-%H%M%S).log}"
ln -sf "$(basename "$LOG_FILE")" "$LOG_DIR/latest.log" 2>/dev/null || true
echo "Logging to: $LOG_FILE  (symlink: $LOG_DIR/latest.log)"

# ── Build Go binaries ────────────────────────────────────────────────────
echo "Building telemetry-core…"
(cd telemetry-core && go build -o ../workspace/bin/telemetry-core ./cmd/server)
echo "Building racedb…"
(cd telemetry-core && go build -o ../workspace/bin/racedb ./cmd/query)
echo "Build complete."

# ── Dashboard deps ───────────────────────────────────────────────────────
# npm install is idempotent and fast (~1s on warm cache), so we always
# run it. Skip with SKIP_NPM_INSTALL=1 if you know your node_modules is
# up to date and want a faster start.
if [ "${SKIP_NPM_INSTALL:-0}" != "1" ]; then
    echo "Installing dashboard deps…"
    (cd dashboard && npm install --silent)
fi

# ── Process list ─────────────────────────────────────────────────────────
# `exec env …` makes each binary REPLACE its shell wrapper so
# concurrently's SIGTERM lands directly on the Go binary / Vite. Without
# exec the shell can outlive the signal and orphan the child (which is
# what was holding the DuckDB lock before this rewrite).
ANALYST_MODE="${ANALYST_MODE:-internal}"
DB_PATH="${DB_PATH:-workspace/telemetry.duckdb}"

PROCS=(
    "exec env DB_PATH=\"$DB_PATH\" ANALYST_MODE=$ANALYST_MODE ./workspace/bin/telemetry-core"
    "cd dashboard && exec npm run dev"
)
NAMES="GO,VITE"
COLORS="blue,green"

echo "Starting Go core + Vite…"
echo "  Dashboard: http://localhost:8092  (Vite proxies /api → :${API_PORT:-8081})"
echo

# concurrently runs in the foreground so Ctrl+C reaches it directly;
# output goes through a background `tee` so logs are colorized in the
# terminal and ANSI-stripped in the file. The previous `| tee …` pipe
# put `tee` in the foreground and let concurrently get orphaned.
set -o pipefail
concurrently \
    --names "$NAMES" \
    --prefix-colors "$COLORS" \
    --kill-others \
    --kill-signal SIGTERM \
    "${PROCS[@]}" \
    > >(tee >(awk '{ gsub(/\033\[[0-9;]*[mK]/, ""); print; fflush() }' >> "$LOG_FILE")) 2>&1
