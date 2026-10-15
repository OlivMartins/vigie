//go:build unix

package cli

import (
	"os"
	"os/signal"
	"syscall"
)

// catchSIGPIPE turns writes to a closed standard output into EPIPE errors
// instead of killing Vigie, so that the session ends cleanly: spans flushed,
// server stopped. Notify, not Ignore: an ignored signal would stay ignored in
// the server process too.
func catchSIGPIPE() {
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
}
