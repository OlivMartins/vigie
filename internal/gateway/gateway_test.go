package gateway

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/OlivMartins/vigie/internal/audit"
	"github.com/OlivMartins/vigie/internal/config"
	"github.com/OlivMartins/vigie/internal/telemetry"
	"github.com/OlivMartins/vigie/internal/testserver"
)

// awsKey is assembled at run time: the repository holds no literal secret.
var awsKey = "AKIA" + "Z7QJ4XWVN2L5KT3M"

const modernVersion = "2026-07-28"

func billingTools(description string) []testserver.Tool {
	return []testserver.Tool{
		{Name: "get_invoice", Description: description, InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}},
		}, Call: func(args map[string]any) (string, bool) {
			return fmt.Sprintf("invoice %v: 1200 EUR", args["id"]), false
		}},
		{Name: "send_reminder", Description: "Send a payment reminder"},
		{Name: "leaky", Description: "Returns a credential", Call: func(map[string]any) (string, bool) { return "here: " + awsKey, false }},
		{Name: "delete_invoice", Description: "Delete an invoice"},
	}
}

type harness struct {
	t        *testing.T
	up       *testserver.HTTPServer
	upstream *httptest.Server
	gateway  *Gateway
	front    *httptest.Server
	spans    *tracetest.SpanRecorder
	dir      string
}

type harnessOptions struct {
	sse, sessions bool
	tools         []testserver.Tool
	gatewayYAML   string // lines under "gateway:"
	serverYAML    string // extra lines under "servers.billing:"
	topYAML       string // extra top-level sections
	dir           string // reuse a pins directory
	script        func(w http.ResponseWriter, r *http.Request, body []byte) bool
	upstreamURL   string // instead of the test server
}

func newHarness(t *testing.T, o harnessOptions) *harness {
	t.Helper()
	if o.tools == nil {
		o.tools = billingTools("Fetch an invoice by its number.")
	}
	if o.dir == "" {
		o.dir = t.TempDir()
	}
	h := &harness{t: t, dir: o.dir, spans: tracetest.NewSpanRecorder()}
	h.up = testserver.NewHTTP(&testserver.Server{Name: "billing", Version: "1.0.0", Tools: o.tools})
	h.up.SSE, h.up.Sessions, h.up.Script = o.sse, o.sessions, o.script
	h.upstream = httptest.NewServer(h.up)
	t.Cleanup(h.upstream.Close)
	url := o.upstreamURL
	if url == "" {
		url = h.upstream.URL + "/mcp"
	}
	yaml := fmt.Sprintf("version: 1\npins_dir: %s\naudit:\n  file: %s\n%s", filepath.Join(o.dir, "pins"), filepath.Join(o.dir, "audit.jsonl"), o.topYAML)
	if o.gatewayYAML != "" {
		yaml += "gateway:\n" + o.gatewayYAML
	}
	yaml += fmt.Sprintf("servers:\n  billing:\n    url: %s\n    tools:\n      deny: [\"delete_*\"]\n%s", url, o.serverYAML)
	path := filepath.Join(o.dir, "vigie.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(h.spans))
	tel, err := telemetry.NewProvider(tp, sdkmetric.NewMeterProvider())
	if err != nil {
		t.Fatal(err)
	}
	log, err := audit.Open(audit.Options{Path: cfg.Audit.File})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	h.gateway, err = New(Options{Config: cfg, Telemetry: tel, Logger: slog.New(slog.DiscardHandler), Audit: log})
	if err != nil {
		t.Fatal(err)
	}
	h.front = httptest.NewServer(h.gateway.Handler())
	t.Cleanup(h.front.Close)
	return h
}

// request sends a request to the gateway. A header set to "" is removed.
func (h *harness) request(method, body string, headers map[string]string) (*http.Response, string) {
	h.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.front.URL+"/mcp/billing", rd)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}

func (h *harness) post(body string, headers map[string]string) (*http.Response, string) {
	h.t.Helper()
	return h.request(http.MethodPost, body, headers)
}

