package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/OlivMartins/vigie/internal/config"
	"github.com/OlivMartins/vigie/internal/jsonrpc"
	"github.com/OlivMartins/vigie/internal/mcp"
	"github.com/OlivMartins/vigie/internal/proxy"
	"github.com/OlivMartins/vigie/internal/sse"
	"github.com/OlivMartins/vigie/internal/telemetry"
)

// exchange is one HTTP request from a client and what Vigie does with it.
type exchange struct {
	g       *Gateway
	route   *route
	w       http.ResponseWriter
	r       *http.Request
	conv    *proxy.Conversation
	client  string // authenticated client name
	session string // MCP session id (revisions up to 2025-11-25)
	start   time.Time
	status  int
	// longLived marks a stream meant to stay open: GET, subscriptions/listen.
	longLived bool
}

func (g *Gateway) serveMCP(w http.ResponseWriter, r *http.Request) {
	rt, ok := g.routes[r.PathValue("server")]
	if !ok {
		// Not a JSON-RPC error: a client must not mistake this for an MCP
		// endpoint that lacks a method.
		http.NotFound(w, r)
		return
	}
	x := &exchange{g: g, route: rt, w: w, r: r, start: time.Now(), session: r.Header.Get(HeaderSessionID)}
	defer x.logAccess()
	if !g.hostAllowed(r) {
		x.refuse(http.StatusForbidden, "host not allowed")
		return
	}
	if !g.originAllowed(r) {
		x.refuse(http.StatusForbidden, "origin not allowed")
		return
	}
	g.setCORS(w, r)
	if r.Method == http.MethodOptions {
		x.status = http.StatusNoContent
		w.WriteHeader(http.StatusNoContent)
		return
	}
	name, ok := g.authenticate(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="vigie"`)
		x.refuse(http.StatusUnauthorized, "missing or invalid token")
		return
	}
	x.client = name
	x.conv = x.newConversation()
	switch r.Method {
	case http.MethodPost:
		x.post()
	case http.MethodGet:
		x.longLived = true
		x.forwardStream(nil)
	case http.MethodDelete:
		x.forwardStream(nil)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		x.refuse(http.StatusMethodNotAllowed, "method not allowed")
	}
}

var versionFormat = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// newConversation prepares the policy and tracing of the exchange, with what
// the HTTP request says about the transport.
func (x *exchange) newConversation() *proxy.Conversation {
	r := x.r
	network := []attribute.KeyValue{
		telemetry.NetworkTransport.String(telemetry.TransportTCP),
		telemetry.NetworkProtocolName.String("http"),
		telemetry.NetworkProtocolVersion.String(protocolVersion(r)),
	}
	var inbound []attribute.KeyValue
	if host, port, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		inbound = append(inbound, telemetry.ClientAddress.String(host))
		if p, err := strconv.Atoi(port); err == nil {
			inbound = append(inbound, telemetry.ClientPort.Int(p))
		}
	}
	session := ""
	if x.session != "" {
		session = sessionHash(x.session)
		inbound = append(inbound, telemetry.MCPSessionID.String(session))
	}
	if x.client != "" {
		inbound = append(inbound, telemetry.VigieClient.String(x.client))
	}
	conv := proxy.NewConversation(proxy.Options{
		Engine:         x.route.engine,
		Telemetry:      x.g.opts.Telemetry,
		Scanner:        x.g.opts.Scanner,
		Logger:         x.g.log.With("vigie", x.route.name),
		Audit:          x.g.opts.Audit,
		Propagate:      x.g.opts.Config.Telemetry.PropagateContext,
		CaptureContent: x.g.opts.Config.Telemetry.CaptureContent,
		Transport: proxy.Transport{
			Name: "http", Network: network, Inbound: inbound, Outbound: x.route.outbound,
			Session: session, Client: x.client, CarriesContext: true,
		},
		IgnoreServerRequests: true,
	})
	if v := r.Header.Get(HeaderProtocolVersion); versionFormat.MatchString(v) {
		conv.SetVersion(v)
	}
	conv.SetParent(telemetry.ExtractHTTP(context.Background(), r.Header))
	return conv
}

func protocolVersion(r *http.Request) string {
	if r.ProtoMajor >= 2 {
		return strconv.Itoa(r.ProtoMajor)
	}
	return fmt.Sprintf("%d.%d", r.ProtoMajor, r.ProtoMinor)
}

// post handles a POST: one JSON-RPC message (or, up to 2025-03-26, a batch).
func (x *exchange) post() {
	body, err := io.ReadAll(http.MaxBytesReader(x.w, x.r.Body, x.g.maxRequest))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			x.rejectMessage(http.StatusRequestEntityTooLarge, jsonrpc.NullID, jsonrpc.CodeInvalidRequest,
				fmt.Sprintf("message rejected by Vigie: larger than %d bytes", x.g.maxRequest))
			return
		}
		x.status = 0 // the client went away
		return
	}
	if ct := mediaType(x.r.Header.Get("Content-Type")); ct != "application/json" {
		x.rejectMessage(http.StatusUnsupportedMediaType, jsonrpc.NullID, jsonrpc.CodeInvalidRequest,
			"message rejected by Vigie: Content-Type must be application/json")
		return
	}
	frame, err := jsonrpc.ParseFrame(body)
	if err != nil {
		x.conv.Logger().Warn("message from client is not valid JSON-RPC", "error", err, "bytes", len(body))
		if x.conv.Mode() == config.ModeEnforce {
			x.rejectMessage(http.StatusBadRequest, jsonrpc.NullID, jsonrpc.CodeParseError, "message rejected by Vigie: not valid JSON-RPC")
			return
		}
		x.forwardRaw(body)
		return
	}
	if herr := checkHeaders(x.r.Header, frame); herr != nil {
		reason := x.g.opts.Scanner.RedactText(herr.message)
		x.conv.Logger().Warn("HTTP headers disagree with the message", "error", reason)
		if x.conv.Mode() == config.ModeEnforce {
			id := jsonrpc.NullID
			if !frame.Batch {
				id = frame.Messages[0].ID
			}
			x.rejectMessage(http.StatusBadRequest, id, herr.code, reason)
			return
		}
	}
	var forward, replies []*jsonrpc.Message
	for _, m := range frame.Messages {
		fwd, reply := x.conv.FromClient(m)
		if fwd {
			forward = append(forward, m)
		}
		if reply != nil {
			replies = append(replies, reply)
		}
	}
	if len(forward) == 0 {
		if len(replies) == 0 {
			// Notifications Vigie would not relay: the HTTP status says so.
			x.rejectMessage(http.StatusBadRequest, jsonrpc.NullID, jsonrpc.CodeInvalidRequest, "message rejected by Vigie")
			return
		}
		x.writeMessages(http.StatusOK, frame.Batch, replies)
		return
	}
	out, err := proxy.Encode(frame, forward)
	if err != nil {
		x.conv.Fail("_OTHER")
		x.rejectMessage(http.StatusInternalServerError, jsonrpc.NullID, jsonrpc.CodeInternalError, "Vigie could not encode the message")
		return
	}
	if len(forward) == 1 && forward[0].Method == mcp.MethodSubscriptionsListen {
		x.longLived = true
	}
	x.forwardStream(&postBody{data: out, frame: frame, forward: forward, replies: replies})
}

// postBody is what a POST sends to the server, and what Vigie already
// answered in its place.
type postBody struct {
	data    []byte
	frame   *jsonrpc.Frame
	forward []*jsonrpc.Message
	replies []*jsonrpc.Message
}

// forwardStream sends the request to the server and relays the response.
func (x *exchange) forwardStream(pb *postBody) {
	ctx := x.r.Context()
	if x.longLived {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		stop := context.AfterFunc(x.g.streams, cancel)
		defer stop()
	}
	var body io.Reader
	if pb != nil {
		body = bytes.NewReader(pb.data)
	}
	req, err := http.NewRequestWithContext(ctx, x.r.Method, x.route.upstream.URL.String(), body)
	if err != nil {
		x.conv.Fail(telemetry.ErrorUpstreamUnavailable)
		x.rejectMessage(http.StatusInternalServerError, jsonrpc.NullID, jsonrpc.CodeInternalError, "Vigie could not build the request")
		return
	}
	req.Header = x.upstreamHeaders(pb)
	if pb != nil {
		req.ContentLength = int64(len(pb.data))
	}
	resp, err := x.g.client.Do(req)
	if err != nil {
		if x.r.Context().Err() != nil {
			x.conv.Close(telemetry.ErrorClientClosed)
			return
		}
		x.conv.Logger().Error("server unreachable", "url", x.route.upstream.URL.Redacted(), "error", err)
		x.conv.Fail(telemetry.ErrorUpstreamUnavailable)
		x.unreachable(pb)
		return
	}
	defer resp.Body.Close()
	x.relay(resp, pb)
}

// relay passes the server's response to the client, checking every message
// it carries.
func (x *exchange) relay(resp *http.Response, pb *postBody) {
	var replies []*jsonrpc.Message
	batch, requests := false, false
	if pb != nil {
		replies, batch = pb.replies, pb.frame.Batch
		for _, m := range pb.forward {
			requests = requests || m.Kind == jsonrpc.KindRequest
		}
	}
	ct := mediaType(resp.Header.Get("Content-Type"))
	if x.r.Method == http.MethodDelete || (!requests && ct != "application/json" && ct != "text/event-stream") {
		// No request waits for an answer: a session ended, a notification
		// was accepted. Whatever the body, it carries no JSON-RPC message.
		x.conv.Close(telemetry.ErrorUpstreamClosed)
		if len(replies) > 0 {
			x.writeMessages(http.StatusOK, batch, replies)
			return
		}
		x.passThrough(resp)
		return
	}
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// Followed by the client, a redirect would bypass Vigie.
		x.conv.Logger().Error("server answered with a redirect, not followed", "status", resp.StatusCode)
		x.conv.Fail(strconv.Itoa(resp.StatusCode), telemetry.HTTPResponseStatusCode.Int(resp.StatusCode))
		if x.conv.Mode() == config.ModeEnforce {
			x.unreachable(pb)
			return
		}
		x.passThrough(resp)
	case resp.StatusCode >= 400:
		x.conv.Fail(strconv.Itoa(resp.StatusCode), telemetry.HTTPResponseStatusCode.Int(resp.StatusCode))
		x.passError(resp)
	case ct == "text/event-stream":
		x.relayEvents(resp, replies)
	case ct == "application/json":
		x.relayJSON(resp, replies, batch)
	case resp.StatusCode == http.StatusAccepted || resp.ContentLength == 0 || ct == "":
		// Accepted notifications or responses: nothing to check.
		x.conv.Close(telemetry.ErrorUpstreamClosed)
		if len(replies) > 0 {
			x.writeMessages(http.StatusOK, batch, replies)
			return
		}
		x.passThrough(resp)
	default:
		x.conv.Logger().Warn("server answered with an unexpected content type", "content_type", ct)
		if x.conv.Mode() == config.ModeEnforce {
			x.conv.Fail(telemetry.ErrorUpstreamClosed)
			x.unreachable(pb)
			return
		}
		x.conv.Close(telemetry.ErrorUpstreamClosed)
		x.passThrough(resp)
	}
}

// relayJSON checks a JSON response. The transport defines it as one JSON-RPC
// message (or, up to 2025-03-26, a batch); some servers stream several
// messages in it, one after the other, as the conformance reference server
// does for progress notifications and subscriptions/listen. Every message is
// checked either way.
func (x *exchange) relayJSON(resp *http.Response, replies []*jsonrpc.Message, batch bool) {
	if resp.ContentLength < 0 {
		x.relayJSONValues(resp, resp.Body, replies)
		return
	}
	data, err := readLimited(resp.Body, x.g.maxMessage)
	if err != nil {
		x.conv.Logger().Error("response from server not read", "error", err)
		x.conv.Fail(telemetry.ErrorUpstreamClosed)
		x.rejectMessage(http.StatusBadGateway, jsonrpc.NullID, jsonrpc.CodeInternalError, "Vigie could not read the response of the server")
		return
	}
	frame, err := jsonrpc.ParseFrame(data)
	if err != nil {
		x.relayJSONValues(resp, bytes.NewReader(data), replies)
		return
	}
	keep := make([]*jsonrpc.Message, 0, len(frame.Messages)+len(replies))
	for _, m := range frame.Messages {
		if x.conv.FromServer(m) {
			keep = append(keep, m)
		}
	}
	x.conv.Close(telemetry.ErrorUpstreamClosed)
	x.copyHeaders(resp.Header)
	if len(replies) == 0 && len(keep) == len(frame.Messages) && !modified(keep) {
		x.status = resp.StatusCode
		x.w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		x.w.WriteHeader(resp.StatusCode)
		_, _ = x.w.Write(data)
		return
	}
	keep = append(keep, replies...)
	if len(keep) == 0 {
		x.rejectMessage(http.StatusBadGateway, jsonrpc.NullID, jsonrpc.CodeInternalError, "Vigie rejected the response of the server")
		return
	}
	x.writeMessages(resp.StatusCode, batch || frame.Batch, keep)
}

// relayJSONValues relays a JSON body made of successive JSON values, each
// checked and written on its own line as soon as it is read.
func (x *exchange) relayJSONValues(resp *http.Response, body io.Reader, replies []*jsonrpc.Message) {
	enforce := x.conv.Mode() == config.ModeEnforce
	limit := &valueLimiter{r: body, max: int64(x.g.maxMessage)}
	dec := json.NewDecoder(limit)
	limit.dec = dec
	x.copyHeaders(resp.Header)
	rc := http.NewResponseController(x.w)
	started, clientGone := false, false
	write := func(b []byte) {
		if clientGone {
			return
		}
		if !started {
			started = true
			x.status = resp.StatusCode
			x.w.WriteHeader(resp.StatusCode)
		}
		if _, err := x.w.Write(append(b, '\n')); err != nil {
			clientGone = true
			return
		}
		if err := rc.Flush(); err != nil {
			clientGone = true
		}
	}
	for _, m := range replies {
		if b, err := m.Bytes(); err == nil {
			write(b)
		}
	}
	for !clientGone {
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if x.r.Context().Err() == nil && x.g.streams.Err() == nil {
				// What follows cannot be read: the relay stops here.
				x.conv.Logger().Warn("server answered with something that is not JSON", "error", err)
			}
			break
		}
		frame, err := jsonrpc.ParseFrame(raw)
		if err != nil {
			x.conv.Logger().Warn("server answered with something that is not JSON-RPC", "error", err, "bytes", len(raw))
			if !enforce {
				write(raw)
			}
			continue
		}
		keep := make([]*jsonrpc.Message, 0, len(frame.Messages))
		for _, m := range frame.Messages {
			if x.conv.FromServer(m) {
				keep = append(keep, m)
			}
		}
		if len(keep) == 0 {
			continue
		}
		if out, err := proxy.Encode(frame, keep); err == nil {
			write(out)
		}
	}
	reason := telemetry.ErrorUpstreamClosed
	switch {
	case clientGone || x.r.Context().Err() != nil:
		reason = telemetry.ErrorClientClosed
	case x.g.streams.Err() != nil && x.longLived:
		reason = telemetry.ErrorShutdown
	}
	if !started {
		x.conv.Fail(reason)
		x.rejectMessage(http.StatusBadGateway, jsonrpc.NullID, jsonrpc.CodeInternalError, "Vigie rejected a response of the server that is not valid JSON-RPC")
		return
	}
	x.conv.Close(reason)
}

// valueLimiter fails a JSON decoder when a single value grows beyond max
// bytes.
type valueLimiter struct {
	r    io.Reader
	dec  *json.Decoder
	read int64
	max  int64
}

func (l *valueLimiter) Read(p []byte) (int, error) {
	if l.dec != nil && l.read-l.dec.InputOffset() > l.max {
		return 0, errTooLarge
	}
	n, err := l.r.Read(p)
	l.read += int64(n)
	return n, err
}

// relayEvents relays an SSE stream event by event, checking each message.
func (x *exchange) relayEvents(resp *http.Response, replies []*jsonrpc.Message) {
	x.copyHeaders(resp.Header)
	h := x.w.Header()
	h.Del("Content-Length")
	h.Set("X-Accel-Buffering", "no")
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "no-cache")
	}
	x.status = resp.StatusCode
	x.w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(x.w)
	clientGone := false
	emit := func(e *sse.Event) {
		if clientGone {
			return
		}
		if _, err := x.w.Write(sse.Encode(e)); err != nil {
			clientGone = true
			return
		}
		if err := rc.Flush(); err != nil {
			clientGone = true
		}
	}
	for _, m := range replies {
		if b, err := m.Bytes(); err == nil {
			emit(&sse.Event{Data: string(b), HasData: true})
		}
	}
	_ = rc.Flush()
	events := sse.NewReader(resp.Body, x.g.maxMessage)
	var readErr error
	for !clientGone {
		e, err := events.Next()
		if err != nil {
			readErr = err
			break
		}
		if out := x.checkEvent(e); out != nil {
			emit(out)
		}
	}
	reason := telemetry.ErrorUpstreamClosed
	switch {
	case clientGone || x.r.Context().Err() != nil:
		reason = telemetry.ErrorClientClosed
	case errors.Is(readErr, sse.ErrEventTooLarge):
		x.conv.Logger().Error("event from server dropped: too large, stream closed", "limit_bytes", x.g.maxMessage)
	case readErr != nil && !errors.Is(readErr, io.EOF):
		if x.g.streams.Err() != nil {
			reason = telemetry.ErrorShutdown
		}
	}
	if x.session != "" && reason != telemetry.ErrorShutdown {
		// Revisions up to 2025-11-25: the client may resume this stream with
		// a GET; the responses replayed there must still be checked.
		if pending := x.conv.Unanswered(); len(pending) > 0 {
			x.g.resumed.put(x.route.name, x.session, pending)
		}
	}
	x.conv.Close(reason)
}

// checkEvent returns the event to send the client in place of e, nil to send
// nothing.
func (x *exchange) checkEvent(e *sse.Event) *sse.Event {
	enforce := x.conv.Mode() == config.ModeEnforce
	cursor := func() *sse.Event {
		// Keep the event id: the client resumes from it.
		if !e.HasID && e.Retry == "" {
			return nil
		}
		return &sse.Event{ID: e.ID, HasID: e.HasID, Retry: e.Retry}
	}
	if !e.HasData || e.Data == "" {
		return e // comments, priming event, retry
	}
	if !e.IsMessage() {
		x.conv.Logger().Warn("event of another type than message from server", "type", e.Type)
		if enforce {
			return cursor()
		}
		return e
	}
	frame, err := jsonrpc.ParseFrame([]byte(e.Data))
	if err != nil {
		x.conv.Logger().Warn("event from server is not valid JSON-RPC", "error", err, "bytes", len(e.Data))
		if enforce {
			return cursor()
		}
		return e
	}
	keep := make([]*jsonrpc.Message, 0, len(frame.Messages))
	for _, m := range frame.Messages {
		if m.Kind == jsonrpc.KindResponse && x.session != "" && !x.conv.Expects(m.ID) {
			if p, ok := x.g.resumed.take(x.route.name, x.session, m.ID); ok {
				x.conv.Adopt(p)
			}
		}
		if x.conv.FromServer(m) {
			keep = append(keep, m)
		}
	}
	if len(keep) == 0 {
		return cursor()
	}
	data, err := proxy.Encode(frame, keep)
	if err != nil {
		return cursor()
	}
	out := *e
	if len(keep) != len(frame.Messages) || modified(keep) {
		out.Data = string(data)
	}
	return &out
}

// forwardRaw relays a body Vigie could not read, in audit mode, and its
// response unchecked.
func (x *exchange) forwardRaw(body []byte) {
	req, err := http.NewRequestWithContext(x.r.Context(), x.r.Method, x.route.upstream.URL.String(), bytes.NewReader(body))
	if err != nil {
		x.rejectMessage(http.StatusInternalServerError, jsonrpc.NullID, jsonrpc.CodeInternalError, "Vigie could not build the request")
		return
	}
	req.Header = x.upstreamHeaders(nil)
	req.ContentLength = int64(len(body))
	resp, err := x.g.client.Do(req)
	if err != nil {
		x.unreachable(nil)
		return
	}
	defer resp.Body.Close()
	x.passThrough(resp)
}

// passThrough copies a response the gateway does not inspect: HTTP errors,
// 202 Accepted, DELETE answers, and audit-mode leftovers.
func (x *exchange) passThrough(resp *http.Response) {
	x.copyHeaders(resp.Header)
	x.status = resp.StatusCode
	x.w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(x.w, io.LimitReader(resp.Body, int64(x.g.maxMessage)))
}

// passError relays an HTTP error: authentication challenges, expired
// sessions, unsupported versions... Its body explains the error; in enforce
// mode, a body that holds a successful JSON-RPC result is not relayed: a
// lenient client could take it for the answer, unchecked.
func (x *exchange) passError(resp *http.Response) {
	data, err := readLimited(resp.Body, x.g.maxMessage)
	if err != nil {
		data = nil
	}
	if x.conv.Mode() == config.ModeEnforce {
		if frame, err := jsonrpc.ParseFrame(data); err == nil {
			for _, m := range frame.Messages {
				if _, hasResult := m.Members["result"]; hasResult || m.Kind != jsonrpc.KindResponse {
					x.conv.Logger().Warn("server answered an HTTP error with a JSON-RPC result: body not relayed", "status", resp.StatusCode)
					x.copyHeaders(resp.Header)
					x.rejectMessage(resp.StatusCode, jsonrpc.NullID, jsonrpc.CodeInternalError,
						fmt.Sprintf("server %q answered with HTTP status %d", x.route.name, resp.StatusCode))
					return
				}
			}
		}
	}
	x.copyHeaders(resp.Header)
	x.status = resp.StatusCode
	x.w.WriteHeader(resp.StatusCode)
	_, _ = x.w.Write(data)
}

// unreachable answers the requests of a POST the server could not take.
func (x *exchange) unreachable(pb *postBody) {
	if pb == nil {
		x.refuse(http.StatusBadGateway, "server unreachable")
		return
	}
	msgs := append([]*jsonrpc.Message(nil), pb.replies...)
	for _, m := range pb.forward {
		if m.Kind == jsonrpc.KindRequest {
			msgs = append(msgs, jsonrpc.NewErrorResponse(m.ID, jsonrpc.CodeInternalError,
				fmt.Sprintf("Vigie could not reach server %q", x.route.name)))
		}
	}
	if len(msgs) == 0 {
		x.refuse(http.StatusBadGateway, "server unreachable")
		return
	}
	x.writeMessages(http.StatusBadGateway, pb.frame.Batch, msgs)
}

// upstreamHeaders returns the headers of the request to the server: the
// client's, without hop-by-hop headers, with the configured ones on top.
func (x *exchange) upstreamHeaders(pb *postBody) http.Header {
	r := x.r
	h := r.Header.Clone()
	removeHopByHop(h)
	h.Del("Accept-Encoding") // the transport negotiates compression itself
	h.Del("Content-Length")
	if len(x.g.tokens) > 0 {
		h.Del("Authorization") // the gateway's own credential
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if prior := h.Get("X-Forwarded-For"); prior != "" {
			host = prior + ", " + host
		}
		h.Set("X-Forwarded-For", host)
	}
	proto := "http"
	if r.TLS != nil {
		proto = "https"
	}
	h.Set("X-Forwarded-Proto", proto)
	h.Set("X-Forwarded-Host", r.Host)
	for k, v := range x.route.upstream.Headers {
		h[k] = append([]string(nil), v...)
	}
	if pb != nil && x.g.propagate && len(pb.forward) == 1 {
		if ctx, ok := x.conv.OutboundContext(pb.forward[0].ID); ok {
			telemetry.InjectHTTP(ctx, h)
		}
	}
	return h
}

// hopByHop headers concern one connection, not the end-to-end request.
var hopByHop = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func removeHopByHop(h http.Header) {
	for _, f := range h.Values("Connection") {
		for _, name := range strings.Split(f, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopByHop {
		h.Del(name)
	}
}

func (x *exchange) copyHeaders(from http.Header) {
	h := x.w.Header()
	for k, v := range from {
		h[k] = append([]string(nil), v...)
	}
	removeHopByHop(h)
	h.Del("Content-Length")
}

// writeMessages answers with JSON-RPC messages Vigie wrote or rewrote.
func (x *exchange) writeMessages(status int, batch bool, msgs []*jsonrpc.Message) {
	var data []byte
	var err error
	if batch {
		data, err = proxy.Encode(&jsonrpc.Frame{Batch: true}, msgs)
	} else {
		data, err = msgs[0].Bytes()
	}
	if err != nil {
		status, data = http.StatusInternalServerError, []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"Vigie could not encode the response"}}`)
	}
	h := x.w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	x.status = status
	x.w.WriteHeader(status)
	_, _ = x.w.Write(data)
}

