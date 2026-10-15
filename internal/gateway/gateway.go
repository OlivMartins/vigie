// Package gateway serves Vigie over the MCP Streamable HTTP transport: an
// HTTP reverse proxy in front of remote MCP servers, one path per server,
// applying the same policy and tracing as the stdio relay.
//
// Every HTTP exchange is processed on its own: the gateway keeps no session
// state, so instances behind a load balancer need no affinity. The only
// shared state is the pin file of each server.
package gateway

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/OlivMartins/vigie/internal/audit"
	"github.com/OlivMartins/vigie/internal/config"
	"github.com/OlivMartins/vigie/internal/pins"
	"github.com/OlivMartins/vigie/internal/policy"
	"github.com/OlivMartins/vigie/internal/secrets"
	"github.com/OlivMartins/vigie/internal/telemetry"
)

// PathPrefix is where servers are served: /mcp/{server}.
const PathPrefix = "/mcp/"

// DefaultStopTimeout is how long requests in flight get to finish when the
// gateway stops.
const DefaultStopTimeout = 15 * time.Second

// Options configures a Gateway.
type Options struct {
	Config    *config.Config
	Telemetry *telemetry.Provider
	Scanner   *secrets.Scanner
	Logger    *slog.Logger
	Audit     *audit.Log
	// Mode overrides the policy mode of every server when set.
	Mode config.Mode
	// Transport sends requests to the servers; nil uses a transport with
	// connection pooling and sane timeouts.
	Transport http.RoundTripper
}

// Gateway is the HTTP handler and server.
type Gateway struct {
	opts    Options
	log     *slog.Logger
	routes  map[string]*route
	tokens  []clientToken
	origins map[string]bool
	// loopbackOrigins accepts pages served from this machine (localhost,
	// 127.0.0.1, [::1], any port): the default of a gateway on loopback.
	loopbackOrigins bool
	hosts           map[string]bool // nil accepts any host
	maxRequest      int64
	maxMessage      int
	stop            time.Duration
	client          *http.Client
	resumed         *registry
	propagate       bool
	draining        atomic.Bool
	// streams is cancelled when the gateway starts draining: long-lived
	// streams (GET, subscriptions/listen) close at once, clients reconnect
	// to another instance.
	streams      context.Context
	closeStreams context.CancelFunc
}

type route struct {
	name     string
	upstream config.Upstream
	engine   *policy.Engine
	store    *pins.Store
	outbound []attribute.KeyValue
}

type clientToken struct {
	name string
	hash [sha256.Size]byte
}

// New builds a gateway for every server of the configuration that has a url.
func New(opts Options) (*Gateway, error) {
	cfg := opts.Config
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Telemetry == nil {
		opts.Telemetry = telemetry.Disabled()
	}
	if opts.Scanner == nil {
		opts.Scanner = secrets.NewScanner()
	}
	if err := CheckListen(cfg.Gateway); err != nil {
		return nil, err
	}
	names := cfg.Upstreams()
	if len(names) == 0 {
		return nil, errors.New("no server to serve: give at least one server section a url")
	}
	g := &Gateway{
		opts:       opts,
		log:        opts.Logger,
		routes:     make(map[string]*route, len(names)),
		maxRequest: cfg.Gateway.MaxRequestSize,
		maxMessage: int(cfg.Limits.MaxMessageSize),
		stop:       cfg.Limits.StopTimeout,
		resumed:    newRegistry(),
		propagate:  cfg.Telemetry.PropagateContext && opts.Telemetry.Tracing,
	}
	if g.stop <= 0 {
		g.stop = DefaultStopTimeout
	}
	g.streams, g.closeStreams = context.WithCancel(context.Background())
	for _, name := range names {
		up, _ := cfg.Upstream(name)
		pol := cfg.Policy(name)
		if opts.Mode != "" {
			pol.Mode = opts.Mode
		}
		var store *pins.Store
		if pol.Pins.Enabled {
			s, err := pins.Open(cfg.PinsDir, name)
			if err != nil {
				return nil, err
			}
			if err := s.BeginShared(); err != nil {
				return nil, fmt.Errorf("%w (fix the file or delete it to start over)", err)
			}
			store = s
		}
		g.routes[name] = &route{
			name: name, upstream: up, store: store,
			engine:   policy.NewEngine(name, pol, store, opts.Scanner),
			outbound: serverAttributes(up.URL),
		}
	}
	for name, token := range cfg.Gateway.Tokens {
		g.tokens = append(g.tokens, clientToken{name: name, hash: sha256.Sum256([]byte(token))})
	}
	sort.Slice(g.tokens, func(i, j int) bool { return g.tokens[i].name < g.tokens[j].name })
	g.origins = make(map[string]bool)
	for _, o := range cfg.Gateway.AllowedOrigins {
		g.origins[strings.ToLower(o)] = true
	}
	g.loopbackOrigins = len(cfg.Gateway.AllowedOrigins) == 0 && isLoopback(cfg.Gateway.Listen)
	g.hosts = allowedHosts(cfg.Gateway)
	rt := opts.Transport
	if rt == nil {
		rt = newTransport()
	}
	g.client = &http.Client{
		Transport: rt,
		// A redirect would send the client's request, credentials included,
		// somewhere the policy does not know. It is answered with an error.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return g, nil
}

// CheckListen refuses to expose an unauthenticated gateway beyond loopback,
// unless the configuration says a proxy in front authenticates clients.
func CheckListen(gw config.Gateway) error {
	if len(gw.Tokens) > 0 || gw.AllowAnonymous || isLoopback(gw.Listen) {
		return nil
	}
	return fmt.Errorf("gateway.listen %s is reachable beyond this machine, and no client token is configured: "+
		"add gateway.tokens, or set gateway.allow_anonymous: true if a proxy in front authenticates clients", gw.Listen)
}

// Servers lists the servers served, sorted.
func (g *Gateway) Servers() []string {
	names := make([]string, 0, len(g.routes))
	for n := range g.routes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Upstream returns the URL a server is forwarded to.
func (g *Gateway) Upstream(server string) string {
	if r, ok := g.routes[server]; ok {
		return r.upstream.URL.String()
	}
	return ""
}

// PinsPath returns the pin file of a server, empty when pinning is off.
func (g *Gateway) PinsPath(server string) string {
	if r, ok := g.routes[server]; ok && r.store != nil {
		return r.store.Path()
	}
	return ""
}

// Handler returns the HTTP handler: /mcp/{server}, /healthz and /readyz.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeStatus(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if g.draining.Load() {
			writeStatus(w, http.StatusServiceUnavailable, "draining")
			return
		}
		writeStatus(w, http.StatusOK, "ready")
	})
	mux.HandleFunc(PathPrefix+"{server}", g.serveMCP)
	return mux
}

