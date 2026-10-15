// Package proxy applies Vigie's policy to the MCP messages exchanged between a
// client (the agent's host) and an upstream server, and traces every request
// on the way. The Conversation does the work and knows nothing of transports;
// the Relay carries it over stdio, the gateway package over HTTP.
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/OlivMartins/vigie/internal/audit"
	"github.com/OlivMartins/vigie/internal/config"
	"github.com/OlivMartins/vigie/internal/jsonrpc"
	"github.com/OlivMartins/vigie/internal/mcp"
	"github.com/OlivMartins/vigie/internal/policy"
	"github.com/OlivMartins/vigie/internal/secrets"
	"github.com/OlivMartins/vigie/internal/telemetry"
)

// MaxCapturedBytes bounds tool arguments and results attached to spans when
// content capture is on.
const MaxCapturedBytes = 32 << 10

// Options configures a Conversation, and the Relay that carries one.
type Options struct {
	Engine    *policy.Engine
	Telemetry *telemetry.Provider
	Scanner   *secrets.Scanner
	Logger    *slog.Logger
	// Audit records every decision in the audit log; nil records nothing.
	Audit *audit.Log
	// Propagate writes Vigie's trace context into relayed requests.
	Propagate bool
	// CaptureContent records tool arguments and results on spans, with
	// credentials masked.
	CaptureContent bool
	// Transport describes the connection the messages travel on. The zero
	// value describes stdio.
	Transport Transport
	// IgnoreServerRequests leaves requests sent by the server untraced. Over
	// HTTP, the client answers them in another HTTP exchange, possibly on
	// another Vigie instance: their spans could never be ended.
	IgnoreServerRequests bool
}

// Transport holds the attributes that describe where messages travel, after
// the OpenTelemetry conventions.
type Transport struct {
	// Name is recorded in the audit log: "stdio" or "http".
	Name string
	// Network goes on both spans and on the metrics: network.transport and,
	// over HTTP, network.protocol.name and version.
	Network []attribute.KeyValue
	// Inbound goes on SERVER spans only: client.address, mcp.session.id...
	Inbound []attribute.KeyValue
	// Outbound goes on CLIENT spans only: server.address, server.port.
	Outbound []attribute.KeyValue
	// Session identifies the MCP session in the audit log, already hashed.
	Session string
	// Client names the authenticated client, if any.
	Client string
	// CarriesContext reports that the transport carries the trace context
	// outside the message (HTTP headers). Vigie then writes it into
	// params._meta only when the request already has one: some servers take
	// a _meta object for the mark of revision 2026-07-28.
	CarriesContext bool
}

// StdioTransport describes the stdio transport.
func StdioTransport() Transport {
	return Transport{Name: "stdio", Network: []attribute.KeyValue{telemetry.NetworkTransport.String(telemetry.TransportPipe)}}
}

// side is where a message comes from.
type side int

const (
	fromClient side = iota // the MCP host running the agent
	fromServer             // the upstream MCP server
)

type callKey struct {
	from side
	id   string
}

// call is a request in flight. Vigie records two spans for it: a SERVER span
// for receiving it, and a CLIENT span for relaying it to the peer. Their
// difference is the time Vigie itself adds.
type call struct {
	from        side
	id          jsonrpc.ID
	req         mcp.Request
	version     string
	ctx         context.Context
	outCtx      context.Context
	inbound     trace.Span
	outbound    trace.Span
	attrs       []attribute.KeyValue
	metricAttrs []attribute.KeyValue
	start       time.Time
	relayedAt   time.Time
	decision    policy.Decision
	// ended is set when the spans were closed before the response, after a
	// cancellation, or for a call adopted after a stream resumed. The call
	// stays tracked: a late tools/list response must still be filtered.
	ended bool
}

// Pending is a client request relayed to the server and not answered yet.
type Pending struct {
	ID      jsonrpc.ID
	Request mcp.Request
}

