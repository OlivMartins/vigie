//go:build windows

package proxy

import (
	"os"
	"os/exec"
)

// isolate is a no-op on Windows: there is no process group to signal.
func isolate(*exec.Cmd) {}

// terminate kills the server: Windows has no SIGTERM to send.
func terminate(cmd *exec.Cmd) { kill(cmd) }

func kill(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func exitCode(ps *os.ProcessState) int {
	if ps == nil {
		return 1
	}
	return ps.ExitCode()
}