// modern builds a 2026-07-28 request and its mirrored headers.
func modern(id int, method, params string) (string, map[string]string) {
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"` + modernVersion + `"}`
	if params == "" {
		params = "{" + meta + "}"
	} else {
		params = "{" + params + "," + meta + "}"
	}
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, id, method, params)
	headers := map[string]string{"MCP-Protocol-Version": modernVersion, "Mcp-Method": method}
	var p map[string]any
	_ = json.Unmarshal([]byte(params), &p)
	if name, ok := p["name"].(string); ok {
		headers["Mcp-Name"] = name
	}
	if uri, ok := p["uri"].(string); ok {
		headers["Mcp-Name"] = uri
	}
	return body, headers
}

func (h *harness) span(name string, kind trace.SpanKind) sdktrace.ReadOnlySpan {
	h.t.Helper()
	for _, s := range h.spans.Ended() {
		if s.Name() == name && s.SpanKind() == kind {
			return s
		}
	}
	h.t.Fatalf("no %s span %q among %d", kind, name, len(h.spans.Ended()))
	return nil
}

func attr(s sdktrace.ReadOnlySpan, key attribute.Key) string {
	for _, a := range s.Attributes() {
		if a.Key == key {
			return a.Value.String()
		}
	}
	return ""
}

func (h *harness) upstreamReceived(substr string) bool {
	for _, r := range h.up.Requests() {
		if strings.Contains(string(r.Body), substr) {
			return true
		}
	}
	return false
}

func toolNames(t *testing.T, body string) []string {
	t.Helper()
	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
			TTL *int `json:"ttlMs"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("not a JSON-RPC response: %v\n%s", err, body)
	}
	var names []string
	for _, tool := range resp.Result.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// sseData returns the data of the message events of an SSE body.
func sseData(body string) []string {
	var out []string
	for _, block := range strings.Split(body, "\n\n") {
		var data []string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data: ") {
				data = append(data, strings.TrimPrefix(line, "data: "))
			}
		}
		if len(data) > 0 && strings.Join(data, "") != "" {
			out = append(out, strings.Join(data, "\n"))
		}
	}
	return out
}

func TestModernRequests(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	body, headers := modern(1, "tools/list", "")
	resp, out := h.post(body, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list: %d %s", resp.StatusCode, out)
	}
	if got := strings.Join(toolNames(t, out), ","); got != "get_invoice,send_reminder,leaky" {
		t.Errorf("tools seen by the client: %s", got)
	}
	if !strings.Contains(out, `"ttlMs":0`) {
		t.Errorf("a filtered list must not stay in the client cache: %s", out)
	}

	body, headers = modern(2, "tools/call", `"name":"get_invoice","arguments":{"id":"F-1"}`)
	resp, out = h.post(body, headers)
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, "invoice F-1: 1200 EUR") {
		t.Fatalf("allowed call: %d %s", resp.StatusCode, out)
	}

	body, headers = modern(3, "tools/call", `"name":"send_reminder","arguments":{"note":"key `+awsKey+`"}`)
	resp, out = h.post(body, headers)
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, `"isError":true`) || !strings.Contains(out, `"resultType":"complete"`) {
		t.Fatalf("blocked call: %d %s", resp.StatusCode, out)
	}
	if !strings.Contains(out, "arguments.note looks like an AWS access key") {
		t.Errorf("refusal reason: %s", out)
	}
	if h.upstreamReceived(awsKey) {
		t.Error("the key reached the server")
	}

	server := h.span("tools/call get_invoice", trace.SpanKindServer)
	client := h.span("tools/call get_invoice", trace.SpanKindClient)
	for key, want := range map[attribute.Key]string{
		telemetry.NetworkTransport:    "tcp",
		telemetry.NetworkProtocolName: "http",
		telemetry.MCPProtocolVersion:  modernVersion,
		telemetry.ClientAddress:       "127.0.0.1",
	} {
		if got := attr(server, key); got != want {
			t.Errorf("SERVER span %s = %q, want %q", key, got, want)
		}
	}
	if attr(client, telemetry.ServerAddress) != "127.0.0.1" || attr(client, telemetry.ServerPort) == "" {
		t.Errorf("CLIENT span lacks the upstream address: %v", client.Attributes())
	}
	if attr(client, telemetry.ClientAddress) != "" {
		t.Error("client.address belongs to the SERVER span only")
	}
	if client.Parent().SpanID() != server.SpanContext().SpanID() {
		t.Error("the CLIENT span must be a child of the SERVER span")
	}

	// The trace context reaches the server in params._meta and in headers.
	var relayed testserver.Recorded
	for _, r := range h.up.Requests() {
		if strings.Contains(string(r.Body), `"get_invoice"`) {
			relayed = r
		}
	}
	if !strings.Contains(string(relayed.Body), client.SpanContext().TraceID().String()) {
		t.Errorf("no trace context in the relayed body: %s", relayed.Body)
	}
	if !strings.Contains(relayed.Header.Get("Traceparent"), client.SpanContext().SpanID().String()) {
		t.Errorf("traceparent header: %q", relayed.Header.Get("Traceparent"))
	}
	if relayed.Header.Get("Mcp-Name") != "get_invoice" || relayed.Header.Get("Mcp-Method") != "tools/call" {
		t.Errorf("mirrored headers must reach the server: %v", relayed.Header)
	}
}

