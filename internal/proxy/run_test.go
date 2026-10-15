package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OlivMartins/vigie/internal/config"
	"github.com/OlivMartins/vigie/internal/pins"
	"github.com/OlivMartins/vigie/internal/policy"
	"github.com/OlivMartins/vigie/internal/secrets"
	"github.com/OlivMartins/vigie/internal/testserver"
)

// The test binary doubles as the upstream server: Run starts it again with
// VIGIE_TEST_SERVER set.
func TestMain(m *testing.M) {
	switch os.Getenv("VIGIE_TEST_SERVER") {
	case "":
		os.Exit(m.Run())
	case "mcp":
		os.Stderr.WriteString("server diagnostics on stderr\n")
		s := &testserver.Server{Name: "billing", Version: "1", Tools: demoTools("Fetch an invoice by id")}
		_ = s.Serve(os.Stdin, os.Stdout)
		os.Exit(0)
	case "crash":
		os.Exit(3)
	case "stubborn":
		// Ignores the end of its input, like a server stuck in a loop.
		_, _ = io.Copy(io.Discard, os.Stdin)
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func testOptions(t *testing.T) Options {
	cfg, _ := config.Default()
	store, _ := pins.Open(t.TempDir(), "billing")
	_ = store.BeginSession()
	return Options{
		Engine: policy.NewEngine("billing", cfg.Policy("billing"), store, secrets.NewScanner()),
		Logger: slog.New(slog.DiscardHandler),
	}
}

func process(mode string, stdin io.Reader, stdout, stderr io.Writer) Process {
	return Process{
		Command: []string{os.Args[0], "-test.run=^$"},
		Env:     append(os.Environ(), "VIGIE_TEST_SERVER="+mode),
		Stdin:   stdin, Stdout: stdout, Stderr: stderr,
	}
}

func TestRunRelaysAndExitsCleanly(t *testing.T) {
	stdin, toVigie := io.Pipe()
	fromVigie, stdout := io.Pipe()
	stderr := &lockedBuffer{}
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := Run(context.Background(), process("mcp", stdin, stdout, stderr), testOptions(t))
		stdout.Close()
		done <- result{code, err}
	}()

	lines := bufio.NewReader(fromVigie)
	exchange := func(req string) string {
		if _, err := io.WriteString(toVigie, req+"\n"); err != nil {
			t.Fatal(err)
		}
		line, err := lines.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		return line
	}
	exchange(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	list := exchange(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if !strings.Contains(list, "get_invoice") {
		t.Errorf("tools/list = %s", list)
	}
	blocked := exchange(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"send_email","arguments":{"body":"` + awsKey + `"}}}`)
	if !strings.Contains(blocked, "Vigie blocked") {
		t.Errorf("tools/call = %s", blocked)
	}
	toVigie.Close() // the host ends the session

	rest, _ := io.ReadAll(lines)
	res := <-done
	if res.err != nil || res.code != 0 {
		t.Fatalf("Run = %d, %v", res.code, res.err)
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		t.Errorf("unexpected output after the session: %q", rest)
	}
	if !strings.Contains(stderr.String(), "server diagnostics on stderr") {
		t.Errorf("server stderr not passed through: %q", stderr.String())
	}
}

func TestStdoutCarriesOnlyProtocol(t *testing.T) {
	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_invoice","arguments":{"id":"F-1"}}}`,
	}, "\n") + "\n")
	var out lockedBuffer
	opts := testOptions(t)
	opts.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	code, err := Run(context.Background(), process("mcp", in, &out, io.Discard), opts)
	if err != nil || code != 0 {
		t.Fatalf("Run = %d, %v", code, err)
	}
	ids := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &m); err != nil || string(m["jsonrpc"]) != `"2.0"` {
			t.Fatalf("stdout line is not JSON-RPC: %q", line)
		}
		ids[string(m["id"])] = true
	}
	if len(ids) != 3 {
		t.Errorf("responses for ids %v, want 1, 2 and 3", ids)
	}
}

func TestRunReportsServerExit(t *testing.T) {
	stdin, toVigie := io.Pipe()
	defer toVigie.Close()
	code, err := Run(context.Background(), process("crash", stdin, io.Discard, io.Discard), testOptions(t))
	if err != nil || code != 3 {
		t.Errorf("Run = %d, %v; want the server's exit code 3", code, err)
	}
}

func TestRunStartFailure(t *testing.T) {
	p := Process{Command: []string{"/does/not/exist/mcp-server"}, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard}
	code, err := Run(context.Background(), p, testOptions(t))
	if code != ExitStartFailure || err == nil {
		t.Errorf("Run = %d, %v", code, err)
	}
	if _, err := Run(context.Background(), Process{}, testOptions(t)); err == nil {
		t.Error("an empty command must be an error")
	}
}

func TestRunTerminatesAStubbornServer(t *testing.T) {
	exitGrace, terminateGrace = 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { exitGrace, terminateGrace = 5*time.Second, 2*time.Second })
	start := time.Now()
	code, err := Run(context.Background(), process("stubborn", strings.NewReader(""), io.Discard, io.Discard), testOptions(t))
	if err != nil || code != 0 {
		t.Errorf("Run = %d, %v", code, err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("shutdown took %v", d)
	}
}

func TestRunStopsOnSignal(t *testing.T) {
	terminateGrace = 500 * time.Millisecond
	t.Cleanup(func() { terminateGrace = 2 * time.Second })
	stdin, toVigie := io.Pipe()
	defer toVigie.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		code, _ := Run(ctx, process("mcp", stdin, io.Discard, io.Discard), testOptions(t))
		done <- code
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after the context was cancelled")
	}
}

// The host stops reading Vigie's output without closing its input: the
// session ends as if the client had closed it.
func TestRunEndsWhenTheClientStopsReading(t *testing.T) {
	exitGrace, terminateGrace = 300*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { exitGrace, terminateGrace = 5*time.Second, 2*time.Second })
	stdin, toVigie := io.Pipe()
	defer toVigie.Close()
	fromVigie, stdout := io.Pipe()
	fromVigie.Close() // nobody reads anymore
	done := make(chan int, 1)
	go func() {
		code, _ := Run(context.Background(), process("mcp", stdin, stdout, io.Discard), testOptions(t))
		done <- code
	}()
	go func() {
		_, _ = io.WriteString(toVigie, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`+"\n")
	}()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code %d, want 0: the client ended the session", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not end after the client stopped reading")
	}
}
