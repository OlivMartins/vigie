package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OlivMartins/vigie/internal/jsonrpc"
)

func parse(t *testing.T, s string) *jsonrpc.Message {
	t.Helper()
	f, err := jsonrpc.ParseFrame([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return f.Messages[0]
}

func TestInspect(t *testing.T) {
	tests := []struct {
		in   string
		want Request
	}{
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_invoice","arguments":{"id":"F-1"}}}`,
			Request{Method: "tools/call", Tool: "get_invoice", Arguments: json.RawMessage(`{"id":"F-1"}`)}},
		{`{"jsonrpc":"2.0","id":1,"method":"prompts/get","params":{"name":"summary"}}`,
			Request{Method: "prompts/get", Prompt: "summary"}},
		{`{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"file:///etc/hosts"}}`,
			Request{Method: "resources/read", ResourceURI: "file:///etc/hosts"}},
		{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
			Request{Method: "initialize", RequestedVersion: "2025-11-25"}},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`,
			Request{Method: "tools/list", ProtocolVersion: "2026-07-28"}},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":42}}`,
			Request{Method: "tools/call"}},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"oops"}`,
			Request{Method: "tools/call"}},
	}
	for _, tt := range tests {
		got := Inspect(parse(t, tt.in))
		got.Params = nil
		if got.Method != tt.want.Method || got.Tool != tt.want.Tool || got.Prompt != tt.want.Prompt ||
			got.ResourceURI != tt.want.ResourceURI || got.ProtocolVersion != tt.want.ProtocolVersion ||
			got.RequestedVersion != tt.want.RequestedVersion || string(got.Arguments) != string(tt.want.Arguments) {
			t.Errorf("Inspect(%s)\n got %+v\nwant %+v", tt.in, got, tt.want)
		}
	}
	c := Inspect(parse(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"r-9"}}`))
	if c.CancelledID.String() != "r-9" {
		t.Errorf("cancelled id = %q", c.CancelledID.String())
	}
}

func TestIsStateless(t *testing.T) {
	for v, want := range map[string]bool{
		"": false, "2024-11-05": false, "2025-11-25": false, "2026-07-28": true, "2027-01-01": true,
	} {
		if IsStateless(v) != want {
			t.Errorf("IsStateless(%q) = %v", v, !want)
		}
	}
}

func TestToolListRoundTrip(t *testing.T) {
	result := json.RawMessage(`{"tools":[{"name":"a","inputSchema":{"type":"object"}},{"name":"b"},"junk"],"nextCursor":"c2","ttlMs":60000,"cacheScope":"private","resultType":"complete"}`)
	l, err := ParseToolList(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Tools) != 3 || l.Tools[0].Name != "a" || l.Tools[1].Name != "b" || l.Tools[2].Name != "" {
		t.Fatalf("tools = %+v", l.Tools)
	}
	l.ExpireCache()
	out, err := l.Encode(l.Tools[:1])
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	_ = json.Unmarshal(out, &got)
	if string(got["nextCursor"]) != `"c2"` || string(got["cacheScope"]) != `"private"` || string(got["resultType"]) != `"complete"` {
		t.Errorf("members lost: %s", out)
	}
	if string(got["ttlMs"]) != "0" {
		t.Errorf("ttlMs = %s, want 0 after ExpireCache", got["ttlMs"])
	}
	if string(got["tools"]) != `[{"name":"a","inputSchema":{"type":"object"}}]` {
		t.Errorf("tools = %s", got["tools"])
	}

	if _, err := ParseToolList(json.RawMessage(`{"prompts":[]}`)); err == nil {
		t.Error("a result without tools must be rejected")
	}
	l, _ = ParseToolList(json.RawMessage(`{"tools":[]}`))
	l.ExpireCache()
	out, _ = l.Encode(nil)
	if strings.Contains(string(out), "ttlMs") {
		t.Errorf("ExpireCache must not add ttlMs: %s", out)
	}
}

func TestToolResults(t *testing.T) {
	if !ToolResultIsError(json.RawMessage(`{"content":[],"isError":true}`)) {
		t.Error("isError true")
	}
	for _, r := range []string{`{"content":[]}`, `{"isError":false}`, `{"IsError":true}`, `{"isError":"true"}`, `[]`} {
		if ToolResultIsError(json.RawMessage(r)) {
			t.Errorf("ToolResultIsError(%s) = true", r)
		}
	}
	if ResultType(json.RawMessage(`{"resultType":"input_required"}`)) != "input_required" {
		t.Error("resultType")
	}

	legacy := string(ErrorToolResult("blocked <here>", false))
	if legacy != `{"content":[{"type":"text","text":"blocked <here>"}],"isError":true}` {
		t.Errorf("legacy result = %s", legacy)
	}
	stateless := string(ErrorToolResult("blocked", true))
	if !strings.Contains(stateless, `"resultType":"complete"`) {
		t.Errorf("stateless result = %s", stateless)
	}
}