func TestHeadersMustMatchTheBody(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	call := func(id int) (string, map[string]string) {
		return modern(id, "tools/call", `"name":"get_invoice","arguments":{"id":"F-1"}`)
	}
	encoded := "=?base64?" + base64.StdEncoding.EncodeToString([]byte("get_invoice")) + "?="
	cases := []struct {
		name    string
		headers map[string]string
		status  int
	}{
		{"name differs", map[string]string{"Mcp-Name": "send_reminder"}, 400},
		{"method differs", map[string]string{"Mcp-Method": "tools/list"}, 400},
		{"version differs", map[string]string{"MCP-Protocol-Version": "2025-11-25"}, 400},
		{"name missing", map[string]string{"Mcp-Name": ""}, 400},
		{"method missing", map[string]string{"Mcp-Method": ""}, 400},
		{"version missing", map[string]string{"MCP-Protocol-Version": ""}, 400},
		{"invalid characters", map[string]string{"Mcp-Name": "get_invoiceé"}, 400},
		{"broken base64", map[string]string{"Mcp-Name": "=?base64?***?="}, 400},
		{"base64 name", map[string]string{"Mcp-Name": encoded}, 200},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body, headers := call(10 + i)
			for k, v := range c.headers {
				headers[k] = v
			}
			resp, out := h.post(body, headers)
			if resp.StatusCode != c.status {
				t.Fatalf("status %d, want %d: %s", resp.StatusCode, c.status, out)
			}
			if c.status == 400 && !strings.Contains(out, `"code":-32020`) {
				t.Errorf("want a HeaderMismatch error: %s", out)
			}
		})
	}
	if n := len(h.up.Requests()); n != 1 {
		t.Errorf("only the valid request may reach the server, it got %d", n)
	}

	// Any revision: a mirrored header that disagrees with the body is refused.
	resp, out := h.post(`{"jsonrpc":"2.0","id":30,"method":"tools/call","params":{"name":"get_invoice"}}`,
		map[string]string{"MCP-Protocol-Version": "2025-11-25", "Mcp-Name": "send_reminder"})
	if resp.StatusCode != 400 || !strings.Contains(out, "-32020") {
		t.Errorf("legacy request with a lying header: %d %s", resp.StatusCode, out)
	}
	// Two values for one header are ambiguous.
	req, _ := http.NewRequest(http.MethodPost, h.front.URL+"/mcp/billing", strings.NewReader(`{"jsonrpc":"2.0","id":31,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Add("Mcp-Method", "tools/list")
	req.Header.Add("Mcp-Method", "tools/call")
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != 400 {
		t.Errorf("duplicate header: %d", r2.StatusCode)
	}
}

func TestLegacySessionOverSSE(t *testing.T) {
	h := newHarness(t, harnessOptions{sse: true, sessions: true})
	resp, out := h.post(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`, nil)
	session := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != 200 || session == "" {
		t.Fatalf("initialize: %d %q %s", resp.StatusCode, session, out)
	}
	if resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Errorf("stream headers: %v", resp.Header)
	}
	if !strings.Contains(out, "id: ev-1\n\n") && !strings.Contains(out, "id: ev-1\ndata: \n") {
		t.Errorf("the priming event must reach the client, with its id: %q", out)
	}
	if !strings.Contains(out, ": keep-alive") {
		t.Errorf("comments must pass: %q", out)
	}
	headers := map[string]string{"Mcp-Session-Id": session, "MCP-Protocol-Version": "2025-11-25"}
	resp, _ = h.post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, headers)
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("notification: %d", resp.StatusCode)
	}
	resp, out = h.post(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, headers)
	data := sseData(out)
	if resp.StatusCode != 200 || len(data) != 1 {
		t.Fatalf("tools/list: %d %q", resp.StatusCode, out)
	}
	if got := strings.Join(toolNames(t, data[0]), ","); got != "get_invoice,send_reminder,leaky" {
		t.Errorf("tools seen by the client: %s", got)
	}
	if !strings.Contains(out, "id: ev-4") {
		t.Errorf("event ids must survive filtering: %q", out)
	}
	span := h.span("tools/list", trace.SpanKindServer)
	if got := attr(span, telemetry.MCPSessionID); got == "" || got == session || got != sessionHash(session) {
		t.Errorf("mcp.session.id must hold a hash of the session id: %q", got)
	}
	if attr(span, telemetry.MCPProtocolVersion) != "2025-11-25" {
		t.Errorf("version from the header: %v", span.Attributes())
	}
	resp, _ = h.request(http.MethodDelete, "", headers)
	if resp.StatusCode != 200 {
		t.Errorf("DELETE: %d", resp.StatusCode)
	}
	resp, _ = h.post(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, headers)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("an ended session answers 404, relayed as is: %d", resp.StatusCode)
	}
}

