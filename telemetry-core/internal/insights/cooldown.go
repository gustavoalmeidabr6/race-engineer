package insights

import (
	"sync"
	"time"
)

// CooldownManager tracks per-rule cooldown state and debounce timers.
// Every field is guarded by mu so the engine goroutine and (future) reset
// callers stay race-free.
type CooldownManager struct {
	mu sync.Mutex

	// --- Per-lap flags (reset on each new lap) ---
	lastLap            uint8
	tireWarnIssued     bool
	fuelWarnIssued     bool // < 5 laps
	fuelCriticalIssued bool // < 3 laps
	ersLowWarned       bool
	brakeTempWarned    bool
	tireTempWarned     bool
	pitEntryWarned     bool

	// --- Session-scoped flags (reset on condition clear) ---
	rainWarned        bool
	safetyCarNotified bool
	damageWarned      map[string]bool // component name -> warned

	// --- Position tracking ---
	lastPosition uint8
	overtakeLap  uint8 // one notification per lap per direction

	// --- Braking zone debounce ---
	brakingInZone        bool
	brakingWarnedThisZone bool

	// --- Rate limiters ---
	lastCarStatusTime     time.Time // for fuel + ERS (1 Hz)
	lastPositionCheckTime time.Time // for position changes (1 Hz)
	lastCarTelemetryTime  time.Time // for brake/tire temp (0.5 Hz)

	// --- Event dedup ---
	lastEventCode string

	// --- Interrupts Bus producer state ---
	vscNotified           bool
	pitWindowOpenNotified bool
	weatherChangeNotified bool
	fastestLapNotified    bool

	// --- Race lifecycle latches (race_lifecycle.go) ---
	// One-shot fires per race. ResetForNewSession (called when SessionUID
	// changes) clears them so a fresh race re-arms each event.
	lightsOutNotified   bool
	finalLapNotified    bool
	raceFinishNotified  bool
	lastSessionUID      uint64 // tracked so ResetForNewSession can detect a fresh session

	// --- Setup-advice cooldown (setup_advice.go) ---
	// Keyed by `<topic>:<target_value>` so a fresh recommendation re-fires
	// the moment the suggested value changes. Stale entries linger but cost
	// nothing — the topic list is tiny (≤6 channels × handful of values).
	setupAdviceLast map[string]time.Time
}

// NewCooldownManager returns an initialised CooldownManager.
func NewCooldownManager() *CooldownManager {
	return &CooldownManager{
		damageWarned:    make(map[string]bool),
		setupAdviceLast: make(map[string]time.Time),
	}
}

// setupAdviceCooldown is how long any single (topic, target) recommendation
// is muted after firing. 90s keeps a stable suggestion from spamming the
// dashboard while still letting the rule re-fire if the target value moves.
const setupAdviceCooldown = 90 * time.Second

// ShouldSetupAdvise returns true if (topic, target) hasn't fired in the
// last setupAdviceCooldown window, AND records the new fire time. The
// keyed-on-target design means a "BB 54" recommendation re-fires the
// moment the rule recomputes to "BB 53".
func (c *CooldownManager) ShouldSetupAdvise(topic string, target int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.setupAdviceLast == nil {
		c.setupAdviceLast = make(map[string]time.Time)
	}
	key := topicTargetKey(topic, target)
	last, ok := c.setupAdviceLast[key]
	if ok && time.Since(last) < setupAdviceCooldown {
		return false
	}
	c.setupAdviceLast[key] = time.Now()
	return true
}

func topicTargetKey(topic string, target int) string {
	// Avoid fmt to keep this hot-path allocation-free in the common case.
	// 32 bytes is plenty for topic + small int.
	b := make([]byte, 0, 32)
	b = append(b, topic...)
	b = append(b, ':')
	// Manual int → ascii to skip fmt allocations.
	if target < 0 {
		b = append(b, '-')
		target = -target
	}
	if target == 0 {
		b = append(b, '0')
	} else {
		var digits [10]byte
		n := 0
		for target > 0 {
			digits[n] = byte('0' + target%10)
			target /= 10
			n++
		}
		for i := n - 1; i >= 0; i-- {
			b = append(b, digits[i])
		}
	}
	return string(b)
}

// ResetForNewLap clears every flag that should reset at the start of a new lap.
func (c *CooldownManager) ResetForNewLap() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.tireWarnIssued = false
	c.brakeTempWarned = false
	c.tireTempWarned = false
	c.ersLowWarned = false
	c.pitEntryWarned = false
}

// ShouldRateLimit returns true if the caller should skip this evaluation round.
//
// Supported kinds:
//
//	"car_status"    - 1 Hz (fuel, ERS)
//	"position"      - 1 Hz
//	"car_telemetry" - 0.5 Hz (brake/tire temps)
func (c *CooldownManager) ShouldRateLimit(kind string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()

	switch kind {
	case "car_status":
		if now.Sub(c.lastCarStatusTime) < 1*time.Second {
			return true
		}
		c.lastCarStatusTime = now
	case "position":
		if now.Sub(c.lastPositionCheckTime) < 1*time.Second {
			return true
		}
		c.lastPositionCheckTime = now
	case "car_telemetry":
		if now.Sub(c.lastCarTelemetryTime) < 2*time.Second {
			return true
		}
		c.lastCarTelemetryTime = now
	}
	return false
}

