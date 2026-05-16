package insights

import (
	"fmt"
	"time"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/enums"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// emitPositionChange fires a P3 silent context event whenever the player's
// Position number changes — in any direction, in any session. Unlike
// emitOvertaken (P5, race-only, "you got passed!") this one is purely
// informational state for the Live agent's brain snapshot: Gemini Live
// reads the session_type from DebugData and decides whether/how to mention
// it on the radio (a quali position move means something very different
// from a race overtake, and a practice move usually means nothing at all).
//
// At P3 this never triggers TTS — the event bus only voices priority ≥4.
//
// Bursts of consecutive changes (a driver getting passed by three cars in
// quick succession) are coalesced via positionChangeSettleWindow: the
// emitter waits until the position has been stable for the window, then
// fires ONE event with the burst's original "from" and the final "to".
// Without this, the same overtaking train produced three back-to-back P3
// events that flooded the brain snapshot.
//
// Dedup is keyed by (session_type, lap, new_position) so a position that
// oscillates within one lap reports each unique destination at most once
// per lap. Different laps refire freely.
func (e *Engine) emitPositionChange(state *models.RaceState, talkLevel int32) {
	if state == nil || state.Position == 0 {
		return
	}
	// First sighting of a real position — bank it and skip. Session starts
	// often report Position=0 then a transient ranking that would otherwise
	// look like a P22→P5 jump.
	if e.prevPositionAny == 0 {
		e.prevPositionAny = state.Position
		return
	}
	now := e.clock()
	if state.Position == e.prevPositionAny {
		// Position back to last-emitted — cancel any in-flight burst.
		e.pendingPositionPrev = 0
		e.pendingPositionLast = 0
		e.pendingPositionChange = time.Time{}
		return
	}
	if e.pendingPositionPrev == 0 {
		// First detected change of a fresh burst — capture the "from"
		// and start the settle timer.
		e.pendingPositionPrev = e.prevPositionAny
		e.pendingPositionLast = state.Position
		e.pendingPositionChange = now
		return
	}
	if state.Position != e.pendingPositionLast {
		// Still moving — reset the settle timer, update the target.
		// pendingPositionPrev stays at the burst's original "from".
		e.pendingPositionLast = state.Position
		e.pendingPositionChange = now
		return
	}
	// Same position as last tick — has it been quiet for long enough?
	if now.Sub(e.pendingPositionChange) < positionChangeSettleWindow {
		return
	}

	prev := e.pendingPositionPrev
	e.prevPositionAny = state.Position
	e.pendingPositionPrev = 0
	e.pendingPositionLast = 0
	e.pendingPositionChange = time.Time{}

	gained := state.Position < prev // lower number = better
	sessionName := enums.SessionType(int(state.SessionType))

	// Summary is intentionally terse + neutral — Gemini Live phrases the
	// actual radio call based on session context. Race overtakes already
	// have their own urgent voiced event (EventOvertaken), so this stays
	// matter-of-fact even in race sessions.
	var summary string
	switch {
	case gained:
		summary = fmt.Sprintf("Up to P%d (was P%d).", state.Position, prev)
	default:
		summary = fmt.Sprintf("Down to P%d (was P%d).", state.Position, prev)
	}

	reasoning := fmt.Sprintf(
		"Position changed P%d → P%d on lap %d (%s).",
		prev, state.Position, state.CurrentLap, sessionName,
	)

	debug := map[string]any{
		"prev_position":     int(prev),
		"new_position":      int(state.Position),
		"delta":             int(prev) - int(state.Position), // positive = gained
		"gained":            gained,
		"lap":               int(state.CurrentLap),
		"session_type":      int(state.SessionType),
		"session_type_name": sessionName,
		"is_race":           isRaceSession(state.SessionType),
	}

	ev := brain.Event{
		Type:      brain.EventPositionChange,
		Priority:  brain.DefaultEventPriority(brain.EventPositionChange), // 3 — silent
		Summary:   summary,
		Reasoning: reasoning,
		DebugData: debug,
		Source:    "rule:position_change",
		DedupKey:  fmt.Sprintf("position_change:%d:%d:%d", state.SessionType, state.CurrentLap, state.Position),
	}
	enqueueAndLog(e, ev, talkLevel)
}