// Conversation applies the policy to the messages of one MCP conversation and
// traces them. Over stdio, it lives as long as the session; over HTTP, as long
// as one HTTP exchange. Its methods are safe for concurrent use.
type Conversation struct {
	opts      Options
	log       *slog.Logger
	tracer    trace.Tracer
	inst      *telemetry.Instruments
	server    string
	mode      config.Mode
	propagate bool
	network   []attribute.KeyValue
	parent    context.Context

	mu        sync.Mutex
	calls     map[callKey]*call
	version   string // negotiated by initialize, or given by the transport
	requested string // asked for by initialize
}

// NewConversation returns an empty conversation.
func NewConversation(opts Options) *Conversation {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Telemetry == nil {
		opts.Telemetry = telemetry.Disabled()
	}
	if opts.Scanner == nil {
		opts.Scanner = secrets.NewScanner()
	}
	if opts.Transport.Network == nil {
		opts.Transport = StdioTransport()
	}
	return &Conversation{
		opts:      opts,
		log:       opts.Logger,
		tracer:    opts.Telemetry.Tracer,
		inst:      opts.Telemetry.Instruments,
		server:    opts.Engine.Server(),
		mode:      opts.Engine.Policy().Mode,
		propagate: opts.Propagate && opts.Telemetry.Tracing,
		network:   opts.Transport.Network,
		parent:    context.Background(),
		calls:     make(map[callKey]*call),
	}
}

// Mode returns the policy mode: enforce or audit.
func (c *Conversation) Mode() config.Mode { return c.mode }

// Logger returns the logger of the conversation.
func (c *Conversation) Logger() *slog.Logger { return c.log }

// SetVersion records the protocol version the transport announced (the
// MCP-Protocol-Version HTTP header), for requests that do not carry one.
func (c *Conversation) SetVersion(v string) {
	c.mu.Lock()
	c.version = v
	c.mu.Unlock()
}

// SetParent sets the trace context the transport carried (HTTP headers). A
// request with a trace context in params._meta keeps its own.
func (c *Conversation) SetParent(ctx context.Context) { c.parent = ctx }

// paramKeys are the params members Vigie reads.
var paramKeys = []string{"name", "arguments", "_meta", "uri", "protocolVersion", "requestId", "cursor"}

// FromClient processes a message from the client. It reports whether the
// message goes on to the server, and the reply Vigie sends the client in its
// place, if any.
func (c *Conversation) FromClient(m *jsonrpc.Message) (forward bool, reply *jsonrpc.Message) {
	if m.Kind != jsonrpc.KindResponse {
		if err := CheckRequestKeys(m); err != nil {
			reason := c.opts.Scanner.RedactText(err.Error())
			c.log.Warn("ambiguous message from client", "method", m.Method, "error", reason)
			if c.mode == config.ModeEnforce {
				if m.Kind == jsonrpc.KindRequest {
					return false, jsonrpc.NewErrorResponse(m.ID, jsonrpc.CodeInvalidRequest, "message rejected by Vigie: "+reason)
				}
				return false, nil
			}
		}
	}
	switch m.Kind {
	case jsonrpc.KindRequest:
		req := mcp.Inspect(m)
		if m.Method == mcp.MethodInitialize {
			c.mu.Lock()
			c.requested = req.RequestedVersion
			c.mu.Unlock()
		}
		k := c.begin(fromClient, m, req)
		if m.Method == mcp.MethodToolsCall {
			if c.opts.CaptureContent && len(req.Arguments) > 0 {
				k.inbound.SetAttributes(telemetry.GenAIToolCallArguments.String(c.capture(req.Arguments)))
			}
			v := c.opts.Engine.CheckCall(req.Tool, req.Arguments)
			c.recordCallVerdict(k, v)
			if v.Decision == policy.Block {
				c.endBlocked(k, v)
				text := fmt.Sprintf("Vigie blocked this call to tool %q: %s. The request was not sent to the server.",
					c.opts.Engine.DisplayName(req.Tool), v.Primary().Reason)
				return false, jsonrpc.NewResponse(m.ID, mcp.ErrorToolResult(text, mcp.IsStateless(k.version)))
			}
		}
		c.relay(k, m)
		return true, nil
	case jsonrpc.KindNotification:
		if !strings.HasPrefix(m.Method, "notifications/") {
			// A request without id is a notification to JSON-RPC: nothing
			// answers it, but its bytes would still reach the server unchecked.
			c.log.Warn("request without id from client", "method", m.Method)
			if c.mode == config.ModeEnforce {
				return false, nil
			}
		}
		if m.Method == mcp.NotificationCancelled {
			c.cancel(fromClient, mcp.Inspect(m).CancelledID)
		}
		return true, nil
	default: // a response to a request the server sent
		if k := c.take(fromServer, m.ID); k != nil && !k.ended {
			c.end(k, m)
		}
		return true, nil
	}
}

