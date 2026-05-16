#!/bin/bash
# ---------------------------------------------------------------------------
# Race Engineer — Graceful shutdown
#
# Sends SIGTERM first, gives processes ~500ms to flush, then SIGKILL the
# survivors. Pattern set matches start.sh so adding a new launchable
# means updating both files in lockstep.
# ---------------------------------------------------------------------------

echo "Stopping Race Engineer components..."

PATTERNS=(
    "workspace/bin/telemetry-core"
    "dashboard/node_modules/.bin/vite"
    "voice_service.py"
    "gemini_live_service.py"
    "opencode serve"
)

LABELS=(
    "Go core"
    "Dashboard"
    "Voice service"
    "Gemini Live"
    "Analyst agent"
)

# Graceful pass
for i in "${!PATTERNS[@]}"; do
    if pkill -TERM -f "${PATTERNS[$i]}" 2>/dev/null; then
        echo "  ${LABELS[$i]} signalled"
    fi
done

sleep 0.5

# Force pass — silent unless something refused to die
for i in "${!PATTERNS[@]}"; do
    if pkill -KILL -f "${PATTERNS[$i]}" 2>/dev/null; then
        echo "  ${LABELS[$i]} force-killed"
    fi
done

echo "All components stopped."
