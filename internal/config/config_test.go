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
		{"bad server name", "version: 1\nservers:\n  ../evil: {}\n", "invalid server name"},
		{"duplicate key", "version: 1\ndefaults:\n  mode: audit\n  mode: enforce\n", "duplicate"},
		{"several errors", "version: 1\ndefaults:\n  mode: x\n  secrets:\n    arguments: y\n", "secrets.arguments"},
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
