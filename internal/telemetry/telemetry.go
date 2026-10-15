// Package telemetry sets up OpenTelemetry for Vigie: spans and metrics that
// follow the MCP semantic conventions, exported with OTLP.
//
// Export is configured with the standard OTEL_* environment variables. With
// stdio, standard output carries the protocol itself: nothing here may write
// to it, so the console exporter is refused and a trace file is offered
// instead.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// ScopeName is the instrumentation scope of every span and metric.
const ScopeName = "github.com/OlivMartins/vigie"

// Options configures Setup.
type Options struct {
	// ServiceVersion is recorded as service.version.
	ServiceVersion string
	// TraceFile, when set, receives every span as one JSON object per line.
	TraceFile string
	// Logger receives export errors, rate limited.
	Logger *slog.Logger
}

// Provider holds the tracer, the meter and the instruments.
type Provider struct {
	Tracer      trace.Tracer
	Instruments *Instruments
	// Tracing reports whether spans are recorded anywhere. When false,
	// Vigie leaves relayed messages untouched instead of injecting a context
	// nobody records.
	Tracing bool

	shutdowns []func(context.Context) error
}

// Instruments are the metrics Vigie records.
type Instruments struct {
	ServerDuration metric.Float64Histogram
	ClientDuration metric.Float64Histogram
	Decisions      metric.Int64Counter
}

// NewProvider builds a provider on existing tracer and meter providers, for
// tests and for programs that configure OpenTelemetry themselves.
func NewProvider(tp trace.TracerProvider, mp metric.MeterProvider) (*Provider, error) {
	inst, err := newInstruments(mp.Meter(ScopeName))
	if err != nil {
		return nil, err
	}
	return &Provider{Tracer: tp.Tracer(ScopeName), Instruments: inst, Tracing: true}, nil
}

// Disabled returns a provider that records nothing.
func Disabled() *Provider {
	p := &Provider{Tracer: tracenoop.NewTracerProvider().Tracer(ScopeName)}
	p.Instruments, _ = newInstruments(metricnoop.NewMeterProvider().Meter(ScopeName))
	return p
}

// Setup configures exporters from the environment and the options. Telemetry
// stays off unless an OTLP endpoint is set, an exporter is chosen explicitly
// or a trace file is given: a local MCP server should not log export errors
// because no collector runs on the machine.
func Setup(ctx context.Context, opts Options) (*Provider, error) {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	otel.SetErrorHandler(newThrottledHandler(opts.Logger, 30*time.Second))
	otel.SetTextMapPropagator(Propagator)

	traceExporter, err := exporterChoice("TRACES")
	if err != nil {
		return nil, err
	}
	metricExporter, err := exporterChoice("METRICS")
	if err != nil {
		return nil, err
	}
	if traceExporter == "none" && metricExporter == "none" && opts.TraceFile == "" {
		return Disabled(), nil
	}

	res, err := newResource(ctx, opts.ServiceVersion)
	if errors.Is(err, resource.ErrPartialResource) {
		opts.Logger.Warn("some resource attributes were ignored", "error", err)
	} else if err != nil {
		return nil, err
	}
	p := &Provider{}

	var tpOpts []sdktrace.TracerProviderOption
	tpOpts = append(tpOpts, sdktrace.WithResource(res))
	if traceExporter == "otlp" {
		exp, err := newTraceExporter(ctx)
		if err != nil {
			return nil, err
		}
		tpOpts = append(tpOpts, sdktrace.WithBatcher(exp))
		p.Tracing = true
	}
	if opts.TraceFile != "" {
		f, err := os.OpenFile(opts.TraceFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, fmt.Errorf("opening trace file: %w", err)
		}
		exp, err := stdouttrace.New(stdouttrace.WithWriter(f))
		if err != nil {
			f.Close()
			return nil, err
		}
		// Synchronous: the file is meant to be read while Vigie runs.
		tpOpts = append(tpOpts, sdktrace.WithSyncer(exp))
		p.shutdowns = append(p.shutdowns, func(context.Context) error { return f.Close() })
		p.Tracing = true
	}
	if p.Tracing {
		tp := sdktrace.NewTracerProvider(tpOpts...)
		p.Tracer = tp.Tracer(ScopeName, trace.WithInstrumentationVersion(opts.ServiceVersion))
		// Spans are flushed before the trace file is closed.
		p.shutdowns = append([]func(context.Context) error{tp.Shutdown}, p.shutdowns...)
	} else {
		p.Tracer = tracenoop.NewTracerProvider().Tracer(ScopeName)
	}

	var meter metric.Meter
	if metricExporter == "otlp" {
		exp, err := newMetricExporter(ctx)
		if err != nil {
			return nil, err
		}
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)))
		meter = mp.Meter(ScopeName, metric.WithInstrumentationVersion(opts.ServiceVersion))
		p.shutdowns = append(p.shutdowns, mp.Shutdown)
	} else {
		meter = metricnoop.NewMeterProvider().Meter(ScopeName)
	}
	if p.Instruments, err = newInstruments(meter); err != nil {
		return nil, err
	}
	return p, nil
}