func TestBatchWithABlockedCall(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	resp, out := h.post(`[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_invoice","arguments":{"id":"F-2"}}},`+
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"delete_invoice","arguments":{}}}]`,
		map[string]string{"MCP-Protocol-Version": "2025-03-26"})
	var batch []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &batch); err != nil || resp.StatusCode != 200 || len(batch) != 2 {
		t.Fatalf("batch: %d %s", resp.StatusCode, out)
	}
	if !strings.Contains(out, "invoice F-2") || !strings.Contains(out, `delete_invoice\" is denied`) {
		t.Errorf("one answer from the server, one from Vigie: %s", out)
	}
	if h.upstreamReceived("delete_invoice") {
		t.Error("the denied call reached the server")
	}
}

// Responses Vigie cannot match to a request of the exchange never reach the
// client: a lenient client could take them for the answer to another request.
func TestResponsesOnStreamsMustMatch(t *testing.T) {
	poisoned := `{"tools":[{"name":"delete_invoice","inputSchema":{"type":"object"}}]}`
	h := newHarness(t, harnessOptions{sessions: true, script: func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method != http.MethodPost || !strings.Contains(string(body), `"tools/list"`) {
			return false
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":\"1\",\"result\":%s}\n\n", poisoned)
		fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":1.0,\"result\":%s}\n\n", poisoned)
		fmt.Fprint(w, "event: other\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n")
		fmt.Fprint(w, "data: not json\n\n")
		fmt.Fprintf(w, "id: 9\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":%s}\n\n", poisoned)
		return true
	}})
	resp, _ := h.post(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`, nil)
	session := resp.Header.Get("Mcp-Session-Id")
	_, out := h.post(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, map[string]string{"Mcp-Session-Id": session})
	data := sseData(out)
	if len(data) != 1 {
		t.Fatalf("only the matching response may pass: %q", out)
	}
	if names := toolNames(t, data[0]); len(names) != 0 {
		t.Errorf("the denied tool must be filtered: %v", names)
	}
	if !strings.Contains(out, "id: 9") {
		t.Errorf("event id lost: %q", out)
	}
}

// Revisions up to 2025-11-25: a stream cut before its response is resumed
// with a GET, where the server replays the response. Vigie still filters it.
func TestResumedStreamsAreChecked(t *testing.T) {
	replay := make(chan string, 1)
	h := newHarness(t, harnessOptions{sessions: true, script: func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		switch {
		case r.Method == http.MethodPost && strings.Contains(string(body), `"tools/list"`):
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "id: s1-0\ndata: \n\nretry: 10\n\n")
			// The connection closes before the response.
			replay <- `{"jsonrpc":"2.0","id":7,"result":{"tools":[{"name":"get_invoice","inputSchema":{"type":"object"}},{"name":"delete_invoice","inputSchema":{"type":"object"}}]}}`
			return true
		case r.Method == http.MethodGet && r.Header.Get("Last-Event-Id") == "s1-0":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "id: s1-1\ndata: %s\n\n", <-replay)
			// An unsolicited response, for no request at all.
			fmt.Fprint(w, `id: s1-2`+"\n"+`data: {"jsonrpc":"2.0","id":8,"result":{"tools":[{"name":"delete_invoice"}]}}`+"\n\n")
			return true
		}
		return false
	}})
	resp, _ := h.post(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`, nil)
	session := resp.Header.Get("Mcp-Session-Id")
	headers := map[string]string{"Mcp-Session-Id": session, "MCP-Protocol-Version": "2025-11-25"}
	_, out := h.post(`{"jsonrpc":"2.0","id":7,"method":"tools/list"}`, headers)
	if len(sseData(out)) != 0 || !strings.Contains(out, "retry: 10") {
		t.Fatalf("interrupted stream: %q", out)
	}
	headers["Last-Event-ID"] = "s1-0"
	resp, out = h.request(http.MethodGet, "", headers)
	data := sseData(out)
	if resp.StatusCode != 200 || len(data) != 1 {
		t.Fatalf("resumed stream: %d %q", resp.StatusCode, out)
	}
	if got := strings.Join(toolNames(t, data[0]), ","); got != "get_invoice" {
		t.Errorf("replayed list must be filtered: %s", got)
	}
	if strings.Contains(out, `"id":8`) {
		t.Error("an unsolicited response passed")
	}
}

