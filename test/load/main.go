// Command load measures the gateway under load: throughput and latency of
// tools/call requests (revision 2026-07-28) sent directly to an MCP server,
// through a bare reverse proxy (net/http/httputil, the cost of any HTTP hop),
// and through "vigie gateway", at several concurrency levels.
//
// Usage: go run ./test/load -vigie bin/vigie [-duration 10s] [-concurrency 1,8,32,128]
//
// The server answers in memory, without work: the differences measure the
// cost of the hop and of Vigie's checks, on the same machine.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OlivMartins/vigie/internal/testserver"
)

func main() {
	vigie := flag.String("vigie", "bin/vigie", "vigie binary")
	duration := flag.Duration("duration", 10*time.Second, "duration of each run")
	levels := flag.String("concurrency", "1,8,32,128", "concurrent clients, comma separated")
	proxyAddr := flag.String("serve-proxy", "", "internal: serve a bare reverse proxy on this address")
	proxyTo := flag.String("to", "", "internal: where the bare reverse proxy forwards")
	flag.Parse()
	if *proxyAddr != "" {
		target, _ := url.Parse(*proxyTo)
		p := httputil.NewSingleHostReverseProxy(target)
		p.Transport = &http.Transport{MaxIdleConnsPerHost: 256}
		srv := &http.Server{Addr: *proxyAddr, Handler: p, ReadHeaderTimeout: 10 * time.Second}
		_ = srv.ListenAndServe()
		return
	}
	if err := run(*vigie, *duration, *levels); err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		os.Exit(1)
	}
}

