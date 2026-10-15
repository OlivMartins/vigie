package cli

import (
	"context"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/OlivMartins/vigie/internal/config"
	"github.com/OlivMartins/vigie/internal/gateway"
	"github.com/OlivMartins/vigie/internal/secrets"
	"github.com/OlivMartins/vigie/internal/telemetry"
	"github.com/OlivMartins/vigie/internal/version"
)

func gatewayCommand(ctx context.Context, args []string, s Streams) int {
	fs := newFlagSet("gateway", "vigie gateway [flags]", s)
	configPath := fs.String("config", "", "configuration file (default: $"+config.EnvConfig+", then the user configuration directory)")
	listen := fs.String("listen", "", "address to listen on, host:port (default: gateway.listen, else "+config.DefaultListen+")")
	mode := fs.String("mode", "", "override the policy mode of every server: enforce or audit")
	traceFile := fs.String("trace-file", "", "also write every span to this file, one JSON object per line")
	logLevel := fs.String("log-level", "info", "log level on standard error: debug, info, warn or error")
	logFormat := fs.String("log-format", "text", "log format on standard error: text or json")
	if err := fs.Parse(args); err != nil {
		if isHelp(err) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		return usageError(s, fs, "unexpected argument %q", fs.Arg(0))
	}
	switch config.Mode(*mode) {
	case "", config.ModeEnforce, config.ModeAudit:
	default:
		return usageError(s, fs, "invalid --mode %q: use enforce or audit", *mode)
	}
	logger, err := newLogger(s, *logLevel, *logFormat)
	if err != nil {
		return usageError(s, fs, "%v", err)
	}
	path, err := config.Resolve(*configPath)
	if err != nil {
		return fail(s, exitUsage, err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return fail(s, exitUsage, err)
	}
	if *listen != "" {
		if _, _, err := net.SplitHostPort(*listen); err != nil {
			return usageError(s, fs, "invalid --listen %q: use host:port", *listen)
		}
		cfg.Gateway.Listen = *listen
	}
	auditLog, err := openAudit(cfg)
	if err != nil {
		return fail(s, exitUsage, err)
	}
	if auditLog != nil {
		defer auditLog.Close()
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
	g, err := gateway.New(gateway.Options{
		Config:    cfg,
		Telemetry: tel,
		Scanner:   secrets.NewScanner(),
		Logger:    logger,
		Audit:     auditLog,
		Mode:      config.Mode(*mode),
	})
	if err != nil {
		return fail(s, exitUsage, err)
	}
	ln, err := net.Listen("tcp", cfg.Gateway.Listen)
	if err != nil {
		return fail(s, exitError, err)
	}
	scheme := "http"
	if cfg.Gateway.TLSCertFile != "" {
		scheme = "https"
	}
	source := cfg.Source
	if source == "" {
		source = "built-in defaults"
	}
	auth := "none"
	if n := len(cfg.Gateway.Tokens); n > 0 {
		auth = "bearer tokens"
	}
	logger.Info("vigie gateway started", "version", version.Version(), "listen", scheme+"://"+ln.Addr().String(),
		"config", source, "auth", auth, "tracing", tel.Tracing)
	for _, name := range g.Servers() {
		attrs := []any{"path", gateway.PathPrefix + name, "upstream", g.Upstream(name)}
		if p := g.PinsPath(name); p != "" {
			attrs = append(attrs, "pins", p)
		}
		if strings.HasPrefix(g.Upstream(name), "http://") {
			attrs = append(attrs, "warning", "plain HTTP: requests and credentials travel unencrypted")
		}
		logger.Info("serving "+name, attrs...)
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := g.Serve(ctx, ln); err != nil {
		logger.Error("gateway failed", "error", err)
		return exitError
	}
	logger.Info("vigie gateway stopped")
	return exitOK
}
