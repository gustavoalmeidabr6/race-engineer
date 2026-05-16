//go:build darwin

package audio

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// defaultDarwinTargets is the AppleScript-controllable subset we know
// works on macOS. Other apps (browser music, Discord, etc.) have no
// public scripting interface for volume; ducking them would require a
// CoreAudio HAL plugin which is out of scope.
var defaultDarwinTargets = []string{"Spotify", "Music"}

// darwinDucker drives Spotify and Music app via AppleScript. Two modes:
//
//   - ModeDuck:  save the prior `sound volume` on the rising edge and
//                restore it on the falling edge.
//   - ModePause: send `pause` on the rising edge and `play` on the
//                falling edge, but only if the app was actually playing
//                — otherwise restoring `play` later would un-pause a
//                song the user paused themselves.
type darwinDucker struct {
	targets []string
	level   int // target volume on duck, 0-100 (ignored in pause mode)
	mode    Mode

	mu      sync.Mutex
	saved   map[string]int  // ModeDuck: app -> prior sound volume
	paused  map[string]bool // ModePause: apps we paused on this duck
}

// NewOSDucker returns a darwinDucker. Empty targets falls back to the
// default Spotify+Music pair. level is the float fraction (0..1) the
// Coordinator picked up from config; we convert to 0..100 here because
// AppleScript's "sound volume" property is integer percent. mode picks
// duck-volume vs pause-media semantics.
func NewOSDucker(targets []string, level float64, mode Mode) (Ducker, error) {
	if len(targets) == 0 {
		targets = defaultDarwinTargets
	}
	lvl := int(level * 100)
	if lvl < 0 {
		lvl = 0
	}
	if lvl > 100 {
		lvl = 100
	}
	if _, err := exec.LookPath("osascript"); err != nil {
		return nil, fmt.Errorf("osascript not found in PATH: %w", err)
	}
	log.Info().
		Strs("targets", targets).
		Int("level_pct", lvl).
		Str("mode", modeName(mode)).
		Msg("audio: macOS ducker ready")
	return &darwinDucker{
		targets: append([]string(nil), targets...),
		level:   lvl,
		mode:    mode,
		saved:   map[string]int{},
		paused:  map[string]bool{},
	}, nil
}

func (d *darwinDucker) SetDucked(ducked bool) error {
	for _, app := range d.targets {
		go d.applyOne(app, ducked) // fire-and-forget; osascript is slow (~150 ms cold)
	}
	return nil
}

func (d *darwinDucker) applyOne(app string, ducked bool) {
	if !d.isRunning(app) {
		return
	}
	if d.mode == ModePause {
		d.applyPause(app, ducked)
		return
	}
	d.applyVolume(app, ducked)
}

func (d *darwinDucker) applyVolume(app string, ducked bool) {
	if ducked {
		cur, err := d.readVolume(app)
		if err != nil {
			log.Debug().Err(err).Str("app", app).Msg("audio darwin: read volume failed; skipping duck")
			return
		}
		d.mu.Lock()
		d.saved[app] = cur
		d.mu.Unlock()
		if err := d.writeVolume(app, d.level); err != nil {
			log.Debug().Err(err).Str("app", app).Msg("audio darwin: duck failed")
		}
		return
	}
	d.mu.Lock()
	prior, had := d.saved[app]
	delete(d.saved, app)
	d.mu.Unlock()
	if !had {
		return
	}
	if err := d.writeVolume(app, prior); err != nil {
		log.Debug().Err(err).Str("app", app).Int("restore", prior).Msg("audio darwin: restore failed")
	}
}

func (d *darwinDucker) applyPause(app string, ducked bool) {
	if ducked {
		// Only pause if the app is currently playing. Spotify reports
		// "playing" / "paused" / "stopped" via `player state`. If the
		// user already paused it, leave it alone — otherwise unduck
		// would start it playing without their consent.
		state, err := d.playerState(app)
		if err != nil || state != "playing" {
			return
		}
		if err := d.sendCommand(app, "pause"); err != nil {
			log.Debug().Err(err).Str("app", app).Msg("audio darwin: pause failed")
			return
		}
		d.mu.Lock()
		d.paused[app] = true
		d.mu.Unlock()
		return
	}
	d.mu.Lock()
	wasPaused := d.paused[app]
	delete(d.paused, app)
	d.mu.Unlock()
	if !wasPaused {
		return
	}
	if err := d.sendCommand(app, "play"); err != nil {
		log.Debug().Err(err).Str("app", app).Msg("audio darwin: resume failed")
	}
}

func (d *darwinDucker) Close() error {
	// Best-effort restore for anything we touched.
	d.mu.Lock()
	leftoverVol := make(map[string]int, len(d.saved))
	for k, v := range d.saved {
		leftoverVol[k] = v
	}
	leftoverPause := make(map[string]bool, len(d.paused))
	for k, v := range d.paused {
		leftoverPause[k] = v
	}
	d.saved = map[string]int{}
	d.paused = map[string]bool{}
	d.mu.Unlock()
	for app, prior := range leftoverVol {
		_ = d.writeVolume(app, prior)
	}
	for app := range leftoverPause {
		_ = d.sendCommand(app, "play")
	}
	return nil
}

func (d *darwinDucker) isRunning(app string) bool {
	out, err := runOsa(`tell application "System Events" to (name of processes) contains "` + app + `"`)
	if err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(out), "true")
}

func (d *darwinDucker) readVolume(app string) (int, error) {
	out, err := runOsa(`tell application "` + app + `" to get sound volume`)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("parse volume %q: %w", out, err)
	}
	return n, nil
}

func (d *darwinDucker) writeVolume(app string, vol int) error {
	_, err := runOsa(`tell application "` + app + `" to set sound volume to ` + strconv.Itoa(vol))
	return err
}

// playerState returns "playing", "paused", "stopped", or "" on error.
// Spotify and Music both expose the same `player state` property.
func (d *darwinDucker) playerState(app string) (string, error) {
	out, err := runOsa(`tell application "` + app + `" to get player state as string`)
	if err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(out)), nil
}

func (d *darwinDucker) sendCommand(app, command string) error {
	_, err := runOsa(`tell application "` + app + `" to ` + command)
	return err
}

// runOsa invokes osascript with a 2 s ceiling. We don't want a hung
// AppleEvent (rare but possible if the app is stuck) to leak a
// goroutine on every TTS phrase.
func runOsa(script string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "osascript", "-e", script).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
