// Package proxy relays MCP messages between a client (the agent's host) and
// an upstream server, tracing every request and applying the policy on the
// way.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/OlivMartins/vigie/internal/config"
	"github.com/OlivMartins/vigie/internal/jsonrpc"
	"github.com/OlivMartins/vigie/internal/mcp"
	"github.com/OlivMartins/vigie/internal/policy"
	"github.com/OlivMartins/vigie/internal/secrets"
	"github.com/OlivMartins/vigie/internal/telemetry"
)

// Errors returned when a stream cannot be written to anymore.
var (
	ErrClientGone = errors.New("client stream closed")
	ErrServerGone = errors.New("server stream closed")
)

// MaxCapturedBytes bounds tool arguments and results attached to spans when
// content capture is on.
const MaxCapturedBytes = 32 << 10

// Options configures a Relay.
type Options struct {
	Engine    *policy.Engine
	Telemetry *telemetry.Provider
	Scanner   *secrets.Scanner
	Logger    *slog.Logger
	// Propagate writes Vigie's trace context into relayed requests.
	Propagate bool
	// CaptureContent records tool arguments and results on spans, with
	// credentials masked.
	CaptureContent bool
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
	inbound     trace.Span
	outbound    trace.Span
	attrs       []attribute.KeyValue
	metricAttrs []attribute.KeyValue
	start       time.Time
	relayedAt   time.Time
	decision    policy.Decision
	// ended is set when the spans were closed before the response, after a
	// cancellation. The call stays tracked: a late tools/list response must
	// still be filtered.
	ended bool
}

// Relay processes the frames flowing in both directions. Handle* methods are
// called from one goroutine per direction.
type Relay struct {
	opts      Options
	log       *slog.Logger
	tracer    trace.Tracer
	inst      *telemetry.Instruments
	server    string
	mode      config.Mode
	propagate bool
	toClient  *jsonrpc.Writer
	toServer  *jsonrpc.Writer

	mu        sync.Mutex
	calls     map[callKey]*call
	version   string // negotiated by initialize
	requested string // asked for by initialize
	garbage   int    // unparseable frames from the server, for log throttling
}

// NewRelay returns a relay writing to the given client and server streams.
func NewRelay(opts Options, toClient, toServer *jsonrpc.Writer) *Relay {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Telemetry == nil {
		opts.Telemetry = telemetry.Disabled()
	}
	if opts.Scanner == nil {
		opts.Scanner = secrets.NewScanner()
	}
	return &Relay{
		opts:      opts,
		log:       opts.Logger,
		tracer:    opts.Telemetry.Tracer,
		inst:      opts.Telemetry.Instruments,
		server:    opts.Engine.Server(),
		mode:      opts.Engine.Policy().Mode,
		propagate: opts.Propagate && opts.Telemetry.Tracing,
		toClient:  toClient,
		toServer:  toServer,
		calls:     make(map[callKey]*call),
	}
}

// HandleClientFrame processes one frame read from the client. It returns an
// error only when a stream cannot be written to anymore.
func (r *Relay) HandleClientFrame(line []byte) error {
	frame, err := jsonrpc.ParseFrame(line)
	if err != nil {
		return r.unparseableFromClient(line, err)
	}
	var forward, replies []*jsonrpc.Message
	for _, m := range frame.Messages {
		fwd, reply := r.fromClient(m)
		if fwd {
			forward = append(forward, m)
		}
		if reply != nil {
			replies = append(replies, reply)
		}
	}
	if err := send(r.toServer, frame, forward); err != nil {
		return fmt.Errorf("%w: %w", ErrServerGone, err)
	}
	if err := send(r.toClient, &jsonrpc.Frame{Batch: frame.Batch}, replies); err != nil {
		return fmt.Errorf("%w: %w", ErrClientGone, err)
	}
	return nil
}

