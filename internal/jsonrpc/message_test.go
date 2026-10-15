package jsonrpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestParseFrameKinds(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		kind   Kind
		method string
		id     string
	}{
		{"request", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, KindRequest, "tools/list", "1"},
		{"string id", `{"jsonrpc":"2.0","id":"a-1","method":"ping"}`, KindRequest, "ping", "a-1"},
		{"notification", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, KindNotification, "notifications/initialized", ""},
		{"result", `{"jsonrpc":"2.0","id":7,"result":{}}`, KindResponse, "", "7"},
		{"error", `{"jsonrpc":"2.0","id":7,"error":{"code":-32601,"message":"nope"}}`, KindResponse, "", "7"},
		{"surrounding spaces", "  {\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"ping\"} \t", KindRequest, "ping", "2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := ParseFrame([]byte(tt.in))
			if err != nil {
				t.Fatal(err)
			}
			m := f.Messages[0]
			if m.Kind != tt.kind || m.Method != tt.method || m.ID.String() != tt.id {
				t.Errorf("got kind=%v method=%q id=%q", m.Kind, m.Method, m.ID.String())
			}
		})
	}
}

func TestParseFrameRejects(t *testing.T) {
	for _, in := range []string{
		``, `[]`, `"text"`, `42`, `{}`, `{"jsonrpc":"2.0"}`,
		`{"jsonrpc":"2.0","id":1}`, // neither method nor result
		`{"jsonrpc":"2.0","method":""}`,
		`{"jsonrpc":"2.0","method":3}`,
		`{"jsonrpc":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":1,"method":"ping"`, // truncated
		`[{"jsonrpc":"2.0","method":"ping"},{}]`,
		`null`,
	} {
		if _, err := ParseFrame([]byte(in)); err == nil {
			t.Errorf("ParseFrame(%q) accepted an invalid frame", in)
		}
	}
}

// Keys must match exactly: Go struct decoding would map "NAME" to "name", a
// parser differential a malicious client could use against a policy.
func TestParamsKeysAreCaseSensitive(t *testing.T) {
	f, err := ParseFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"NAME":"evil","name":"safe"}}`))
	if err != nil {
		t.Fatal(err)
	}
	params, err := f.Messages[0].Params()
	if err != nil {
		t.Fatal(err)
	}
	if string(params["name"]) != `"safe"` || string(params["NAME"]) != `"evil"` {
		t.Errorf("params = %v", params)
	}
	f, _ = ParseFrame([]byte(`{"jsonrpc":"2.0","ID":1,"Method":"tools/call","result":{}}`))
	if f != nil {
		t.Error("upper-case member names must not count as id or method")
	}
}

func TestIDsKeepTheirType(t *testing.T) {
	a, _ := ParseFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	b, _ := ParseFrame([]byte(`{"jsonrpc":"2.0","id":"1","method":"ping"}`))
	if a.Messages[0].ID.Key() == b.Messages[0].ID.Key() {
		t.Error(`1 and "1" must be distinct request ids`)
	}
	if a.Messages[0].ID.String() != "1" || b.Messages[0].ID.String() != "1" {
		t.Error("both ids render as 1 in telemetry")
	}
	c, _ := ParseFrame([]byte(`{"jsonrpc":"2.0","id": 1 ,"method":"ping"}`))
	if c.Messages[0].ID.Key() != a.Messages[0].ID.Key() {
		t.Error("whitespace around an id must not change its key")
	}
}

func TestUnmodifiedMessageKeepsItsBytes(t *testing.T) {
	in := `{"method":"tools/call", "id":3,"jsonrpc":"2.0","params":{"name":"x","arguments":{"html":"<b>&</b>"}}}`
	f, err := ParseFrame([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in {
		t.Errorf("unmodified frame changed:\n got %s\nwant %s", out, in)
	}
}

func TestModifiedMessageKeepsUnknownMembers(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":3,"method":"tools/call","x-extension":{"a":[1,2.50]},"params":{"name":"x","note":"<b>&</b>"}}`
	f, _ := ParseFrame([]byte(in))
	m := f.Messages[0]
	params, _ := m.Params()
	params["added"] = json.RawMessage(`true`)
	if err := m.SetParams(params); err != nil {
		t.Fatal(err)
	}
	out, _ := f.Bytes()
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["x-extension"]) != `{"a":[1,2.50]}` {
		t.Errorf("unknown member altered: %s", got["x-extension"])
	}
	if !strings.Contains(string(out), `"note":"<b>&</b>"`) {
		t.Errorf("strings must not be HTML-escaped on rewrite: %s", out)
	}
	if !strings.Contains(string(out), `"added":true`) {
		t.Errorf("change missing: %s", out)
	}
}

