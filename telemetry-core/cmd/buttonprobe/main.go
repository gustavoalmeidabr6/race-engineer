package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/packets"
)

type buttonEvent struct {
	Time         string   `json:"time"`
	UnixMs       int64    `json:"unix_ms"`
	SessionTime  float32  `json:"session_time"`
	FrameID      uint32   `json:"frame_id"`
	ButtonStatus uint32   `json:"button_status"`
	ButtonHex    string   `json:"button_hex"`
	PTTActive    bool     `json:"ptt_active"`
	Transition   string   `json:"transition"`
	PressedBit   string   `json:"pressed_bit,omitempty"`
	PressedName  string   `json:"pressed_name,omitempty"`
	ReleasedBit  string   `json:"released_bit,omitempty"`
	ReleasedName string   `json:"released_name,omitempty"`
	HeldNames    []string `json:"held_names,omitempty"`
	PacketCount  uint64   `json:"packet_count"`
	ButtonsCount uint64   `json:"buttons_count"`
	SinceLastMs  int64    `json:"since_last_ms"`
	DecodeError  string   `json:"decode_error,omitempty"`
}

type broadcaster struct {
	mu      sync.Mutex
	clients map[chan buttonEvent]struct{}
	last    *buttonEvent
}

func newBroadcaster() *broadcaster {
	return &broadcaster{clients: make(map[chan buttonEvent]struct{})}
}

func (b *broadcaster) subscribe() chan buttonEvent {
	ch := make(chan buttonEvent, 16)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	if b.last != nil {
		ch <- *b.last
	}
	b.mu.Unlock()
	return ch
}

func (b *broadcaster) unsubscribe(ch chan buttonEvent) {
	b.mu.Lock()
	delete(b.clients, ch)
	close(ch)
	b.mu.Unlock()
}

func (b *broadcaster) publish(ev buttonEvent) {
	b.mu.Lock()
	b.last = &ev
	for ch := range b.clients {
		select {
		case ch <- ev:
		default:
		}
	}
	b.mu.Unlock()
}

func main() {
	host := flag.String("host", "0.0.0.0", "UDP bind host")
	port := flag.Int("port", 20777, "UDP bind port")
	httpAddr := flag.String("http", "127.0.0.1:8099", "HTTP status UI address")
	pttMaskRaw := flag.String("ptt", envOr("PTT_BUTTON", "0x00100000"), "PTT button bitmask, e.g. 0x00100000")
	flag.Parse()

	pttMask, err := parseUint32(*pttMaskRaw)
	if err != nil {
		log.Fatalf("invalid -ptt value %q: %v", *pttMaskRaw, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	b := newBroadcaster()
	var packetsRx atomic.Uint64
	var buttonsRx atomic.Uint64

	go runHTTP(ctx, *httpAddr, b, *host, *port, pttMask)

	if err := runUDP(ctx, *host, *port, pttMask, b, &packetsRx, &buttonsRx); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

func parseUint32(raw string) (uint32, error) {
	v, err := strconv.ParseUint(raw, 0, 32)
	return uint32(v), err
}

func labelOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func runUDP(ctx context.Context, host string, port int, pttMask uint32, b *broadcaster, packetsRx, buttonsRx *atomic.Uint64) error {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetReadBuffer(2 * 1024 * 1024)

	log.Printf("buttonprobe listening on udp://%s, ptt mask 0x%08X", addr.String(), pttMask)

	var prevStatus uint32
	var lastButtonAt time.Time
	buf := make([]byte, 2048)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return err
		}

		packetCount := packetsRx.Add(1)
		raw := buf[:n]
		hdr, err := packets.ParseHeader(raw)
		if err != nil || hdr.PacketID != 3 {
			continue
		}
		event, err := packets.ParseEventData(raw)
		if err != nil {
			continue
		}
		if event.GetEventCode() != packets.EventButtons {
			continue
		}
		detail, err := packets.ParseEventDetails(packets.EventButtons, event.EventDetails)
		if err != nil {
			continue
		}
		buttons := detail.(*packets.ButtonsEvent)

		now := time.Now()
		sinceLast := int64(0)
		if !lastButtonAt.IsZero() {
			sinceLast = now.Sub(lastButtonAt).Milliseconds()
		}
		lastButtonAt = now

		nowPressed := buttons.ButtonStatus&pttMask != 0
		wasPressed := prevStatus&pttMask != 0
		transition := "change"
		if nowPressed && !wasPressed {
			transition = "press"
		} else if !nowPressed && wasPressed {
			transition = "release"
		}

		// Diff against the prior status so we can attach the human-readable
		// label of whichever button just changed — the user doesn't have
		// to memorise bitmasks while configuring PTT.
		pressedBits := buttons.ButtonStatus & ^prevStatus
		releasedBits := prevStatus & ^buttons.ButtonStatus
		prevStatus = buttons.ButtonStatus

		buttonCount := buttonsRx.Add(1)
		ev := buttonEvent{
			Time:         now.Format("15:04:05.000"),
			UnixMs:       now.UnixMilli(),
			SessionTime:  hdr.SessionTime,
			FrameID:      hdr.FrameIdentifier,
			ButtonStatus: buttons.ButtonStatus,
			ButtonHex:    fmt.Sprintf("0x%08X", buttons.ButtonStatus),
			PTTActive:    nowPressed,
			Transition:   transition,
			HeldNames:    packets.ButtonNamesForStatus(buttons.ButtonStatus),
			PacketCount:  packetCount,
			ButtonsCount: buttonCount,
			SinceLastMs:  sinceLast,
		}
		if pressedBits != 0 {
			ev.PressedBit = fmt.Sprintf("0x%08X", pressedBits)
			ev.PressedName = packets.ButtonName(pressedBits)
		}
		if releasedBits != 0 {
			ev.ReleasedBit = fmt.Sprintf("0x%08X", releasedBits)
			ev.ReleasedName = packets.ButtonName(releasedBits)
		}

		log.Printf("%s status=%s ptt=%t pressed=%s released=%s session=%.3f frame=%d dt=%dms",
			transition, ev.ButtonHex, ev.PTTActive,
			labelOrDash(ev.PressedName), labelOrDash(ev.ReleasedName),
			ev.SessionTime, ev.FrameID, ev.SinceLastMs)
		b.publish(ev)
	}
}

func runHTTP(ctx context.Context, addr string, b *broadcaster, host string, port int, pttMask uint32) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := page.Execute(w, map[string]any{
			"UDPHost": host,
			"UDPPort": port,
			"PTTMask": fmt.Sprintf("0x%08X", pttMask),
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		ch := b.subscribe()
		defer b.unsubscribe(ch)

		for {
			select {
			case <-r.Context().Done():
				return
			case ev := <-ch:
				data, _ := json.Marshal(ev)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}
		}
	})

	server := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Printf("buttonprobe UI at http://%s", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("http server error: %v", err)
	}
}