// HandleServerFrame processes one frame read from the server.
func (r *Relay) HandleServerFrame(line []byte) error {
	frame, err := jsonrpc.ParseFrame(line)
	if err != nil {
		r.unparseableFromServer(line, err)
		if r.mode == config.ModeEnforce {
			// What Vigie cannot read, it cannot filter: a lenient client
			// might read a tool list in it.
			return nil
		}
		if err := r.toClient.Write(line); err != nil {
			return fmt.Errorf("%w: %w", ErrClientGone, err)
		}
		return nil
	}
	keep := make([]*jsonrpc.Message, 0, len(frame.Messages))
	for _, m := range frame.Messages {
		if r.fromServer(m) {
			keep = append(keep, m)
		}
	}
	if err := send(r.toClient, frame, keep); err != nil {
		return fmt.Errorf("%w: %w", ErrClientGone, err)
	}
	return nil
}

// RejectOversized answers the client when one of its frames was too large to
// be read: the frame was dropped, its id is unknown.
func (r *Relay) RejectOversized(limit int) error {
	r.log.Error("message from client dropped: too large", "limit_bytes", limit)
	return r.toClient.WriteMessage(jsonrpc.NewErrorResponse(jsonrpc.NullID, jsonrpc.CodeInvalidRequest,
		fmt.Sprintf("message dropped by Vigie: larger than %d bytes", limit)))
}

// DropOversized records a server frame too large to be read.
func (r *Relay) DropOversized(limit int) {
	r.log.Error("message from server dropped: too large", "limit_bytes", limit)
}

// Close ends the spans of every call still in flight, with reason as their
// error.type.
func (r *Relay) Close(reason string) {
	r.mu.Lock()
	calls := r.calls
	r.calls = make(map[callKey]*call)
	r.mu.Unlock()
	for _, c := range calls {
		if !c.ended {
			r.endWithError(c, reason)
		}
	}
}

// paramKeys are the params members Vigie reads.
var paramKeys = []string{"name", "arguments", "_meta", "uri", "protocolVersion", "requestId", "cursor"}

