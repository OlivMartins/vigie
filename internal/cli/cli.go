// Package cli implements the vigie command line.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/OlivMartins/vigie/internal/version"
)

// Exit codes.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// Streams are the standard streams of the process.
type Streams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

const usage = `Vigie sits between an MCP client and an MCP server: it traces every request
with OpenTelemetry, pins the tool definitions you approved and blocks what the
policy does not allow.

Usage:
  vigie run --server NAME [flags] -- COMMAND [ARGS...]   relay a stdio MCP server
  vigie gateway [flags]                                  serve remote MCP servers over HTTP
  vigie pins list [SERVER]                               show pinned tools
  vigie pins diff SERVER TOOL                            review a tool definition change
  vigie pins approve SERVER TOOL --fingerprint F         approve the definition you reviewed
  vigie pins forget SERVER TOOL...                       drop tools from a pin file
  vigie check                                            validate the configuration
  vigie version                                          print the version

Run "vigie COMMAND -h" for the flags of a command.
Documentation: https://github.com/OlivMartins/vigie
`

// Main runs the command line and returns the exit code.
func Main(ctx context.Context, args []string, s Streams) int {
	if len(args) == 0 {
		fmt.Fprint(s.Err, usage)
		return exitUsage
	}
	switch args[0] {
	case "run":
		return runCommand(ctx, args[1:], s)
	case "gateway":
		return gatewayCommand(ctx, args[1:], s)
	case "pins":
		return pinsCommand(args[1:], s)
	case "check":
		return checkCommand(args[1:], s)
	case "version", "--version":
		fmt.Fprintln(s.Out, version.String())
		return exitOK
	case "help", "-h", "-help", "--help":
		fmt.Fprint(s.Out, usage)
		return exitOK
	default:
		fmt.Fprintf(s.Err, "vigie: unknown command %q\n\n%s", args[0], usage)
		return exitUsage
	}
}

// newFlagSet returns a flag set that reports errors on s.Err.
func newFlagSet(name, synopsis string, s Streams) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(s.Err)
	fs.Usage = func() {
		fmt.Fprintf(s.Err, "Usage: %s\n", synopsis)
		hasFlags := false
		fs.VisitAll(func(*flag.Flag) { hasFlags = true })
		if hasFlags {
			fmt.Fprintln(s.Err, "\nFlags:")
			fs.PrintDefaults()
		}
	}
	return fs
}

// parseInterleaved parses flags placed before, between or after positional
// arguments, and returns the positional ones.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		if rest[0] == "--" {
			return append(positional, rest[1:]...), nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// fail prints an error and returns the exit code for it.
func fail(s Streams, code int, err error) int {
	fmt.Fprintf(s.Err, "vigie: %v\n", err)
	return code
}

func usageError(s Streams, fs *flag.FlagSet, format string, a ...any) int {
	fmt.Fprintf(s.Err, "vigie: "+format+"\n\n", a...)
	fs.Usage()
	return exitUsage
}

func isHelp(err error) bool { return errors.Is(err, flag.ErrHelp) }

// colors returns ANSI color helpers when out is a terminal and NO_COLOR is
// not set.
func colors(out io.Writer) (red, green, bold func(string) string) {
	plain := func(s string) string { return s }
	f, ok := out.(*os.File)
	if !ok || os.Getenv("NO_COLOR") != "" || strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return plain, plain, plain
	}
	if info, err := f.Stat(); err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return plain, plain, plain
	}
	wrap := func(code string) func(string) string {
		return func(s string) string { return "\x1b[" + code + "m" + s + "\x1b[0m" }
	}
	return wrap("31"), wrap("32"), wrap("1")
}
