//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// sendShutdownSignal asks the embedded telemetry-core to flush + exit
// politely. On unix this is SIGTERM, which the server traps to close the
// DuckDB writer and stop the UDP listener. Caller still waits with a
// timeout and falls back to Kill().
func sendShutdownSignal(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Signal(syscall.SIGTERM)
}