// CheckRequestKeys rejects a request or notification that peers could read
// differently: a member given twice, or one whose name differs from a member
// Vigie reads only by case.
func CheckRequestKeys(m *jsonrpc.Message) error {
	if raw := m.Original(); raw != nil {
		if err := jsonrpc.CheckKeys(raw, jsonrpc.Envelope...); err != nil {
			return err
		}
	}
	if params, ok := m.Members["params"]; ok {
		return jsonrpc.CheckKeys(params, paramKeys...)
	}
	return nil
}

// FromServer processes a message from the server and reports whether it
// should reach the client.
func (c *Conversation) FromServer(m *jsonrpc.Message) bool {
	switch m.Kind {
	case jsonrpc.KindResponse:
		k := c.take(fromClient, m.ID)
		if k == nil {
			// Ids are matched exactly. A response the relay did not ask for,
			// or an id spelled differently ("5" for 5, 5.0...), could still be
			// taken by a lenient client as the answer to a request: it would
			// bypass the checks below.
			c.log.Warn("response for no pending request", "id", c.opts.Scanner.RedactText(m.ID.String()))
			return c.mode == config.ModeAudit
		}
		if raw := m.Original(); raw != nil {
			if err := jsonrpc.CheckKeys(raw, jsonrpc.Envelope...); err != nil {
				c.log.Warn("ambiguous response from server", "method", k.req.Method, "error", c.opts.Scanner.RedactText(err.Error()))
				if c.mode == config.ModeEnforce {
					*m = *jsonrpc.NewErrorResponse(m.ID, jsonrpc.CodeInternalError, "Vigie rejected an ambiguous response from the server")
				}
			}
		}
		switch k.req.Method {
		case mcp.MethodInitialize:
			if v := mcp.NegotiatedVersion(m.Result()); v != "" {
				c.mu.Lock()
				c.version = v
				c.mu.Unlock()
				k.version = v
				k.inbound.SetAttributes(telemetry.MCPProtocolVersion.String(v))
				if k.outbound != nil {
					k.outbound.SetAttributes(telemetry.MCPProtocolVersion.String(v))
				}
			}
		case mcp.MethodToolsList:
			c.filterToolList(k, m)
		case mcp.MethodToolsCall:
			c.inspectToolResult(k, m)
		}
		if !k.ended {
			c.end(k, m)
		}
	case jsonrpc.KindRequest:
		if c.opts.IgnoreServerRequests {
			return true
		}
		k := c.begin(fromServer, m, mcp.Inspect(m))
		c.relay(k, m)
	case jsonrpc.KindNotification:
		switch m.Method {
		case mcp.NotificationCancelled:
			c.cancel(fromServer, mcp.Inspect(m).CancelledID)
		case mcp.NotificationToolsChanged:
			c.log.Debug("server reports that its tool list changed")
		}
	}
	return true
}

// Expects reports whether a response with this id would be matched to a
// request.
func (c *Conversation) Expects(id jsonrpc.ID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.calls[callKey{fromClient, id.Key()}]
	return ok
}

