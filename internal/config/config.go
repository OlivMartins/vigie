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
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

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

// Limits bounds what Vigie accepts and how long it waits.
type Limits struct {
	// MaxMessageSize bounds a single JSON-RPC message, in both directions.
	MaxMessageSize int64
	// StopTimeout is how long Vigie waits for a graceful stop: the stdio
	// server to exit once its input is closed, the gateway's requests in
	// flight to finish. Zero means the built-in default of each.
	StopTimeout time.Duration
}

// Audit configures the audit log.
type Audit struct {
	// File is the audit log; empty disables it.
	File     string
	MaxSize  int64
	MaxFiles int
}

// Gateway configures the HTTP gateway (vigie gateway).
type Gateway struct {
	// Listen is the address the gateway listens on, host:port.
	Listen string
	// AllowedOrigins lists the Origin header values accepted. A request
	// carrying another Origin is refused: it comes from a web page.
	AllowedOrigins []string
	// AllowedHosts lists the Host header values accepted. Empty means the
	// loopback names when listening on loopback, any otherwise.
	AllowedHosts []string
	// Tokens maps client names to the bearer tokens they present.
	Tokens map[string]string
	// AllowAnonymous lets the gateway listen beyond loopback without
	// tokens, for deployments where a proxy in front authenticates clients.
	AllowAnonymous bool
	// TLS serves HTTPS when both files are set.
	TLSCertFile string
	TLSKeyFile  string
	// MaxRequestSize bounds the body of an HTTP request from a client.
	MaxRequestSize int64
}

// Upstream is where the gateway forwards the requests of a server.
type Upstream struct {
	// URL is the MCP endpoint of the server.
	URL *url.URL
	// Headers are added to every request sent to the server, replacing the
	// client's: credentials of the gateway itself, for instance.
	Headers http.Header
}

// Built-in defaults.
const (
	DefaultListen         = "127.0.0.1:8787"
	DefaultMaxMessageSize = 64 << 20
	DefaultMaxRequestSize = 4 << 20
	// MinTokenLength refuses tokens short enough to be guessed.
	MinTokenLength = 24
)

// Config is a loaded, validated configuration.
type Config struct {
	// Source is the file the configuration came from, empty for built-in
	// defaults.
	Source    string
	PinsDir   string
	Telemetry Telemetry
	Limits    Limits
	Audit     Audit
	Gateway   Gateway

	defaults  policySpec
	servers   map[string]serverSpec
	upstreams map[string]Upstream
}

// Upstream returns the HTTP upstream of a server, if it has one.
func (c *Config) Upstream(server string) (Upstream, bool) {
	u, ok := c.upstreams[server]
	return u, ok
}

