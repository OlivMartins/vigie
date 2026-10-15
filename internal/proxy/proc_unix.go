//go:build unix

package proxy

import (
	"os"
	"os/exec"
	"syscall"
)

// isolate starts the server in its own process group, so that terminating it
// also reaches the processes it spawned (npx, uvx and the like start the real
// server as a child).
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminate(cmd *exec.Cmd) { signalGroup(cmd, syscall.SIGTERM) }

func kill(cmd *exec.Cmd) { signalGroup(cmd, syscall.SIGKILL) }

func signalGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil {
		_ = cmd.Process.Signal(sig)
	}
}

// exitCode reports the server's exit status the way a shell does: its exit
// code, or 128 plus the signal that ended it.
func exitCode(ps *os.ProcessState) int {
	if ps == nil {
		return 1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}
