// Package jsonrpc reads, inspects and rewrites JSON-RPC 2.0 messages without
// losing anything it does not understand.
//
// A proxy must forward what it does not touch byte for byte, and must read the
// members it does inspect exactly as the receiving peer will. Messages are
// therefore decoded into maps of raw members: keys match exactly (Go struct
// decoding would also accept "NAME" for "name", a parser differential an
// attacker could use to smuggle a tool name past a policy), and unknown members
// survive a rewrite untouched.
package jsonrpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// Version is the only JSON-RPC version MCP uses.
const Version = "2.0"

// Standard JSON-RPC error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Kind classifies a message.
type Kind int

// Message kinds.
const (
	KindRequest Kind = iota + 1
	KindNotification
	KindResponse
)

func (k Kind) String() string {
	switch k {
	case KindRequest:
		return "request"
	case KindNotification:
		return "notification"
	case KindResponse:
		return "response"
	default:
		return "invalid"
	}
}

// ErrNotJSONRPC reports a frame that is valid JSON but not a JSON-RPC message.
var ErrNotJSONRPC = errors.New("not a JSON-RPC message")

// ID is a request identifier. It keeps its JSON form, so 1 and "1" stay
// distinct, as the specification requires.
type ID struct {
	raw json.RawMessage
}

// NullID is the identifier used to answer a message whose id is unknown.
var NullID = ID{raw: json.RawMessage("null")}

// IsZero reports whether the message carried no id at all.
func (id ID) IsZero() bool { return len(id.raw) == 0 }

// Key returns a map key that distinguishes 1 from "1".
func (id ID) Key() string { return string(id.raw) }

// Raw returns the id as it appeared on the wire.
func (id ID) Raw() json.RawMessage { return id.raw }

// String returns the id as recorded in telemetry: the unquoted string, or the
// number as written. It is empty for a missing or null id.
func (id ID) String() string {
	if id.IsZero() || string(id.raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(id.raw, &s) == nil {
		return s
	}
	return string(id.raw)
}

// IDFromRaw builds an ID from its JSON form.
func IDFromRaw(raw json.RawMessage) ID { return ID{raw: compact(raw)} }

// Message is one JSON-RPC message. Members keeps every top-level member
// verbatim; Raw holds the original bytes until the message is modified.
type Message struct {
	Kind    Kind
	Method  string
	ID      ID
	Version string
	Members map[string]json.RawMessage
	raw     []byte
}

// ErrorObject is the error member of a response.
type ErrorObject struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func parseMessage(data []byte) (*Message, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return nil, err
	}
	if members == nil {
		return nil, ErrNotJSONRPC
	}
	m := &Message{Members: members, raw: data}
	if v, ok := members["jsonrpc"]; ok {
		if err := json.Unmarshal(v, &m.Version); err != nil {
			return nil, fmt.Errorf("%w: jsonrpc member is not a string", ErrNotJSONRPC)
		}
	}
	if v, ok := members["id"]; ok {
		m.ID = IDFromRaw(v)
	}
	if v, ok := members["method"]; ok {
		if err := json.Unmarshal(v, &m.Method); err != nil || m.Method == "" {
			return nil, fmt.Errorf("%w: method member is not a non-empty string", ErrNotJSONRPC)
		}
		if m.ID.IsZero() {
			m.Kind = KindNotification
		} else {
			m.Kind = KindRequest
		}
		return m, nil
	}
	_, hasResult := members["result"]
	_, hasError := members["error"]
	if !m.ID.IsZero() && (hasResult || hasError) {
		m.Kind = KindResponse
		return m, nil
	}
	return nil, ErrNotJSONRPC
}

// Bytes returns the encoded message: the original bytes when it was not
// modified, a fresh encoding otherwise.
func (m *Message) Bytes() ([]byte, error) {
	if m.raw != nil {
		return m.raw, nil
	}
	return Marshal(m.Members)
}

// Modified reports whether the message was changed since it was read.
func (m *Message) Modified() bool { return m.raw == nil }

// Params decodes the params member as an object. It returns nil, nil when the
// member is absent.
func (m *Message) Params() (map[string]json.RawMessage, error) {
	return m.object("params")
}

