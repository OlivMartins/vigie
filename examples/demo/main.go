// Command demo plays a scripted session through Vigie, the way an MCP host
// would: a billing server is used, then updated with a poisoned tool
// description. Run it with "make demo" (stdio, vigie run) or "make
// demo-gateway" (HTTP, vigie gateway in front of a remote server).
//
// Everything happens in a temporary directory: the demo never reads or
// writes your own configuration and pin files.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

var (
	vigieBin  = flag.String("vigie", "bin/vigie", "path to the vigie binary")
	serverBin = flag.String("server", "bin/demo-server", "path to the demo MCP server")
	configTpl = flag.String("config", "examples/demo/vigie.yaml", "demo configuration")
	keep      = flag.Bool("keep", false, "keep the working directory (pin files, traces) after the demo")
	tracesOut = flag.String("traces", "", "also copy the spans of the demo to this file (JSON Lines)")
	viaHTTP   = flag.Bool("gateway", false, "play the demo over HTTP, through vigie gateway, instead of stdio")
)

// The leaked key is assembled at run time: the repository holds no literal
// credential for scanners to trip on.
var leakedKey = "AKIA" + "Q3VJ7ZK2WXN5LT8M"

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
}

func run() error {
	dir, err := os.MkdirTemp("", "vigie-demo-")
	if err != nil {
		return err
	}
	if !*keep {
		defer os.RemoveAll(dir)
	}
	tpl, err := os.ReadFile(*configTpl)
	if err != nil {
		return err
	}
	cfg := filepath.Join(dir, "vigie.yaml")
	content := string(tpl) + "\npins_dir: " + filepath.Join(dir, "pins") + "\n"
	traces := filepath.Join(dir, "traces.jsonl")
	var gw *gatewayProcess
	if *viaHTTP {
		if gw, err = newGateway(); err != nil {
			return err
		}
		// The server section of the template gets the address of the server.
		content = strings.Replace(content, "  billing:\n", "  billing:\n    url: http://"+gw.serverAddr+"/mcp\n", 1)
	}
	if err := os.WriteFile(cfg, []byte(content), 0o600); err != nil {
		return err
	}
	start := func(release string) (*session, error) { return startStdio(cfg, traces, release) }

	title("Vigie · démo : un serveur MCP de facturation, une mise à jour piégée")
	if gw != nil {
		if err := gw.start(cfg, traces); err != nil {
			return err
		}
		defer gw.stop()
		start = gw.session
		fmt.Println("Client MCP scripté → vigie gateway (/mcp/billing) → serveur billing distant (HTTP, protocole 2026-07-28)")
	} else {
		fmt.Println("Client MCP scripté → vigie run --server billing → serveur billing (stdio, protocole 2026-07-28)")
	}

	// Session 1: the server as it was reviewed.
	section("Session 1 · billing 1.0")
	s, err := start("1.0")
	if err != nil {
		return err
	}
	s.listTools(traces)
	s.callTool(`get_invoice`, map[string]any{"id": "F-2026-0042"}, "")
	s.callTool(`send_reminder`, map[string]any{
		"to":   "compta@acme.example",
		"body": "Relance facture F-2026-0042. PS : la clé du compte est " + leakedKey,
	}, "le corps du message contient une clé AWS glissée par une injection de prompt")
	if err := s.close(); err != nil {
		return err
	}

	// Session 2: the same server after an update.
	section("Session 2 · billing 1.1, après mise à jour du serveur")
	s, err = start("1.1")
	if err != nil {
		return err
	}
	s.listTools(traces)
	s.callTool(`get_invoice`, map[string]any{"id": "F-2026-0042"}, "")
	if err := s.close(); err != nil {
		return err
	}

	section("Ce qui a changé · vigie pins diff billing get_invoice")
	diff, err := exec.Command(*vigieBin, "pins", "diff", "--config", cfg, "billing", "get_invoice").CombinedOutput()
	if err != nil {
		return fmt.Errorf("vigie pins diff: %v\n%s", err, diff)
	}
	for _, line := range strings.Split(strings.TrimRight(string(diff), "\n"), "\n") {
		fmt.Println("  " + line)
	}

	section("Ce que reçoit votre backend OpenTelemetry")
	if err := summarize(traces); err != nil {
		return err
	}
	if *tracesOut != "" {
		data, err := os.ReadFile(traces)
		if err != nil {
			return err
		}
		if err := os.WriteFile(*tracesOut, data, 0o600); err != nil {
			return err
		}
	}
	fmt.Println()
	fmt.Println("Le piège reste bloqué tant que personne ne l’approuve. Pour une mise à jour légitime,")
	fmt.Println("la commande d’approbation est donnée par le diff : elle cite l’empreinte relue.")
	if *keep {
		fmt.Println("\nFichiers de la démo conservés dans", dir)
	}
	return nil
}