// Adopt tracks a request relayed in another exchange whose response arrives
// in this one: a stream resumed after a disconnection. Its spans were ended
// with the original exchange; the response is still checked.
func (c *Conversation) Adopt(p Pending) {
	span := noop.Span{}
	k := &call{
		from: fromClient, id: p.ID, req: p.Request, ctx: context.Background(),
		inbound: span, start: time.Now(), decision: policy.Allow, ended: true,
	}
	c.mu.Lock()
	c.calls[callKey{fromClient, p.ID.Key()}] = k
	c.mu.Unlock()
}

// Unanswered returns the client requests relayed to the server that got no
// response yet.
func (c *Conversation) Unanswered() []Pending {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Pending
	for key, k := range c.calls {
		if key.from == fromClient {
			out = append(out, Pending{ID: k.id, Request: k.req})
		}
	}
	return out
}

// OutboundContext returns the trace context of the CLIENT span of a relayed
// client request, for transports that also carry it outside the message.
func (c *Conversation) OutboundContext(id jsonrpc.ID) (context.Context, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k, ok := c.calls[callKey{fromClient, id.Key()}]
	if !ok || k.outCtx == nil {
		return nil, false
	}
	return k.outCtx, true
}

// Fail ends the spans of every call still in flight with errorType, and
// records attrs on their CLIENT span: the transport failed to deliver them.
func (c *Conversation) Fail(errorType string, attrs ...attribute.KeyValue) {
	c.mu.Lock()
	calls := c.calls
	c.calls = make(map[callKey]*call)
	c.mu.Unlock()
	for _, k := range calls {
		if k.ended {
			continue
		}
		o := outcome{attrs: []attribute.KeyValue{telemetry.ErrorType.String(errorType)}, status: codes.Error, desc: errorType}
		out := o
		out.attrs = append(append([]attribute.KeyValue(nil), o.attrs...), attrs...)
		c.finish(k, o, out)
	}
}

// Close ends the spans of every call still in flight, with reason as their
// error.type. A subscriptions/listen stream closed by the client ended
// normally: it has no response.
func (c *Conversation) Close(reason string) {
	c.mu.Lock()
	calls := c.calls
	c.calls = make(map[callKey]*call)
	c.mu.Unlock()
	for _, k := range calls {
		switch {
		case k.ended:
		case k.req.Method == mcp.MethodSubscriptionsListen && reason == telemetry.ErrorClientClosed:
			c.finish(k, outcome{}, outcome{})
		default:
			c.endWithError(k, reason)
		}
	}
}

// begin opens the SERVER span of a request, as a child of the trace context
// the sender put in params._meta, else of the one the transport carried.
func (c *Conversation) begin(from side, m *jsonrpc.Message, req mcp.Request) *call {
	version := req.ProtocolVersion
	if version == "" {
		c.mu.Lock()
		version = c.version
		if version == "" {
			version = c.requested
		}
		c.mu.Unlock()
	}
	attrs, metricAttrs := c.requestAttributes(m, req, version)
	parent := c.parent
	if from == fromClient {
		parent = telemetry.Extract(parent, req.Params)
	} else {
		parent = telemetry.Extract(context.Background(), req.Params)
	}
	inbound := append(append([]attribute.KeyValue(nil), attrs...), c.opts.Transport.Inbound...)
	ctx, span := c.tracer.Start(parent, c.spanName(req),
		trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(inbound...))
	return &call{
		from: from, id: m.ID, req: req, version: version, ctx: ctx,
		inbound: span, attrs: attrs, metricAttrs: metricAttrs,
		start: time.Now(), decision: policy.Allow,
	}
}

