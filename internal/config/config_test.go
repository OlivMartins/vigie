package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OlivMartins/vigie/internal/pins"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vigie.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuiltinDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Policy("anything")
	if p.Mode != ModeEnforce || strings.Join(p.Tools.Allow, ",") != "*" || len(p.Tools.Deny) != 0 {
		t.Errorf("policy = %+v", p)
	}
	if !p.Pins.Enabled || p.Pins.OnNew != pins.PinFirstSession || p.Pins.OnChange != ActionBlock {
		t.Errorf("pins = %+v", p.Pins)
	}
	if p.Secrets.Arguments != ActionBlock || p.Secrets.Results != ActionWarn {
		t.Errorf("secrets = %+v", p.Secrets)
	}
	if p.Detections.Action != ActionBlock || len(p.Detections.Exempt) != 0 {
		t.Errorf("detections = %+v", p.Detections)
	}
	if cfg.Telemetry.CaptureContent || !cfg.Telemetry.PropagateContext {
		t.Errorf("telemetry = %+v", cfg.Telemetry)
	}
	if !strings.HasSuffix(cfg.PinsDir, filepath.Join("vigie", "pins")) {
		t.Errorf("pins dir = %s", cfg.PinsDir)
	}
}

func TestOverrides(t *testing.T) {
	path := write(t, `
version: 1
pins_dir: ./approvals
telemetry:
  capture_content: true
  propagate_context: false
defaults:
  mode: audit
  tools:
    deny: ["delete_*"]
  secrets:
    results: "off"
servers:
  billing:
    mode: enforce
    tools:
      allow: ["get_*", "send_reminder"]
    pins:
      on_new: block
  sandbox:
    pins:
      enabled: false
    tools:
      deny: []
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PinsDir != filepath.Join(filepath.Dir(path), "approvals") {
		t.Errorf("pins_dir = %s", cfg.PinsDir)
	}
	if !cfg.Telemetry.CaptureContent || cfg.Telemetry.PropagateContext {
		t.Errorf("telemetry = %+v", cfg.Telemetry)
	}

	billing := cfg.Policy("billing")
	if billing.Mode != ModeEnforce {
		t.Errorf("billing mode = %s", billing.Mode)
	}
	if strings.Join(billing.Tools.Allow, ",") != "get_*,send_reminder" || strings.Join(billing.Tools.Deny, ",") != "delete_*" {
		t.Errorf("billing tools = %+v (deny comes from defaults)", billing.Tools)
	}
	if billing.Pins.OnNew != pins.HoldForApproval || billing.Pins.OnChange != ActionBlock {
		t.Errorf("billing pins = %+v", billing.Pins)
	}
	if billing.Secrets.Results != ActionOff || billing.Secrets.Arguments != ActionBlock {
		t.Errorf("billing secrets = %+v", billing.Secrets)
	}

	sandbox := cfg.Policy("sandbox")
	if sandbox.Mode != ModeAudit || sandbox.Pins.Enabled || len(sandbox.Tools.Deny) != 0 {
		t.Errorf("sandbox = %+v (an explicit empty list clears the default)", sandbox)
	}
	other := cfg.Policy("other")
	if other.Mode != ModeAudit || strings.Join(other.Tools.Deny, ",") != "delete_*" {
		t.Errorf("other = %+v", other)
	}
	if strings.Join(cfg.Servers(), ",") != "billing,sandbox" || !cfg.HasServer("billing") || cfg.HasServer("other") {
		t.Errorf("servers = %v", cfg.Servers())
	}
}

func TestDetectionsSection(t *testing.T) {
	cfg, err := Load(write(t, `
version: 1
defaults:
  detections:
    action: warn
    exempt:
      "*": [sensitive_path]
servers:
  docs:
    detections:
      exempt:
        "read_*": ["*"]
  sandbox:
    detections:
      action: "off"
`))
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.Policy("other"); p.Detections.Action != ActionWarn || strings.Join(p.Detections.Exempt["*"], ",") != "sensitive_path" {
		t.Errorf("defaults = %+v", p.Detections)
	}
	if p := cfg.Policy("docs"); p.Detections.Action != ActionWarn || len(p.Detections.Exempt) != 1 || p.Detections.Exempt["read_*"][0] != "*" {
		t.Errorf("a server's exemptions replace the defaults: %+v", p.Detections)
	}
	if p := cfg.Policy("sandbox"); p.Detections.Action != ActionOff || len(p.Detections.Exempt) != 1 {
		t.Errorf("sandbox = %+v", p.Detections)
	}
}

func TestInvalidFiles(t *testing.T) {
	tests := []struct {
		name, content, want string
	}{
		{"missing version", "defaults:\n  mode: audit\n", `add "version: 1"`},
		{"future version", "version: 2\n", "format version 2 is not supported"},
		{"unknown field", "version: 1\ndefaults:\n  mdoe: audit\n", "mdoe"},
		{"unknown field position", "version: 1\nservers:\n  billing:\n    tool:\n      deny: [x]\n", "[4:"},
		{"bad mode", "version: 1\ndefaults:\n  mode: strict\n", `defaults.mode: "strict" is not one of enforce, audit`},
		{"bad on_change", "version: 1\nservers:\n  b:\n    pins:\n      on_change: ignore\n", "servers.b.pins.on_change"},
		{"block results", "version: 1\ndefaults:\n  secrets:\n    results: block\n", "block is not supported yet"},
		{"empty pattern", "version: 1\ndefaults:\n  tools:\n    deny: [\"\"]\n", "defaults.tools.deny[0]: empty pattern"},
		{"bad detections action", "version: 1\ndefaults:\n  detections:\n    action: ignore\n", "defaults.detections.action"},
		{"unknown detection rule", "version: 1\ndefaults:\n  detections:\n    exempt:\n      x: [typo]\n", `defaults.detections.exempt["x"]: "typo" is not one of *, hidden_instructions`},
		{"empty detection rule", "version: 1\ndefaults:\n  detections:\n    exempt:\n      x: [\"\"]\n", "empty rule"},
		{"empty exempt pattern", "version: 1\ndefaults:\n  detections:\n    exempt:\n      \"\": [\"*\"]\n", "empty tool pattern"},
		{"bad server name", "version: 1\nservers:\n  ../evil: {}\n", "invalid server name"},
		{"duplicate key", "version: 1\ndefaults:\n  mode: audit\n  mode: enforce\n", "duplicate"},
		{"several errors", "version: 1\ndefaults:\n  mode: x\n  secrets:\n    arguments: y\n", "secrets.arguments"},
		{"bad url", "version: 1\nservers:\n  b:\n    url: ftp://x/mcp\n", "servers.b.url"},
		{"credentials in url", "version: 1\nservers:\n  b:\n    url: https://u:p@x/mcp\n", "put credentials in headers"},
		{"headers without url", "version: 1\nservers:\n  b:\n    headers:\n      X-A: b\n", "only used with url"},
		{"reserved header", "version: 1\nservers:\n  b:\n    url: https://x/mcp\n    headers:\n      Mcp-Session-Id: s\n", "set by the client or by Vigie"},
		{"param header", "version: 1\nservers:\n  b:\n    url: https://x/mcp\n    headers:\n      Mcp-Param-Region: eu\n", "set by the client or by Vigie"},
		{"unset variable", "version: 1\nservers:\n  b:\n    url: https://x/mcp\n    headers:\n      Authorization: Bearer ${VIGIE_UNSET_FOR_TEST}\n", "VIGIE_UNSET_FOR_TEST is not set"},
		{"short token", "version: 1\ngateway:\n  tokens:\n    ci: short\n", "24 characters at least"},
		{"bad listen", "version: 1\ngateway:\n  listen: 8787\n", "gateway.listen"},
		{"bad origin", "version: 1\ngateway:\n  allowed_origins: [\"https://a.example/\"]\n", "allowed_origins[0]"},
		{"half tls", "version: 1\ngateway:\n  tls:\n    cert_file: c.pem\n", "give both cert_file and key_file"},
		{"bad size", "version: 1\nlimits:\n  max_message_size: 4MB\n", "is not a size"},
		{"tiny size", "version: 1\nlimits:\n  max_message_size: 1KiB\n", "64KiB at least"},
		{"bad duration", "version: 1\nlimits:\n  stop_timeout: soon\n", "limits.stop_timeout"},
		{"audit without file", "version: 1\naudit:\n  max_files: 3\n", "need a file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(write(t, tt.content))
			if err == nil {
				t.Fatal("invalid configuration accepted")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AppData", t.TempDir())
	t.Setenv(EnvConfig, "")
	if p, err := Resolve(""); err != nil || p != "" {
		t.Errorf("no file anywhere: %q, %v", p, err)
	}
	def, _ := DefaultPath()
	if err := os.MkdirAll(filepath.Dir(def), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(def, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p, _ := Resolve(""); p != def {
		t.Errorf("default file: %q", p)
	}
	t.Setenv(EnvConfig, "/from/env.yaml")
	if p, _ := Resolve(""); p != "/from/env.yaml" {
		t.Errorf("env: %q", p)
	}
	if p, _ := Resolve("/explicit.yaml"); p != "/explicit.yaml" {
		t.Errorf("explicit: %q", p)
	}
	if _, err := Load("/does/not/exist.yaml"); err == nil {
		t.Error("an explicit missing file must be an error")
	}
}

func TestExampleConfigurationIsValid(t *testing.T) {
	for _, path := range []string{"../../examples/vigie.yaml", "../../examples/demo/vigie.yaml"} {
		if _, err := Load(path); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
}

func TestGatewaySections(t *testing.T) {
	t.Setenv("VIGIE_TEST_TOKEN", "0123456789abcdef0123456789abcdef")
	t.Setenv("VIGIE_TEST_UPSTREAM", "upstream-secret")
	path := write(t, `
version: 1
audit:
  file: ./audit/vigie.jsonl
  max_size: 10MiB
  max_files: 3
limits:
  max_message_size: 8MiB
  stop_timeout: 20s
gateway:
  listen: 0.0.0.0:9000
  allowed_origins: ["https://inspector.example"]
  tokens:
    equipe-data: ${VIGIE_TEST_TOKEN}
  tls:
    cert_file: tls/cert.pem
    key_file: tls/key.pem
  max_request_size: 512KiB
servers:
  billing:
    url: https://billing.example.com/mcp
    headers:
      authorization: Bearer ${VIGIE_TEST_UPSTREAM}
    tools:
      deny: ["delete_*"]
  local: {}
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if cfg.Audit.File != filepath.Join(dir, "audit", "vigie.jsonl") || cfg.Audit.MaxSize != 10<<20 || cfg.Audit.MaxFiles != 3 {
		t.Errorf("audit = %+v", cfg.Audit)
	}
	if cfg.Limits.MaxMessageSize != 8<<20 || cfg.Limits.StopTimeout.String() != "20s" {
		t.Errorf("limits = %+v", cfg.Limits)
	}
	g := cfg.Gateway
	if g.Listen != "0.0.0.0:9000" || g.Tokens["equipe-data"] != "0123456789abcdef0123456789abcdef" || g.MaxRequestSize != 512<<10 {
		t.Errorf("gateway = %+v", g)
	}
	if g.TLSCertFile != filepath.Join(dir, "tls", "cert.pem") || g.TLSKeyFile != filepath.Join(dir, "tls", "key.pem") {
		t.Errorf("tls = %s %s", g.TLSCertFile, g.TLSKeyFile)
	}
	up, ok := cfg.Upstream("billing")
	if !ok || up.URL.String() != "https://billing.example.com/mcp" || up.Headers.Get("Authorization") != "Bearer upstream-secret" {
		t.Errorf("upstream = %+v", up)
	}
	if _, ok := cfg.Upstream("local"); ok {
		t.Error("a server without url has no upstream")
	}
	if got := strings.Join(cfg.Upstreams(), ","); got != "billing" {
		t.Errorf("upstreams = %s", got)
	}
	if p := cfg.Policy("billing"); strings.Join(p.Tools.Deny, ",") != "delete_*" {
		t.Errorf("policy of a server with a url: %+v", p)
	}
}

func TestBuiltinLimits(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limits.MaxMessageSize != DefaultMaxMessageSize || cfg.Gateway.Listen != DefaultListen || cfg.Gateway.MaxRequestSize != DefaultMaxRequestSize || cfg.Audit.File != "" {
		t.Errorf("%+v %+v %+v", cfg.Limits, cfg.Gateway, cfg.Audit)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"512": 512, "512B": 512, "64KiB": 64 << 10, "4 MiB": 4 << 20, "1GiB": 1 << 30} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("%q = %d, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "0", "-1", "1.5MiB", "4MB", "MiB", "9999999GiB"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
