package proxy

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/OlivMartins/vigie/internal/config"
	"github.com/OlivMartins/vigie/internal/jsonrpc"
	"github.com/OlivMartins/vigie/internal/pins"
	"github.com/OlivMartins/vigie/internal/policy"
	"github.com/OlivMartins/vigie/internal/secrets"
	"github.com/OlivMartins/vigie/internal/telemetry"
)

// benchRelay returns a relay with tracing, metrics, pinning and secret
// scanning on, writing nowhere: what is measured is Vigie's own work.
func benchRelay(b *testing.B) *Relay {
	b.Helper()
	cfg, _ := config.Default()
	store, _ := pins.Open(b.TempDir(), "billing")
	// Spans are recorded but not exported: export runs in the background.
	tp := sdktrace.NewTracerProvider()
	tel, err := telemetry.NewProvider(tp, sdkmetric.NewMeterProvider())
	if err != nil {
		b.Fatal(err)
	}
	scanner := secrets.NewScanner()
	return NewRelay(Options{
		Engine:    policy.NewEngine("billing", cfg.Policy("billing"), store, scanner),
		Telemetry: tel,
		Scanner:   scanner,
		Logger:    slog.New(slog.DiscardHandler),
		Propagate: true,
	}, jsonrpc.NewWriter(io.Discard), jsonrpc.NewWriter(io.Discard))
}

// BenchmarkToolCall measures one tools/call round trip: request in, policy,
// two spans, context injection, response out.
func BenchmarkToolCall(b *testing.B) {
	r := benchRelay(b)
	arguments := `{"id":"F-2026-0042","customer":"ACME","note":"` + strings.Repeat("lorem ipsum ", 80) + `"}`
	result := `{"content":[{"type":"text","text":"` + strings.Repeat("invoice line ", 80) + `"}],"isError":false}`
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		req := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"get_invoice","arguments":%s}}`, i, arguments)
		if err := r.HandleClientFrame([]byte(req)); err != nil {
			b.Fatal(err)
		}
		resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, i, result)
		if err := r.HandleServerFrame([]byte(resp)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkToolsList measures a tools/list of 50 tools: every definition is
// fingerprinted and compared with the pin file.
func BenchmarkToolsList(b *testing.B) {
	r := benchRelay(b)
	var tools []string
	for i := range 50 {
		tools = append(tools, fmt.Sprintf(`{"name":"tool_%d","description":"%s","inputSchema":{"type":"object","properties":{"id":{"type":"string"},"limit":{"type":"integer"}}}}`,
			i, strings.Repeat("Does something useful. ", 10)))
	}
	result := `{"tools":[` + strings.Join(tools, ",") + `]}`
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		if err := r.HandleClientFrame(fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%d,"method":"tools/list"}`, i)); err != nil {
			b.Fatal(err)
		}
		if err := r.HandleServerFrame(fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%d,"result":%s}`, i, result)); err != nil {
			b.Fatal(err)
		}
	}
}
