package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/OlivMartins/vigie/internal/config"
	"github.com/OlivMartins/vigie/internal/pins"
	"github.com/OlivMartins/vigie/internal/policy"
	"github.com/OlivMartins/vigie/internal/proxy"
	"github.com/OlivMartins/vigie/internal/secrets"
	"github.com/OlivMartins/vigie/internal/telemetry"
	"github.com/OlivMartins/vigie/internal/version"
)

func runCommand(ctx context.Context, args []string, s Streams) int {
	fs := newFlagSet("run", "vigie run --server NAME [flags] -- COMMAND [ARGS...]", s)
	server := fs.String("server", "", "name of the upstream server: selects its policy section and its pin file (required)")
	configPath := fs.String("config", "", "configuration file (default: $"+config.EnvConfig+", then the user configuration directory)")
	mode := fs.String("mode", "", "override the policy mode: enforce or audit")
	traceFile := fs.String("trace-file", "", "also write every span to this file, one JSON object per line")
	logLevel := fs.String("log-level", "info", "log level on standard error: debug, info, warn or error")
	logFormat := fs.String("log-format", "text", "log format on standard error: text or json")
	if err := fs.Parse(args); err != nil {
		if isHelp(err) {
			return exitOK
		}
		return exitUsage
	}
	command := fs.Args()
	if *server == "" {
		return usageError(s, fs, "--server is required")
	}
	if !pins.ValidServerName(*server) {
		return usageError(s, fs, "invalid --server %q: use letters, digits, '.', '_' or '-'", *server)
	}
	if len(command) == 0 {
		return usageError(s, fs, "missing server command after --")
	}

	logger, err := newLogger(s, *logLevel, *logFormat)
	if err != nil {
		return usageError(s, fs, "%v", err)
	}
	logger = logger.With("vigie", *server)

	path, err := config.Resolve(*configPath)
	if err != nil {
		return fail(s, exitUsage, err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return fail(s, exitUsage, err)
	}
	pol := cfg.Policy(*server)
	switch config.Mode(*mode) {
	case "":
	case config.ModeEnforce, config.ModeAudit:
		pol.Mode = config.Mode(*mode)
	default:
		return usageError(s, fs, "invalid --mode %q: use enforce or audit", *mode)
	}

	var store *pins.Store
	if pol.Pins.Enabled {
		if store, err = pins.Open(cfg.PinsDir, *server); err != nil {
			return fail(s, exitUsage, err)
		}
		if err := store.BeginSession(); err != nil {
			return fail(s, exitError, fmt.Errorf("%w (fix the file or delete it to start over)", err))
		}
	}

	tel, err := telemetry.Setup(ctx, telemetry.Options{
		ServiceVersion: version.Version(),
		TraceFile:      *traceFile,
		Logger:         logger,
	})
	if err != nil {
		return fail(s, exitUsage, err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := tel.Shutdown(sctx); err != nil {
			logger.Warn("telemetry not fully flushed", "error", err)
		}
	}()

	source := cfg.Source
	if source == "" {
		source = "built-in defaults"
	}
	attrs := []any{"version", version.Version(), "mode", pol.Mode, "config", source, "tracing", tel.Tracing}
	if store != nil {
		attrs = append(attrs, "pins", store.Path())
	}
	if path != "" && !cfg.HasServer(*server) {
		attrs = append(attrs, "policy", "defaults (no section for this server)")
	}
	logger.Info("vigie started", attrs...)

	catchSIGPIPE()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	scanner := secrets.NewScanner()
	code, err := proxy.Run(ctx, proxy.Process{
		Command: command,
		Stdin:   s.In,
		Stdout:  s.Out,
		Stderr:  s.Err,
	}, proxy.Options{
		Engine:         policy.NewEngine(*server, pol, store, scanner),
		Telemetry:      tel,
		Scanner:        scanner,
		Logger:         logger,
		Propagate:      cfg.Telemetry.PropagateContext,
		CaptureContent: cfg.Telemetry.CaptureContent,
	})
	if err != nil {
		logger.Error("relay failed", "error", err)
	}
	logger.Info("vigie stopped", "exit_code", code)
	return code
}

func newLogger(s Streams, level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid --log-level %q: use debug, info, warn or error", level)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(s.Err, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(s.Err, opts)), nil
	default:
		return nil, fmt.Errorf("invalid --log-format %q: use text or json", format)
	}
}