func run(vigie string, duration time.Duration, levels string) error {
	note := strings.Repeat("lorem ipsum ", 80)
	up := testserver.NewHTTP(&testserver.Server{Name: "billing", Version: "1", NoHistory: true, Tools: []testserver.Tool{{
		Name: "get_invoice", Description: "Fetch an invoice",
		Call: func(map[string]any) (string, bool) { return strings.Repeat("invoice line ", 80), false },
	}}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	server := &http.Server{Handler: up, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(ln) }()
	defer server.Close()
	upstream := "http://" + ln.Addr().String() + "/mcp"

	dir, err := os.MkdirTemp("", "vigie-load-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	config := filepath.Join(dir, "vigie.yaml")
	if err := os.WriteFile(config, []byte("version: 1\npins_dir: "+filepath.Join(dir, "pins")+"\nservers:\n  billing:\n    url: "+upstream+"\n"), 0o600); err != nil {
		return err
	}
	addr, err := freeAddr()
	if err != nil {
		return err
	}
	cmd := exec.Command(vigie, "gateway", "--config", config, "--listen", addr, "--log-level", "error")
	cmd.Env = append(os.Environ(), "OTEL_EXPORTER_OTLP_ENDPOINT=")
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { _ = cmd.Process.Signal(os.Interrupt); _ = cmd.Wait() }()
	gateway := "http://" + addr + "/mcp/billing"
	if err := waitFor("http://" + addr + "/healthz"); err != nil {
		return err
	}
	// The bare reverse proxy runs in its own process, as Vigie does.
	proxyAddr, err := freeAddr()
	if err != nil {
		return err
	}
	bare := exec.Command(os.Args[0], "-serve-proxy", proxyAddr, "-to", upstream)
	if err := bare.Start(); err != nil {
		return err
	}
	defer func() { _ = bare.Process.Kill(); _ = bare.Wait() }()
	proxied := "http://" + proxyAddr + "/mcp"
	if err := waitFor(proxied); err != nil {
		return err
	}

	body := func(id int64) []byte {
		return []byte(`{"jsonrpc":"2.0","id":` + strconv.FormatInt(id, 10) + `,"method":"tools/call","params":{"name":"get_invoice",` +
			`"arguments":{"id":"F-2026-0042","note":"` + note + `"},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`)
	}
	fmt.Printf("tools/call, revision 2026-07-28, arguments and result of about 1 KiB, %s per run, %d CPU (%s/%s)\n\n",
		duration, runtime.NumCPU(), runtime.GOOS, runtime.GOARCH)
	fmt.Println("| Clients | Direct | Bare reverse proxy | Vigie gateway | Vigie p99 | Vigie CPU per call |")
	fmt.Println("| ---: | ---: | ---: | ---: | ---: | ---: |")
	for _, level := range strings.Split(levels, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(level))
		if err != nil || n <= 0 {
			return fmt.Errorf("invalid concurrency %q", level)
		}
		direct, err := load(upstream, n, duration, body)
		if err != nil {
			return err
		}
		hop, err := load(proxied, n, duration, body)
		if err != nil {
			return err
		}
		before := cpuTime(cmd.Process.Pid)
		through, err := load(gateway, n, duration, body)
		if err != nil {
			return err
		}
		perCall := "n/a"
		if spent := cpuTime(cmd.Process.Pid) - before; spent > 0 {
			perCall = fmt.Sprintf("%.0f µs", float64(spent.Microseconds())/float64(through.count))
		}
		fmt.Printf("| %d | %s | %s | %s | %s | %s |\n", n, direct, hop, through, ms(through.p99), perCall)
	}
	return nil
}

type result struct {
	rate     float64
	count    int
	p50, p99 time.Duration
}

// String renders throughput and median latency.
func (r result) String() string { return fmt.Sprintf("%.0f req/s, p50 %s", r.rate, ms(r.p50)) }

// load sends requests from n clients for d and measures them.
func load(url string, n int, d time.Duration, body func(int64) []byte) (result, error) {
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: n, MaxConnsPerHost: n}}
	defer client.CloseIdleConnections()
	var next atomic.Int64
	var failures atomic.Int64
	latencies := make([][]time.Duration, n)
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	// A short warm-up opens the connections.
	for i := 0; i < n; i++ {
		_ = send(client, url, body(next.Add(1)))
	}
	var wg sync.WaitGroup
	start := time.Now()
	for c := 0; c < n; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				t := time.Now()
				if err := send(client, url, body(next.Add(1))); err != nil {
					failures.Add(1)
					continue
				}
				latencies[c] = append(latencies[c], time.Since(t))
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	var all []time.Duration
	for _, l := range latencies {
		all = append(all, l...)
	}
	if f := failures.Load(); f > 0 || len(all) == 0 {
		return result{}, fmt.Errorf("%s: %d failed requests", url, f)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	pct := func(p float64) time.Duration { return all[int(p*float64(len(all)-1))] }
	return result{rate: float64(len(all)) / elapsed.Seconds(), count: len(all), p50: pct(0.50), p99: pct(0.99)}, nil
}

func send(client *http.Client, url string, body []byte) error {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "get_invoice")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK || !bytes.Contains(data, []byte(`"isError":false`)) {
		return fmt.Errorf("status %d: %.200s", resp.StatusCode, data)
	}
	return nil
}

// cpuTime returns the CPU time a process used so far (Linux only, zero
// elsewhere).
func cpuTime(pid int) time.Duration {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// Fields after the command name, which ends with ")": utime and stime
	// are the 12th and 13th, in clock ticks (100 per second on Linux).
	fields := strings.Fields(string(data[bytes.LastIndexByte(data, ')')+1:]))
	if len(fields) < 13 {
		return 0
	}
	utime, _ := strconv.ParseInt(fields[11], 10, 64)
	stime, _ := strconv.ParseInt(fields[12], 10, 64)
	return time.Duration(utime+stime) * 10 * time.Millisecond
}

func ms(d time.Duration) string { return fmt.Sprintf("%.2f ms", float64(d.Microseconds())/1000) }

func freeAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	return ln.Addr().String(), nil
}

func waitFor(url string) error {
	for i := 0; i < 100; i++ {
		if resp, err := http.Get(url); err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("%s did not come up", url)
}
