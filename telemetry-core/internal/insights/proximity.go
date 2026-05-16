package insights

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/state"
)

// proximity.go — proximity awareness + heads-up event firing.
//
// Owns two responsibilities:
//
//   1. Maintain a per-car sample history for every car within
//      proximityWatchM metres of the player on track. Compute closing
//      rates from those samples. Publish a ProximitySnapshot atomically
//      on every tick — that snapshot powers the /api/state/proximity
//      tool.
//
//   2. Fire EventCarApproaching (P2) through the brain bus when the
//      time-to-impact (distance / closing_rate) for a car BEHIND drops
//      below proximityFireTTISec. TTI-based firing is more natural than
//      raw closure: a car 400 m behind closing at 50 m/s is "8 s away"
//      whether it's behind us in practice traffic or in race-mode dirty
//      air.
//
// Runs at 1 s — fast enough to catch hot-lap traffic at ~40 m/s
// closure, slow enough that map updates aren't a CPU cost.

const (
	// Snapshot watch range: cars within ±this on track go into the
	// per-car history and the published snapshot. Wider than the firing
	// threshold so the agent's tool can see further ahead.
	proximityWatchM = 2000.0

	// Firing threshold (event-only): fire EventCarApproaching when the
	// computed time-to-impact for a car behind drops below this.
	proximityFireTTISec = 8.0

	// Don't fire if they're effectively on top of us — race-mode threat
	// rules cover that.
	proximityMinFireGap = 15.0

	// Per-car re-arm: a tracked car must drop back beyond this before
	// we'll fire on them again (prevents repeats while they sit close).
	proximityRearmM = 350.0

	// Closing rate must exceed this to be considered "actively closing".
	// 1 m/s = 3.6 km/h relative — anything below is noise.
	proximityMinClosingMps = 1.0

	// Per-car sample history. 12 × 1 s = 12 s of history.
	proximitySamplesCap = 12

	// Per-car cooldown: don't refire on the SAME car within this window.
	proximityRefireSec = 25

	// Cars older than this in the history map (no new samples) are dropped.
	proximityStaleSec = 5
)

// proxSample is one (timestamp, signed-on-track-distance) reading.
// `signedM` is positive when the car is ahead of the player, negative
// when behind.
type proxSample struct {
	at      time.Time
	signedM float32
}

// proxCarHistory is the rolling-sample buffer kept for each car within
// the watch range. Samples are appended on every tick; entries that
// haven't been refreshed in proximityStaleSec are dropped from the map.
type proxCarHistory struct {
	samples    []proxSample
	lastSeen   time.Time
	lastFireAt time.Time // per-car cooldown anchor
	armed      bool      // re-arm latch — false until the car drops back beyond proximityRearmM
}

// NearbyCar is one car in the published ProximitySnapshot, decoded for
// the API tool layer.
type NearbyCar struct {
	CarIndex     uint8   `json:"car_index"`
	DriverName   string  `json:"driver_name,omitempty"` // surname from Participants packet (Hamilton, Norris) — falls back to "" when roster not yet populated
	Position     uint8   `json:"position"`
	CurrentLap   uint8   `json:"current_lap"`
	DistanceM    float32 `json:"distance_m"`     // always positive
	SignedM      float32 `json:"signed_m"`       // positive=ahead, negative=behind
	ClosingMps   float32 `json:"closing_mps"`    // positive=closing on us, negative=opening
	EtaSec       float32 `json:"eta_sec"`        // distance / closing_mps; -1 when not closing
	Threat       string  `json:"threat"`         // "closing fast" | "closing" | "steady" | "opening" | "unknown"
	PitStatus    string  `json:"pit_status"`
	DriverStatus string  `json:"driver_status"`
	LapDistanceM float32 `json:"lap_distance_m"`
}

