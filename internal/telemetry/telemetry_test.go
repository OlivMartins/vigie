package telemetry

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestExtractInject(t *testing.T) {
	const incoming = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	params := map[string]json.RawMessage{
		"name":  json.RawMessage(`"get_invoice"`),
		"_meta": json.RawMessage(`{"traceparent":"` + incoming + `","tracestate":"rojo=1","baggage":"tenant=acme","progressToken":7,"io.modelcontextprotocol/protocolVersion":"2026-07-28"}`),
	}
	ctx := Extract(context.Background(), params)
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsRemote() || sc.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" || sc.SpanID().String() != "00f067aa0ba902b7" {
		t.Fatalf("extracted %v", sc)
	}

	child := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: sc.TraceID(), SpanID: trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8}, TraceFlags: trace.FlagsSampled,
	})
	out, changed, err := Inject(trace.ContextWithSpanContext(ctx, child), params)
	if err != nil || !changed {
		t.Fatalf("Inject: %v %v", changed, err)
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(out["_meta"], &meta); err != nil {
		t.Fatal(err)
	}
	if string(meta["traceparent"]) != `"00-4bf92f3577b34da6a3ce929d0e0e4736-0102030405060708-01"` {
		t.Errorf("traceparent = %s", meta["traceparent"])
	}
	if _, kept := meta["tracestate"]; kept {
		t.Errorf("stale tracestate kept: %s", meta["tracestate"])
	}
	if string(meta["baggage"]) != `"tenant=acme"` {
		t.Errorf("baggage = %s", meta["baggage"])
	}
	if string(meta["progressToken"]) != "7" || string(meta["io.modelcontextprotocol/protocolVersion"]) != `"2026-07-28"` {
		t.Errorf("other _meta keys altered: %v", meta)
	}
	if string(out["name"]) != `"get_invoice"` {
		t.Errorf("other params altered: %v", out)
	}
}

func TestInjectCreatesParamsAndLeavesMalformedMeta(t *testing.T) {
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	out, changed, err := Inject(ctx, nil)
	if err != nil || !changed || !strings.Contains(string(out["_meta"]), "traceparent") {
		t.Errorf("nil params: %v %v %v", out, changed, err)
	}
	bad := map[string]json.RawMessage{"_meta": json.RawMessage(`"not an object"`)}
	out, changed, _ = Inject(ctx, bad)
	if changed || string(out["_meta"]) != `"not an object"` {
		t.Errorf("malformed _meta rewritten: %s", out["_meta"])
	}
	if got := Extract(context.Background(), bad); trace.SpanContextFromContext(got).IsValid() {
		t.Error("nothing to extract from a malformed _meta")
	}
}

func TestExporterChoice(t *testing.T) {
	for _, k := range []string{"OTEL_TRACES_EXPORTER", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"} {
		t.Setenv(k, "")
	}
	if v, _ := exporterChoice("TRACES"); v != "none" {
		t.Errorf("no endpoint: %s", v)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
	if v, _ := exporterChoice("TRACES"); v != "otlp" {
		t.Errorf("endpoint set: %s", v)
	}
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	if v, _ := exporterChoice("TRACES"); v != "none" {
		t.Errorf("explicit none: %s", v)
	}
	t.Setenv("OTEL_TRACES_EXPORTER", "console")
	if _, err := exporterChoice("TRACES"); err == nil || !strings.Contains(err.Error(), "standard output") {
		t.Errorf("console must be refused: %v", err)
	}
	t.Setenv("OTEL_TRACES_EXPORTER", "zipkin")
	if _, err := exporterChoice("TRACES"); err == nil {
		t.Error("unsupported exporter accepted")
	}
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/json")
	if _, err := otlpProtocol("TRACES"); err == nil {
		t.Error("http/json is not supported by the Go SDK")
	}
}

func TestSetupDisabledByDefault(t *testing.T) {
	for _, k := range []string{"OTEL_TRACES_EXPORTER", "OTEL_METRICS_EXPORTER", "OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"} {
		t.Setenv(k, "")
	}
	p, err := Setup(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Tracing {
		t.Error("tracing must stay off without an endpoint or a trace file")
	}
}

func TestTraceFile(t *testing.T) {
	for _, k := range []string{"OTEL_TRACES_EXPORTER", "OTEL_METRICS_EXPORTER", "OTEL_EXPORTER_OTLP_ENDPOINT"} {
		t.Setenv(k, "")
	}
	path := filepath.Join(t.TempDir(), "spans.jsonl")
	p, err := Setup(context.Background(), Options{TraceFile: path, ServiceVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Tracing {
		t.Fatal("a trace file turns tracing on")
	}
	_, span := p.Tracer.Start(context.Background(), "tools/call get_invoice")
	span.End()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var span1 struct {
		Name     string
		Resource []struct {
			Key   string
			Value struct{ Value any }
		}
	}
	if err := json.Unmarshal([]byte(strings.SplitN(string(data), "\n", 2)[0]), &span1); err != nil {
		t.Fatalf("trace file is not JSON lines: %v\n%s", err, data)
	}
	if span1.Name != "tools/call get_invoice" {
		t.Errorf("span name = %q", span1.Name)
	}
	found := false
	for _, kv := range span1.Resource {
		if kv.Key == "service.name" && kv.Value.Value == "vigie" {
			found = true
		}
	}
	if !found {
		t.Errorf("service.name=vigie missing from resource: %s", data)
	}
}
