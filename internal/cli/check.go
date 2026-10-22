package cli

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/OlivMartins/vigie/internal/audit"
	"github.com/OlivMartins/vigie/internal/config"
	"github.com/OlivMartins/vigie/internal/gateway"
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
	fmt.Fprintf(s.Out, "Telemetry:      capture_content=%t propagate_context=%t\n", cfg.Telemetry.CaptureContent, cfg.Telemetry.PropagateContext)
	auditCol := "off"
	if cfg.Audit.File != "" {
		maxSize, maxFiles := cfg.Audit.MaxSize, cfg.Audit.MaxFiles
		if maxSize == 0 {
			maxSize = audit.DefaultMaxSize
		}
		if maxFiles == 0 {
			maxFiles = audit.DefaultMaxFiles
		}
		auditCol = fmt.Sprintf("%s (rotated at %s, %d files kept)", cfg.Audit.File, size(maxSize), maxFiles)
	}
	fmt.Fprintf(s.Out, "Audit log:      %s\n", auditCol)
	stop := "built-in (5s for stdio servers, 15s for the gateway)"
	if cfg.Limits.StopTimeout > 0 {
		stop = cfg.Limits.StopTimeout.String()
	}
	fmt.Fprintf(s.Out, "Limits:         messages up to %s, stop timeout %s\n\n", size(cfg.Limits.MaxMessageSize), stop)

	tw := tabwriter.NewWriter(s.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVER\tMODE\tTOOLS\tPINS\tSECRETS\tDETECTIONS")
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
		detectionsCol := string(p.Detections.Action)
		if n := len(p.Detections.Exempt); n > 0 {
			detectionsCol += fmt.Sprintf(" (%d exempt)", n)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", name, p.Mode, tools, pinsCol, secretsCol, detectionsCol)
	}
	row("(any other)", cfg.Policy(""))
	for _, name := range cfg.Servers() {
		row(name, cfg.Policy(name))
	}
	if err := tw.Flush(); err != nil {
		return fail(s, exitError, err)
	}
	if code := checkGateway(cfg, s); code != exitOK {
		return code
	}
	fmt.Fprintln(s.Out, "\nConfiguration is valid.")
	return exitOK
}

// checkGateway describes what vigie gateway would serve.
func checkGateway(cfg *config.Config, s Streams) int {
	names := cfg.Upstreams()
	if len(names) == 0 {
		return exitOK
	}
	g := cfg.Gateway
	scheme := "http"
	if g.TLSCertFile != "" {
		scheme = "https"
	}
	fmt.Fprintf(s.Out, "\nGateway:        %s://%s (vigie gateway)\n", scheme, g.Listen)
	auth := "none: listening on loopback only"
	switch {
	case len(g.Tokens) > 0:
		clients := make([]string, 0, len(g.Tokens))
		for name := range g.Tokens {
			clients = append(clients, name)
		}
		sort.Strings(clients)
		auth = fmt.Sprintf("bearer tokens (%s)", strings.Join(clients, ", "))
	case g.AllowAnonymous:
		auth = "none: a proxy in front authenticates clients (allow_anonymous)"
	}
	fmt.Fprintf(s.Out, "Clients:        %s\n", auth)
	if len(g.AllowedOrigins) > 0 {
		fmt.Fprintf(s.Out, "Web origins:    %s\n", strings.Join(g.AllowedOrigins, ", "))
	}
	tw := tabwriter.NewWriter(s.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "\nPATH\tUPSTREAM\tHEADERS ADDED")
	for _, name := range names {
		up, _ := cfg.Upstream(name)
		headers := make([]string, 0, len(up.Headers))
		for h := range up.Headers {
			headers = append(headers, h)
		}
		sort.Strings(headers)
		added := "-"
		if len(headers) > 0 {
			added = strings.Join(headers, ", ") + " (values hidden)"
		}
		fmt.Fprintf(tw, "%s%s\t%s\t%s\n", gateway.PathPrefix, name, up.URL.Redacted(), added)
	}
	if err := tw.Flush(); err != nil {
		return fail(s, exitError, err)
	}
	if err := gateway.CheckListen(g); err != nil {
		return fail(s, exitUsage, err)
	}
	return exitOK
}

func size(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%dGiB", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%dMiB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%dKiB", n>>10)
	default:
		return fmt.Sprintf("%dB", n)
	}
}