// ProximitySnapshot is the atomic-published view that the API tool reads.
type ProximitySnapshot struct {
	GeneratedAt       time.Time   `json:"generated_at"`
	PlayerCarIndex    uint8       `json:"player_car_index"`
	PlayerLapDistance float32     `json:"player_lap_distance_m"`
	PlayerSpeedKmh    uint16      `json:"player_speed_kmh"`
	WatchRadiusM      float32     `json:"watch_radius_m"`
	NearbyAhead       []NearbyCar `json:"nearby_ahead"`  // sorted ascending by DistanceM
	NearbyBehind      []NearbyCar `json:"nearby_behind"` // sorted ascending by DistanceM
}

// ProximityWatcher polls the live RaceState at 1 s, maintains per-car
// distance histories, fires EventCarApproaching when TTI < threshold,
// and publishes a ProximitySnapshot atomically for the API tool.
type ProximityWatcher struct {
	state     func() *models.RaceState
	bus       *brain.RaceBrain
	talkLevel func() int32
	interval  time.Duration
	roster    *state.Roster // optional — when set, snapshot/event reasoning uses driver surnames

	// cars tracks per-car sample history. Lives only on the watcher
	// goroutine — accessed in tick() only — so no mutex needed.
	cars map[uint8]*proxCarHistory

	// snapshot is the atomically-published view. Read by the API
	// handler; written by tick().
	snapshot atomic.Pointer[ProximitySnapshot]
}

// NewProximityWatcher wires the watcher. interval defaults to 1 s when zero.
func NewProximityWatcher(
	stateLoader func() *models.RaceState,
	bus *brain.RaceBrain,
	talkLevel func() int32,
	interval time.Duration,
) *ProximityWatcher {
	if interval <= 0 {
		interval = 1 * time.Second
	}
	return &ProximityWatcher{
		state:     stateLoader,
		bus:       bus,
		talkLevel: talkLevel,
		interval:  interval,
		cars:      make(map[uint8]*proxCarHistory, 22),
	}
}

// SetRoster wires the driver roster so snapshot entries and event
// reasoning use surnames instead of car_index. Optional — without it,
// names fall back to the existing "Car #N" labelling.
func (w *ProximityWatcher) SetRoster(r *state.Roster) { w.roster = r }

// Snapshot returns the latest published proximity snapshot, or nil if
// no tick has run yet. Cheap (atomic load) — safe to call from any
// goroutine.
func (w *ProximityWatcher) Snapshot() *ProximitySnapshot {
	if w == nil {
		return nil
	}
	return w.snapshot.Load()
}

// Run blocks until ctx is cancelled, polling on every tick.
func (w *ProximityWatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	log.Info().
		Dur("interval", w.interval).
		Float64("watch_m", proximityWatchM).
		Float64("fire_tti_sec", proximityFireTTISec).
		Msg("Proximity watcher started")
	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("Proximity watcher stopping")
			return
		case <-ticker.C:
			w.tick()
		}
	}
}

