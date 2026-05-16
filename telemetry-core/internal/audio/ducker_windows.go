//go:build windows

package audio

import (
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
	"github.com/rs/zerolog/log"
	"golang.org/x/sys/windows"
)

// defaultWindowsTargets is the executable-name set we duck unless the
// user overrides AUDIO_DUCKING_TARGETS. Match is case-insensitive
// suffix (filepath.Base) so e.g. "spotify.exe" hits regardless of the
// install location.
var defaultWindowsTargets = []string{
	"spotify.exe", "chrome.exe", "firefox.exe", "msedge.exe",
	"applemusic.exe", "vlc.exe", "brave.exe", "opera.exe",
}

// windowsDucker drives per-process audio-session volume via WASAPI.
// All COM calls happen on a single dedicated goroutine (cmdLoop) with
// runtime.LockOSThread held, because:
//   - COM is apartment-threaded; mixing calls across OS threads without
//     an MTA promise is undefined.
//   - go-wca's vtable-pointer trampoline assumes the call comes from
//     the same thread that did CoInitializeEx.
//
// SetDucked sends a command to the loop and returns immediately; the
// loop processes it asynchronously. We accept the latency cost (a
// dropped frame at the very start of a phrase) in exchange for not
// blocking the speech path on COM.
type windowsDucker struct {
	targets []string // lower-cased exe names
	level   float32  // 0..1 duck target (ignored in pause mode)
	mode    Mode

	cmds chan windowsCmd
	done chan struct{}

	mu     sync.Mutex
	saved  map[uint32]float32 // pid -> prior master volume (ModeDuck)
	paused bool               // ModePause: true if we sent a pause and owe the next play
}

type windowsCmd struct {
	op windowsOp
}

type windowsOp int

const (
	opDuck windowsOp = iota
	opUnduck
	opClose
)

func NewOSDucker(targets []string, level float64, mode Mode) (Ducker, error) {
	if len(targets) == 0 {
		targets = defaultWindowsTargets
	}
	low := make([]string, len(targets))
	for i, t := range targets {
		low[i] = strings.ToLower(t)
	}
	lvl := float32(level)
	if lvl < 0 {
		lvl = 0
	}
	if lvl > 1 {
		lvl = 1
	}

	d := &windowsDucker{
		targets: low,
		level:   lvl,
		mode:    mode,
		cmds:    make(chan windowsCmd, 4),
		done:    make(chan struct{}),
		saved:   map[uint32]float32{},
	}
	go d.cmdLoop()
	log.Info().
		Strs("targets", low).
		Float32("level", lvl).
		Str("mode", modeName(mode)).
		Msg("audio: windows ducker ready")
	return d, nil
}

func (d *windowsDucker) SetDucked(ducked bool) error {
	op := opUnduck
	if ducked {
		op = opDuck
	}
	select {
	case d.cmds <- windowsCmd{op: op}:
	default:
		// Channel full — coordinator queued faster than the COM loop
		// drains. Dropping is correct: the latest command in the queue
		// already represents the same intent or the opposite, and a
		// fresh extend() will re-issue if needed.
	}
	return nil
}

func (d *windowsDucker) Close() error {
	select {
	case d.cmds <- windowsCmd{op: opClose}:
	default:
	}
	<-d.done
	return nil
}

// cmdLoop owns the COM apartment for its lifetime.
func (d *windowsDucker) cmdLoop() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(d.done)

	if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
		// HRESULT 0x80010106 (RPC_E_CHANGED_MODE) means the apartment
		// was already initialised STA — harmless for our use, the
		// existing apartment will accept our calls.
		if oerr, ok := err.(*ole.OleError); !ok || oerr.Code() != 0x80010106 {
			log.Warn().Err(err).Msg("audio windows: CoInitializeEx failed; ducking disabled")
			for range d.cmds {
				// drain so SetDucked doesn't block
			}
			return
		}
	}
	defer ole.CoUninitialize()

	for cmd := range d.cmds {
		switch cmd.op {
		case opDuck:
			if d.mode == ModePause {
				d.sendPause()
			} else {
				d.duckAll()
			}
		case opUnduck:
			if d.mode == ModePause {
				d.sendPlay()
			} else {
				d.unduckAll()
			}
		case opClose:
			if d.mode == ModePause {
				d.sendPlay()
			} else {
				d.unduckAll()
			}
			return
		}
	}
}

// sendPause sends the global VK_MEDIA_PLAY_PAUSE virtual key. Windows
// routes this to the active media session — usually whichever player
// is currently producing audio (Spotify, Chrome, Edge, etc.). The key
// is a toggle, so we only send it when we don't already believe a
// pause is outstanding; otherwise a back-to-back duck pair would
// flip-flop the player. sendPlay mirrors this on the unduck edge.
func (d *windowsDucker) sendPause() {
	d.mu.Lock()
	if d.paused {
		d.mu.Unlock()
		return
	}
	d.paused = true
	d.mu.Unlock()
	sendMediaKey(vkMediaPlayPause)
}

func (d *windowsDucker) sendPlay() {
	d.mu.Lock()
	if !d.paused {
		d.mu.Unlock()
		return
	}
	d.paused = false
	d.mu.Unlock()
	sendMediaKey(vkMediaPlayPause)
}