func TestBatch(t *testing.T) {
	in := `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/x"}]`
	f, err := ParseFrame([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if !f.Batch || len(f.Messages) != 2 {
		t.Fatalf("batch not recognized: %+v", f)
	}
	if out, _ := f.Bytes(); string(out) != in {
		t.Errorf("unmodified batch changed: %s", out)
	}
	f.Messages[1].Members["extra"] = json.RawMessage(`1`)
	f.Messages[1].raw = nil
	out, _ := f.Bytes()
	if !strings.HasPrefix(string(out), "[") || !strings.Contains(string(out), `"extra":1`) {
		t.Errorf("modified batch: %s", out)
	}
}

func TestErrorResponses(t *testing.T) {
	m := NewErrorResponse(ID{}, CodeParseError, "bad")
	b, _ := m.Bytes()
	if string(b) != `{"error":{"code":-32700,"message":"bad"},"id":null,"jsonrpc":"2.0"}` {
		t.Errorf("error response = %s", b)
	}
	f, _ := ParseFrame(b)
	if e := f.Messages[0].Error(); e == nil || e.Code != CodeParseError {
		t.Errorf("Error() = %+v", e)
	}
	ok := NewResponse(IDFromRaw(json.RawMessage(`"x"`)), json.RawMessage(`{}`))
	if ok.Error() != nil {
		t.Error("a success response has no error")
	}
}

func TestReader(t *testing.T) {
	in := "{\"a\":1}\r\n\n   \n{\"b\":2}\n{\"c\":3}"
	r := NewReader(strings.NewReader(in), 1024)
	var got []string
	for {
		line, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, string(line))
	}
	want := []string{`{"a":1}`, `{"b":2}`, `{"c":3}`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("lines = %q", got)
	}
}

func TestReaderLimit(t *testing.T) {
	big := strings.Repeat("x", 300<<10) // larger than the internal buffer too
	r := NewReader(strings.NewReader(big+"\n{\"ok\":true}\n"), 100<<10)
	if _, err := r.Next(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
	line, err := r.Next()
	if err != nil || string(line) != `{"ok":true}` {
		t.Fatalf("reader not usable after an oversized frame: %q %v", line, err)
	}
}

func TestWriterAppendsNewline(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.Write([]byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "{\"a\":1}\n" {
		t.Errorf("written %q", buf.String())
	}
}

func FuzzParseFrame(f *testing.F) {
	for _, s := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x","arguments":{}}}`,
		`[{"jsonrpc":"2.0","id":1,"result":{}},{"jsonrpc":"2.0","method":"n"}]`,
		`{"jsonrpc":"2.0","id":"x","error":{"code":-1,"message":""}}`,
		`{"id":null,"method":"m"}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		frame, err := ParseFrame(data)
		if err != nil {
			return
		}
		out, err := frame.Bytes()
		if err != nil {
			t.Fatalf("Bytes: %v", err)
		}
		again, err := ParseFrame(out)
		if err != nil {
			t.Fatalf("re-parse of %q: %v", out, err)
		}
		if len(again.Messages) != len(frame.Messages) {
			t.Fatalf("message count changed")
		}
		for i := range again.Messages {
			a, b := frame.Messages[i], again.Messages[i]
			if a.Kind != b.Kind || a.Method != b.Method || a.ID.Key() != b.ID.Key() {
				t.Fatalf("message %d changed: %+v vs %+v", i, a, b)
			}
		}
	})
}

func TestCheckKeys(t *testing.T) {
	for _, tt := range []struct {
		in    string
		names []string
		ok    bool
	}{
		{`{"method":"ping","id":1}`, Envelope, true},
		{`{"method":"ping","METHOD":"tools/call"}`, Envelope, false},
		{`{"name":"a","Name":"b"}`, []string{"name"}, false},
		{`{"name":"a","name":"b"}`, nil, false},
		{`{"name":"a","n\u0061me":"b"}`, nil, false}, // same key once unescaped
		{`{"nameſ":"a"}`, []string{"names"}, false},  // long s folds to s, as Go's decoder does
		{`{"Kind":"a"}`, []string{"kind"}, false},
		{`{"other":"a","Other":"b"}`, nil, true}, // not inspected: case variants are distinct members
		{`[1,2]`, Envelope, true},
		{`"text"`, Envelope, true},
	} {
		err := CheckKeys(json.RawMessage(tt.in), tt.names...)
		if (err == nil) != tt.ok {
			t.Errorf("CheckKeys(%s) = %v", tt.in, err)
		}
		if err != nil && !errors.Is(err, ErrAmbiguous) {
			t.Errorf("CheckKeys(%s) error is not ErrAmbiguous: %v", tt.in, err)
		}
	}
}
