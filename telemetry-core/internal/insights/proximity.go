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

// proximity.go — unified traffic awareness + heads-up event firing.
//
// Owns two responsibilities:
//
//   1. Maintain a per-car sample history for every car within
//      proximityWatchM metres of the player on track. Compute closing
//      rates AND each car's own track speed from those samples. Publish
//      a ProximitySnapshot atomically on every tick — that snapshot
//      powers the /api/state/proximity tool.
//
//   2. Fire EventTrafficUpdate (P2) through the brain bus when ANY car
//      ahead OR behind has a time-to-impact (distance / closing_rate)
//      below proximityFireTTISec. One bundled event carries the top 5
//      cars on each side, so the Live agent can phrase a single
//      combined radio call ("two closing behind, slow car ahead") and
//      the driver hears one heads-up instead of two back-to-back.
//
// Runs at 1 s — fast enough to catch hot-lap traffic at ~40 m/s
// closure, slow enough that map updates aren't a CPU cost.

const (
	// Snapshot watch range: cars within ±this on track go into the
	// per-car history and the published snapshot. Wider than the firing
	// threshold so the agent's tool can see further ahead.
	proximityWatchM = 2000.0

	// Firing threshold (event-only): fire EventTrafficUpdate when any
	// tracked car's TTI drops below this in either direction.
	proximityFireTTISec = 5.0

	// Don't fire if they're effectively on top of us — race-mode threat
	// rules cover that.
	proximityMinFireGap = 15.0

	// Re-arm distance: the watcher relatches the traffic event only once
	// every closing car (ahead or behind) has opened beyond this. Keeps
	// us from refiring while the same drivers sit close.
	proximityRearmM = 350.0

	// Closing rate must exceed this to be considered "actively closing".
	// 1 m/s = 3.6 km/h relative — anything below is noise.
	proximityMinClosingMps = 1.0

	// Per-car sample history. 12 × 1 s = 12 s of history.
	proximitySamplesCap = 12

	// Watcher-level cooldown: minimum gap between two traffic_update
	// fires. Avoids spam while the same traffic pattern lingers; a
	// genuinely new situation can still refire after this elapses.
	proximityRefireSec = 25

	// Top-N cars per side to include in the published event payload.
	// Five gives the LLM enough spatial context to say "three closing
	// behind, two slow cars two corners ahead" without blowing the
	// 1KB DebugData budget.
	proximityEventTopN = 5

	// Cars older than this in the history map (no new samples) are dropped.
	proximityStaleSec = 5
)

// proxSample is one (timestamp, signed-on-track-distance, total-distance)
// reading. `signedM` is positive when the car is ahead of the player,
// negative when behind. `totalM` is the car's monotonically-increasing
// race-distance (carries lap rollovers cleanly) so we can derive each
// car's own track speed from the sample window.
type proxSample struct {
	at      time.Time
	signedM float32
	totalM  float32
}