// relay opens the CLIENT span, injects its context into the request and
// tracks the call until the response comes back.
func (c *Conversation) relay(k *call, m *jsonrpc.Message) {
	outbound := append(append([]attribute.KeyValue(nil), k.attrs...), c.opts.Transport.Outbound...)
	ctx, span := c.tracer.Start(k.ctx, c.spanName(k.req),
		trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(outbound...))
	k.outbound, k.outCtx, k.relayedAt = span, ctx, time.Now()
	if c.propagate {
		_, hasMeta := k.req.Params["_meta"]
		// Only touch params when they are absent or an object.
		if _, has := m.Members["params"]; (!has || k.req.Params != nil) && (hasMeta || !c.opts.Transport.CarriesContext) {
			if params, changed, err := telemetry.Inject(ctx, k.req.Params); err == nil && changed {
				if err := m.SetParams(params); err != nil {
					c.log.Debug("trace context not injected", "error", err)
				}
			}
		}
	}
	key := callKey{k.from, k.id.Key()}
	c.mu.Lock()
	prev := c.calls[key]
	c.calls[key] = k
	c.mu.Unlock()
	if prev != nil && !prev.ended {
		c.endWithError(prev, telemetry.ErrorDuplicateID)
	}
}

func (c *Conversation) take(from side, id jsonrpc.ID) *call {
	key := callKey{from, id.Key()}
	c.mu.Lock()
	defer c.mu.Unlock()
	k := c.calls[key]
	delete(c.calls, key)
	return k
}

func (c *Conversation) cancel(from side, id jsonrpc.ID) {
	if id.IsZero() {
		return
	}
	c.mu.Lock()
	k := c.calls[callKey{from, id.Key()}]
	if k == nil || k.ended {
		c.mu.Unlock()
		return
	}
	k.ended = true
	c.mu.Unlock()
	c.endWithError(k, telemetry.ErrorCancelled)
}

// end closes both spans of a call with the outcome of the response.
func (c *Conversation) end(k *call, resp *jsonrpc.Message) {
	c.finish(k, c.classify(k.req.Method, resp, false), c.classify(k.req.Method, resp, true))
}

func (c *Conversation) endWithError(k *call, errorType string) {
	o := outcome{attrs: []attribute.KeyValue{telemetry.ErrorType.String(errorType)}, status: codes.Error, desc: errorType}
	c.finish(k, o, o)
}

// finish ends the SERVER span with in and the CLIENT span with out.
func (c *Conversation) finish(k *call, in, out outcome) {
	now := time.Now()
	if k.outbound != nil {
		k.outbound.SetAttributes(out.attrs...)
		k.outbound.SetStatus(out.status, out.desc)
		k.outbound.End(trace.WithTimestamp(now))
		c.inst.ClientDuration.Record(context.Background(), now.Sub(k.relayedAt).Seconds(),
			metric.WithAttributes(append(k.metricAttrs, errorAttrs(out.attrs)...)...))
	}
	k.inbound.SetAttributes(in.attrs...)
	k.inbound.SetStatus(in.status, in.desc)
	k.inbound.End(trace.WithTimestamp(now))
	c.inst.ServerDuration.Record(context.Background(), now.Sub(k.start).Seconds(),
		metric.WithAttributes(append(k.metricAttrs, errorAttrs(in.attrs)...)...))
}

func (c *Conversation) endBlocked(k *call, v policy.Verdict) {
	o := outcome{attrs: []attribute.KeyValue{telemetry.ErrorType.String(telemetry.ErrorToolError)},
		status: codes.Error, desc: "blocked by Vigie: " + v.Primary().Rule}
	c.finish(k, o, o)
}

func (c *Conversation) recordCallVerdict(k *call, v policy.Verdict) {
	k.decision = v.Decision
	attrs := []attribute.KeyValue{
		telemetry.VigieDecision.String(string(v.Decision)),
		telemetry.VigieMode.String(string(c.mode)),
	}
	if p := v.Primary(); p != nil {
		attrs = append(attrs, telemetry.VigieRule.String(p.Rule))
	}
	if v.PinStatus != "" {
		attrs = append(attrs, telemetry.VigiePinStatus.String(string(v.PinStatus)))
	}
	k.inbound.SetAttributes(attrs...)
	addFindings(k.inbound, v.Findings)
	c.countDecision(k, v)
	c.logVerdict("tool call", k.req.Tool, v)
	c.record(k, audit.EventToolCall, v)
}

