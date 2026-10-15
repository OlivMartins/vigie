package gateway

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/OlivMartins/vigie/internal/config"
	"github.com/OlivMartins/vigie/internal/telemetry"
)

// cannedTransport answers every request in memory, with the id of the
// request: what is measured is the gateway's own work, not the network.
type cannedTransport struct {
	contentType string
	result      string
}

func (c cannedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	id := string(body[strings.Index(string(body), `"id":`)+5 : strings.Index(string(body), `,"method"`)])
	msg := `{"jsonrpc":"2.0","id":` + id + `,"result":` + c.result + `}`
	if c.contentType == "text/event-stream" {
		msg = "id: 1\ndata: \n\nid: 2\ndata: " + msg + "\n\n"
	}
	return &http.Response{
		StatusCode: 200, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header:        http.Header{"Content-Type": {c.contentType}},
		Body:          io.NopCloser(strings.NewReader(msg)),
		ContentLength: int64(len(msg)), Request: r,
	}, nil
}

func benchGateway(b *testing.B, contentType string) http.Handler {
	b.Helper()
	dir := b.TempDir()
	path := filepath.Join(dir, "vigie.yaml")
	_ = os.WriteFile(path, []byte("version: 1\npins_dir: "+filepath.Join(dir, "pins")+"\nservers:\n  billing:\n    url: http://billing.internal/mcp\n"), 0o600)
	cfg, err := config.Load(path)
	if err != nil {
		b.Fatal(err)
	}
	// Spans are recorded but not exported: export runs in the background.
	tel, err := telemetry.NewProvider(sdktrace.NewTracerProvider(), sdkmetric.NewMeterProvider())
	if err != nil {
		b.Fatal(err)
	}
	result := `{"content":[{"type":"text","text":"` + strings.Repeat("invoice line ", 80) + `"}],"isError":false,"resultType":"complete"}`
	g, err := New(Options{Config: cfg, Telemetry: tel, Logger: slog.New(slog.DiscardHandler),
		Transport: cannedTransport{contentType: contentType, result: result}})
	if err != nil {
		b.Fatal(err)
	}
	return g.Handler()
}

// benchmarkToolCall measures one tools/call exchange of revision 2026-07-28:
// header checks, policy, two spans, context injection, response relayed.
func benchmarkToolCall(b *testing.B, contentType string) {
	h := benchGateway(b, contentType)
	arguments := `{"id":"F-2026-0042","customer":"ACME","note":"` + strings.Repeat("lorem ipsum ", 80) + `"}`
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"get_invoice","arguments":%s,"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`, i, arguments)
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/mcp/billing", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2026-07-28")
		req.Header.Set("Mcp-Method", "tools/call")
		req.Header.Set("Mcp-Name", "get_invoice")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":`+strconv.Itoa(i)) {
			b.Fatalf("%d %s", w.Code, w.Body)
		}
	}
}

func BenchmarkGatewayToolCallJSON(b *testing.B) { benchmarkToolCall(b, "application/json") }

func BenchmarkGatewayToolCallSSE(b *testing.B) { benchmarkToolCall(b, "text/event-stream") }
