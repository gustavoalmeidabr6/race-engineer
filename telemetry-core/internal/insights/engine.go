package insights

import (
	"context"
	"database/sql"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// Engine evaluates insight rules against the live RaceState on a fixed ticker.
// It reads from an atomic *models.RaceState pointer (never from DuckDB) for
// sub-microsecond evaluations.
type Engine struct {
	stateLoader func() *models.RaceState    // returns the current atomic RaceState snapshot
	rules       []Rule                       // the 12 ported PerformanceAnalyzer rules
	cooldown    *CooldownManager             // per-rule cooldown and debounce state
	insightChan chan<- models.DrivingInsight  // non-blocking send target
	interval    time.Duration                // evaluation period (5 s)
	talkLevel   func() int32                 // returns current verbosity from config
	onInsight   func(models.DrivingInsight)  // optional callback for every generated insight

	// Pi-agent trigger hooks. Fired regardless of talk level — the pi agent
	// decides whether to act. Both nil-safe.
	onLapComplete   func(lap int)
	onSignificant   func(code, detail string, meta map[string]any)

	// bus is the Interrupts Bus where typed proactive Events are pushed
	// (lap_summary, threats, weather, etc.). Set via SetBrain — nil disables
	// event emission so non-Live deployments behave as before.
	bus *brain.RaceBrain

	// reader is the DuckDB read-only handle used by the lap-summary aggregator.
	// Set via SetReader — nil disables SQL-backed event emitters.
	reader *sql.DB

	// roster is the optional driver-name lookup used by event reasoning so
	// the engineer says "Hamilton" instead of "Car #5". Set via SetRoster;
	// nil falls back to the legacy "Car #N" / "P+1" labelling.
	roster rosterLookup

	// --- Closing-threat detector state (see closing_threat.go) ---
	// All fields are accessed only from evaluate() (single-goroutine ticker),
	// so no mutex is needed.
	prevPosition       uint8       // for detecting overtakes against current Position
	gapBehindSamples   []gapSample // ring of recent gap-to-car-behind readings
	closingThreatLatch bool        // true while a "closing" event has fired and the gap hasn't re-opened
}

// gapSample is one reading of the player's gap to the car directly behind.
// Used by emitClosingThreat to compute closing rate over a short window.
type gapSample struct {
	at     time.Time
	gapMs  int32 // positive = car behind is THIS many ms behind us
	myPos  uint8 // player position when the sample was taken — we discard the
	// ring on a position change so a pit cycle doesn't poison the trend.
}

// NewEngine creates a ready-to-run insight engine.
//
//   - stateLoader: a closure that loads the current *models.RaceState from an
//     atomic pointer. Returns nil when no telemetry has been received yet.
//   - talkLevel: a closure that reads the current talk level (1-10) from the
//     config's atomic Int32.
//   - insightChan: insights that pass the talk-level filter are sent here
//     (non-blocking).
func NewEngine(
	stateLoader func() *models.RaceState,
	talkLevel func() int32,
	insightChan chan<- models.DrivingInsight,
) *Engine {
	return &Engine{
		stateLoader: stateLoader,
		rules:       DefaultRules(),
		cooldown:    NewCooldownManager(),
		insightChan: insightChan,
		interval:    5 * time.Second,
		talkLevel:   talkLevel,
	}
}

// SetOnInsight registers a callback that fires for every generated insight
// (before talk-level filtering). Used by the workspace writer to record findings.
func (e *Engine) SetOnInsight(fn func(models.DrivingInsight)) {
	e.onInsight = fn
}

// SetBrain wires the Interrupts Bus so Engine can push typed Events
// (lap_summary, etc.) on top of the legacy DrivingInsight stream.
func (e *Engine) SetBrain(b *brain.RaceBrain) { e.bus = b }

// SetReader wires the DuckDB read handle used by SQL-backed event producers
// (lap_summary aggregator). Nil disables those producers.
func (e *Engine) SetReader(r *sql.DB) { e.reader = r }

// SetRoster wires the driver roster so emitOvertaken / emitClosingThreat
// can name competitors by surname instead of position number.
func (e *Engine) SetRoster(r rosterLookup) { e.roster = r }

// SetLapCompleteHook registers a callback that fires once each time the
// player car crosses the start/finish line (the rollover transition). Used
// by the pi-agent trigger queue to wake up its planner on every lap.
func (e *Engine) SetLapCompleteHook(fn func(lap int)) { e.onLapComplete = fn }

// SetSignificantEventHook registers a callback that fires for every insight
// that survives talk-level filtering. The pi agent uses this to detect
// significant changes (tire cliff, weather, damage, safety car, …) without
// re-implementing the rules engine.
func (e *Engine) SetSignificantEventHook(fn func(code, detail string, meta map[string]any)) {
	e.onSignificant = fn
}

// rosterLookup is the minimal interface emitters need from the roster.
// Defined here so tests can inject a stub without importing state pkg.
//
// ResolveRadioName returns "" when the roster only has a positional
// placeholder ("P5") — emitters use that signal to build their own
// position-aware fallback rather than voicing the placeholder verbatim.
type rosterLookup interface {
	ResolveRadioName(carIdx uint8) string
}

// Run starts the evaluation loop. It ticks every 5 seconds, evaluates all 12
// rules against the latest RaceState, filters by talk level, and pushes
// surviving insights to insightChan. It blocks until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()

	log.Info().Dur("interval", e.interval).Msg("Insight engine started")

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("Insight engine stopping")
			return
		case <-ticker.C:
			e.evaluate()
		}
	}
}

