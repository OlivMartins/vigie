// Command vigie relays a stdio MCP server, traces every request with
// OpenTelemetry and applies a tool policy. See "vigie help".
package main

import (
	"context"
	"os"

	"github.com/OlivMartins/vigie/internal/cli"
)

func main() {
	os.Exit(cli.Main(context.Background(), os.Args[1:], cli.Streams{
		In:  os.Stdin,
		Out: os.Stdout,
		Err: os.Stderr,
	}))
}