func (c *Conversation) filterToolList(k *call, resp *jsonrpc.Message) {
	result := resp.Result()
	if result == nil {
		return
	}
	list, err := mcp.ParseToolList(result)
	if err == nil {
		err = jsonrpc.CheckKeys(result, "tools")
	}
	if err != nil {
		c.log.Warn("tools/list result could not be checked", "error", c.opts.Scanner.RedactText(err.Error()))
		if c.mode == config.ModeEnforce {
			*resp = *jsonrpc.NewErrorResponse(resp.ID, jsonrpc.CodeInternalError, "Vigie could not check the tool list")
		}
		return
	}
	out := c.opts.Engine.CheckList(list.Tools)
	attrs := []attribute.KeyValue{
		telemetry.VigieToolsListed.Int(len(list.Tools)),
		telemetry.VigieToolsHidden.Int(out.Hidden),
		telemetry.VigieToolsNew.Int(out.New),
		telemetry.VigieToolsChanged.Int(out.Changed),
		telemetry.VigieToolsPending.Int(out.Pending),
		telemetry.VigieDecision.String(string(out.Verdict.Decision)),
		telemetry.VigieMode.String(string(c.mode)),
	}
	if p := out.Verdict.Primary(); p != nil {
		attrs = append(attrs, telemetry.VigieRule.String(p.Rule))
	}
	k.inbound.SetAttributes(attrs...)
	addFindings(k.inbound, out.Verdict.Findings)
	k.decision = out.Verdict.Decision
	c.countDecision(k, out.Verdict)
	if len(out.Verdict.Findings) > 0 {
		c.record(k, audit.EventToolList, out.Verdict)
	}
	if out.Hidden > 0 {
		list.ExpireCache()
		encoded, err := list.Encode(out.Kept)
		if err != nil {
			// Fail closed: an unfiltered list would expose unapproved tools.
			c.log.Error("tools/list result could not be filtered, answering with an error", "error", err)
			*resp = *jsonrpc.NewErrorResponse(resp.ID, jsonrpc.CodeInternalError, "Vigie could not filter the tool list")
			return
		}
		resp.SetResult(encoded)
	}
	for _, f := range out.Verdict.Findings {
		c.logFinding(f, out.Verdict.Decision)
	}
}

func (c *Conversation) inspectToolResult(k *call, resp *jsonrpc.Message) {
	result := resp.Result()
	if result == nil {
		return
	}
	v := c.opts.Engine.CheckResult(c.opts.Engine.DisplayName(k.req.Tool), result)
	if len(v.Findings) > 0 {
		addFindings(k.inbound, v.Findings)
		if v.Decision.Severity() > k.decision.Severity() {
			k.decision = v.Decision
			k.inbound.SetAttributes(
				telemetry.VigieDecision.String(string(v.Decision)),
				telemetry.VigieRule.String(v.Primary().Rule))
		}
		c.logVerdict("tool result", k.req.Tool, v)
		c.record(k, audit.EventToolResult, v)
	}
	if c.opts.CaptureContent {
		k.inbound.SetAttributes(telemetry.GenAIToolCallResult.String(c.capture(result)))
	}
}

// record writes a decision to the audit log.
func (c *Conversation) record(k *call, event string, v policy.Verdict) {
	if c.opts.Audit == nil {
		return
	}
	e := audit.Entry{
		Time:      time.Now().UTC(),
		Event:     event,
		Server:    c.server,
		Transport: c.opts.Transport.Name,
		Session:   c.opts.Transport.Session,
		Client:    c.opts.Transport.Client,
		Method:    k.req.Method,
		Tool:      c.opts.Engine.DisplayName(k.req.Tool),
		Decision:  string(v.Decision),
		Mode:      string(c.mode),
		PinStatus: string(v.PinStatus),
	}
	if p := v.Primary(); p != nil {
		e.Rule = p.Rule
	}
	if sc := k.inbound.SpanContext(); sc.IsValid() {
		e.TraceID, e.SpanID = sc.TraceID().String(), sc.SpanID().String()
	}
	for _, f := range v.Findings {
		e.Findings = append(e.Findings, audit.Finding{
			Rule: f.Rule, Action: string(f.Action), Tool: f.Tool,
			Location: f.Location, SecretType: f.SecretType, Reason: f.Reason,
		})
	}
	if err := c.opts.Audit.Write(e); err != nil {
		c.log.Error("audit log not written", "error", err)
	}
}

