package gateway

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/OlivMartins/vigie/internal/jsonrpc"
	"github.com/OlivMartins/vigie/internal/mcp"
)

// MCP HTTP headers.
const (
	HeaderProtocolVersion = "Mcp-Protocol-Version"
	HeaderSessionID       = "Mcp-Session-Id"
	HeaderMethod          = "Mcp-Method"
	HeaderName            = "Mcp-Name"
	HeaderParamPrefix     = "Mcp-Param-"
	HeaderLastEventID     = "Last-Event-Id"
)

// CodeHeaderMismatch is the JSON-RPC error of revision 2026-07-28 for HTTP
// headers that are missing, malformed, or disagree with the message body.
const CodeHeaderMismatch = -32020

// headerError is a request refused before its messages are processed.
type headerError struct {
	code    int
	message string
}

func (e *headerError) Error() string { return e.message }

func mismatch(format string, args ...any) *headerError {
	return &headerError{code: CodeHeaderMismatch, message: "Header mismatch: " + fmt.Sprintf(format, args...)}
}

// checkHeaders compares the MCP headers of a POST request with its body.
//
// Revision 2026-07-28 mirrors the protocol version, the method and the target
// of a request into headers, so that load balancers and gateways can route
// without reading the body, and requires servers to reject any disagreement.
// Vigie reads the body, but whatever sits after it may trust the headers: a
// request whose headers say one thing and whose body says another is refused,
// whatever its revision.
func checkHeaders(h http.Header, frame *jsonrpc.Frame) *headerError {
	for name, values := range h {
		if !strings.HasPrefix(name, "Mcp-") {
			continue
		}
		if len(values) > 1 {
			return mismatch("%s header given %d times", name, len(values))
		}
		if !validHeaderValue(values[0]) {
			return mismatch("%s header holds invalid characters", name)
		}
		if name == HeaderName || strings.HasPrefix(name, HeaderParamPrefix) {
			if _, err := decodeHeaderValue(values[0]); err != nil {
				return mismatch("%s header: %v", name, err)
			}
		}
	}
	version := h.Get(HeaderProtocolVersion)
	modern := mcp.IsStateless(version)
	for _, m := range frame.Messages {
		if v := mcp.Inspect(m).ProtocolVersion; mcp.IsStateless(v) {
			modern = true
		}
	}
	if modern {
		if frame.Batch {
			return &headerError{code: jsonrpc.CodeInvalidRequest, message: "batches are not part of protocol revision " + mcp.StatelessRevision}
		}
		if frame.Messages[0].Kind == jsonrpc.KindResponse {
			return &headerError{code: jsonrpc.CodeInvalidRequest, message: "clients send no responses in protocol revision " + mcp.StatelessRevision}
		}
	}
	if frame.Batch {
		// Mirrored headers describe one message: on a batch they would be
		// ambiguous.
		if h.Get(HeaderMethod) != "" || h.Get(HeaderName) != "" {
			return mismatch("%s and %s headers cannot describe a batch", HeaderMethod, HeaderName)
		}
		return nil
	}
	m := frame.Messages[0]
	if m.Kind == jsonrpc.KindResponse {
		return nil
	}
	req := mcp.Inspect(m)
	if modern && m.Kind == jsonrpc.KindRequest {
		switch {
		case version == "":
			return mismatch("%s header is missing", HeaderProtocolVersion)
		case req.ProtocolVersion != "" && req.ProtocolVersion != version:
			// A body without its version is malformed: the server answers
			// that one (Invalid params), not Vigie.
			return mismatch("%s header %q does not match the body (%q)", HeaderProtocolVersion, version, req.ProtocolVersion)
		case h.Get(HeaderMethod) == "":
			return mismatch("%s header is missing", HeaderMethod)
		}
	}
	if method := h.Get(HeaderMethod); method != "" && method != m.Method {
		return mismatch("%s header does not match the body method %q", HeaderMethod, m.Method)
	}
	target, named := nameOf(m.Method, req)
	if !named {
		return nil
	}
	header := h.Get(HeaderName)
	if header == "" {
		if modern && m.Kind == jsonrpc.KindRequest {
			return mismatch("%s header is missing", HeaderName)
		}
		return nil
	}
	decoded, _ := decodeHeaderValue(header)
	if decoded != target {
		return mismatch("%s header does not match the body", HeaderName)
	}
	return nil
}

// nameOf returns the value a request mirrors into Mcp-Name.
func nameOf(method string, req mcp.Request) (string, bool) {
	switch method {
	case mcp.MethodToolsCall:
		return req.Tool, true
	case mcp.MethodPromptsGet:
		return req.Prompt, true
	case mcp.MethodResourcesRead:
		return req.ResourceURI, true
	}
	return "", false
}

const (
	sentinelPrefix = "=?base64?"
	sentinelSuffix = "?="
)

// decodeHeaderValue reads a header value that may use the Base64 sentinel
// encoding of revision 2026-07-28: =?base64?{value}?=.
func decodeHeaderValue(v string) (string, error) {
	if !strings.HasPrefix(v, sentinelPrefix) || !strings.HasSuffix(v, sentinelSuffix) || len(v) < len(sentinelPrefix)+len(sentinelSuffix) {
		return v, nil
	}
	raw, err := base64.StdEncoding.DecodeString(v[len(sentinelPrefix) : len(v)-len(sentinelSuffix)])
	if err != nil {
		return "", errors.New("invalid Base64 value")
	}
	if !utf8.Valid(raw) {
		return "", errors.New("Base64 value is not UTF-8")
	}
	return string(raw), nil
}

// validHeaderValue reports whether v only holds visible ASCII, spaces and
// tabs, as RFC 9110 field values must.
func validHeaderValue(v string) bool {
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c != '\t' && (c < 0x20 || c > 0x7e) {
			return false
		}
	}
	return true
}