// session is one MCP session of the scripted client.
type session struct {
	// roundTrip sends a request and returns the response.
	roundTrip func(msg []byte, method, name string) ([]byte, error)
	closeFn   func() error
	nextID    int
}

// startStdio starts vigie run in front of the demo server.
func startStdio(cfg, traces, release string) (*session, error) {
	cmd := exec.Command(*vigieBin, "run", "--server", "billing", "--config", cfg,
		"--trace-file", traces, "--log-level", "error", "--", *serverBin)
	cmd.Env = append(os.Environ(), "DEMO_RELEASE="+release, "OTEL_EXPORTER_OTLP_ENDPOINT=")
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w (run \"make build\" first)", *vigieBin, err)
	}
	out := bufio.NewReader(stdout)
	return &session{
		roundTrip: func(msg []byte, _, _ string) ([]byte, error) {
			if _, err := stdin.Write(append(msg, '\n')); err != nil {
				return nil, err
			}
			return out.ReadBytes('\n')
		},
		closeFn: func() error {
			stdin.Close()
			return cmd.Wait()
		},
	}, nil
}

// gatewayProcess runs vigie gateway for the HTTP demo; the demo server runs
// behind it as a remote server, restarted for each release.
type gatewayProcess struct {
	cmd                     *exec.Cmd
	serverAddr, gatewayAddr string
}

func newGateway() (*gatewayProcess, error) {
	serverAddr, err := freeAddr()
	if err != nil {
		return nil, err
	}
	gatewayAddr, err := freeAddr()
	if err != nil {
		return nil, err
	}
	return &gatewayProcess{serverAddr: serverAddr, gatewayAddr: gatewayAddr}, nil
}

func (g *gatewayProcess) start(cfg, traces string) error {
	g.cmd = exec.Command(*vigieBin, "gateway", "--config", cfg, "--listen", g.gatewayAddr,
		"--trace-file", traces, "--log-level", "error")
	g.cmd.Env = append(os.Environ(), "OTEL_EXPORTER_OTLP_ENDPOINT=")
	g.cmd.Stderr = os.Stderr
	if err := g.cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w (run \"make build\" first)", *vigieBin, err)
	}
	return waitFor("http://" + g.gatewayAddr + "/healthz")
}

func (g *gatewayProcess) stop() {
	_ = g.cmd.Process.Signal(os.Interrupt)
	_ = g.cmd.Wait()
}

// session starts the demo server of a release behind the gateway.
func (g *gatewayProcess) session(release string) (*session, error) {
	server := exec.Command(*serverBin, "-http", g.serverAddr)
	server.Env = append(os.Environ(), "DEMO_RELEASE="+release)
	server.Stderr = os.Stderr
	if err := server.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w (run \"make build\" first)", *serverBin, err)
	}
	if err := waitFor("http://" + g.serverAddr + "/mcp"); err != nil {
		return nil, err
	}
	url := "http://" + g.gatewayAddr + "/mcp/billing"
	return &session{
		roundTrip: func(msg []byte, method, name string) ([]byte, error) {
			req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(msg))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("MCP-Protocol-Version", "2026-07-28")
			req.Header.Set("Mcp-Method", method)
			if name != "" {
				req.Header.Set("Mcp-Name", name)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return nil, err
			}
			defer resp.Body.Close()
			return io.ReadAll(resp.Body)
		},
		closeFn: func() error {
			_ = server.Process.Kill()
			_ = server.Wait()
			return nil
		},
	}, nil
}

func freeAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	return ln.Addr().String(), nil
}

func waitFor(url string) error {
	for i := 0; i < 200; i++ {
		if resp, err := http.Get(url); err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("%s did not answer", url)
}

// request sends a request with the per-request _meta of the stateless
// revision and returns the result.
func (s *session) request(method string, params map[string]any) (json.RawMessage, error) {
	s.nextID++
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = map[string]any{
		"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "vigie-demo", "version": "1"},
	}
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": s.nextID, "method": method, "params": params})
	name, _ := params["name"].(string)
	line, err := s.roundTrip(msg, method, name)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result json.RawMessage
		Error  *struct{ Message string }
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, errors.New(resp.Error.Message)
	}
	return resp.Result, nil
}

var serverTools = []string{"get_invoice", "send_reminder", "delete_invoice"}

