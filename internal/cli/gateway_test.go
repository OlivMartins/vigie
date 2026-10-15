package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OlivMartins/vigie/internal/testserver"
)

// syncWriter collects what a running command writes.
type syncWriter struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func TestGatewayEndToEnd(t *testing.T) {
	up := testserver.NewHTTP(&testserver.Server{Name: "billing", Version: "1", Tools: []testserver.Tool{
		{Name: "get_invoice", Description: "Fetch an invoice"},
		{Name: "delete_invoice", Description: "Delete an invoice"},
	}})
	upstream := httptest.NewServer(up)
	defer upstream.Close()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "vigie.yaml")
	content := fmt.Sprintf("version: 1\npins_dir: %s\naudit:\n  file: %s\nservers:\n  billing:\n    url: %s/mcp\n    tools:\n      deny: [\"delete_*\"]\n",
		filepath.Join(dir, "pins"), filepath.Join(dir, "audit.jsonl"), upstream.URL)
	if err := os.WriteFile(cfg, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	stderr := &syncWriter{}
	done := make(chan int, 1)
	go func() {
		done <- Main(ctx, []string{"gateway", "--config", cfg, "--listen", addr, "--log-format", "json"},
			Streams{In: strings.NewReader(""), Out: io.Discard, Err: stderr})
	}()
	base := "http://" + addr
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("gateway not up: %v\n%s", err, stderr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`
	req, _ := http.NewRequest(http.MethodPost, base+"/mcp/billing", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/list")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(out), "get_invoice") || strings.Contains(string(out), "delete_invoice") {
		t.Errorf("tools/list through the gateway: %d %s", resp.StatusCode, out)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code %d\n%s", code, stderr)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("gateway did not stop")
	}
	logs := stderr.String()
	for _, want := range []string{`"msg":"vigie gateway started"`, `"path":"/mcp/billing"`, `"upstream":"` + upstream.URL + `/mcp"`, `"warning":"plain HTTP`, `"msg":"vigie gateway stopped"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %s:\n%s", want, logs)
		}
	}
}

func TestGatewayUsageErrors(t *testing.T) {
	noURL, _ := writeConfig(t)
	exposed := filepath.Join(t.TempDir(), "exposed.yaml")
	_ = os.WriteFile(exposed, []byte("version: 1\ngateway:\n  listen: 0.0.0.0:8787\nservers:\n  billing:\n    url: https://billing.example/mcp\n"), 0o600)
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"gateway", "extra"}, `unexpected argument "extra"`},
		{[]string{"gateway", "--mode", "strict"}, `invalid --mode "strict"`},
		{[]string{"gateway", "--config", noURL, "--listen", "8787"}, `invalid --listen "8787"`},
		{[]string{"gateway", "--config", noURL}, "no server to serve"},
		{[]string{"gateway", "--config", exposed}, "reachable beyond this machine"},
		{[]string{"check", "--config", exposed}, "add gateway.tokens"},
	} {
		r := run(t, "", tt.args...)
		if r.code != exitUsage || !strings.Contains(r.out+r.err, tt.want) {
			t.Errorf("%v: code %d out %q err %q, want %q", tt.args, r.code, r.out, r.err, tt.want)
		}
	}
}