// tick — one watcher cycle. Exposed via the type for test access.
func (w *ProximityWatcher) tick() {
	if w.state == nil {
		return
	}
	state := w.state()
	if state == nil || state.TrackLength == 0 {
		w.publishEmpty(state)
		return
	}
	// Pit lane / pit area / garage: don't track or fire. Publish an
	// empty snapshot so the API doesn't return stale data.
	if state.PitStatus != 0 || state.DriverStatus == 0 {
		w.cars = make(map[uint8]*proxCarHistory, 22)
		w.publishEmpty(state)
		return
	}

	now := time.Now()
	lapLen := float32(state.TrackLength)
	half := lapLen / 2
	me := state.GridPositions[state.PlayerCarIndex]

	// 1. Update per-car samples for every active car within watch range.
	seen := make(map[uint8]bool, 22)
	for i := 0; i < 22; i++ {
		idx := uint8(i)
		if idx == state.PlayerCarIndex {
			continue
		}
		gp := state.GridPositions[i]
		if gp.ResultStatus < 2 {
			continue
		}
		if gp.PitStatus != 0 || gp.DriverStatus == 0 {
			continue
		}
		signed := signedTrackDelta(me.LapDistance, gp.LapDistance, lapLen, half)
		if math.Abs(float64(signed)) > proximityWatchM {
			continue
		}
		seen[idx] = true
		hist := w.cars[idx]
		if hist == nil {
			hist = &proxCarHistory{}
			w.cars[idx] = hist
		}
		hist.samples = append(hist.samples, proxSample{at: now, signedM: signed})
		if len(hist.samples) > proximitySamplesCap {
			hist.samples = hist.samples[len(hist.samples)-proximitySamplesCap:]
		}
		hist.lastSeen = now
	}

	// 2. Drop stale entries (cars that left the watch window).
	for idx, hist := range w.cars {
		if !seen[idx] && now.Sub(hist.lastSeen) > proximityStaleSec*time.Second {
			delete(w.cars, idx)
		}
	}

	// 3. Build snapshot + evaluate firing rule.
	ahead := make([]NearbyCar, 0, 8)
	behind := make([]NearbyCar, 0, 8)
	behindCands := make([]approachCandidate, 0, 8)
	for idx, hist := range w.cars {
		if !seen[idx] {
			continue // stale, will be dropped next tick
		}
		nc := w.buildNearbyCar(idx, state, hist)
		if nc.SignedM >= 0 {
			ahead = append(ahead, nc)
		} else {
			behind = append(behind, nc)
			behindCands = append(behindCands, approachCandidate{nc: nc, hist: hist})
		}
	}
	// One coalesced fire per tick: multiple cars closing become a single
	// bundled radio call rather than N separate events.
	w.maybeFireGroup(now, state, behindCands)
	sort.Slice(ahead, func(i, j int) bool { return ahead[i].DistanceM < ahead[j].DistanceM })
	sort.Slice(behind, func(i, j int) bool { return behind[i].DistanceM < behind[j].DistanceM })

	snap := &ProximitySnapshot{
		GeneratedAt:       now,
		PlayerCarIndex:    state.PlayerCarIndex,
		PlayerLapDistance: state.LapDistance,
		PlayerSpeedKmh:    state.Speed,
		WatchRadiusM:      proximityWatchM,
		NearbyAhead:       ahead,
		NearbyBehind:      behind,
	}
	w.snapshot.Store(snap)
}

// signedTrackDelta returns metres from `from` to `to` along the racing
// line, signed (positive = forward, negative = backward) and wrapped to
// the shorter direction (always in (-half, half]).
func signedTrackDelta(from, to, lapLen, half float32) float32 {
	d := to - from
	for d > half {
		d -= lapLen
	}
	for d <= -half {
		d += lapLen
	}
	return d
}

// closingRateMps returns the closing rate (positive = closing on us,
// negative = opening) from the car's sample history. Returns 0 when
// fewer than 2 samples exist.
func closingRateMps(hist *proxCarHistory) float32 {
	n := len(hist.samples)
	if n < 2 {
		return 0
	}
	first := hist.samples[0]
	last := hist.samples[n-1]
	dt := last.at.Sub(first.at).Seconds()
	if dt <= 0 {
		return 0
	}
	// Closing = absolute distance shrinking. Use |signed| so wrap-flip
	// doesn't poison the rate.
	d0 := abs32(first.signedM)
	d1 := abs32(last.signedM)
	return float32((float64(d0) - float64(d1)) / dt)
}

func abs32(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}

// classifyThreat returns a stable string label for the agent's prompt.
// Thresholds are intentionally coarse — the model uses the label for
// quick reasoning; numeric fields are there for nuance.
func classifyThreat(closingMps float32, samples int) string {
	if samples < 2 {
		return "unknown"
	}
	switch {
	case closingMps >= 8:
		return "closing fast"
	case closingMps >= 1:
		return "closing"
	case closingMps >= -1:
		return "steady"
	default:
		return "opening"
	}
}

