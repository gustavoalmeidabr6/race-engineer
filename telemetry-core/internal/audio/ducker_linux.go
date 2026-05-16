//go:build linux

package audio

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// defaultLinuxTargets matches `application.name` (case-insensitive
// substring). PipeWire's pactl shim and Pulse both populate this
// property from PA_PROP_APPLICATION_NAME or the .desktop file name.
var defaultLinuxTargets = []string{"spotify", "firefox", "chromium", "chrome", "vlc", "mpv", "rhythmbox", "brave"}

// linuxDucker drives external music players on Linux. Two modes:
//
//   - ModeDuck:  enumerates PulseAudio/PipeWire sink-inputs via pactl
//                and lowers per-stream volume.
//   - ModePause: sends MPRIS pause/play via playerctl. Works for any
//                MPRIS-compliant player (Spotify, Firefox, Chromium,
//                VLC, mpv, Rhythmbox, …) without needing the targets
//                list — playerctl --all-players is enough. Falls back
//                to volume-zero via pactl if playerctl isn't installed.
type linuxDucker struct {
	targets []string
	level   int // duck volume, 0-100 (ignored in pause mode)
	mode    Mode

	// hasPlayerctl is true when playerctl is on PATH at construction.
	// In pause mode, false means we fall back to ModeDuck with level=0
	// — effectively muting players which gives a pause-ish experience
	// without the song-doesn't-progress semantic.
	hasPlayerctl bool

	mu      sync.Mutex
	saved   map[uint32]int  // ModeDuck: sink-input id -> prior volume %
	paused  map[string]bool // ModePause: MPRIS player names we paused
}

func NewOSDucker(targets []string, level float64, mode Mode) (Ducker, error) {
	if len(targets) == 0 {
		targets = defaultLinuxTargets
	}
	lvl := int(level * 100)
	if lvl < 0 {
		lvl = 0
	}
	if lvl > 100 {
		lvl = 100
	}
	if _, err := exec.LookPath("pactl"); err != nil {
		return nil, fmt.Errorf("pactl not found in PATH (install pulseaudio-utils or pipewire-pulse): %w", err)
	}
	low := make([]string, len(targets))
	for i, t := range targets {
		low[i] = strings.ToLower(t)
	}
	_, perr := exec.LookPath("playerctl")
	hasPC := perr == nil
	if mode == ModePause && !hasPC {
		log.Warn().Msg("audio: AUDIO_DUCKING_MODE=pause requested but playerctl not installed; falling back to volume-zero")
	}
	log.Info().
		Strs("targets", low).
		Int("level_pct", lvl).
		Str("mode", modeName(mode)).
		Bool("playerctl", hasPC).
		Msg("audio: linux ducker ready")
	return &linuxDucker{
		targets:      low,
		level:        lvl,
		mode:         mode,
		hasPlayerctl: hasPC,
		saved:        map[uint32]int{},
		paused:       map[string]bool{},
	}, nil
}

func (d *linuxDucker) SetDucked(ducked bool) error {
	go d.apply(ducked)
	return nil
}

func (d *linuxDucker) apply(ducked bool) {
	if d.mode == ModePause && d.hasPlayerctl {
		d.applyPause(ducked)
		return
	}
	d.applyVolume(ducked)
}

// applyPause uses playerctl to send MPRIS Pause/Play. We query the
// status BEFORE pausing so we only resume players that we put into
// the paused state; a player the user already paused stays paused.
func (d *linuxDucker) applyPause(ducked bool) {
	if ducked {
		players := d.listPlayingPlayers()
		for _, p := range players {
			if err := d.playerctl(p, "pause"); err != nil {
				log.Debug().Err(err).Str("player", p).Msg("audio linux: playerctl pause failed")
				continue
			}
			d.mu.Lock()
			d.paused[p] = true
			d.mu.Unlock()
		}
		return
	}
	d.mu.Lock()
	leftover := make([]string, 0, len(d.paused))
	for p := range d.paused {
		leftover = append(leftover, p)
	}
	d.paused = map[string]bool{}
	d.mu.Unlock()
	for _, p := range leftover {
		if err := d.playerctl(p, "play"); err != nil {
			log.Debug().Err(err).Str("player", p).Msg("audio linux: playerctl play failed")
		}
	}
}

// listPlayingPlayers returns MPRIS player names currently in the
// Playing state. We filter by target list when the user has set one;
// empty target list pauses every playing MPRIS source.
func (d *linuxDucker) listPlayingPlayers() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "playerctl", "--list-all").Output()
	if err != nil {
		return nil
	}
	var playing []string
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !d.targetMatches(strings.ToLower(name)) {
			continue
		}
		status, err := d.playerctlStatus(name)
		if err != nil || !strings.EqualFold(status, "Playing") {
			continue
		}
		playing = append(playing, name)
	}
	return playing
}

