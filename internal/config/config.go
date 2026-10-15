// Package config loads and validates the Vigie configuration file.
//
// The file holds default rules and per-server overrides. Every field is
// optional except the format version; what is left out falls back to the
// defaults section, then to built-in defaults that block what is unambiguous
// (credentials in arguments, changed tool definitions, tools that appear
// after the first session) and pin the tools of the first session.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/OlivMartins/vigie/internal/pins"
)

// Version is the configuration format version this build reads.
const Version = 1

// EnvConfig names the environment variable that points to the configuration
// file.
const EnvConfig = "VIGIE_CONFIG"

// Mode says whether rules are applied or only recorded.
type Mode string

// Modes.
const (
	// ModeEnforce applies the rules: blocked calls never reach the server.
	ModeEnforce Mode = "enforce"
	// ModeAudit only records what would have been blocked.
	ModeAudit Mode = "audit"
)

// Action is what a rule does when it matches.
type Action string

// Actions.
const (
	ActionBlock Action = "block"
	ActionWarn  Action = "warn"
	ActionOff   Action = "off"
)

// Policy is the resolved set of rules for one upstream server.
type Policy struct {
	Mode    Mode
	Tools   ToolRules
	Pins    PinRules
	Secrets SecretRules
}

// ToolRules lists the tools a server may expose, by name pattern.
type ToolRules struct {
	Allow []string
	Deny  []string
}

// PinRules configures tool definition pinning.
type PinRules struct {
	Enabled  bool
	OnNew    pins.NewTools
	OnChange Action
}

// SecretRules configures credential detection.
type SecretRules struct {
	Arguments Action
	Results   Action
}

// Telemetry configures what goes into spans and how context flows.
type Telemetry struct {
	// CaptureContent attaches tool arguments and results to spans, after
	// masking credentials. Off by default.
	CaptureContent bool
	// PropagateContext writes Vigie's trace context into params._meta of
	// relayed requests, so the server's own spans join the trace.
	PropagateContext bool
}

// Config is a loaded, validated configuration.
type Config struct {
	// Source is the file the configuration came from, empty for built-in
	// defaults.
	Source    string
	PinsDir   string
	Telemetry Telemetry

	defaults policySpec
	servers  map[string]policySpec
}

// Policy returns the rules for a server: built-in defaults, overridden by the
// defaults section, overridden by the server's own section.
func (c *Config) Policy(server string) Policy {
	p := builtinPolicy()
	c.defaults.applyTo(&p)
	if s, ok := c.servers[server]; ok {
		s.applyTo(&p)
	}
	return p
}