func TestServerFailures(t *testing.T) {
	t.Run("unreachable", func(t *testing.T) {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		addr := ln.Addr().String()
		ln.Close()
		h := newHarness(t, harnessOptions{upstreamURL: "http://" + addr + "/mcp"})
		body, headers := modern(5, "tools/call", `"name":"get_invoice","arguments":{}`)
		resp, out := h.post(body, headers)
		if resp.StatusCode != http.StatusBadGateway || !strings.Contains(out, `"id":5`) || !strings.Contains(out, "could not reach server") {
			t.Errorf("%d %s", resp.StatusCode, out)
		}
		if attr(h.span("tools/call get_invoice", trace.SpanKindClient), telemetry.ErrorType) != telemetry.ErrorUpstreamUnavailable {
			t.Error("error.type")
		}
	})
	t.Run("redirect", func(t *testing.T) {
		h := newHarness(t, harnessOptions{script: func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
			http.Redirect(w, r, "http://elsewhere.example/mcp", http.StatusTemporaryRedirect)
			return true
		}})
		body, headers := modern(6, "tools/list", "")
		resp, out := h.post(body, headers)
		if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Location") != "" {
			t.Errorf("a redirect must not reach the client: %d %v %s", resp.StatusCode, resp.Header, out)
		}
	})
	t.Run("not JSON-RPC", func(t *testing.T) {
		h := newHarness(t, harnessOptions{script: func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"tools":[{"name":"delete_invoice"}]}`)
			return true
		}})
		body, headers := modern(7, "tools/list", "")
		resp, out := h.post(body, headers)
		if resp.StatusCode != http.StatusBadGateway || strings.Contains(out, "delete_invoice") {
			t.Errorf("%d %s", resp.StatusCode, out)
		}
	})
	t.Run("HTTP error with a result", func(t *testing.T) {
		h := newHarness(t, harnessOptions{script: func(w http.ResponseWriter, _ *http.Request, body []byte) bool {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":9,"result":{"tools":[{"name":"delete_invoice"}]}}`)
			return true
		}})
		body, headers := modern(9, "tools/list", "")
		resp, out := h.post(body, headers)
		if resp.StatusCode != http.StatusBadRequest || strings.Contains(out, "delete_invoice") {
			t.Errorf("%d %s", resp.StatusCode, out)
		}
	})
	t.Run("HTTP error passes", func(t *testing.T) {
		h := newHarness(t, harnessOptions{script: func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://billing.example/.well-known/oauth-protected-resource"`)
			w.WriteHeader(http.StatusUnauthorized)
			return true
		}})
		body, headers := modern(8, "tools/list", "")
		resp, _ := h.post(body, headers)
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%d %v", resp.StatusCode, resp.Header)
		}
		if attr(h.span("tools/list", trace.SpanKindClient), telemetry.HTTPResponseStatusCode) != "401" {
			t.Error("http.response.status_code")
		}
	})
}