func (r *Relay) fromClient(m *jsonrpc.Message) (forward bool, reply *jsonrpc.Message) {
	if m.Kind != jsonrpc.KindResponse {
		err := jsonrpc.CheckKeys(m.Original(), jsonrpc.Envelope...)
		if params, ok := m.Members["params"]; ok && err == nil {
			err = jsonrpc.CheckKeys(params, paramKeys...)
		}
		if err != nil {
			reason := r.opts.Scanner.RedactText(err.Error())
			r.log.Warn("ambiguous message from client", "method", m.Method, "error", reason)
			if r.mode == config.ModeEnforce {
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
			r.mu.Lock()
			r.requested = req.RequestedVersion
			r.mu.Unlock()
		}
		c := r.begin(fromClient, m, req)
		if m.Method == mcp.MethodToolsCall {
			if r.opts.CaptureContent && len(req.Arguments) > 0 {
				c.inbound.SetAttributes(telemetry.GenAIToolCallArguments.String(r.capture(req.Arguments)))
			}
			v := r.opts.Engine.CheckCall(req.Tool, req.Arguments)
			r.recordCallVerdict(c, v)
			if v.Decision == policy.Block {
				r.endBlocked(c, v)
				text := fmt.Sprintf("Vigie blocked this call to tool %q: %s. The request was not sent to the server.",
					r.opts.Engine.DisplayName(req.Tool), v.Primary().Reason)
				return false, jsonrpc.NewResponse(m.ID, mcp.ErrorToolResult(text, mcp.IsStateless(c.version)))
			}
		}
		r.relay(c, m)
		return true, nil
	case jsonrpc.KindNotification:
		if !strings.HasPrefix(m.Method, "notifications/") {
			// A request without id is a notification to JSON-RPC: nothing
			// answers it, but its bytes would still reach the server unchecked.
			r.log.Warn("request without id from client", "method", m.Method)
			if r.mode == config.ModeEnforce {
				return false, nil
			}
		}
		if m.Method == mcp.NotificationCancelled {
			r.cancel(fromClient, mcp.Inspect(m).CancelledID)
		}
		return true, nil
	default: // a response to a request the server sent
		if c := r.take(fromServer, m.ID); c != nil && !c.ended {
			r.end(c, m)
		}
		return true, nil
	}
}

// fromServer processes a message from the server and reports whether it
// should reach the client.
func (r *Relay) fromServer(m *jsonrpc.Message) bool {
	switch m.Kind {
	case jsonrpc.KindResponse:
		c := r.take(fromClient, m.ID)
		if c == nil {
			// Ids are matched exactly. A response the relay did not ask for,
			// or an id spelled differently ("5" for 5, 5.0...), could still be
			// taken by a lenient client as the answer to a request: it would
			// bypass the checks below.
			r.log.Warn("response for no pending request", "id", m.ID.String())
			return r.mode == config.ModeAudit
		}
		if err := jsonrpc.CheckKeys(m.Original(), jsonrpc.Envelope...); err != nil {
			r.log.Warn("ambiguous response from server", "method", c.req.Method, "error", r.opts.Scanner.RedactText(err.Error()))
			if r.mode == config.ModeEnforce {
				*m = *jsonrpc.NewErrorResponse(m.ID, jsonrpc.CodeInternalError, "Vigie rejected an ambiguous response from the server")
			}
		}
		switch c.req.Method {
		case mcp.MethodInitialize:
			if v := mcp.NegotiatedVersion(m.Result()); v != "" {
				r.mu.Lock()
				r.version = v
				r.mu.Unlock()
				c.version = v
				c.inbound.SetAttributes(telemetry.MCPProtocolVersion.String(v))
				if c.outbound != nil {
					c.outbound.SetAttributes(telemetry.MCPProtocolVersion.String(v))
				}
			}
		case mcp.MethodToolsList:
			r.filterToolList(c, m)
		case mcp.MethodToolsCall:
			r.inspectToolResult(c, m)
		}
		if !c.ended {
			r.end(c, m)
		}
	case jsonrpc.KindRequest:
		c := r.begin(fromServer, m, mcp.Inspect(m))
		r.relay(c, m)
	case jsonrpc.KindNotification:
		switch m.Method {
		case mcp.NotificationCancelled:
			r.cancel(fromServer, mcp.Inspect(m).CancelledID)
		case mcp.NotificationToolsChanged:
			r.log.Debug("server reports that its tool list changed")
		}
	}
	return true
}

// begin opens the SERVER span of a request, as a child of the trace context
// the sender put in params._meta.
func (r *Relay) begin(from side, m *jsonrpc.Message, req mcp.Request) *call {
	version := req.ProtocolVersion
	if version == "" {
		r.mu.Lock()
		version = r.version
		if version == "" {
			version = r.requested
		}
		r.mu.Unlock()
	}
	attrs, metricAttrs := r.requestAttributes(m, req, version)
	parent := telemetry.Extract(context.Background(), req.Params)
	ctx, span := r.tracer.Start(parent, r.spanName(req),
		trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attrs...))
	return &call{
		from: from, id: m.ID, req: req, version: version, ctx: ctx,
		inbound: span, attrs: attrs, metricAttrs: metricAttrs,
		start: time.Now(), decision: policy.Allow,
	}
}

// relay opens the CLIENT span, injects its context into the request and
// tracks the call until the response comes back.
func (r *Relay) relay(c *call, m *jsonrpc.Message) {
	ctx, span := r.tracer.Start(c.ctx, r.spanName(c.req),
		trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(c.attrs...))
	c.outbound, c.relayedAt = span, time.Now()
	if r.propagate {
		// Only touch params when they are absent or an object.
		if _, has := m.Members["params"]; !has || c.req.Params != nil {
			if params, changed, err := telemetry.Inject(ctx, c.req.Params); err == nil && changed {
				if err := m.SetParams(params); err != nil {
					r.log.Debug("trace context not injected", "error", err)
				}
			}
		}
	}
	key := callKey{c.from, c.id.Key()}
	r.mu.Lock()
	prev := r.calls[key]
	r.calls[key] = c
	r.mu.Unlock()
	if prev != nil && !prev.ended {
		r.endWithError(prev, telemetry.ErrorDuplicateID)
	}
}

