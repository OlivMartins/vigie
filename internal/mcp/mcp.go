// Package mcp holds what Vigie knows about the Model Context Protocol: method
// names, where requests carry their target and protocol version, and how tool
// lists and tool results are shaped.
//
// Vigie works at the JSON-RPC level on purpose: it relays every revision of the
// protocol, from the 2024-11-05 handshake to the stateless 2026-07-28 revision,
// and only looks inside the few members it needs.
package mcp

import (
	"encoding/json"

	"github.com/OlivMartins/vigie/internal/jsonrpc"
)

// Methods Vigie inspects.
const (
	MethodInitialize           = "initialize"
	MethodDiscover             = "server/discover"
	MethodToolsList            = "tools/list"
	MethodToolsCall            = "tools/call"
	MethodPromptsGet           = "prompts/get"
	MethodResourcesRead        = "resources/read"
	MethodResourcesSubscribe   = "resources/subscribe"
	MethodResourcesUnsubscribe = "resources/unsubscribe"
	NotificationCancelled      = "notifications/cancelled"
	NotificationToolsChanged   = "notifications/tools/list_changed"
)

// Keys of the request _meta object.
const (
	// MetaProtocolVersion carries the protocol version on every request since
	// the stateless 2026-07-28 revision.
	MetaProtocolVersion = "io.modelcontextprotocol/protocolVersion"
	// MetaServerInfo identifies the server on results since 2026-07-28.
	MetaServerInfo = "io.modelcontextprotocol/serverInfo"
)

// StatelessRevision is the first revision without the initialize handshake,
// where results must carry resultType.
const StatelessRevision = "2026-07-28"

// Request describes what a request targets, as far as tracing and policy are
// concerned.
type Request struct {
	Method string
	// Tool is the tool name of a tools/call request.
	Tool string
	// Prompt is the prompt name of a prompts/get request.
	Prompt string
	// ResourceURI is the resource of a resources/read, subscribe or
	// unsubscribe request.
	ResourceURI string
	// ProtocolVersion is the version declared in params._meta, if any.
	ProtocolVersion string
	// RequestedVersion is the version an initialize request asks for.
	RequestedVersion string
	// Arguments holds the arguments of a tools/call request.
	Arguments json.RawMessage
	// CancelledID is the request a notifications/cancelled targets.
	CancelledID jsonrpc.ID
	// Params is the decoded params object, nil when absent or malformed.
	Params map[string]json.RawMessage
}

// Inspect extracts the target of a request or notification. Members of the
// wrong type are ignored: the peer will reject the message, Vigie only
// records what it can read.
func Inspect(m *jsonrpc.Message) Request {
	r := Request{Method: m.Method}
	params, err := m.Params()
	if err != nil || params == nil {
		return r
	}
	r.Params = params
	switch m.Method {
	case MethodToolsCall:
		r.Tool = stringMember(params, "name")
		r.Arguments = params["arguments"]
	case MethodPromptsGet:
		r.Prompt = stringMember(params, "name")
	case MethodResourcesRead, MethodResourcesSubscribe, MethodResourcesUnsubscribe:
		r.ResourceURI = stringMember(params, "uri")
	case MethodInitialize:
		r.RequestedVersion = stringMember(params, "protocolVersion")
	case NotificationCancelled:
		if id, ok := params["requestId"]; ok {
			r.CancelledID = jsonrpc.IDFromRaw(id)
		}
	}
	if meta := objectMember(params, "_meta"); meta != nil {
		r.ProtocolVersion = stringMember(meta, MetaProtocolVersion)
	}
	return r
}

// NegotiatedVersion reads the protocolVersion of an initialize result.
func NegotiatedVersion(result json.RawMessage) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(result, &obj) != nil {
		return ""
	}
	return stringMember(obj, "protocolVersion")
}

// IsStateless reports whether a protocol version belongs to the stateless era,
// where every result carries resultType. Versions are dates, so they compare
// as strings.
func IsStateless(version string) bool {
	return version >= StatelessRevision
}

func stringMember(obj map[string]json.RawMessage, key string) string {
	v, ok := obj[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return ""
	}
	return s
}

func objectMember(obj map[string]json.RawMessage, key string) map[string]json.RawMessage {
	v, ok := obj[key]
	if !ok {
		return nil
	}
	var o map[string]json.RawMessage
	if json.Unmarshal(v, &o) != nil {
		return nil
	}
	return o
}