// ---------- Lap ----------

func (c *CooldownManager) LastLap() uint8 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastLap
}

func (c *CooldownManager) SetLastLap(lap uint8) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastLap = lap
}

// ---------- Tire Wear ----------

func (c *CooldownManager) TireWarnIssued() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tireWarnIssued
}

func (c *CooldownManager) SetTireWarnIssued(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tireWarnIssued = v
}

// ---------- Fuel ----------

func (c *CooldownManager) FuelWarnIssued() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fuelWarnIssued
}

func (c *CooldownManager) SetFuelWarnIssued(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fuelWarnIssued = v
}

func (c *CooldownManager) FuelCriticalIssued() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fuelCriticalIssued
}

func (c *CooldownManager) SetFuelCriticalIssued(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fuelCriticalIssued = v
}

// ---------- ERS ----------

func (c *CooldownManager) ERSLowWarned() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ersLowWarned
}

func (c *CooldownManager) SetERSLowWarned(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ersLowWarned = v
}

// ---------- Rain ----------

func (c *CooldownManager) RainWarned() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rainWarned
}

func (c *CooldownManager) SetRainWarned(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rainWarned = v
}

// ---------- Safety Car ----------

func (c *CooldownManager) SafetyCarNotified() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.safetyCarNotified
}

func (c *CooldownManager) SetSafetyCarNotified(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.safetyCarNotified = v
}

// ---------- Damage ----------

func (c *CooldownManager) DamageWarned(component string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.damageWarned[component]
}

func (c *CooldownManager) SetDamageWarned(component string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.damageWarned[component] = true
}

// ---------- Brake Temp ----------

func (c *CooldownManager) BrakeTempWarned() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.brakeTempWarned
}

func (c *CooldownManager) SetBrakeTempWarned(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.brakeTempWarned = v
}

// ---------- Tire Temp ----------

func (c *CooldownManager) TireTempWarned() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tireTempWarned
}

func (c *CooldownManager) SetTireTempWarned(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tireTempWarned = v
}

// ---------- Position ----------

func (c *CooldownManager) LastPosition() uint8 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastPosition
}

func (c *CooldownManager) SetLastPosition(p uint8) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastPosition = p
}

func (c *CooldownManager) OvertakeLap() uint8 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.overtakeLap
}

func (c *CooldownManager) SetOvertakeLap(lap uint8) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.overtakeLap = lap
}

// ---------- Braking Zone ----------

func (c *CooldownManager) BrakingInZone() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.brakingInZone
}

func (c *CooldownManager) SetBrakingInZone(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.brakingInZone = v
}

func (c *CooldownManager) BrakingWarnedThisZone() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.brakingWarnedThisZone
}

func (c *CooldownManager) SetBrakingWarnedThisZone(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.brakingWarnedThisZone = v
}

// ---------- Event Dedup ----------

func (c *CooldownManager) LastEventCode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastEventCode
}

func (c *CooldownManager) SetLastEventCode(code string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastEventCode = code
}

// ---------- VSC ----------

func (c *CooldownManager) VSCNotified() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.vscNotified
}

func (c *CooldownManager) SetVSCNotified(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.vscNotified = v
}

// ---------- Pit Window Open ----------

func (c *CooldownManager) PitWindowOpenNotified() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pitWindowOpenNotified
}

func (c *CooldownManager) SetPitWindowOpenNotified(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pitWindowOpenNotified = v
}

// ---------- Weather Change ----------

func (c *CooldownManager) WeatherChangeNotified() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.weatherChangeNotified
}

func (c *CooldownManager) SetWeatherChangeNotified(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.weatherChangeNotified = v
}

// ---------- Fastest Lap ----------

func (c *CooldownManager) FastestLapNotified() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fastestLapNotified
}

func (c *CooldownManager) SetFastestLapNotified(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fastestLapNotified = v
}

// ---------- Race Lifecycle ----------

func (c *CooldownManager) LightsOutNotified() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lightsOutNotified
}

func (c *CooldownManager) SetLightsOutNotified(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lightsOutNotified = v
}

func (c *CooldownManager) FinalLapNotified() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.finalLapNotified
}

func (c *CooldownManager) SetFinalLapNotified(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finalLapNotified = v
}

func (c *CooldownManager) RaceFinishNotified() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.raceFinishNotified
}

func (c *CooldownManager) SetRaceFinishNotified(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.raceFinishNotified = v
}

// ResetForNewSession clears every one-shot race-lifecycle latch when a new
// F1 session is detected. Returns true if the uid changed (caller may want
// to log the rollover). Idempotent — repeated calls with the same uid are
// cheap and do nothing.
func (c *CooldownManager) ResetForNewSession(uid uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if uid == 0 || uid == c.lastSessionUID {
		return false
	}
	c.lastSessionUID = uid
	c.lightsOutNotified = false
	c.finalLapNotified = false
	c.raceFinishNotified = false
	// Anything else that should reset on a new session goes here. The
	// per-condition latches (safety car, weather, fastest lap) already
	// reset when their underlying state clears so they're left alone.
	return true
}

// ---------- Pit Entry ----------

func (c *CooldownManager) PitEntryWarned() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pitEntryWarned
}

func (c *CooldownManager) SetPitEntryWarned(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pitEntryWarned = v
}