// evaluate runs a single evaluation cycle.
func (e *Engine) evaluate() {
	state := e.stateLoader()
	if state == nil {
		return
	}

	// Snapshot the previous lap BEFORE rules run — ruleLapStart updates
	// cd.LastLap as a side-effect, so we have to capture it now if we want
	// to fire a lap_summary event off the rollover transition.
	prevLap := e.cooldown.LastLap()

	// Calculate the minimum priority an insight must have to pass the
	// talk-level filter.  talkLevel=10 (chatty) lets everything through
	// (minPriority=1), talkLevel=1 (quiet) requires priority >= 10.
	tl := e.talkLevel()
	minPriority := int(11 - tl)

	for _, rule := range e.rules {
		insight := rule.Evaluate(state, e.cooldown)
		if insight == nil {
			continue
		}

		// Record to workspace (all findings, regardless of talk-level)
		if e.onInsight != nil {
			e.onInsight(*insight)
		}

		// Talk-level filter
		if insight.Priority < minPriority {
			log.Debug().
				Str("rule", rule.Name).
				Int("priority", insight.Priority).
				Int("min_priority", minPriority).
				Msg("Insight filtered by talk level")
			continue
		}

		// Non-blocking send — drop if the channel is full rather than
		// blocking the evaluation loop.
		select {
		case e.insightChan <- *insight:
			log.Info().Str("rule", rule.Name).Str("type", insight.Type).Msg("Insight generated")
		default:
			log.Warn().Str("rule", rule.Name).Msg("Insight channel full, dropping insight")
		}

		// Pi-agent significant-event hook — every surviving insight is by
		// definition significant enough to wake the agent.
		if e.onSignificant != nil {
			e.onSignificant(rule.Name, insight.Message, map[string]any{
				"type":     insight.Type,
				"priority": insight.Priority,
			})
		}
	}

	// Event-bus producers — fire AFTER rules so cd.LastLap is the new lap.
	// Each producer reads prevLap / state and pushes a typed Event when its
	// trigger condition holds.
	lapJustCompleted := state.CurrentLap > prevLap && prevLap > 0
	if e.bus != nil {
		if lapJustCompleted {
			e.emitLapSummary(state, prevLap, tl)
		}
		e.emitGameStateEvents(state, tl)
	}

	// Pi-agent lap-complete hook fires regardless of bus state so the agent
	// gets woken even in non-Live deployments.
	if lapJustCompleted && e.onLapComplete != nil {
		e.onLapComplete(int(prevLap))
	}
}