func (w *ProximityWatcher) buildNearbyCar(idx uint8, raceState *models.RaceState, hist *proxCarHistory) NearbyCar {
	gp := raceState.GridPositions[idx]
	signed := hist.samples[len(hist.samples)-1].signedM
	dist := abs32(signed)
	rate := closingRateMps(hist)
	eta := float32(-1)
	if rate >= proximityMinClosingMps && dist > 0 {
		eta = dist / rate
	}
	name := ""
	if w.roster != nil {
		name = w.roster.ResolveRadioName(idx)
	}
	return NearbyCar{
		CarIndex:     idx,
		DriverName:   name,
		Position:     gp.Position,
		CurrentLap:   gp.CurrentLap,
		DistanceM:    dist,
		SignedM:      signed,
		ClosingMps:   rate,
		EtaSec:       eta,
		Threat:       classifyThreat(rate, len(hist.samples)),
		PitStatus:    pitStatusLabel(gp.PitStatus),
		DriverStatus: driverStatusLabel(gp.DriverStatus),
		LapDistanceM: gp.LapDistance,
	}
}

// approachCandidate pairs a NearbyCar with its history entry so the
// group-fire logic can apply per-car cooldown / arm state after deciding
// which cars belong in the bundle.
type approachCandidate struct {
	nc   NearbyCar
	hist *proxCarHistory
}

// maybeFireGroup evaluates the eligible-to-fire subset of cars BEHIND the
// player and, when non-empty, emits a SINGLE EventCarApproaching that
// bundles them. The event payload carries the full list (sorted by ETA
// ascending) so the dispatcher LLM can synthesise one radio call covering
// every closer rather than N back-to-back per-car calls.
//
// Per-car cooldown + re-arm bookkeeping still runs per car so the latch
// state isn't shared across drivers. Skips the car at P+1 —
// closing_threat.go owns that slot with a tighter gap-trend rule and a P4
// EventThreatOvertake.
func (w *ProximityWatcher) maybeFireGroup(now time.Time, state *models.RaceState, behind []approachCandidate) {
	group := make([]approachCandidate, 0, len(behind))
	for _, c := range behind {
		if state != nil && state.Position > 0 && c.nc.Position == state.Position+1 {
			// closing_threat.go owns the P+1 slot. Keep re-arm running so we
			// fire promptly if the car drops out of P+1 and is still closing.
			if c.nc.DistanceM > proximityRearmM {
				c.hist.armed = true
			}
			continue
		}
		if c.nc.ClosingMps < proximityMinClosingMps {
			// Not actively closing — re-arm if they've dropped back.
			if c.nc.DistanceM > proximityRearmM {
				c.hist.armed = true
			}
			continue
		}
		if c.nc.DistanceM < proximityMinFireGap {
			continue // already on top of us — race-mode threat rules cover this
		}
		if c.nc.EtaSec < 0 || c.nc.EtaSec > proximityFireTTISec {
			continue // not arriving in the danger window
		}
		// Per-car cooldown.
		if !c.hist.lastFireAt.IsZero() && now.Sub(c.hist.lastFireAt) < proximityRefireSec*time.Second {
			continue
		}
		// First encounter latches armed so the cooldown-only logic above
		// handles refires after the rearm distance is crossed.
		if !c.hist.armed && c.hist.lastFireAt.IsZero() {
			c.hist.armed = true
		}
		if !c.hist.armed {
			continue
		}
		group = append(group, c)
	}
	if len(group) == 0 {
		return
	}
	// Sort by ETA ascending — closest threat first in the bundle.
	sort.Slice(group, func(i, j int) bool { return group[i].nc.EtaSec < group[j].nc.EtaSec })

	// Mark per-car cooldown for ALL bundled cars before submitting to the
	// bus. This matches the previous semantics: even if the bus rejects
	// (subject dedup, TalkLevel), we still want to suppress immediate
	// refire on the same group next tick — the rule will rearm via the
	// rearm-distance latch.
	for _, c := range group {
		c.hist.armed = false
		c.hist.lastFireAt = now
	}

	ev := buildApproachingEvent(state, group)
	talkLevel := int32(5)
	if w.talkLevel != nil {
		talkLevel = w.talkLevel()
	}
	if _, err := w.bus.EnqueueEvent(ev, talkLevel); err != nil {
		if errors.Is(err, brain.ErrEventDeduped) || errors.Is(err, brain.ErrEventTalkLevel) {
			log.Debug().Str("type", string(ev.Type)).Int("count", len(group)).Err(err).Msg("Proximity group event dropped")
			return
		}
		log.Warn().Err(err).Str("type", string(ev.Type)).Int("count", len(group)).Msg("Proximity group event rejected")
		return
	}
	lead := group[0].nc
	log.Info().
		Int("count", len(group)).
		Uint8("lead_idx", lead.CarIndex).
		Float32("lead_dist_m", lead.DistanceM).
		Float32("lead_eta_sec", lead.EtaSec).
		Msg("Proximity → car(s) approaching from behind")
}