var page = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>F1 Button Probe</title>
  <style>
    :root { color-scheme: dark; font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background: #080a0d; color: #e6edf3; }
    body { margin: 0; padding: 28px; }
    main { max-width: 920px; margin: 0 auto; }
    h1 { font-size: 22px; margin: 0 0 4px; }
    .sub { color: #8b949e; margin-bottom: 22px; font-size: 13px; }
    .grid { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 12px; margin-bottom: 18px; }
    .box { border: 1px solid #30363d; background: #0d1117; border-radius: 8px; padding: 14px; min-height: 78px; }
    .label { color: #8b949e; font-size: 11px; text-transform: uppercase; letter-spacing: .06em; margin-bottom: 8px; }
    .value { font-size: 24px; font-weight: 700; font-variant-numeric: tabular-nums; }
    #lamp { width: 90px; height: 90px; border-radius: 50%; border: 1px solid #30363d; background: #21262d; box-shadow: inset 0 0 18px rgba(0,0,0,.55); }
    #lamp.on { background: #2ea043; box-shadow: 0 0 32px rgba(46,160,67,.75), inset 0 0 12px rgba(255,255,255,.22); }
    .row { display: flex; align-items: center; gap: 18px; margin-bottom: 18px; }
    table { width: 100%; border-collapse: collapse; background: #0d1117; border: 1px solid #30363d; border-radius: 8px; overflow: hidden; }
    th, td { text-align: left; padding: 9px 10px; border-bottom: 1px solid #21262d; font-size: 13px; font-variant-numeric: tabular-nums; }
    th { color: #8b949e; font-size: 11px; text-transform: uppercase; letter-spacing: .06em; }
    tr:last-child td { border-bottom: 0; }
    .press { color: #3fb950; font-weight: 700; }
    .release { color: #f85149; font-weight: 700; }
    .change { color: #d29922; font-weight: 700; }
    @media (max-width: 760px) { body { padding: 16px; } .grid { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
  </style>
</head>
<body>
  <main>
    <h1>F1 Button Probe</h1>
    <div class="sub">Listening on UDP {{.UDPHost}}:{{.UDPPort}}. PTT mask {{.PTTMask}}. This bypasses DuckDB, React app state, voice, and the race engineer pipeline.</div>

    <div class="row">
      <div id="lamp"></div>
      <div>
        <div class="label">PTT state</div>
        <div id="state" class="value">waiting</div>
      </div>
    </div>

    <section class="grid">
      <div class="box"><div class="label">Button mask</div><div id="mask" class="value">-</div></div>
      <div class="box"><div class="label">Last event</div><div id="last" class="value">-</div></div>
      <div class="box"><div class="label">Session time</div><div id="session" class="value">-</div></div>
      <div class="box"><div class="label">Button packets</div><div id="count" class="value">0</div></div>
    </section>

    <table>
      <thead><tr><th>Wall time</th><th>Transition</th><th>Mask</th><th>PTT</th><th>Frame</th><th>Delta</th></tr></thead>
      <tbody id="events"></tbody>
    </table>
  </main>
  <script>
    const rows = document.getElementById('events');
    const lamp = document.getElementById('lamp');
    const state = document.getElementById('state');
    const mask = document.getElementById('mask');
    const last = document.getElementById('last');
    const session = document.getElementById('session');
    const count = document.getElementById('count');

    const source = new EventSource('/events');
    source.onmessage = (event) => {
      const ev = JSON.parse(event.data);
      lamp.classList.toggle('on', ev.ptt_active);
      state.textContent = ev.ptt_active ? 'pressed' : 'released';
      mask.textContent = ev.button_hex;
      last.textContent = ev.time;
      session.textContent = ev.session_time.toFixed(3);
      count.textContent = ev.buttons_count;

      const tr = document.createElement('tr');
      tr.innerHTML = '<td>' + ev.time + '</td>' +
        '<td class="' + ev.transition + '">' + ev.transition + '</td>' +
        '<td>' + ev.button_hex + '</td>' +
        '<td>' + ev.ptt_active + '</td>' +
        '<td>' + ev.frame_id + '</td>' +
        '<td>' + (ev.since_last_ms || 0) + ' ms</td>';
      rows.prepend(tr);
      while (rows.children.length > 40) rows.lastChild.remove();
    };
  </script>
</body>
</html>`))