// duckAll enumerates current audio sessions, matches by exe name, and
// drops each one to d.level while remembering its prior volume.
func (d *windowsDucker) duckAll() {
	d.forEachMatchingSession(func(pid uint32, sav *wca.ISimpleAudioVolume) {
		var cur float32
		if err := sav.GetMasterVolume(&cur); err != nil {
			return
		}
		d.mu.Lock()
		// Only save the first-seen volume so repeated duck calls
		// (Coordinator filters these, but defensive) don't overwrite
		// the original.
		if _, had := d.saved[pid]; !had {
			d.saved[pid] = cur
		}
		d.mu.Unlock()
		_ = sav.SetMasterVolume(d.level, nil)
	})
}

func (d *windowsDucker) unduckAll() {
	d.mu.Lock()
	leftover := make(map[uint32]float32, len(d.saved))
	for k, v := range d.saved {
		leftover[k] = v
	}
	d.saved = map[uint32]float32{}
	d.mu.Unlock()
	if len(leftover) == 0 {
		return
	}
	d.forEachMatchingSession(func(pid uint32, sav *wca.ISimpleAudioVolume) {
		prior, had := leftover[pid]
		if !had {
			return
		}
		_ = sav.SetMasterVolume(prior, nil)
	})
}

// forEachMatchingSession walks every session on the default render
// endpoint, filtering to processes whose executable name matches d.targets.
// The visitor must NOT retain sav after returning — it's released here.
func (d *windowsDucker) forEachMatchingSession(visit func(pid uint32, sav *wca.ISimpleAudioVolume)) {
	var mmde *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(
		wca.CLSID_MMDeviceEnumerator, 0,
		wca.CLSCTX_ALL,
		wca.IID_IMMDeviceEnumerator,
		&mmde,
	); err != nil {
		log.Debug().Err(err).Msg("audio windows: device enumerator unavailable")
		return
	}
	defer mmde.Release()

	var mmd *wca.IMMDevice
	if err := mmde.GetDefaultAudioEndpoint(wca.ERender, wca.EConsole, &mmd); err != nil {
		log.Debug().Err(err).Msg("audio windows: no default render endpoint")
		return
	}
	defer mmd.Release()

	var asm2 *wca.IAudioSessionManager2
	if err := mmd.Activate(wca.IID_IAudioSessionManager2, wca.CLSCTX_ALL, nil, &asm2); err != nil {
		log.Debug().Err(err).Msg("audio windows: session manager unavailable")
		return
	}
	defer asm2.Release()

	var asEnum *wca.IAudioSessionEnumerator
	if err := asm2.GetSessionEnumerator(&asEnum); err != nil {
		log.Debug().Err(err).Msg("audio windows: session enumerator failed")
		return
	}
	defer asEnum.Release()

	var count int
	if err := asEnum.GetCount(&count); err != nil {
		return
	}
	for i := 0; i < count; i++ {
		var asCtrl *wca.IAudioSessionControl
		if err := asEnum.GetSession(i, &asCtrl); err != nil {
			continue
		}
		// go-wca uses the standard go-ole COM pattern: QueryInterface
		// returns an *IDispatch which we reinterpret to the target
		// type. All these structs are just `{ RawVTable *interface{} }`
		// at the memory level, so the cast is safe — the vtable
		// behind the pointer is the right one because QI returns the
		// interface-specific vtable per COM contract.
		disp2, err := asCtrl.QueryInterface(wca.IID_IAudioSessionControl2)
		if err != nil {
			asCtrl.Release()
			continue
		}
		asCtrl2 := (*wca.IAudioSessionControl2)(unsafe.Pointer(disp2))
		var pid uint32
		_ = asCtrl2.GetProcessId(&pid)
		exeLow := strings.ToLower(processExeName(pid))
		if !d.matches(exeLow) {
			asCtrl2.Release()
			asCtrl.Release()
			continue
		}
		dispV, err := asCtrl.QueryInterface(wca.IID_ISimpleAudioVolume)
		if err != nil {
			asCtrl2.Release()
			asCtrl.Release()
			continue
		}
		sav := (*wca.ISimpleAudioVolume)(unsafe.Pointer(dispV))
		visit(pid, sav)
		sav.Release()
		asCtrl2.Release()
		asCtrl.Release()
	}
}

func (d *windowsDucker) matches(exeLow string) bool {
	if exeLow == "" {
		return false
	}
	for _, t := range d.targets {
		if exeLow == t {
			return true
		}
	}
	return false
}

// vkMediaPlayPause is the virtual-key code for the Play/Pause media
// key. Windows routes this to the system-wide active media session
// (Spotify, browser, etc.) regardless of focus — exactly what we want.
const vkMediaPlayPause = 0xB3

const (
	keyeventfKeyup    = 0x0002
	keyeventfExtended = 0x0001
)

// sendMediaKey injects a virtual-key press+release via the user32
// keybd_event API. The function exists for backward compat with the
// modern SendInput API and works on all supported Windows versions.
// Errors are swallowed — there's no useful recovery if the media key
// can't be dispatched, and the goroutine will retry on the next edge.
func sendMediaKey(vk uintptr) {
	user32 := windows.NewLazySystemDLL("user32.dll")
	keybdEvent := user32.NewProc("keybd_event")
	// Press.
	_, _, _ = keybdEvent.Call(vk, 0, keyeventfExtended, 0)
	// Release.
	_, _, _ = keybdEvent.Call(vk, 0, keyeventfExtended|keyeventfKeyup, 0)
}

// processExeName looks up the basename of pid's executable. Returns ""
// for the system idle / system process (pid 0, 4) or any pid we don't
// have rights to open.
func processExeName(pid uint32) string {
	if pid == 0 {
		return ""
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	var buf [windows.MAX_PATH]uint16
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return ""
	}
	full := windows.UTF16PtrToString(&buf[0])
	return filepath.Base(full)
}