// rejectMessage refuses a POST with a JSON-RPC error.
func (x *exchange) rejectMessage(status int, id jsonrpc.ID, code int, message string) {
	x.writeMessages(status, false, []*jsonrpc.Message{jsonrpc.NewErrorResponse(id, code, message)})
}

// refuse answers a request refused before its messages were read. The body
// is a JSON-RPC error without id, as the transport allows.
func (x *exchange) refuse(status int, reason string) {
	x.writeMessages(status, false, []*jsonrpc.Message{
		jsonrpc.NewErrorResponse(jsonrpc.NullID, jsonrpc.CodeInvalidRequest, "request refused by Vigie: "+reason),
	})
}

func (x *exchange) logAccess() {
	if x.status == 0 {
		return
	}
	x.g.log.Debug("http request", "server", x.route.name, "method", x.r.Method, "status", x.status,
		"duration_ms", time.Since(x.start).Milliseconds(), "client", x.client)
}

// setCORS lets web pages from an allowed origin call the gateway.
func (g *Gateway) setCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Add("Vary", "Origin")
	h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	// Mcp-Param-{Name} headers vary per tool: the preflight's own list is
	// echoed, the origin being allowed.
	allowHeaders := r.Header.Get("Access-Control-Request-Headers")
	if allowHeaders == "" {
		allowHeaders = "Authorization, Content-Type, Accept, Last-Event-ID, Mcp-Session-Id, Mcp-Protocol-Version, Mcp-Method, Mcp-Name, traceparent, tracestate, baggage"
	}
	h.Set("Access-Control-Allow-Headers", allowHeaders)
	h.Set("Access-Control-Expose-Headers", "Mcp-Session-Id, WWW-Authenticate")
	h.Set("Access-Control-Max-Age", "600")
}

func modified(msgs []*jsonrpc.Message) bool {
	for _, m := range msgs {
		if m.Modified() {
			return true
		}
	}
	return false
}

func mediaType(v string) string {
	if v == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(v)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(v))
	}
	return mt
}

var errTooLarge = errors.New("response larger than the message size limit")

func readLimited(r io.Reader, limit int) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errTooLarge
	}
	return data, nil
}
