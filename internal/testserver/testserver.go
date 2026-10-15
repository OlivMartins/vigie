// Package testserver is a small MCP server over stdio, for tests and the
// demo. It speaks both protocol eras: the initialize handshake (2024-11-05 to
// 2025-11-25) and the stateless revision (2026-07-28), where every request
// carries its version in _meta.
package testserver

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// Versions this server accepts.
var (
	HandshakeVersions = []string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25"}
	StatelessVersion  = "2026-07-28"
)

// Tool is a tool the server exposes.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	// Call returns the text result of a call, and whether it is an error.
	Call func(args map[string]any) (string, bool)
}

// Server answers MCP requests on a reader/writer pair.
type Server struct {
	Name    string
	Version string
	Tools   []Tool
	// NoHistory keeps no record of the messages received: load tests send
	// millions.
	NoHistory bool

	mu       sync.Mutex
	received []map[string]json.RawMessage
}

// Received returns every message the server read, decoded one level deep.
func (s *Server) Received() []map[string]json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]json.RawMessage(nil), s.received...)
}

// Serve reads requests until r ends.
func (s *Server) Serve(r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	enc := json.NewEncoder(w)
	for sc.Scan() {
		line := sc.Bytes()
		var batch []map[string]json.RawMessage
		if json.Unmarshal(line, &batch) == nil {
			var responses []map[string]any
			for _, msg := range batch {
				if resp := s.process(msg); resp != nil {
					responses = append(responses, resp)
				}
			}
			if len(responses) > 0 {
				if err := enc.Encode(responses); err != nil {
					return err
				}
			}
			continue
		}
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if resp := s.process(msg); resp != nil {
			if err := enc.Encode(resp); err != nil {
				return err
			}
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		return err
	}
	return nil
}

// process records a message and returns the response to a request, nil for a
// notification.
func (s *Server) process(msg map[string]json.RawMessage) map[string]any {
	if !s.NoHistory {
		s.mu.Lock()
		s.received = append(s.received, msg)
		s.mu.Unlock()
	}
	id, isRequest := msg["id"]
	if !isRequest {
		return nil
	}
	var method string
	_ = json.Unmarshal(msg["method"], &method)
	var params map[string]json.RawMessage
	_ = json.Unmarshal(msg["params"], &params)
	result, rpcErr := s.handle(method, params)
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		resp["error"] = rpcErr
	} else {
		resp["result"] = result
	}
	return resp
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) handle(method string, params map[string]json.RawMessage) (map[string]any, *rpcError) {
	stateless := requestVersion(params) >= StatelessVersion
	complete := func(m map[string]any) map[string]any {
		if stateless {
			m["resultType"] = "complete"
		}
		return m
	}
	switch method {
	case "initialize":
		var requested string
		_ = json.Unmarshal(params["protocolVersion"], &requested)
		version := HandshakeVersions[len(HandshakeVersions)-1]
		for _, v := range HandshakeVersions {
			if v == requested {
				version = v
			}
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
			"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
		}, nil
	case "server/discover":
		return complete(map[string]any{
			"supportedVersions": []string{StatelessVersion, HandshakeVersions[len(HandshakeVersions)-1]},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"ttlMs":             0,
			"cacheScope":        "public",
		}), nil
	case "ping":
		return complete(map[string]any{}), nil
	case "tools/list":
		tools := make([]map[string]any, 0, len(s.Tools))
		for _, t := range s.Tools {
			schema := t.InputSchema
			if schema == nil {
				schema = map[string]any{"type": "object"}
			}
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": schema})
		}
		result := map[string]any{"tools": tools}
		if stateless {
			result["ttlMs"], result["cacheScope"] = 60000, "public"
		}
		return complete(result), nil
	case "tools/call":
		var name string
		_ = json.Unmarshal(params["name"], &name)
		var args map[string]any
		_ = json.Unmarshal(params["arguments"], &args)
		for _, t := range s.Tools {
			if t.Name != name {
				continue
			}
			text, isError := "ok", false
			if t.Call != nil {
				text, isError = t.Call(args)
			}
			return complete(map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
				"isError": isError,
			}), nil
		}
		return nil, &rpcError{Code: -32602, Message: "unknown tool: " + name}
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + method}
	}
}

func requestVersion(params map[string]json.RawMessage) string {
	var meta map[string]json.RawMessage
	if json.Unmarshal(params["_meta"], &meta) != nil {
		return ""
	}
	var v string
	_ = json.Unmarshal(meta["io.modelcontextprotocol/protocolVersion"], &v)
	return v
}