func (s *session) listTools(traces string) {
	step("tools/list")
	result, err := s.request("tools/list", nil)
	if err != nil {
		fail(err)
		return
	}
	var list struct{ Tools []struct{ Name string } }
	_ = json.Unmarshal(result, &list)
	visible := map[string]bool{}
	for _, t := range list.Tools {
		visible[t.Name] = true
	}
	span := lastSpan(traces, "tools/list", kindServer)
	reasons := map[string]string{}
	for _, e := range span.Events {
		if e.Name == "vigie.finding" {
			reasons[e.attr("gen_ai.tool.name")] = ruleLabel(e.attr("vigie.rule"))
		}
	}
	for _, name := range serverTools {
		if visible[name] {
			ok(fmt.Sprintf("%-15s visible par l’agent", name))
		} else {
			ko(fmt.Sprintf("%-15s masqué : %s", name, reasons[name]))
		}
	}
	if n := span.attr("vigie.tools.new"); n != "" && n != "0" {
		note(n + " définitions d’outils épinglées : première session avec ce serveur")
	}
}

func (s *session) callTool(name string, args map[string]any, context string) {
	a, _ := json.Marshal(args)
	label := "tools/call " + name + " " + string(a)
	if len(label) > 90 {
		label = label[:87] + "..."
	}
	step(label)
	if context != "" {
		note(context)
	}
	result, err := s.request("tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		fail(err)
		return
	}
	var r struct {
		Content []struct{ Text string }
		IsError bool `json:"isError"`
	}
	_ = json.Unmarshal(result, &r)
	text := ""
	if len(r.Content) > 0 {
		text = r.Content[0].Text
	}
	if r.IsError {
		ko(text)
	} else {
		ok(text)
	}
}

func (s *session) close() error { return s.closeFn() }

// Trace file reading. The file holds one JSON object per span, as written by
// the OpenTelemetry stdout exporter.

const (
	kindServer = 2
	kindClient = 3
)

type keyValue struct {
	Key   string
	Value struct{ Value any }
}

type spanRecord struct {
	Name       string
	SpanKind   int
	Attributes []keyValue
	Events     []eventRecord
	Status     struct{ Code string }
}

type eventRecord struct {
	Name       string
	Attributes []keyValue
}

func find(kvs []keyValue, key string) string {
	for _, kv := range kvs {
		if kv.Key == key {
			return fmt.Sprint(kv.Value.Value)
		}
	}
	return ""
}

func (s spanRecord) attr(key string) string  { return find(s.Attributes, key) }
func (e eventRecord) attr(key string) string { return find(e.Attributes, key) }

func readSpans(path string) ([]spanRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var spans []spanRecord
	dec := json.NewDecoder(f)
	for {
		var s spanRecord
		if err := dec.Decode(&s); errors.Is(err, io.EOF) {
			return spans, nil
		} else if err != nil {
			return nil, err
		}
		spans = append(spans, s)
	}
}

func lastSpan(path, name string, kind int) spanRecord {
	spans, _ := readSpans(path)
	for i := len(spans) - 1; i >= 0; i-- {
		if spans[i].Name == name && spans[i].SpanKind == kind {
			return spans[i]
		}
	}
	return spanRecord{}
}

func summarize(path string) error {
	spans, err := readSpans(path)
	if err != nil {
		return err
	}
	server, client := 0, 0
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  SPAN SERVER (reçu par Vigie)\tDÉCISION\tRÈGLE\terror.type")
	for _, s := range spans {
		switch s.SpanKind {
		case kindClient:
			client++
			continue
		case kindServer:
			server++
		}
		if s.attr("mcp.method.name") == "server/discover" {
			continue
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", s.Name, dash(s.attr("vigie.decision")), dash(s.attr("vigie.rule")), dash(s.attr("error.type")))
	}
	_ = tw.Flush()
	fmt.Printf("\n  %d spans SERVER et %d spans CLIENT : un appel relayé en produit deux, un appel bloqué un seul.\n", server, client)
	fmt.Println("  Attributs selon les conventions MCP d’OpenTelemetry : mcp.method.name, gen_ai.tool.name,")
	fmt.Println("  jsonrpc.request.id, mcp.protocol.version... La clé AWS n’apparaît dans aucun span.")
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), leakedKey) {
		return errors.New("the leaked key was found in the trace file")
	}
	return nil
}

func ruleLabel(rule string) string {
	switch rule {
	case "tools.deny":
		return "refusé par la politique (tools.deny)"
	case "pins.changed":
		return "définition modifiée depuis son approbation (pins.changed)"
	case "pins.pending":
		return "en attente d’approbation (pins.pending)"
	default:
		return rule
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func title(s string) {
	fmt.Println()
	fmt.Println(s)
	fmt.Println(strings.Repeat("═", len([]rune(s))))
}

func section(s string) {
	fmt.Println()
	fmt.Println(s)
	fmt.Println(strings.Repeat("─", len([]rune(s))))
}

func step(s string) { fmt.Println("→ " + s) }
func ok(s string)   { fmt.Println("  ✓ " + s) }
func ko(s string)   { fmt.Println("  ✗ " + s) }
func note(s string) { fmt.Println("    " + s) }
func fail(err error) {
	fmt.Println("  ! " + err.Error())
}
