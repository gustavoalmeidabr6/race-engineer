// buttonwatch — minimal real-time F1 25 BUTN watcher.
//
// Binds to the F1 telemetry UDP port with SO_REUSEPORT so it can run
// side-by-side with the main telemetry-core server. Prints every BUTN
// packet to stdout with millisecond timestamps. No HTTP, no fanout, no
// state — just packet → terminal.
//
// Usage:
//
//	./workspace/bin/buttonwatch              # listen on default :20777
//	./workspace/bin/buttonwatch -port 20778  # custom port
//	./workspace/bin/buttonwatch -all         # log every BUTN packet, even no-change
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/packets"
)

func main() {
	host := flag.String("host", "0.0.0.0", "UDP bind host")
	port := flag.Int("port", 20777, "UDP bind port (matches TELEMETRY_PORT)")
	all := flag.Bool("all", false, "log every BUTN packet, including no-change ones")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", *host, *port))
	if err != nil {
		log.Fatalf("resolve %s:%d: %v", *host, *port, err)
	}

	// SO_REUSEADDR + SO_REUSEPORT so we can bind alongside the running
	// telemetry-core server. F1 25's broadcast packets are delivered to all
	// listening sockets on macOS / Linux, so both processes see every packet.
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
				_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEPORT, 1)
				_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
			})
		},
	}
	pc, err := lc.ListenPacket(ctx, "udp", addr.String())
	if err != nil {
		log.Fatalf("bind %s: %v", addr.String(), err)
	}
	conn := pc.(*net.UDPConn)
	defer conn.Close()
	_ = conn.SetReadBuffer(2 * 1024 * 1024)

	fmt.Printf("buttonwatch listening on udp://%s (REUSEPORT — coexists with telemetry-core)\n", addr.String())
	fmt.Println("Press buttons on your wheel/pad. Ctrl+C to quit.")
	if *all {
		fmt.Println("(-all enabled: logging every BUTN packet, even no-change)")
	}
	fmt.Println()

	var prev uint32
	first := true
	buf := make([]byte, 2048)
	pktCount := 0
	butnCount := 0

	for {
		select {
		case <-ctx.Done():
			fmt.Printf("\nStopped. Saw %d packets, %d BUTN events.\n", pktCount, butnCount)
			return
		default:
		}

		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			log.Printf("read err: %v", err)
			continue
		}
		pktCount++

		hdr, err := packets.ParseHeader(buf[:n])
		if err != nil || hdr.PacketID != 3 {
			continue
		}
		ev, err := packets.ParseEventData(buf[:n])
		if err != nil || ev.GetEventCode() != packets.EventButtons {
			continue
		}
		detail, err := packets.ParseEventDetails(packets.EventButtons, ev.EventDetails)
		if err != nil {
			continue
		}
		btn := detail.(*packets.ButtonsEvent)
		butnCount++

		now := time.Now().Format("15:04:05.000")
		status := btn.ButtonStatus

		if first {
			prev = status
			first = false
			fmt.Printf("[%s] BASELINE  status=0x%08X  (initial state)\n", now, status)
			continue
		}

		if status == prev {
			if *all {
				fmt.Printf("[%s] heartbeat status=0x%08X (no change)\n", now, status)
			}
			continue
		}

		pressed := status & ^prev
		released := prev & ^status
		prev = status

		if pressed != 0 {
			fmt.Printf("[%s] PRESS    bit=0x%08X  status=0x%08X  %s\n",
				now, pressed, status, packets.ButtonName(pressed))
		}
		if released != 0 {
			fmt.Printf("[%s] RELEASE  bit=0x%08X  status=0x%08X  %s\n",
				now, released, status, packets.ButtonName(released))
		}
	}
}