func (c *Conversation) countDecision(k *call, v policy.Verdict) {
	attrs := []attribute.KeyValue{
		telemetry.VigieUpstream.String(c.server),
		telemetry.MCPMethodName.String(k.req.Method),
		telemetry.VigieDecision.String(string(v.Decision)),
	}
	if k.req.Tool != "" {
		attrs = append(attrs, telemetry.GenAIToolName.String(c.opts.Engine.DisplayName(k.req.Tool)))
	}
	if p := v.Primary(); p != nil {
		attrs = append(attrs, telemetry.VigieRule.String(p.Rule))
	}
	c.inst.Decisions.Add(context.Background(), 1, metric.WithAttributes(attrs...))
}

func (c *Conversation) logVerdict(what, tool string, v policy.Verdict) {
	p := v.Primary()
	if p == nil {
		return
	}
	level, msg := slog.LevelInfo, what+" flagged"
	switch v.Decision {
	case policy.Block:
		level, msg = slog.LevelWarn, what+" blocked"
	case policy.Audit:
		level, msg = slog.LevelWarn, what+" would be blocked (audit mode)"
	}
	c.log.Log(context.Background(), level, msg, "tool", c.opts.Engine.DisplayName(tool), "rule", p.Rule, "reason", p.Reason)
}

func (c *Conversation) logFinding(f policy.Finding, d policy.Decision) {
	level, msg := slog.LevelInfo, "tool flagged in tools/list"
	if f.Action == config.ActionBlock {
		level, msg = slog.LevelWarn, "tool hidden from tools/list"
		if d == policy.Audit {
			msg = "tool would be hidden from tools/list (audit mode)"
		}
	}
	c.log.Log(context.Background(), level, msg, "tool", f.Tool, "rule", f.Rule, "reason", f.Reason)
}

func (c *Conversation) capture(raw json.RawMessage) string {
	s := string(c.opts.Scanner.Redact(raw))
	if len(s) > MaxCapturedBytes {
		s = s[:MaxCapturedBytes] + "…[truncated by Vigie]"
	}
	return s
}

// requestAttributes returns the span attributes of a request, and the subset
// suitable for metrics (low cardinality).
func (c *Conversation) requestAttributes(m *jsonrpc.Message, req mcp.Request, version string) (span, metrics []attribute.KeyValue) {
	metrics = append([]attribute.KeyValue{telemetry.MCPMethodName.String(req.Method)}, c.network...)
	metrics = append(metrics, telemetry.VigieUpstream.String(c.server))
	if version != "" {
		metrics = append(metrics, telemetry.MCPProtocolVersion.String(version))
	}
	if m.Version != "" && m.Version != jsonrpc.Version {
		metrics = append(metrics, telemetry.JSONRPCProtocolVersion.String(m.Version))
	}
	switch {
	case req.Method == mcp.MethodToolsCall:
		metrics = append(metrics, telemetry.GenAIOperationName.String(telemetry.OperationExecuteTool))
		if req.Tool != "" {
			metrics = append(metrics, telemetry.GenAIToolName.String(c.opts.Engine.DisplayName(req.Tool)))
		}
	case req.Prompt != "":
		metrics = append(metrics, telemetry.GenAIPromptName.String(c.opts.Engine.DisplayName(req.Prompt)))
	}
	span = append([]attribute.KeyValue(nil), metrics...)
	if id := m.ID.String(); id != "" {
		span = append(span, telemetry.JSONRPCRequestID.String(c.opts.Scanner.RedactText(id)))
	}
	if req.ResourceURI != "" {
		span = append(span, telemetry.MCPResourceURI.String(c.opts.Scanner.RedactText(req.ResourceURI)))
	}
	return span, metrics
}