func TestRefusedRequests(t *testing.T) {
	t.Setenv("VIGIE_TEST_TOKEN", "0123456789abcdef0123456789abcdef")
	h := newHarness(t, harnessOptions{
		gatewayYAML: "  tokens:\n    equipe-data: ${VIGIE_TEST_TOKEN}\n  allowed_origins: [\"https://inspector.example\"]\n  max_request_size: 2KiB\n",
		serverYAML:  "    headers:\n      X-Api-Key: server-side-credential\n",
	})
	body, headers := modern(1, "tools/list", "")
	resp, _ := h.post(body, headers)
	if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("no token: %d", resp.StatusCode)
	}
	headers["Authorization"] = "Bearer wrong-token-wrong-token-wrong"
	if resp, _ = h.post(body, headers); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", resp.StatusCode)
	}
	headers["Authorization"] = "Bearer 0123456789abcdef0123456789abcdef"
	if resp, _ = h.post(body, headers); resp.StatusCode != http.StatusOK {
		t.Fatalf("valid token: %d", resp.StatusCode)
	}
	last := h.up.Requests()[len(h.up.Requests())-1]
	if last.Header.Get("Authorization") != "" || last.Header.Get("X-Api-Key") != "server-side-credential" {
		t.Errorf("the gateway's token stays with the gateway, the server gets its own: %v", last.Header)
	}
	if attr(h.span("tools/list", trace.SpanKindServer), telemetry.VigieClient) != "equipe-data" {
		t.Error("vigie.client.name")
	}

	headers["Origin"] = "https://evil.example"
	if resp, _ = h.post(body, headers); resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign origin: %d", resp.StatusCode)
	}
	headers["Origin"] = "https://inspector.example"
	resp, _ = h.post(body, headers)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != "https://inspector.example" {
		t.Errorf("allowed origin: %d %v", resp.StatusCode, resp.Header)
	}
	resp, _ = h.request(http.MethodOptions, "", map[string]string{"Origin": "https://inspector.example", "Access-Control-Request-Headers": "mcp-param-region"})
	if resp.StatusCode != http.StatusNoContent || !strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "mcp-param-region") {
		t.Errorf("preflight: %d %v", resp.StatusCode, resp.Header)
	}
	delete(headers, "Origin")

	big := strings.Repeat("x", 4<<10)
	resp, out := h.post(`{"jsonrpc":"2.0","id":9,"method":"tools/list","params":{"cursor":"`+big+`"}}`, headers)
	if resp.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(out, "larger than 2048 bytes") {
		t.Errorf("oversized: %d %s", resp.StatusCode, out)
	}
	resp, _ = h.post("{not json", headers)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unparseable: %d", resp.StatusCode)
	}
	headers["Content-Type"] = "text/plain"
	if resp, _ = h.post(body, headers); resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("content type: %d", resp.StatusCode)
	}
	delete(headers, "Content-Type")
	if resp, _ = h.request(http.MethodPut, body, headers); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT: %d", resp.StatusCode)
	}

	// DNS rebinding: a loopback gateway only answers loopback host names.
	req, _ := http.NewRequest(http.MethodPost, h.front.URL+"/mcp/billing", strings.NewReader(body))
	req.Host = "attacker.example"
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusForbidden {
		t.Errorf("foreign Host: %d", r2.StatusCode)
	}

	r3, err := http.Get(h.front.URL + "/mcp/unknown")
	if err != nil {
		t.Fatal(err)
	}
	r3.Body.Close()
	if r3.StatusCode != http.StatusNotFound {
		t.Errorf("unknown server: %d", r3.StatusCode)
	}
}