// buildApproachingEvent constructs a brain.Event for the given non-empty
// group of behind-car candidates. The lead (closest ETA) car drives the
// dedup keys and the legacy single-car DebugData fields; every car in the
// group is also enumerated under DebugData["behind_cars"] so the
// dispatcher LLM can address them individually.
//
// Caller guarantees len(group) >= 1 and that group is sorted by ETA asc.
func buildApproachingEvent(state *models.RaceState, group []approachCandidate) brain.Event {
	lead := group[0].nc
	label := lead.DriverName
	if label == "" {
		label = fmt.Sprintf("Car #%d", lead.CarIndex)
	}
	n := len(group)

	var summary, reasoning string
	if n == 1 {
		summary = fmt.Sprintf("%s closing — %.0fm behind, ~%.1fs to contact.", label, lead.DistanceM, lead.EtaSec)
		reasoning = fmt.Sprintf(
			"%s (P%d, lap %d) closing at %.1f m/s. Distance %.0fm; estimated time to crossing %.1fs at current rate. Player on %s status, %d km/h.",
			label, lead.Position, lead.CurrentLap,
			lead.ClosingMps, lead.DistanceM, lead.EtaSec,
			driverStatusLabel(state.DriverStatus), state.Speed,
		)
	} else {
		parts := make([]string, 0, n)
		for _, c := range group {
			l := c.nc.DriverName
			if l == "" {
				l = fmt.Sprintf("#%d", c.nc.CarIndex)
			}
			parts = append(parts, fmt.Sprintf("%s ~%.1fs", l, c.nc.EtaSec))
		}
		summary = fmt.Sprintf("%d cars closing — %s.", n, strings.Join(parts, ", "))
		if len(summary) > brain.MaxEventSummaryChars {
			// Fall back to a denser form that always fits.
			summary = fmt.Sprintf("%d cars closing — lead %s ~%.1fs.", n, label, lead.EtaSec)
			if len(summary) > brain.MaxEventSummaryChars {
				summary = fmt.Sprintf("%d cars closing behind.", n)
			}
		}
		rparts := make([]string, 0, n)
		for _, c := range group {
			l := c.nc.DriverName
			if l == "" {
				l = fmt.Sprintf("#%d", c.nc.CarIndex)
			}
			rparts = append(rparts, fmt.Sprintf("%s P%d %.0fm %.1fm/s ETA %.1fs", l, c.nc.Position, c.nc.DistanceM, c.nc.ClosingMps, c.nc.EtaSec))
		}
		reasoning = fmt.Sprintf("%d behind cars closing. ", n) + strings.Join(rparts, "; ")
		if len(reasoning) > brain.MaxEventReasoningChars {
			reasoning = reasoning[:brain.MaxEventReasoningChars]
		}
	}

	behindCars := make([]map[string]any, 0, n)
	for _, c := range group {
		behindCars = append(behindCars, map[string]any{
			"car_index":   int(c.nc.CarIndex),
			"driver":      c.nc.DriverName,
			"position":    int(c.nc.Position),
			"current_lap": int(c.nc.CurrentLap),
			"distance_m":  c.nc.DistanceM,
			"closing_mps": c.nc.ClosingMps,
			"eta_sec":     c.nc.EtaSec,
		})
	}

	return brain.Event{
		Type:     brain.EventCarApproaching,
		Priority: brain.DefaultEventPriority(brain.EventCarApproaching),
		Summary:  summary,
		Reasoning: reasoning,
		DebugData: map[string]any{
			// Legacy single-car fields — refer to the closest (lead) car.
			"behind_car_index": int(lead.CarIndex),
			"behind_driver":    lead.DriverName,
			"behind_position":  int(lead.Position),
			"distance_m":       lead.DistanceM,
			"closing_mps":      lead.ClosingMps,
			"eta_sec":          lead.EtaSec,
			// Group payload.
			"behind_count": n,
			"behind_cars":  behindCars,
			// Common.
			"session_type": int(state.SessionType),
			"speed_kmh":    int(state.Speed),
		},
		Source:       "rule:car_approaching",
		DedupKey:     fmt.Sprintf("car_approaching:%d:%d", lead.CarIndex, state.CurrentLap),
		DedupSubject: fmt.Sprintf("car:%d", lead.CarIndex),
	}
}