func (r *Relay) take(from side, id jsonrpc.ID) *call {
	key := callKey{from, id.Key()}
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.calls[key]
	delete(r.calls, key)
	return c
}

func (r *Relay) cancel(from side, id jsonrpc.ID) {
	if id.IsZero() {
		return
	}
	r.mu.Lock()
	c := r.calls[callKey{from, id.Key()}]
	if c == nil || c.ended {
		r.mu.Unlock()
		return
	}
	c.ended = true
	r.mu.Unlock()
	r.endWithError(c, telemetry.ErrorCancelled)
}

// end closes both spans of a call with the outcome of the response.
func (r *Relay) end(c *call, resp *jsonrpc.Message) {
	r.finish(c, r.classify(c.req.Method, resp, false), r.classify(c.req.Method, resp, true))
}

func (r *Relay) endWithError(c *call, errorType string) {
	o := outcome{attrs: []attribute.KeyValue{telemetry.ErrorType.String(errorType)}, status: codes.Error, desc: errorType}
	r.finish(c, o, o)
}

// finish ends the SERVER span with in and the CLIENT span with out.
func (r *Relay) finish(c *call, in, out outcome) {
	now := time.Now()
	if c.outbound != nil {
		c.outbound.SetAttributes(out.attrs...)
		c.outbound.SetStatus(out.status, out.desc)
		c.outbound.End(trace.WithTimestamp(now))
		r.inst.ClientDuration.Record(context.Background(), now.Sub(c.relayedAt).Seconds(),
			metric.WithAttributes(append(c.metricAttrs, errorAttrs(out.attrs)...)...))
	}
	c.inbound.SetAttributes(in.attrs...)
	c.inbound.SetStatus(in.status, in.desc)
	c.inbound.End(trace.WithTimestamp(now))
	r.inst.ServerDuration.Record(context.Background(), now.Sub(c.start).Seconds(),
		metric.WithAttributes(append(c.metricAttrs, errorAttrs(in.attrs)...)...))
}

func (r *Relay) endBlocked(c *call, v policy.Verdict) {
	o := outcome{attrs: []attribute.KeyValue{telemetry.ErrorType.String(telemetry.ErrorToolError)},
		status: codes.Error, desc: "blocked by Vigie: " + v.Primary().Rule}
	r.finish(c, o, o)
}

func (r *Relay) recordCallVerdict(c *call, v policy.Verdict) {
	c.decision = v.Decision
	attrs := []attribute.KeyValue{
		telemetry.VigieDecision.String(string(v.Decision)),
		telemetry.VigieMode.String(string(r.mode)),
	}
	if p := v.Primary(); p != nil {
		attrs = append(attrs, telemetry.VigieRule.String(p.Rule))
	}
	if v.PinStatus != "" {
		attrs = append(attrs, telemetry.VigiePinStatus.String(string(v.PinStatus)))
	}
	c.inbound.SetAttributes(attrs...)
	addFindings(c.inbound, v.Findings)
	r.countDecision(c, v)
	r.logVerdict("tool call", c.req.Tool, v)
}