// Upstreams lists the servers that have an HTTP upstream, sorted.
func (c *Config) Upstreams() []string {
	names := make([]string, 0, len(c.upstreams))
	for n := range c.upstreams {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Policy returns the rules for a server: built-in defaults, overridden by the
// defaults section, overridden by the server's own section.
func (c *Config) Policy(server string) Policy {
	p := builtinPolicy()
	c.defaults.applyTo(&p)
	if s, ok := c.servers[server]; ok {
		s.policy().applyTo(&p)
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
	Audit     auditSpec             `yaml:"audit"`
	Limits    limitsSpec            `yaml:"limits"`
	Gateway   gatewaySpec           `yaml:"gateway"`
	Defaults  policySpec            `yaml:"defaults"`
	Servers   map[string]serverSpec `yaml:"servers"`
}

type auditSpec struct {
	File     string `yaml:"file"`
	MaxSize  string `yaml:"max_size"`
	MaxFiles int    `yaml:"max_files"`
}

type limitsSpec struct {
	MaxMessageSize string `yaml:"max_message_size"`
	StopTimeout    string `yaml:"stop_timeout"`
}

type gatewaySpec struct {
	Listen         string            `yaml:"listen"`
	AllowedOrigins []string          `yaml:"allowed_origins"`
	AllowedHosts   []string          `yaml:"allowed_hosts"`
	Tokens         map[string]string `yaml:"tokens"`
	AllowAnonymous bool              `yaml:"allow_anonymous"`
	TLS            tlsSpec           `yaml:"tls"`
	MaxRequestSize string            `yaml:"max_request_size"`
}

type tlsSpec struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// serverSpec is a server section: its policy, and its HTTP upstream for the
// gateway.
type serverSpec struct {
	Mode    string            `yaml:"mode"`
	Tools   *toolsSpec        `yaml:"tools"`
	Pins    *pinsSpec         `yaml:"pins"`
	Secrets *secretsSpec      `yaml:"secrets"`
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
}

func (s serverSpec) policy() policySpec {
	return policySpec{Mode: s.Mode, Tools: s.Tools, Pins: s.Pins, Secrets: s.Secrets}
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
		Limits:    Limits{MaxMessageSize: DefaultMaxMessageSize},
		Gateway:   Gateway{Listen: DefaultListen, MaxRequestSize: DefaultMaxRequestSize},
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
	cfg, err := Default()
	if err != nil {
		return nil, err
	}
	base := filepath.Dir(path)
	errs := []error{spec.Defaults.validate("defaults")}
	cfg.upstreams = make(map[string]Upstream)
	for name, s := range spec.Servers {
		if !pins.ValidServerName(name) {
			errs = append(errs, fmt.Errorf("servers.%s: invalid server name (letters, digits, '.', '_' or '-', 64 characters at most)", name))
		}
		errs = append(errs, s.policy().validate("servers."+name))
		if s.URL == "" {
			if len(s.Headers) > 0 {
				errs = append(errs, fmt.Errorf("servers.%s.headers: only used with url", name))
			}
			continue
		}
		up, err := parseUpstream("servers."+name, s)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		cfg.upstreams[name] = up
	}
	errs = append(errs, spec.Limits.apply(&cfg.Limits), spec.Audit.apply(&cfg.Audit, base), spec.Gateway.apply(&cfg.Gateway, base))
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
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
		dir, err := expandPath(spec.PinsDir, base)
		if err != nil {
			return nil, fmt.Errorf("%s: pins_dir: %w", path, err)
		}
		cfg.PinsDir = dir
	}
	return cfg, nil
}

func (s limitsSpec) apply(l *Limits) error {
	var errs []error
	if s.MaxMessageSize != "" {
		n, err := ParseSize(s.MaxMessageSize)
		if err != nil {
			errs = append(errs, fmt.Errorf("limits.max_message_size: %w", err))
		} else if n < 64<<10 {
			errs = append(errs, errors.New("limits.max_message_size: 64KiB at least"))
		}
		l.MaxMessageSize = n
	}
	if s.StopTimeout != "" {
		d, err := time.ParseDuration(s.StopTimeout)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("limits.stop_timeout: %q is not a positive duration such as 10s", s.StopTimeout))
		}
		l.StopTimeout = d
	}
	return errors.Join(errs...)
}

func (s auditSpec) apply(a *Audit, base string) error {
	if s.File == "" {
		if s.MaxSize != "" || s.MaxFiles != 0 {
			return errors.New("audit: max_size and max_files need a file")
		}
		return nil
	}
	var errs []error
	file, err := expandPath(s.File, base)
	if err != nil {
		errs = append(errs, fmt.Errorf("audit.file: %w", err))
	}
	a.File = file
	if s.MaxSize != "" {
		n, err := ParseSize(s.MaxSize)
		if err != nil {
			errs = append(errs, fmt.Errorf("audit.max_size: %w", err))
		}
		a.MaxSize = n
	}
	if s.MaxFiles < 0 {
		errs = append(errs, errors.New("audit.max_files: must be positive"))
	}
	a.MaxFiles = s.MaxFiles
	return errors.Join(errs...)
}