// Serve accepts connections on ln until ctx is cancelled, then stops
// gracefully: it reports not ready, closes long-lived streams, and lets
// requests in flight finish within the stop timeout.
func (g *Gateway) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           g.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(g.log.Handler(), slog.LevelDebug),
	}
	cfg := g.opts.Config.Gateway
	errc := make(chan error, 1)
	go func() {
		if cfg.TLSCertFile != "" {
			errc <- srv.ServeTLS(ln, cfg.TLSCertFile, cfg.TLSKeyFile)
			return
		}
		errc <- srv.Serve(ln)
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	g.draining.Store(true)
	g.closeStreams()
	g.log.Info("gateway stopping", "stop_timeout", g.stop.String())
	sctx, cancel := context.WithTimeout(context.Background(), g.stop)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		g.log.Warn("requests still in flight when the stop timeout expired: closed", "error", err)
		_ = srv.Close()
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// authenticate returns the name of the client presenting a valid token, ""
// when no token is configured, and false when the request is refused.
func (g *Gateway) authenticate(r *http.Request) (string, bool) {
	if len(g.tokens) == 0 {
		return "", true
	}
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	name := ""
	for _, t := range g.tokens {
		// Every token is compared, in constant time.
		if subtle.ConstantTimeCompare(sum[:], t.hash[:]) == 1 {
			name = t.name
		}
	}
	return name, name != ""
}

// originAllowed reports whether a request may proceed with its Origin
// header. Requests without one come from programs, not web pages; a page
// from another site is refused, whatever its address resolves to (DNS
// rebinding).
func (g *Gateway) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || g.origins[strings.ToLower(origin)] {
		return true
	}
	if !g.loopbackOrigins {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func (g *Gateway) hostAllowed(r *http.Request) bool {
	if g.hosts == nil {
		return true
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return g.hosts[strings.ToLower(strings.Trim(host, "[]"))]
}

// allowedHosts returns the Host values accepted: the configured ones, else
// the loopback names when listening on loopback (DNS rebinding protection),
// else any.
func allowedHosts(gw config.Gateway) map[string]bool {
	var hosts []string
	switch {
	case len(gw.AllowedHosts) > 0:
		hosts = gw.AllowedHosts
	case isLoopback(gw.Listen):
		hosts = []string{"localhost", "127.0.0.1", "::1"}
	default:
		return nil
	}
	m := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		m[strings.ToLower(strings.Trim(h, "[]"))] = true
	}
	return m
}

func isLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// serverAttributes describe the upstream on CLIENT spans.
func serverAttributes(u *url.URL) []attribute.KeyValue {
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	attrs := []attribute.KeyValue{telemetry.ServerAddress.String(u.Hostname())}
	if p, err := strconv.Atoi(port); err == nil {
		attrs = append(attrs, telemetry.ServerPort.Int(p))
	}
	return attrs
}

// sessionHash identifies an MCP session in telemetry and the audit log
// without revealing its id.
func sessionHash(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:16])
}

func newTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2: true,
		// Clients hold many requests open at once (streams, slow tools):
		// connections are kept for reuse rather than reopened.
		MaxIdleConns:          1024,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

func writeStatus(w http.ResponseWriter, code int, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, "{\"status\":%q}\n", status)
}
