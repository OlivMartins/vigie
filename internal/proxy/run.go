package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/OlivMartins/vigie/internal/jsonrpc"
	"github.com/OlivMartins/vigie/internal/telemetry"
)

// Shutdown delays, after the MCP stdio transport: close the server's input,
// wait for it to exit, then ask it to terminate, then kill it. Variables so
// that tests can shorten them.
var (
	exitGrace      = 5 * time.Second
	terminateGrace = 2 * time.Second
)

// ExitStartFailure is the exit code when the server cannot be started, as a
// shell would report a missing command.
const ExitStartFailure = 127

// Process configures Run.
type Process struct {
	// Command is the upstream server and its arguments.
	Command []string
	// Env is the server's environment; nil inherits Vigie's.
	Env []string
	// Client streams: the MCP host writes to Stdin and reads Stdout.
	Stdin  io.Reader
	Stdout io.Writer
	// Stderr receives the server's own diagnostics, untouched.
	Stderr io.Writer
	// MaxFrameBytes bounds a single message, in both directions.
	MaxFrameBytes int
}

// Run starts the upstream server and relays messages until either side
// closes, or ctx is cancelled (a signal). It returns the exit code Vigie
// should exit with: the server's when it stopped on its own, 0 when the
// client ended the session.
func Run(ctx context.Context, p Process, opts Options) (int, error) {
	if len(p.Command) == 0 {
		return ExitStartFailure, errors.New("no server command given")
	}
	maxBytes := p.MaxFrameBytes
	if maxBytes <= 0 {
		maxBytes = jsonrpc.DefaultMaxFrameBytes
	}
	cmd := exec.Command(p.Command[0], p.Command[1:]...)
	cmd.Env = p.Env
	cmd.Stderr = p.Stderr
	isolate(cmd)
	serverIn, err := cmd.StdinPipe()
	if err != nil {
		return ExitStartFailure, err
	}
	serverOut, err := cmd.StdoutPipe()
	if err != nil {
		return ExitStartFailure, err
	}
	if err := cmd.Start(); err != nil {
		return ExitStartFailure, fmt.Errorf("starting %s: %w", p.Command[0], err)
	}
	log := opts.Logger
	if log != nil {
		log.Info("upstream server started", "command", p.Command[0], "pid", cmd.Process.Pid)
	}

	relay := NewRelay(opts, jsonrpc.NewWriter(p.Stdout), jsonrpc.NewWriter(serverIn))
	clientDone := make(chan error, 1)
	serverDone := make(chan error, 1)
	go func() {
		clientDone <- pump(jsonrpc.NewReader(p.Stdin, maxBytes), relay.HandleClientFrame,
			func() error { return relay.RejectOversized(maxBytes) })
	}()
	go func() {
		serverDone <- pump(jsonrpc.NewReader(serverOut, maxBytes), relay.HandleServerFrame,
			func() error { relay.DropOversized(maxBytes); return nil })
	}()

	var reason string
	clientEnded := false
	select {
	case err := <-clientDone:
		if errors.Is(err, ErrServerGone) {
			// The server stopped reading: it is exiting or gone.
			reason = telemetry.ErrorUpstreamClosed
			logEnd(opts, "server stopped reading its input", err)
		} else {
			// The host closed the session (or its pipe broke).
			reason, clientEnded = telemetry.ErrorClientClosed, true
			logEnd(opts, "client closed the session", err)
		}
		// Let the server exit on its own, then insist.
		serverIn.Close()
		if !wait(serverDone, exitGrace, log) {
			terminate(cmd)
			if !wait(serverDone, terminateGrace, log) {
				kill(cmd)
			}
		}
	case err := <-serverDone:
		serverIn.Close()
		if errors.Is(err, ErrClientGone) {
			// The host stopped reading Vigie's output: the session is over.
			reason, clientEnded = telemetry.ErrorClientClosed, true
			logEnd(opts, "client stopped reading", err)
		} else {
			reason = telemetry.ErrorUpstreamClosed
			logEnd(opts, "server closed its output", err)
		}
	case <-ctx.Done():
		reason = telemetry.ErrorShutdown
		logEnd(opts, "shutting down on signal", nil)
		serverIn.Close()
		terminate(cmd)
		if !wait(serverDone, terminateGrace, log) {
			kill(cmd)
		}
	}
	relay.Close(reason)

	// Wait for the server to exit, escalating as the stdio transport
	// recommends: its input is closed, then SIGTERM, then SIGKILL.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-exited:
	case <-time.After(exitGrace):
		terminate(cmd)
		select {
		case waitErr = <-exited:
		case <-time.After(terminateGrace):
			kill(cmd)
			waitErr = <-exited
		}
	}
	if log != nil && waitErr != nil {
		log.Debug("upstream server exit status", "status", waitErr)
	}
	if clientEnded {
		return 0, nil
	}
	return exitCode(cmd.ProcessState), nil
}

// pump reads frames until the stream ends and hands them to handle.
func pump(r *jsonrpc.Reader, handle func([]byte) error, oversized func() error) error {
	for {
		line, err := r.Next()
		switch {
		case errors.Is(err, jsonrpc.ErrFrameTooLarge):
			if err := oversized(); err != nil {
				return err
			}
			continue
		case errors.Is(err, io.EOF), errors.Is(err, os.ErrClosed):
			return nil
		case err != nil:
			return err
		}
		if err := handle(line); err != nil {
			return err
		}
	}
}

// wait reports whether a pump finished within d.
func wait(done <-chan error, d time.Duration, log *slog.Logger) bool {
	select {
	case err := <-done:
		if err != nil && log != nil {
			log.Debug("stream closed with an error", "error", err)
		}
		return true
	case <-time.After(d):
		return false
	}
}

func logEnd(opts Options, msg string, err error) {
	if opts.Logger == nil {
		return
	}
	if err != nil {
		opts.Logger.Info(msg, "error", err)
		return
	}
	opts.Logger.Info(msg)
}