func (s gatewaySpec) apply(g *Gateway, base string) error {
	var errs []error
	if s.Listen != "" {
		if _, port, err := net.SplitHostPort(s.Listen); err != nil || port == "" {
			errs = append(errs, fmt.Errorf("gateway.listen: %q is not host:port", s.Listen))
		}
		g.Listen = s.Listen
	}
	for i, o := range s.AllowedOrigins {
		u, err := url.Parse(o)
		if err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" || o != strings.TrimRight(o, "/") {
			errs = append(errs, fmt.Errorf("gateway.allowed_origins[%d]: %q is not an origin such as https://app.example.com", i, o))
		}
	}
	g.AllowedOrigins = s.AllowedOrigins
	g.AllowedHosts = s.AllowedHosts
	if len(s.Tokens) > 0 {
		g.Tokens = make(map[string]string, len(s.Tokens))
		seen := make(map[string]string)
		for name, raw := range s.Tokens {
			if !pins.ValidServerName(name) {
				errs = append(errs, fmt.Errorf("gateway.tokens.%s: invalid client name (letters, digits, '.', '_' or '-')", name))
			}
			token, err := expandEnv(raw)
			if err != nil {
				errs = append(errs, fmt.Errorf("gateway.tokens.%s: %w", name, err))
				continue
			}
			if len(token) < MinTokenLength {
				errs = append(errs, fmt.Errorf("gateway.tokens.%s: %d characters at least (openssl rand -hex 32 makes a good one)", name, MinTokenLength))
			}
			if other, dup := seen[token]; dup {
				errs = append(errs, fmt.Errorf("gateway.tokens.%s: same token as %s", name, other))
			}
			seen[token] = name
			g.Tokens[name] = token
		}
	}
	g.AllowAnonymous = s.AllowAnonymous
	if (s.TLS.CertFile == "") != (s.TLS.KeyFile == "") {
		errs = append(errs, errors.New("gateway.tls: give both cert_file and key_file"))
	} else if s.TLS.CertFile != "" {
		var err1, err2 error
		g.TLSCertFile, err1 = expandPath(s.TLS.CertFile, base)
		g.TLSKeyFile, err2 = expandPath(s.TLS.KeyFile, base)
		errs = append(errs, err1, err2)
	}
	if s.MaxRequestSize != "" {
		n, err := ParseSize(s.MaxRequestSize)
		if err != nil {
			errs = append(errs, fmt.Errorf("gateway.max_request_size: %w", err))
		} else if n < 1<<10 {
			errs = append(errs, errors.New("gateway.max_request_size: 1KiB at least"))
		}
		g.MaxRequestSize = n
	}
	return errors.Join(errs...)
}

// reservedHeaders are headers a server section may not set: they belong to
// HTTP itself or to the MCP transport.
var reservedHeaders = map[string]bool{
	"Host": true, "Content-Length": true, "Content-Type": true, "Connection": true,
	"Transfer-Encoding": true, "Te": true, "Trailer": true, "Upgrade": true,
	"Keep-Alive": true, "Proxy-Connection": true, "Accept": true,
	"Mcp-Session-Id": true, "Mcp-Protocol-Version": true, "Mcp-Method": true, "Mcp-Name": true,
	"Last-Event-Id": true, "Traceparent": true, "Tracestate": true, "Baggage": true,
}

var headerToken = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`)

func parseUpstream(path string, s serverSpec) (Upstream, error) {
	u, err := url.Parse(s.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Upstream{}, fmt.Errorf("%s.url: %q is not an http:// or https:// URL", path, s.URL)
	}
	if u.User != nil {
		return Upstream{}, fmt.Errorf("%s.url: put credentials in headers, not in the URL", path)
	}
	if u.Fragment != "" {
		return Upstream{}, fmt.Errorf("%s.url: a URL fragment has no meaning here", path)
	}
	up := Upstream{URL: u, Headers: http.Header{}}
	var errs []error
	for name, raw := range s.Headers {
		canonical := http.CanonicalHeaderKey(name)
		switch {
		case !headerToken.MatchString(name):
			errs = append(errs, fmt.Errorf("%s.headers: %q is not a valid header name", path, name))
			continue
		case reservedHeaders[canonical] || strings.HasPrefix(canonical, "Mcp-Param-"):
			errs = append(errs, fmt.Errorf("%s.headers: %s is set by the client or by Vigie", path, canonical))
			continue
		}
		value, err := expandEnv(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s.headers.%s: %w", path, name, err))
			continue
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			errs = append(errs, fmt.Errorf("%s.headers.%s: line breaks are not allowed", path, name))
			continue
		}
		up.Headers.Set(canonical, value)
	}
	return up, errors.Join(errs...)
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${NAME} with the value of the environment variable
// NAME, so that credentials stay out of the file. An unset or empty variable
// is an error, rather than an empty credential.
func expandEnv(s string) (string, error) {
	var missing []string
	out := envRef.ReplaceAllStringFunc(s, func(ref string) string {
		name := envRef.FindStringSubmatch(ref)[1]
		v := os.Getenv(name)
		if v == "" {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("environment variable %s is not set", strings.Join(missing, ", "))
	}
	return out, nil
}

// ParseSize reads a size such as 512KiB, 4MiB, 1GiB, or a number of bytes.
func ParseSize(s string) (int64, error) {
	units := []struct {
		suffix string
		mult   int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1}}
	num, mult := strings.TrimSpace(s), int64(1)
	for _, u := range units {
		if strings.HasSuffix(num, u.suffix) {
			num, mult = strings.TrimSpace(strings.TrimSuffix(num, u.suffix)), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n <= 0 || n > (1<<40)/mult {
		return 0, fmt.Errorf("%q is not a size such as 512KiB, 4MiB or 1GiB", s)
	}
	return n * mult, nil
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