// SetParams replaces the params member.
func (m *Message) SetParams(params map[string]json.RawMessage) error {
	return m.set("params", params)
}

// Result returns the raw result member of a response.
func (m *Message) Result() json.RawMessage { return m.Members["result"] }

// SetResult replaces the result member of a response.
func (m *Message) SetResult(result json.RawMessage) {
	m.Members["result"] = result
	m.raw = nil
}

// Error decodes the error member of a response. It returns nil when the
// response is a success or the member is malformed.
func (m *Message) Error() *ErrorObject {
	v, ok := m.Members["error"]
	if !ok {
		return nil
	}
	var e ErrorObject
	if json.Unmarshal(v, &e) != nil {
		return nil
	}
	return &e
}

func (m *Message) object(name string) (map[string]json.RawMessage, error) {
	v, ok := m.Members[name]
	if !ok || string(v) == "null" {
		return nil, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(v, &obj); err != nil {
		return nil, fmt.Errorf("%s is not an object: %w", name, err)
	}
	return obj, nil
}

func (m *Message) set(name string, value any) error {
	raw, err := Marshal(value)
	if err != nil {
		return err
	}
	m.Members[name] = raw
	m.raw = nil
	return nil
}

// NewResponse builds a success response.
func NewResponse(id ID, result json.RawMessage) *Message {
	return &Message{
		Kind:    KindResponse,
		ID:      id,
		Version: Version,
		Members: map[string]json.RawMessage{
			"jsonrpc": json.RawMessage(`"2.0"`),
			"id":      id.raw,
			"result":  result,
		},
	}
}

// NewErrorResponse builds an error response.
func NewErrorResponse(id ID, code int, message string) *Message {
	if id.IsZero() {
		id = NullID
	}
	e, _ := Marshal(ErrorObject{Code: code, Message: message})
	return &Message{
		Kind:    KindResponse,
		ID:      id,
		Version: Version,
		Members: map[string]json.RawMessage{
			"jsonrpc": json.RawMessage(`"2.0"`),
			"id":      id.raw,
			"error":   e,
		},
	}
}

// Frame is what one line of the transport carries: a single message, or a
// batch (an array of messages, allowed by MCP revisions up to 2025-03-26).
type Frame struct {
	Batch    bool
	Messages []*Message
	raw      []byte
}

// ParseFrame decodes one transport frame.
func ParseFrame(data []byte) (*Frame, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, ErrNotJSONRPC
	}
	if trimmed[0] == '[' {
		var elems []json.RawMessage
		if err := json.Unmarshal(trimmed, &elems); err != nil {
			return nil, err
		}
		if len(elems) == 0 {
			return nil, fmt.Errorf("%w: empty batch", ErrNotJSONRPC)
		}
		f := &Frame{Batch: true, raw: data}
		for _, e := range elems {
			m, err := parseMessage(e)
			if err != nil {
				return nil, err
			}
			f.Messages = append(f.Messages, m)
		}
		return f, nil
	}
	m, err := parseMessage(trimmed)
	if err != nil {
		return nil, err
	}
	return &Frame{Messages: []*Message{m}, raw: data}, nil
}

// Bytes encodes the frame, reusing the original bytes when nothing changed.
func (f *Frame) Bytes() ([]byte, error) {
	if !f.Batch {
		return f.Messages[0].Bytes()
	}
	changed := false
	for _, m := range f.Messages {
		if m.Modified() {
			changed = true
			break
		}
	}
	if !changed && f.raw != nil {
		return f.raw, nil
	}
	parts := make([]json.RawMessage, 0, len(f.Messages))
	for _, m := range f.Messages {
		b, err := m.Bytes()
		if err != nil {
			return nil, err
		}
		parts = append(parts, b)
	}
	return Marshal(parts)
}

// Marshal encodes v as compact JSON without escaping <, > and &, so text
// written by a peer reads the same after a rewrite.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Quote encodes a string as a JSON string literal.
func Quote(s string) json.RawMessage {
	b, _ := Marshal(s)
	return b
}

// CodeString renders an error code the way telemetry records it.
func CodeString(code int) string { return strconv.Itoa(code) }

func compact(raw json.RawMessage) json.RawMessage {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return raw
	}
	return buf.Bytes()
}