// Shutdown flushes and closes the exporters.
func (p *Provider) Shutdown(ctx context.Context) error {
	var errs []error
	for _, s := range p.shutdowns {
		errs = append(errs, s(ctx))
	}
	return errors.Join(errs...)
}

// exporterChoice reads OTEL_<SIGNAL>_EXPORTER, defaulting to otlp when an
// OTLP endpoint is configured and to none otherwise.
func exporterChoice(signal string) (string, error) {
	v := strings.TrimSpace(os.Getenv("OTEL_" + signal + "_EXPORTER"))
	if v == "" {
		if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_"+signal+"_ENDPOINT") != "" {
			return "otlp", nil
		}
		return "none", nil
	}
	switch v {
	case "otlp", "none":
		return v, nil
	case "console":
		return "", fmt.Errorf("OTEL_%s_EXPORTER=console would write to standard output, which carries the MCP protocol: use --trace-file instead", signal)
	default:
		return "", fmt.Errorf("OTEL_%s_EXPORTER=%s is not supported (use otlp or none)", signal, v)
	}
}

func otlpProtocol(signal string) (string, error) {
	v := os.Getenv("OTEL_EXPORTER_OTLP_" + signal + "_PROTOCOL")
	if v == "" {
		v = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	switch v {
	case "", "http/protobuf":
		return "http/protobuf", nil
	case "grpc":
		return "grpc", nil
	default:
		return "", fmt.Errorf("OTLP protocol %q is not supported (use http/protobuf or grpc)", v)
	}
}

func newTraceExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	proto, err := otlpProtocol("TRACES")
	if err != nil {
		return nil, err
	}
	if proto == "grpc" {
		return otlptracegrpc.New(ctx)
	}
	return otlptracehttp.New(ctx)
}

func newMetricExporter(ctx context.Context) (sdkmetric.Exporter, error) {
	proto, err := otlpProtocol("METRICS")
	if err != nil {
		return nil, err
	}
	if proto == "grpc" {
		return otlpmetricgrpc.New(ctx)
	}
	return otlpmetrichttp.New(ctx)
}

// newResource describes this process. OTEL_SERVICE_NAME and
// OTEL_RESOURCE_ATTRIBUTES override the defaults.
func newResource(ctx context.Context, version string) (*resource.Resource, error) {
	return resource.New(ctx,
		resource.WithAttributes(
			attribute.String("service.name", "vigie"),
			attribute.String("service.version", version),
			attribute.String("service.instance.id", uuid.NewString()),
		),
		resource.WithHost(),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
	)
}

func newInstruments(m metric.Meter) (*Instruments, error) {
	server, err := m.Float64Histogram(MetricServerDuration,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of MCP requests received by Vigie, as seen by the caller."),
		metric.WithExplicitBucketBoundaries(DurationBuckets...))
	if err != nil {
		return nil, err
	}
	client, err := m.Float64Histogram(MetricClientDuration,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of MCP requests relayed by Vigie, until the peer answered."),
		metric.WithExplicitBucketBoundaries(DurationBuckets...))
	if err != nil {
		return nil, err
	}
	decisions, err := m.Int64Counter(MetricDecisions,
		metric.WithUnit("{decision}"),
		metric.WithDescription("Policy decisions taken on tool lists and tool calls."))
	if err != nil {
		return nil, err
	}
	return &Instruments{ServerDuration: server, ClientDuration: client, Decisions: decisions}, nil
}

// throttledHandler logs export errors at most once per interval, with the
// number of errors it held back: a collector that is down must not flood the
// MCP host's logs.
type throttledHandler struct {
	logger   *slog.Logger
	interval time.Duration

	mu         sync.Mutex
	last       time.Time
	suppressed int
}

func newThrottledHandler(l *slog.Logger, interval time.Duration) *throttledHandler {
	return &throttledHandler{logger: l, interval: interval}
}

func (h *throttledHandler) Handle(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	if !h.last.IsZero() && now.Sub(h.last) < h.interval {
		h.suppressed++
		return
	}
	h.logger.Warn("telemetry export failed", "error", err, "suppressed", h.suppressed)
	h.last, h.suppressed = now, 0
}