// proxCarHistory is the rolling-sample buffer kept for each car within
// the watch range. Samples are appended on every tick; entries that
// haven't been refreshed in proximityStaleSec are dropped from the map.
type proxCarHistory struct {
	samples  []proxSample
	lastSeen time.Time
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
	SpeedKmh     uint16  `json:"speed_kmh"`      // derived from Δ total_distance / Δt across the sample window
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
// distance histories, fires EventTrafficUpdate when any tracked car's
// TTI < threshold in either direction, and publishes a
// ProximitySnapshot atomically for the API tool.
type ProximityWatcher struct {
	state     func() *models.RaceState
	bus       *brain.RaceBrain
	talkLevel func() int32
	interval  time.Duration
	roster    *state.Roster // optional — when set, snapshot/event reasoning uses driver surnames

	// cars tracks per-car sample history. Lives only on the watcher
	// goroutine — accessed in tick() only — so no mutex needed.
	cars map[uint8]*proxCarHistory

	// Single watcher-level latch for the unified traffic_update event.
	// trafficArmed flips to false when we fire; flips back to true when
	// every previously-closing car has opened beyond proximityRearmM.
	// trafficLastFireAt enforces the minimum gap between fires.
	trafficArmed      bool
	trafficLastFireAt time.Time

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
		state:        stateLoader,
		bus:          bus,
		talkLevel:    talkLevel,
		interval:     interval,
		cars:         make(map[uint8]*proxCarHistory, 22),
		trafficArmed: true,
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

// LookupHistory returns the watcher's tracked motion fields
// (closing_mps, eta_sec, threat label, sample count) for a given car
// index by scanning the latest published snapshot. Returns ok=false
// when the car isn't in the watcher's tracked window. Callers outside
// the watcher goroutine MUST use this rather than touching the cars
// map directly — the snapshot is the only thread-safe view.
//
// Why scan the snapshot lists instead of exposing the per-car history?
// The snapshot pre-computes closing/eta/threat for every tracked car,
// so distant-car enrichment in /api/state/nearby_cars is a single
// snapshot.Load() + linear scan over ≤44 entries — no locking, no
// duplication of the closing-rate maths.
func (w *ProximityWatcher) LookupHistory(idx uint8) (closingMps, etaSec float32, threat string, samples int, ok bool) {
	if w == nil {
		return 0, -1, "unknown", 0, false
	}
	snap := w.snapshot.Load()
	if snap == nil {
		return 0, -1, "unknown", 0, false
	}
	for i := range snap.NearbyAhead {
		c := &snap.NearbyAhead[i]
		if c.CarIndex == idx {
			return c.ClosingMps, c.EtaSec, c.Threat, sampleCountForThreat(c.Threat), true
		}
	}
	for i := range snap.NearbyBehind {
		c := &snap.NearbyBehind[i]
		if c.CarIndex == idx {
			return c.ClosingMps, c.EtaSec, c.Threat, sampleCountForThreat(c.Threat), true
		}
	}
	return 0, -1, "unknown", 0, false
}

// sampleCountForThreat returns a minimum-sample hint derived from the
// pre-computed Threat label. classifyThreat returns "unknown" for <2
// samples; everything else implies ≥2. Useful for callers that want
// to know whether the watcher had enough data to trust the closing
// rate without re-implementing the threshold check.
func sampleCountForThreat(threat string) int {
	if threat == "" || threat == "unknown" {
		return 0
	}
	return 2
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
		hist.samples = append(hist.samples, proxSample{at: now, signedM: signed, totalM: gp.TotalDistance})
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
	for idx := range w.cars {
		if !seen[idx] {
			continue // stale, will be dropped next tick
		}
		nc := w.buildNearbyCar(idx, state, w.cars[idx])
		if nc.SignedM >= 0 {
			ahead = append(ahead, nc)
		} else {
			behind = append(behind, nc)
		}
	}
	sort.Slice(ahead, func(i, j int) bool { return ahead[i].DistanceM < ahead[j].DistanceM })
	sort.Slice(behind, func(i, j int) bool { return behind[i].DistanceM < behind[j].DistanceM })

	// One coalesced fire per tick covering BOTH directions: the LLM gets
	// full spatial context (top-N ahead + top-N behind with closing rates
	// and per-car speeds) so it can phrase a single combined radio call
	// instead of two back-to-back ones.
	w.maybeFireTrafficUpdate(now, state, ahead, behind)

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

// SignedTrackDelta is the exported alias of signedTrackDelta for the API
// layer that needs to compute track-aware distances without importing
// the private helper. Behaviour is identical.
func SignedTrackDelta(from, to, lapLen, half float32) float32 {
	return signedTrackDelta(from, to, lapLen, half)
}

// PitStatusLabel / DriverStatusLabel expose the local label maps so
// handlers in other packages can produce snapshot entries with the same
// human-readable strings the proximity snapshot uses.
func PitStatusLabel(v uint8) string    { return pitStatusLabel(v) }
func DriverStatusLabel(v uint8) string { return driverStatusLabel(v) }

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
		SpeedKmh:     carSpeedKmh(hist),
		PitStatus:    pitStatusLabel(gp.PitStatus),
		DriverStatus: driverStatusLabel(gp.DriverStatus),
		LapDistanceM: gp.LapDistance,
	}
}

// carSpeedKmh derives a tracked car's own track speed from the
// total-distance samples. Returns 0 when fewer than 2 samples exist or
// when the rate would overflow uint16 (sentinel for "unknown").
func carSpeedKmh(hist *proxCarHistory) uint16 {
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
	mps := float64(last.totalM-first.totalM) / dt
	if mps <= 0 {
		return 0
	}
	kmh := mps * 3.6
	if kmh > 400 {
		return 400
	}
	return uint16(kmh + 0.5)
}

// maybeFireTrafficUpdate is the unified replacement for the old
// per-direction fire helpers. It walks the snapshot lists (already
// sorted by absolute distance ascending), picks out cars whose TTI is
// inside proximityFireTTISec in either direction, and — when at least
// one car qualifies — emits a single EventTrafficUpdate carrying the
// top-N closest cars on each side. The Live agent reads the payload
// once and phrases a combined call, so the driver hears one heads-up
// even when traffic is closing on both sides simultaneously.
//
// Latch semantics: a single watcher-level latch (`trafficArmed`) +
// minimum interval (`trafficLastFireAt`). The latch flips back on once
// every previously-closing car has opened beyond proximityRearmM — that
// keeps us from refiring while the same drivers are still hovering
// close, but lets a genuinely new traffic situation light up promptly.
// Skips P+1 cars behind because closing_threat.go owns that slot.
func (w *ProximityWatcher) maybeFireTrafficUpdate(now time.Time, state *models.RaceState, ahead, behind []NearbyCar) {
	cooldownActive := !w.trafficLastFireAt.IsZero() && now.Sub(w.trafficLastFireAt) < proximityRefireSec*time.Second

	// Collect firing-window cars on each side (TTI inside threshold,
	// distance inside fire band, actively closing). Behind-side skips
	// P+1 — closing_threat.go owns that slot with a tighter rule.
	firingBehind := make([]NearbyCar, 0, len(behind))
	for _, c := range behind {
		if state != nil && state.Position > 0 && c.Position == state.Position+1 {
			continue
		}
		if c.ClosingMps < proximityMinClosingMps {
			continue
		}
		if c.DistanceM < proximityMinFireGap {
			continue
		}
		if c.EtaSec < 0 || c.EtaSec > proximityFireTTISec {
			continue
		}
		firingBehind = append(firingBehind, c)
	}
	firingAhead := make([]NearbyCar, 0, len(ahead))
	for _, c := range ahead {
		if c.ClosingMps < proximityMinClosingMps {
			continue
		}
		if c.DistanceM < proximityMinFireGap {
			continue
		}
		if c.EtaSec < 0 || c.EtaSec > proximityFireTTISec {
			continue
		}
		firingAhead = append(firingAhead, c)
	}

	// Re-arm: if nothing's inside the firing window AND no actively
	// closing car is inside the rearm band, flip the latch back on.
	if len(firingBehind) == 0 && len(firingAhead) == 0 {
		anyHot := false
		for _, c := range behind {
			if c.ClosingMps >= proximityMinClosingMps && c.DistanceM <= proximityRearmM {
				anyHot = true
				break
			}
		}
		if !anyHot {
			for _, c := range ahead {
				if c.ClosingMps >= proximityMinClosingMps && c.DistanceM <= proximityRearmM {
					anyHot = true
					break
				}
			}
		}
		if !anyHot {
			w.trafficArmed = true
		}
		return
	}

	if cooldownActive || !w.trafficArmed {
		return
	}

	ev := buildTrafficUpdateEvent(state, ahead, behind, firingAhead, firingBehind)
	talkLevel := int32(5)
	if w.talkLevel != nil {
		talkLevel = w.talkLevel()
	}
	w.trafficArmed = false
	w.trafficLastFireAt = now
	if _, err := w.bus.EnqueueEvent(ev, talkLevel); err != nil {
		if errors.Is(err, brain.ErrEventDeduped) || errors.Is(err, brain.ErrEventTalkLevel) {
			log.Debug().Str("type", string(ev.Type)).Int("ahead", len(firingAhead)).Int("behind", len(firingBehind)).Err(err).Msg("Traffic update dropped")
			return
		}
		log.Warn().Err(err).Str("type", string(ev.Type)).Int("ahead", len(firingAhead)).Int("behind", len(firingBehind)).Msg("Traffic update rejected")
		return
	}
	log.Info().
		Int("firing_behind", len(firingBehind)).
		Int("firing_ahead", len(firingAhead)).
		Int("snapshot_behind", len(behind)).
		Int("snapshot_ahead", len(ahead)).
		Msg("Proximity → traffic_update fired")
}

// buildTrafficUpdateEvent assembles the EventTrafficUpdate payload. The
// debug_data carries up to proximityEventTopN cars on each side
// regardless of whether they're firing — full spatial context lets the
// Live agent describe the whole picture ("2 closing behind, 3 slower
// cars ahead at T7"). Firing-window counts are exposed separately so
// the model knows which subset triggered the call.
//
// `ahead` / `behind` are the FULL snapshot lists (sorted by |distance|
// ascending). `firingAhead` / `firingBehind` are the subset whose TTI
// is inside proximityFireTTISec; len(firing*) > 0 across the two
// guaranteed by the caller.
func buildTrafficUpdateEvent(state *models.RaceState, ahead, behind, firingAhead, firingBehind []NearbyCar) brain.Event {
	topAhead := ahead
	if len(topAhead) > proximityEventTopN {
		topAhead = topAhead[:proximityEventTopN]
	}
	topBehind := behind
	if len(topBehind) > proximityEventTopN {
		topBehind = topBehind[:proximityEventTopN]
	}

	leadBehindIdx := -1
	if len(firingBehind) > 0 {
		// Closest-by-ETA among firing behind for dedup key.
		min := firingBehind[0]
		for _, c := range firingBehind[1:] {
			if c.EtaSec >= 0 && c.EtaSec < min.EtaSec {
				min = c
			}
		}
		leadBehindIdx = int(min.CarIndex)
	}
	leadAheadIdx := -1
	if len(firingAhead) > 0 {
		min := firingAhead[0]
		for _, c := range firingAhead[1:] {
			if c.EtaSec >= 0 && c.EtaSec < min.EtaSec {
				min = c
			}
		}
		leadAheadIdx = int(min.CarIndex)
	}

	summary := buildTrafficSummary(firingAhead, firingBehind)
	reasoning := buildTrafficReasoning(firingAhead, firingBehind)

	behindCars := make([]map[string]any, 0, len(topBehind))
	for _, c := range topBehind {
		behindCars = append(behindCars, nearbyCarToMap(c))
	}
	aheadCars := make([]map[string]any, 0, len(topAhead))
	for _, c := range topAhead {
		aheadCars = append(aheadCars, nearbyCarToMap(c))
	}

	return brain.Event{
		Type:      brain.EventTrafficUpdate,
		Priority:  brain.DefaultEventPriority(brain.EventTrafficUpdate),
		Summary:   summary,
		Reasoning: reasoning,
		DebugData: map[string]any{
			"behind_count":     len(firingBehind),
			"ahead_count":      len(firingAhead),
			"behind_cars":      behindCars,
			"ahead_cars":       aheadCars,
			"session_type":     int(state.SessionType),
			"player_position":  int(state.Position),
			"player_speed_kmh": int(state.Speed),
		},
		Source:       "rule:traffic_update",
		DedupKey:     fmt.Sprintf("traffic_update:%d:%d:%d", state.CurrentLap, leadBehindIdx, leadAheadIdx),
		DedupSubject: "traffic",
	}
}

// nearbyCarToMap serialises a NearbyCar into the DebugData shape. Keys
// are stable so dashboards / the Live agent can read them positionally.
func nearbyCarToMap(c NearbyCar) map[string]any {
	return map[string]any{
		"car_index":   int(c.CarIndex),
		"driver":      c.DriverName,
		"position":    int(c.Position),
		"current_lap": int(c.CurrentLap),
		"distance_m":  c.DistanceM,
		"signed_m":    c.SignedM,
		"closing_mps": c.ClosingMps,
		"eta_sec":     c.EtaSec,
		"threat":      c.Threat,
		"speed_kmh":   int(c.SpeedKmh),
	}
}

// buildTrafficSummary phrases the ≤80-char headline. Producers send
// FACTS, not radio sentences — the Live agent rewrites this — but a
// dense human-readable form keeps logs and dashboards usable.
func buildTrafficSummary(firingAhead, firingBehind []NearbyCar) string {
	nb := len(firingBehind)
	na := len(firingAhead)
	var leadBehind, leadAhead *NearbyCar
	if nb > 0 {
		l := firingBehind[0]
		for i := range firingBehind[1:] {
			if firingBehind[1+i].EtaSec >= 0 && firingBehind[1+i].EtaSec < l.EtaSec {
				l = firingBehind[1+i]
			}
		}
		leadBehind = &l
	}
	if na > 0 {
		l := firingAhead[0]
		for i := range firingAhead[1:] {
			if firingAhead[1+i].EtaSec >= 0 && firingAhead[1+i].EtaSec < l.EtaSec {
				l = firingAhead[1+i]
			}
		}
		leadAhead = &l
	}

	switch {
	case nb > 0 && na > 0:
		summary := fmt.Sprintf("%d closing behind, %d ahead — lead behind ~%.1fs.", nb, na, leadBehind.EtaSec)
		if len(summary) > brain.MaxEventSummaryChars {
			summary = fmt.Sprintf("%dB %dA closing — lead ~%.1fs.", nb, na, leadBehind.EtaSec)
		}
		return summary
	case nb > 0:
		if nb == 1 {
			label := leadBehind.DriverName
			if label == "" {
				label = fmt.Sprintf("#%d", leadBehind.CarIndex)
			}
			s := fmt.Sprintf("%s closing — %.0fm behind, ~%.1fs.", label, leadBehind.DistanceM, leadBehind.EtaSec)
			if len(s) > brain.MaxEventSummaryChars {
				s = fmt.Sprintf("Car behind closing ~%.1fs.", leadBehind.EtaSec)
			}
			return s
		}
		return fmt.Sprintf("%d closing behind — lead ~%.1fs.", nb, leadBehind.EtaSec)
	default:
		if na == 1 {
			label := leadAhead.DriverName
			if label == "" {
				label = fmt.Sprintf("#%d", leadAhead.CarIndex)
			}
			s := fmt.Sprintf("Catching %s — %.0fm ahead, ~%.1fs.", label, leadAhead.DistanceM, leadAhead.EtaSec)
			if len(s) > brain.MaxEventSummaryChars {
				s = fmt.Sprintf("Catching car ahead ~%.1fs.", leadAhead.EtaSec)
			}
			return s
		}
		return fmt.Sprintf("Catching %d ahead — lead ~%.1fs.", na, leadAhead.EtaSec)
	}
}

// buildTrafficReasoning produces a dense fact line (≤200 chars) for the
// LLM prompt. Lists firing cars only; the full top-N lists live in
// DebugData where the model can read them structured.
func buildTrafficReasoning(firingAhead, firingBehind []NearbyCar) string {
	parts := make([]string, 0, len(firingAhead)+len(firingBehind))
	for _, c := range firingBehind {
		l := c.DriverName
		if l == "" {
			l = fmt.Sprintf("#%d", c.CarIndex)
		}
		parts = append(parts, fmt.Sprintf("BEHIND %s P%d %.0fm %.1fm/s ETA %.1fs", l, c.Position, c.DistanceM, c.ClosingMps, c.EtaSec))
	}
	for _, c := range firingAhead {
		l := c.DriverName
		if l == "" {
			l = fmt.Sprintf("#%d", c.CarIndex)
		}
		parts = append(parts, fmt.Sprintf("AHEAD %s P%d %.0fm %.1fm/s ETA %.1fs", l, c.Position, c.DistanceM, c.ClosingMps, c.EtaSec))
	}
	r := strings.Join(parts, "; ")
	if len(r) > brain.MaxEventReasoningChars {
		r = r[:brain.MaxEventReasoningChars]
	}
	return r
}

// (Old per-direction fire helpers removed — replaced by maybeFireTrafficUpdate above.)

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