func (r *Relay) filterToolList(c *call, resp *jsonrpc.Message) {
	result := resp.Result()
	if result == nil {
		return
	}
	list, err := mcp.ParseToolList(result)
	if err == nil {
		err = jsonrpc.CheckKeys(result, "tools")
	}
	if err != nil {
		r.log.Warn("tools/list result could not be checked", "error", r.opts.Scanner.RedactText(err.Error()))
		if r.mode == config.ModeEnforce {
			*resp = *jsonrpc.NewErrorResponse(resp.ID, jsonrpc.CodeInternalError, "Vigie could not check the tool list")
		}
		return
	}
	out := r.opts.Engine.CheckList(list.Tools)
	attrs := []attribute.KeyValue{
		telemetry.VigieToolsListed.Int(len(list.Tools)),
		telemetry.VigieToolsHidden.Int(out.Hidden),
		telemetry.VigieToolsNew.Int(out.New),
		telemetry.VigieToolsChanged.Int(out.Changed),
		telemetry.VigieToolsPending.Int(out.Pending),
		telemetry.VigieDecision.String(string(out.Verdict.Decision)),
		telemetry.VigieMode.String(string(r.mode)),
	}
	if p := out.Verdict.Primary(); p != nil {
		attrs = append(attrs, telemetry.VigieRule.String(p.Rule))
	}
	c.inbound.SetAttributes(attrs...)
	addFindings(c.inbound, out.Verdict.Findings)
	c.decision = out.Verdict.Decision
	r.countDecision(c, out.Verdict)
	if out.Hidden > 0 {
		list.ExpireCache()
		encoded, err := list.Encode(out.Kept)
		if err != nil {
			// Fail closed: an unfiltered list would expose unapproved tools.
			r.log.Error("tools/list result could not be filtered, answering with an error", "error", err)
			*resp = *jsonrpc.NewErrorResponse(resp.ID, jsonrpc.CodeInternalError, "Vigie could not filter the tool list")
			return
		}
		resp.SetResult(encoded)
	}
	for _, f := range out.Verdict.Findings {
		r.logFinding(f, out.Verdict.Decision)
	}
}

func (r *Relay) inspectToolResult(c *call, resp *jsonrpc.Message) {
	result := resp.Result()
	if result == nil {
		return
	}
	v := r.opts.Engine.CheckResult(c.req.Tool, result)
	if len(v.Findings) > 0 {
		addFindings(c.inbound, v.Findings)
		if v.Decision.Severity() > c.decision.Severity() {
			c.decision = v.Decision
			c.inbound.SetAttributes(
				telemetry.VigieDecision.String(string(v.Decision)),
				telemetry.VigieRule.String(v.Primary().Rule))
		}
		r.logVerdict("tool result", c.req.Tool, v)
	}
	if r.opts.CaptureContent {
		c.inbound.SetAttributes(telemetry.GenAIToolCallResult.String(r.capture(result)))
	}
}

func (r *Relay) countDecision(c *call, v policy.Verdict) {
	attrs := []attribute.KeyValue{
		telemetry.VigieUpstream.String(r.server),
		telemetry.MCPMethodName.String(c.req.Method),
		telemetry.VigieDecision.String(string(v.Decision)),
	}
	if c.req.Tool != "" {
		attrs = append(attrs, telemetry.GenAIToolName.String(r.opts.Engine.DisplayName(c.req.Tool)))
	}
	if p := v.Primary(); p != nil {
		attrs = append(attrs, telemetry.VigieRule.String(p.Rule))
	}
	r.inst.Decisions.Add(context.Background(), 1, metric.WithAttributes(attrs...))
}

func (r *Relay) logVerdict(what, tool string, v policy.Verdict) {
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
	r.log.Log(context.Background(), level, msg, "tool", r.opts.Engine.DisplayName(tool), "rule", p.Rule, "reason", p.Reason)
}

func (r *Relay) logFinding(f policy.Finding, d policy.Decision) {
	level, msg := slog.LevelInfo, "tool flagged in tools/list"
	if f.Action == config.ActionBlock {
		level, msg = slog.LevelWarn, "tool hidden from tools/list"
		if d == policy.Audit {
			msg = "tool would be hidden from tools/list (audit mode)"
		}
	}
	r.log.Log(context.Background(), level, msg, "tool", f.Tool, "rule", f.Rule, "reason", f.Reason)
}

func (r *Relay) capture(raw json.RawMessage) string {
	s := string(r.opts.Scanner.Redact(raw))
	if len(s) > MaxCapturedBytes {
		s = s[:MaxCapturedBytes] + "…[truncated by Vigie]"
	}
	return s
}

