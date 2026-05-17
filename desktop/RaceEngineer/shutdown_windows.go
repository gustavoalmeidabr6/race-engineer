//go:build windows

package main

import "os/exec"

// sendShutdownSignal is a no-op on Windows: there's no portable equivalent
// of SIGTERM for a non-console child process (graceful shutdown there
// would need a JobObject or named-pipe protocol). The caller's 5-second
// timeout in shutdown() then falls through to Process.Kill(), which is
// the same end state Linux/macOS reach when the server ignores SIGTERM.
func sendShutdownSignal(cmd *exec.Cmd) error {
	return nil
}
