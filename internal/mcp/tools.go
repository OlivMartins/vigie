package mcp

import (
	"encoding/json"
	"errors"

	"github.com/OlivMartins/vigie/internal/jsonrpc"
)

// Tool is one entry of a tools/list result, kept verbatim.
type Tool struct {
	Name       string
	Definition json.RawMessage
}

// ToolList is a decoded tools/list result. Members other than tools
// (nextCursor, ttlMs, cacheScope, _meta, resultType...) are kept as they came.
type ToolList struct {
	Tools   []Tool
	members map[string]json.RawMessage
}

// ErrNotToolList reports a result that does not have a tools array.
var ErrNotToolList = errors.New("result has no tools array")

// ParseToolList decodes a tools/list result.
func ParseToolList(result json.RawMessage) (*ToolList, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(result, &members); err != nil {
		return nil, err
	}
	raw, ok := members["tools"]
	if !ok {
		return nil, ErrNotToolList
	}
	var defs []json.RawMessage
	if err := json.Unmarshal(raw, &defs); err != nil {
		return nil, ErrNotToolList
	}
	list := &ToolList{members: members, Tools: make([]Tool, 0, len(defs))}
	for _, d := range defs {
		var obj map[string]json.RawMessage
		// An entry that is not an object, or whose name a peer could read
		// differently ("name" twice, "Name" beside "name"), gets no name:
		// the policy hides nameless tools.
		if json.Unmarshal(d, &obj) != nil || jsonrpc.CheckKeys(d, "name") != nil {
			list.Tools = append(list.Tools, Tool{Definition: d})
			continue
		}
		list.Tools = append(list.Tools, Tool{Name: stringMember(obj, "name"), Definition: d})
	}
	return list, nil
}

// ExpireCache sets ttlMs to 0 when the result carries one (2026-07-28): a
// list Vigie had to filter must not stay cached by the client, or tools
// approved in the meantime would remain invisible until the cache expires.
func (l *ToolList) ExpireCache() {
	if _, ok := l.members["ttlMs"]; ok {
		l.members["ttlMs"] = json.RawMessage("0")
	}
}

// Encode rebuilds the result with the given tools, every other member intact.
func (l *ToolList) Encode(tools []Tool) (json.RawMessage, error) {
	defs := make([]json.RawMessage, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, t.Definition)
	}
	raw, err := jsonrpc.Marshal(defs)
	if err != nil {
		return nil, err
	}
	members := make(map[string]json.RawMessage, len(l.members))
	for k, v := range l.members {
		members[k] = v
	}
	members["tools"] = raw
	return jsonrpc.Marshal(members)
}

// ToolResultIsError reports whether a tools/call result has isError set.
func ToolResultIsError(result json.RawMessage) bool {
	// Exact-key read: Vigie must see the member the client sees.
	var members map[string]json.RawMessage
	if json.Unmarshal(result, &members) != nil {
		return false
	}
	return string(members["isError"]) == "true"
}

// ResultType reads the resultType member introduced by the stateless
// revision. Results from earlier revisions have none and count as complete.
func ResultType(result json.RawMessage) string {
	var members map[string]json.RawMessage
	if json.Unmarshal(result, &members) != nil {
		return ""
	}
	return stringMember(members, "resultType")
}

// ErrorToolResult builds the result of a tool call that failed: text content
// with isError set, so the model reads why and can adapt. Since the stateless
// revision, results also carry resultType.
func ErrorToolResult(text string, stateless bool) json.RawMessage {
	type content struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	result := struct {
		Content    []content `json:"content"`
		IsError    bool      `json:"isError"`
		ResultType string    `json:"resultType,omitempty"`
	}{
		Content: []content{{Type: "text", Text: text}},
		IsError: true,
	}
	if stateless {
		result.ResultType = "complete"
	}
	raw, _ := jsonrpc.Marshal(result)
	return raw
}