// spanName follows the conventions: "{mcp.method.name} {target}", the target
// being a tool or prompt name. Resource URIs are left out: too many distinct
// values for a span name.
func (c *Conversation) spanName(req mcp.Request) string {
	switch {
	case req.Tool != "":
		return req.Method + " " + c.opts.Engine.DisplayName(req.Tool)
	case req.Prompt != "":
		return req.Method + " " + c.opts.Engine.DisplayName(req.Prompt)
	default:
		return req.Method
	}
}

// callerErrors are the JSON-RPC codes that mean the caller sent a request the
// receiver could not serve. The conventions do not count them as errors on
// the SERVER span; on the CLIENT span, every error code is an error.
// Resource not found was -32002 until 2026-07-28, -32602 since.
var callerErrors = map[int]bool{
	jsonrpc.CodeParseError: true, jsonrpc.CodeInvalidRequest: true,
	jsonrpc.CodeMethodNotFound: true, jsonrpc.CodeInvalidParams: true, -32002: true,
}

// outcome is how a span ends.
type outcome struct {
	attrs  []attribute.KeyValue
	status codes.Code
	desc   string
}

// classify reads the outcome of a response the way the MCP conventions
// describe it, for the CLIENT span (client) or the SERVER span. Error
// messages written by peers are redacted before they reach telemetry.
func (c *Conversation) classify(method string, resp *jsonrpc.Message, client bool) outcome {
	if _, isError := resp.Members["error"]; isError {
		e := resp.Error()
		if e == nil {
			return outcome{[]attribute.KeyValue{telemetry.ErrorType.String("_OTHER")}, codes.Error, "malformed error response"}
		}
		code := jsonrpc.CodeString(e.Code)
		attrs := []attribute.KeyValue{telemetry.RPCResponseStatusCode.String(code)}
		if callerErrors[e.Code] && !client {
			return outcome{attrs, codes.Unset, ""}
		}
		return outcome{append(attrs, telemetry.ErrorType.String(code)), codes.Error, c.opts.Scanner.RedactText(e.Message)}
	}
	result := resp.Result()
	if method == mcp.MethodToolsCall && mcp.ToolResultIsError(result) {
		return outcome{[]attribute.KeyValue{telemetry.ErrorType.String(telemetry.ErrorToolError)}, codes.Error, ""}
	}
	if rt := mcp.ResultType(result); rt != "" && rt != "complete" {
		return outcome{[]attribute.KeyValue{telemetry.VigieResultType.String(rt)}, codes.Unset, ""}
	}
	return outcome{}
}

// errorAttrs keeps the attributes metrics may carry.
func errorAttrs(attrs []attribute.KeyValue) []attribute.KeyValue {
	var out []attribute.KeyValue
	for _, a := range attrs {
		if a.Key == telemetry.ErrorType || a.Key == telemetry.RPCResponseStatusCode {
			out = append(out, a)
		}
	}
	return out
}

func addFindings(span trace.Span, findings []policy.Finding) {
	for _, f := range findings {
		attrs := []attribute.KeyValue{
			telemetry.VigieRule.String(f.Rule),
			telemetry.VigieFindingAction.String(string(f.Action)),
			telemetry.VigieFindingReason.String(f.Reason),
		}
		if f.Tool != "" {
			attrs = append(attrs, telemetry.GenAIToolName.String(f.Tool))
		}
		if f.Location != "" {
			attrs = append(attrs, telemetry.VigieFindingLocation.String(f.Location))
		}
		if f.SecretType != "" {
			attrs = append(attrs, telemetry.VigieSecretType.String(f.SecretType))
		}
		span.AddEvent(telemetry.EventFinding, trace.WithAttributes(attrs...))
	}
}
