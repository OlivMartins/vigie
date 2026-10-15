//go:build windows

package cli

// catchSIGPIPE is a no-op: Windows has no SIGPIPE.
func catchSIGPIPE() {}
