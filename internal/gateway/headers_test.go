package gateway

import (
	"net/http"
	"testing"

	"github.com/OlivMartins/vigie/internal/jsonrpc"
)

func frame(t *testing.T, s string) *jsonrpc.Frame {
	t.Helper()
	f, err := jsonrpc.ParseFrame([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestDecodeHeaderValue(t *testing.T) {
	cases := map[string]string{
		"us-west1":                            "us-west1",
		"=?base64?SGVsbG8sIOS4lueVjA==?=":     "Hello, 世界",
		"=?base64?IHBhZGRlZCA=?=":             " padded ",
		"=?base64?PT9iYXNlNjQ/bGl0ZXJhbD89?=": "=?base64?literal?=",
		"=?base64??=":                         "",
		"=?BASE64?eA==?=":                     "=?BASE64?eA==?=", // markers are case-sensitive
	}
	for in, want := range cases {
		got, err := decodeHeaderValue(in)
		if err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"=?base64?not base64?=", "=?base64?/w==?="} {
		if _, err := decodeHeaderValue(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCheckHeadersByRevision(t *testing.T) {
	legacy := frame(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_invoice"}}`)
	if err := checkHeaders(http.Header{}, legacy); err != nil {
		t.Errorf("legacy requests need no mirrored header: %v", err)
	}
	modernBatch := frame(t, `[{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}]`)
	if err := checkHeaders(http.Header{"Mcp-Protocol-Version": {"2026-07-28"}}, modernBatch); err == nil || err.code != jsonrpc.CodeInvalidRequest {
		t.Errorf("modern batch: %v", err)
	}
	response := frame(t, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	if err := checkHeaders(http.Header{"Mcp-Protocol-Version": {"2026-07-28"}}, response); err == nil {
		t.Error("modern clients send no responses")
	}
	if err := checkHeaders(http.Header{"Mcp-Protocol-Version": {"2025-11-25"}}, response); err != nil {
		t.Errorf("legacy clients answer server requests: %v", err)
	}
	legacyBatch := frame(t, `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`)
	if err := checkHeaders(http.Header{"Mcp-Method": {"tools/list"}}, legacyBatch); err == nil {
		t.Error("mirrored headers cannot describe a batch")
	}
	notification := frame(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`)
	if err := checkHeaders(http.Header{}, notification); err != nil {
		t.Errorf("revision 2026-07-28 defines no header for notifications: %v", err)
	}
	// The body says 2026-07-28: the request is modern whatever the header.
	bodyModern := frame(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`)
	if err := checkHeaders(http.Header{"Mcp-Protocol-Version": {"2025-11-25"}, "Mcp-Method": {"tools/list"}}, bodyModern); err == nil {
		t.Error("version header lower than the body")
	}
	resource := frame(t, `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"file:///a b","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`)
	h := http.Header{"Mcp-Protocol-Version": {"2026-07-28"}, "Mcp-Method": {"resources/read"}, "Mcp-Name": {"file:///a b"}}
	if err := checkHeaders(h, resource); err != nil {
		t.Errorf("resource URI: %v", err)
	}
	h.Set("Mcp-Param-Region", "\x7f")
	if err := checkHeaders(h, resource); err == nil {
		t.Error("invalid characters in an Mcp-Param header")
	}
}
