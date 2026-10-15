package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/OlivMartins/vigie/internal/config"
)

func checkCommand(args []string, s Streams) int {
	fs := newFlagSet("check", "vigie check [--config FILE]", s)
	configPath := fs.String("config", "", "configuration file (default: $"+config.EnvConfig+", then the user configuration directory)")
	if err := fs.Parse(args); err != nil {
		if isHelp(err) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		return usageError(s, fs, "unexpected argument %q", fs.Arg(0))
	}
	path, err := config.Resolve(*configPath)
	if err != nil {
		return fail(s, exitUsage, err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return fail(s, exitUsage, err)
	}
	source := cfg.Source
	if source == "" {
		source = "none found, built-in defaults apply"
	}
	fmt.Fprintf(s.Out, "Configuration:  %s\n", source)
	fmt.Fprintf(s.Out, "Pin files:      %s\n", cfg.PinsDir)
	fmt.Fprintf(s.Out, "Telemetry:      capture_content=%t propagate_context=%t\n\n", cfg.Telemetry.CaptureContent, cfg.Telemetry.PropagateContext)

	tw := tabwriter.NewWriter(s.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVER\tMODE\tTOOLS\tPINS\tSECRETS")
	row := func(name string, p config.Policy) {
		tools := "allow " + strings.Join(p.Tools.Allow, ",")
		if len(p.Tools.Deny) > 0 {
			tools += "; deny " + strings.Join(p.Tools.Deny, ",")
		}
		pinsCol := "off"
		if p.Pins.Enabled {
			pinsCol = fmt.Sprintf("new: %s, changed: %s", p.Pins.OnNew, p.Pins.OnChange)
		}
		secretsCol := fmt.Sprintf("arguments: %s, results: %s", p.Secrets.Arguments, p.Secrets.Results)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", name, p.Mode, tools, pinsCol, secretsCol)
	}
	row("(any other)", cfg.Policy(""))
	for _, name := range cfg.Servers() {
		row(name, cfg.Policy(name))
	}
	if err := tw.Flush(); err != nil {
		return fail(s, exitError, err)
	}
	fmt.Fprintln(s.Out, "\nConfiguration is valid.")
	return exitOK
}