func (d *linuxDucker) targetMatches(playerLow string) bool {
	if len(d.targets) == 0 {
		return true
	}
	for _, t := range d.targets {
		if strings.Contains(playerLow, t) {
			return true
		}
	}
	return false
}

func (d *linuxDucker) playerctl(player, command string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "playerctl", "--player="+player, command).Run()
}

func (d *linuxDucker) playerctlStatus(player string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "playerctl", "--player="+player, "status").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (d *linuxDucker) applyVolume(ducked bool) {
	if ducked {
		matches, err := d.matchingSinkInputs()
		if err != nil {
			log.Debug().Err(err).Msg("audio linux: enumerate sink-inputs failed")
			return
		}
		for id, vol := range matches {
			d.mu.Lock()
			d.saved[id] = vol
			d.mu.Unlock()
			if err := d.setVolume(id, d.level); err != nil {
				log.Debug().Err(err).Uint32("id", id).Msg("audio linux: duck failed")
			}
		}
		return
	}
	d.mu.Lock()
	leftover := make(map[uint32]int, len(d.saved))
	for k, v := range d.saved {
		leftover[k] = v
	}
	d.saved = map[uint32]int{}
	d.mu.Unlock()
	for id, prior := range leftover {
		if err := d.setVolume(id, prior); err != nil {
			log.Debug().Err(err).Uint32("id", id).Int("restore", prior).Msg("audio linux: restore failed")
		}
	}
}

func (d *linuxDucker) Close() error {
	d.mu.Lock()
	leftoverVol := make(map[uint32]int, len(d.saved))
	for k, v := range d.saved {
		leftoverVol[k] = v
	}
	leftoverPause := make([]string, 0, len(d.paused))
	for p := range d.paused {
		leftoverPause = append(leftoverPause, p)
	}
	d.saved = map[uint32]int{}
	d.paused = map[string]bool{}
	d.mu.Unlock()
	for id, prior := range leftoverVol {
		_ = d.setVolume(id, prior)
	}
	for _, p := range leftoverPause {
		_ = d.playerctl(p, "play")
	}
	return nil
}

func (d *linuxDucker) matchingSinkInputs() (map[uint32]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "pactl", "list", "sink-inputs").Output()
	if err != nil {
		return nil, err
	}
	return parseSinkInputs(out, d.targets), nil
}

// parseSinkInputs walks `pactl list sink-inputs` output and returns
// every matching id with its current left-channel volume percent.
// Volume on Pulse is per-channel; we use the left value, which matches
// what pavucontrol displays for a stereo stream with linked channels.
//
// Example block we parse:
//
//	Sink Input #423
//	    ...
//	    Volume: front-left: 65536 / 100% / 0.00 dB,   front-right: 65536 / 100% / 0.00 dB
//	    ...
//	    application.name = "Spotify"
func parseSinkInputs(raw []byte, targets []string) map[uint32]int {
	out := map[uint32]int{}
	var (
		curID    uint32
		haveID   bool
		curVol   int
		haveVol  bool
		curApp   string
	)
	flush := func() {
		if !haveID || !haveVol || curApp == "" {
			return
		}
		low := strings.ToLower(curApp)
		for _, t := range targets {
			if strings.Contains(low, t) {
				out[curID] = curVol
				break
			}
		}
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "Sink Input #") {
			flush()
			curID, haveID, curVol, haveVol, curApp = 0, false, 0, false, ""
			n, err := strconv.ParseUint(strings.TrimPrefix(line, "Sink Input #"), 10, 32)
			if err == nil {
				curID = uint32(n)
				haveID = true
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Volume:") && !haveVol {
			// Pick the first "/ NN%" we find.
			if i := strings.Index(trimmed, "/ "); i >= 0 {
				rest := trimmed[i+2:]
				if j := strings.Index(rest, "%"); j > 0 {
					if v, err := strconv.Atoi(strings.TrimSpace(rest[:j])); err == nil {
						curVol = v
						haveVol = true
					}
				}
			}
			continue
		}
		if strings.HasPrefix(trimmed, "application.name = ") {
			val := strings.TrimPrefix(trimmed, "application.name = ")
			curApp = strings.Trim(val, `"`)
		}
	}
	flush()
	return out
}

func (d *linuxDucker) setVolume(id uint32, vol int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "pactl", "set-sink-input-volume",
		strconv.FormatUint(uint64(id), 10),
		strconv.Itoa(vol)+"%",
	).Run()
}