func (r *Relay) unparseableFromClient(line []byte, err error) error {
	r.log.Warn("message from client is not valid JSON-RPC", "error", err, "bytes", len(line))
	if r.mode == config.ModeAudit {
		if err := r.toServer.Write(line); err != nil {
			return fmt.Errorf("%w: %w", ErrServerGone, err)
		}
		return nil
	}
	// Enforce mode: what Vigie cannot read, it cannot check. A lenient
	// server might read it differently, so it is not relayed.
	if err := r.toClient.WriteMessage(jsonrpc.NewErrorResponse(jsonrpc.NullID, jsonrpc.CodeParseError,
		"message rejected by Vigie: not valid JSON-RPC")); err != nil {
		return fmt.Errorf("%w: %w", ErrClientGone, err)
	}
	return nil
}

func (r *Relay) unparseableFromServer(line []byte, err error) {
	r.mu.Lock()
	r.garbage++
	n := r.garbage
	r.mu.Unlock()
	// Servers that print logs on standard output would flood the log.
	if n <= 3 || n%100 == 0 {
		r.log.Warn("server wrote something that is not JSON-RPC on standard output, relayed as is",
			"error", err, "bytes", len(line), "occurrences", n)
	}
}

// requestAttributes returns the span attributes of a request, and the subset
// suitable for metrics (low cardinality).
func (r *Relay) requestAttributes(m *jsonrpc.Message, req mcp.Request, version string) (span, metrics []attribute.KeyValue) {
	metrics = []attribute.KeyValue{
		telemetry.MCPMethodName.String(req.Method),
		telemetry.NetworkTransport.String(telemetry.TransportPipe),
		telemetry.VigieUpstream.String(r.server),
	}
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
			metrics = append(metrics, telemetry.GenAIToolName.String(r.opts.Engine.DisplayName(req.Tool)))
		}
	case req.Prompt != "":
		metrics = append(metrics, telemetry.GenAIPromptName.String(r.opts.Engine.DisplayName(req.Prompt)))
	}
	span = append([]attribute.KeyValue(nil), metrics...)
	if id := m.ID.String(); id != "" {
		span = append(span, telemetry.JSONRPCRequestID.String(r.opts.Scanner.RedactText(id)))
	}
	if req.ResourceURI != "" {
		span = append(span, telemetry.MCPResourceURI.String(r.opts.Scanner.RedactText(req.ResourceURI)))
	}
	return span, metrics
}

// spanName follows the conventions: "{mcp.method.name} {target}", the target
// being a tool or prompt name. Resource URIs are left out: too many distinct
// values for a span name.
func (r *Relay) spanName(req mcp.Request) string {
	switch {
	case req.Tool != "":
		return req.Method + " " + r.opts.Engine.DisplayName(req.Tool)
	case req.Prompt != "":
		return req.Method + " " + r.opts.Engine.DisplayName(req.Prompt)
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
func (r *Relay) classify(method string, resp *jsonrpc.Message, client bool) outcome {
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
		return outcome{append(attrs, telemetry.ErrorType.String(code)), codes.Error, r.opts.Scanner.RedactText(e.Message)}
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

// send writes the messages of a frame that survived processing, as a batch
// when the frame was one.
func send(w *jsonrpc.Writer, frame *jsonrpc.Frame, msgs []*jsonrpc.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	if !frame.Batch {
		return w.WriteMessage(msgs[0])
	}
	if len(msgs) == len(frame.Messages) {
		b, err := frame.Bytes()
		if err != nil {
			return err
		}
		return w.Write(b)
	}
	parts := make([]json.RawMessage, 0, len(msgs))
	for _, m := range msgs {
		b, err := m.Bytes()
		if err != nil {
			return err
		}
		parts = append(parts, b)
	}
	b, err := jsonrpc.Marshal(parts)
	if err != nil {
		return err
	}
	return w.Write(b)
}