func (w *ProximityWatcher) publishEmpty(state *models.RaceState) {
	snap := &ProximitySnapshot{
		GeneratedAt:  time.Now(),
		WatchRadiusM: proximityWatchM,
		NearbyAhead:  []NearbyCar{},
		NearbyBehind: []NearbyCar{},
	}
	if state != nil {
		snap.PlayerCarIndex = state.PlayerCarIndex
		snap.PlayerLapDistance = state.LapDistance
		snap.PlayerSpeedKmh = state.Speed
	}
	w.snapshot.Store(snap)
}

func pitStatusLabel(v uint8) string {
	switch v {
	case 0:
		return "on track"
	case 1:
		return "pit lane"
	case 2:
		return "in pit"
	default:
		return "unknown"
	}
}

func driverStatusLabel(v uint8) string {
	switch v {
	case 0:
		return "in garage"
	case 1:
		return "flying lap"
	case 2:
		return "in lap"
	case 3:
		return "out lap"
	case 4:
		return "on track"
	default:
		return "unknown"
	}
}

// nearestBehindOnTrack — kept for backwards compat with closing_threat
// helpers and existing tests. Returns the closest car behind by signed
// distance, ignoring pit/garage cars.
func nearestBehindOnTrack(state *models.RaceState) (uint8, float32) {
	lapLen := float32(state.TrackLength)
	if lapLen <= 0 {
		return 255, 0
	}
	half := lapLen / 2
	me := state.GridPositions[state.PlayerCarIndex]
	bestIdx := uint8(255)
	bestDist := float32(1e9)
	for i := 0; i < 22; i++ {
		idx := uint8(i)
		if idx == state.PlayerCarIndex {
			continue
		}
		gp := state.GridPositions[i]
		if gp.ResultStatus < 2 || gp.PitStatus != 0 || gp.DriverStatus == 0 {
			continue
		}
		d := signedTrackDelta(me.LapDistance, gp.LapDistance, lapLen, half)
		if d >= 0 {
			continue
		}
		absD := -d
		if absD < bestDist {
			bestDist = absD
			bestIdx = idx
		}
	}
	return bestIdx, bestDist
}

// isRaceSession — F1 25 SessionType ∈ {10, 11, 12} = race types.
func isRaceSession(t uint8) bool {
	return t >= 10 && t <= 12
}