// Servers lists the servers with their own section, sorted.
func (c *Config) Servers() []string {
	names := make([]string, 0, len(c.servers))
	for n := range c.servers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// HasServer reports whether a server has its own section.
func (c *Config) HasServer(name string) bool {
	_, ok := c.servers[name]
	return ok
}

func builtinPolicy() Policy {
	return Policy{
		Mode:    ModeEnforce,
		Tools:   ToolRules{Allow: []string{"*"}},
		Pins:    PinRules{Enabled: true, OnNew: pins.PinFirstSession, OnChange: ActionBlock},
		Secrets: SecretRules{Arguments: ActionBlock, Results: ActionWarn},
	}
}

// File schema. Pointers distinguish "not set" from "set to the zero value".
type fileSpec struct {
	Version   int                   `yaml:"version"`
	PinsDir   string                `yaml:"pins_dir"`
	Telemetry telemetrySpec         `yaml:"telemetry"`
	Defaults  policySpec            `yaml:"defaults"`
	Servers   map[string]policySpec `yaml:"servers"`
}

type telemetrySpec struct {
	CaptureContent   *bool `yaml:"capture_content"`
	PropagateContext *bool `yaml:"propagate_context"`
}

type policySpec struct {
	Mode    string       `yaml:"mode"`
	Tools   *toolsSpec   `yaml:"tools"`
	Pins    *pinsSpec    `yaml:"pins"`
	Secrets *secretsSpec `yaml:"secrets"`
}

type toolsSpec struct {
	Allow *[]string `yaml:"allow"`
	Deny  *[]string `yaml:"deny"`
}

type pinsSpec struct {
	Enabled  *bool  `yaml:"enabled"`
	OnNew    string `yaml:"on_new"`
	OnChange string `yaml:"on_change"`
}

type secretsSpec struct {
	Arguments string `yaml:"arguments"`
	Results   string `yaml:"results"`
}

func (s policySpec) applyTo(p *Policy) {
	if s.Mode != "" {
		p.Mode = Mode(s.Mode)
	}
	if s.Tools != nil {
		if s.Tools.Allow != nil {
			p.Tools.Allow = *s.Tools.Allow
		}
		if s.Tools.Deny != nil {
			p.Tools.Deny = *s.Tools.Deny
		}
	}
	if s.Pins != nil {
		if s.Pins.Enabled != nil {
			p.Pins.Enabled = *s.Pins.Enabled
		}
		if s.Pins.OnNew != "" {
			p.Pins.OnNew = pins.NewTools(s.Pins.OnNew)
		}
		if s.Pins.OnChange != "" {
			p.Pins.OnChange = Action(s.Pins.OnChange)
		}
	}
	if s.Secrets != nil {
		if s.Secrets.Arguments != "" {
			p.Secrets.Arguments = Action(s.Secrets.Arguments)
		}
		if s.Secrets.Results != "" {
			p.Secrets.Results = Action(s.Secrets.Results)
		}
	}
}

func (s policySpec) validate(path string) error {
	var errs []error
	check := func(field, value string, allowed ...string) {
		if value == "" {
			return
		}
		for _, a := range allowed {
			if value == a {
				return
			}
		}
		errs = append(errs, fmt.Errorf("%s.%s: %q is not one of %s", path, field, value, strings.Join(allowed, ", ")))
	}
	check("mode", s.Mode, string(ModeEnforce), string(ModeAudit))
	if s.Tools != nil {
		for _, list := range []struct {
			name string
			v    *[]string
		}{{"allow", s.Tools.Allow}, {"deny", s.Tools.Deny}} {
			if list.v == nil {
				continue
			}
			for i, pattern := range *list.v {
				if strings.TrimSpace(pattern) == "" {
					errs = append(errs, fmt.Errorf("%s.tools.%s[%d]: empty pattern", path, list.name, i))
				}
			}
		}
	}
	if s.Pins != nil {
		check("pins.on_new", s.Pins.OnNew, string(pins.PinFirstSession), string(pins.PinOnFirstUse), string(pins.HoldForApproval))
		check("pins.on_change", s.Pins.OnChange, string(ActionBlock), string(ActionWarn))
	}
	if s.Secrets != nil {
		check("secrets.arguments", s.Secrets.Arguments, string(ActionBlock), string(ActionWarn), string(ActionOff))
		if s.Secrets.Results == string(ActionBlock) {
			errs = append(errs, fmt.Errorf("%s.secrets.results: block is not supported yet: results can be flagged (warn) or ignored (off)", path))
		} else {
			check("secrets.results", s.Secrets.Results, string(ActionWarn), string(ActionOff))
		}
	}
	return errors.Join(errs...)
}

// Default returns the built-in configuration, used when no file exists.
func Default() (*Config, error) {
	dir, err := defaultDir()
	if err != nil {
		return nil, err
	}
	return &Config{
		PinsDir:   filepath.Join(dir, "pins"),
		Telemetry: Telemetry{PropagateContext: true},
	}, nil
}

// DefaultPath returns where Vigie looks for its configuration when neither
// --config nor VIGIE_CONFIG is set.
func DefaultPath() (string, error) {
	dir, err := defaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "vigie.yaml"), nil
}

func defaultDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locating the user configuration directory: %w", err)
	}
	return filepath.Join(dir, "vigie"), nil
}

// Resolve finds the configuration to load: the explicit path if given, else
// VIGIE_CONFIG, else the default path. It returns "" when no file applies and
// built-in defaults should be used. An explicit path that does not exist is an
// error; a missing default file is not.
func Resolve(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env := os.Getenv(EnvConfig); env != "" {
		return env, nil
	}
	path, err := DefaultPath()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return path, nil
}

// Load reads and validates a configuration file. An empty path returns the
// built-in defaults.
func Load(path string) (*Config, error) {
	if path == "" {
		return Default()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parse(path, data)
}

func parse(path string, data []byte) (*Config, error) {
	var spec fileSpec
	if err := yaml.UnmarshalWithOptions(data, &spec, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("%s:\n%s", path, yaml.FormatError(err, false, true))
	}
	if spec.Version != Version {
		if spec.Version == 0 {
			return nil, fmt.Errorf("%s: missing format version, add \"version: %d\" at the top", path, Version)
		}
		return nil, fmt.Errorf("%s: format version %d is not supported by this build (expected %d)", path, spec.Version, Version)
	}
	errs := []error{spec.Defaults.validate("defaults")}
	for name, s := range spec.Servers {
		if !pins.ValidServerName(name) {
			errs = append(errs, fmt.Errorf("servers.%s: invalid server name (letters, digits, '.', '_' or '-', 64 characters at most)", name))
		}
		errs = append(errs, s.validate("servers."+name))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	cfg, err := Default()
	if err != nil {
		return nil, err
	}
	cfg.Source = path
	cfg.defaults = spec.Defaults
	cfg.servers = spec.Servers
	if spec.Telemetry.CaptureContent != nil {
		cfg.Telemetry.CaptureContent = *spec.Telemetry.CaptureContent
	}
	if spec.Telemetry.PropagateContext != nil {
		cfg.Telemetry.PropagateContext = *spec.Telemetry.PropagateContext
	}
	if spec.PinsDir != "" {
		dir, err := expandPath(spec.PinsDir, filepath.Dir(path))
		if err != nil {
			return nil, fmt.Errorf("%s: pins_dir: %w", path, err)
		}
		cfg.PinsDir = dir
	}
	return cfg, nil
}

// expandPath resolves ~ and paths relative to the configuration file.
func expandPath(p, base string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return filepath.Clean(p), nil
}