func TestListenStreamAndShutdown(t *testing.T) {
	h := newHarness(t, harnessOptions{script: func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if !strings.Contains(string(body), "subscriptions/listen") {
			return false
		}
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		fmt.Fprint(w, `data: {"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{}}`+"\n\n")
		_ = rc.Flush()
		for {
			select {
			case <-r.Context().Done():
				return true
			case <-time.After(20 * time.Millisecond):
				fmt.Fprint(w, `data: {"jsonrpc":"2.0","method":"notifications/tools/list_changed","params":{}}`+"\n\n")
				_ = rc.Flush()
			}
		}
	}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- h.gateway.Serve(ctx, ln) }()

	body, headers := modern(1, "subscriptions/listen", `"notifications":{"toolsListChanged":true}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/mcp/billing", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(resp.Body)
	for i := 0; i < 3; i++ {
		if _, err := r.ReadString('\n'); err != nil {
			t.Fatal(err)
		}
	}
	if s, _ := http.Get("http://" + ln.Addr().String() + "/readyz"); s.StatusCode != 200 {
		t.Errorf("readyz before shutdown: %d", s.StatusCode)
	}
	cancel()
	// Draining closes long-lived streams at once.
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, r); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the listen stream stayed open during shutdown")
	}
	resp.Body.Close()
	if err := <-served; err != nil {
		t.Errorf("Serve: %v", err)
	}
	span := h.span("subscriptions/listen", trace.SpanKindServer)
	if attr(span, telemetry.ErrorType) != telemetry.ErrorShutdown {
		t.Errorf("listen span: %v", span.Attributes())
	}
}

// In the gateway, the first session is the first list ever recorded: tools
// that appear later wait for an approval, whatever the client.
func TestFirstListingOnly(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(t, harnessOptions{dir: dir})
	body, headers := modern(1, "tools/list", "")
	if _, out := h.post(body, headers); len(toolNames(t, out)) != 3 {
		t.Fatalf("first list: %s", out)
	}
	h.front.Close()

	tools := append(billingTools("Fetch an invoice by its number."), testserver.Tool{Name: "export_all", Description: "Export everything"})
	later := newHarness(t, harnessOptions{dir: dir, tools: tools})
	_, out := later.post(body, headers)
	if names := strings.Join(toolNames(t, out), ","); names != "get_invoice,send_reminder,leaky" {
		t.Errorf("a tool added after the first list must wait: %s", names)
	}
	body, headers = modern(2, "tools/call", `"name":"export_all","arguments":{}`)
	_, out = later.post(body, headers)
	if !strings.Contains(out, "waiting for approval") {
		t.Errorf("call to the new tool: %s", out)
	}
}

func TestAuditLog(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	body, headers := modern(1, "tools/call", `"name":"get_invoice","arguments":{"id":"F-1"}`)
	h.post(body, headers)
	body, headers = modern(2, "tools/call", `"name":"get_invoice","arguments":{"id":"`+awsKey+`"}`)
	h.post(body, headers)
	data, err := os.ReadFile(filepath.Join(h.dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), awsKey) {
		t.Fatal("the audit log holds the key")
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("one line per call: %s", data)
	}
	var e audit.Entry
	if err := json.Unmarshal([]byte(lines[1]), &e); err != nil {
		t.Fatal(err)
	}
	if e.Event != audit.EventToolCall || e.Decision != "block" || e.Rule != "secrets.arguments" || e.Transport != "http" ||
		e.Tool != "get_invoice" || e.TraceID == "" || len(e.Findings) != 1 || e.Findings[0].SecretType != "aws_access_key_id" {
		t.Errorf("entry: %+v", e)
	}
}

func TestCheckListen(t *testing.T) {
	cases := []struct {
		gw config.Gateway
		ok bool
	}{
		{config.Gateway{Listen: "127.0.0.1:8787"}, true},
		{config.Gateway{Listen: "localhost:8787"}, true},
		{config.Gateway{Listen: "[::1]:8787"}, true},
		{config.Gateway{Listen: "0.0.0.0:8787"}, false},
		{config.Gateway{Listen: ":8787"}, false},
		{config.Gateway{Listen: "0.0.0.0:8787", AllowAnonymous: true}, true},
		{config.Gateway{Listen: "0.0.0.0:8787", Tokens: map[string]string{"a": "b"}}, true},
	}
	for _, c := range cases {
		if err := CheckListen(c.gw); (err == nil) != c.ok {
			t.Errorf("%+v: %v", c.gw, err)
		}
	}
}
