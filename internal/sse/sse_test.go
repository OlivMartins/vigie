package sse

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func readAll(t *testing.T, stream string, max int) ([]*Event, error) {
	t.Helper()
	r := NewReader(strings.NewReader(stream), max)
	var events []*Event
	for {
		e, err := r.Next()
		if errors.Is(err, io.EOF) {
			return events, nil
		}
		if err != nil {
			return events, err
		}
		events = append(events, e)
	}
}

func TestFields(t *testing.T) {
	events, err := readAll(t, "\xEF\xBB\xBF: hello\nid: 7\nevent: message\nretry: 1500\ndata: {\"a\":\ndata:1}\nunknown: x\n\n", 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("%v %d", err, len(events))
	}
	e := events[0]
	if e.ID != "7" || !e.HasID || e.Type != "message" || e.Retry != "1500" || e.Data != "{\"a\":\n1}" || !e.IsMessage() {
		t.Errorf("%+v", e)
	}
	if len(e.Comments) != 1 || e.Comments[0] != " hello" {
		t.Errorf("comments: %q", e.Comments)
	}
}

func TestLineEndings(t *testing.T) {
	for name, stream := range map[string]string{
		"LF":   "data: a\n\ndata: b\n\n",
		"CRLF": "data: a\r\n\r\ndata: b\r\n\r\n",
		"CR":   "data: a\r\rdata: b\r\r",
	} {
		events, err := readAll(t, stream, 0)
		if err != nil || len(events) != 2 || events[0].Data != "a" || events[1].Data != "b" {
			t.Errorf("%s: %v %+v", name, err, events)
		}
	}
}

func TestEventsWithoutData(t *testing.T) {
	events, err := readAll(t, "id: prime\ndata: \n\nretry: 10\n\n:keep-alive\n\nevent: endpoint\ndata: /messages\n\n", 0)
	if err != nil || len(events) != 4 {
		t.Fatalf("%v %+v", err, events)
	}
	if !events[0].HasID || !events[0].HasData || events[0].Data != "" {
		t.Errorf("priming event: %+v", events[0])
	}
	if events[1].Retry != "10" || events[1].HasData {
		t.Errorf("retry: %+v", events[1])
	}
	if len(events[2].Comments) != 1 {
		t.Errorf("comment: %+v", events[2])
	}
	if events[3].IsMessage() {
		t.Error("an endpoint event is not a message")
	}
}

func TestInvalidFieldsAreIgnored(t *testing.T) {
	events, _ := readAll(t, "id: a\x00b\nretry: 12x\ndata\n\n", 0)
	if len(events) != 1 || events[0].HasID || events[0].Retry != "" || !events[0].HasData || events[0].Data != "" {
		t.Errorf("%+v", events)
	}
}

func TestTruncatedEventIsDiscarded(t *testing.T) {
	events, err := readAll(t, "data: complete\n\ndata: cut", 0)
	if err != nil || len(events) != 1 {
		t.Errorf("%v %+v", err, events)
	}
}

func TestSizeLimit(t *testing.T) {
	_, err := readAll(t, "data: "+strings.Repeat("x", 100)+"\n\n", 50)
	if !errors.Is(err, ErrEventTooLarge) {
		t.Errorf("long line: %v", err)
	}
	_, err = readAll(t, strings.Repeat("data: xxxxxxxxxx\n", 10)+"\n", 50)
	if !errors.Is(err, ErrEventTooLarge) {
		t.Errorf("many lines: %v", err)
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	in := &Event{Comments: []string{" ping"}, ID: "3", HasID: true, Type: "message", Retry: "100", Data: "line1\nline2", HasData: true}
	events, err := readAll(t, string(Encode(in)), 0)
	if err != nil || len(events) != 1 {
		t.Fatal(err, events)
	}
	out := events[0]
	if out.ID != in.ID || out.Type != in.Type || out.Retry != in.Retry || out.Data != in.Data || out.Comments[0] != " ping" {
		t.Errorf("%+v", out)
	}
	if got := string(Encode(&Event{ID: "9", HasID: true})); got != "id: 9\n\n" {
		t.Errorf("cursor event: %q", got)
	}
}

func FuzzReader(f *testing.F) {
	f.Add("data: {\"jsonrpc\":\"2.0\"}\n\n")
	f.Add("id: 1\r\ndata: a\r\ndata: b\r\n\r\n")
	f.Add(":c\rretry: 5\r\r")
	f.Add("0\n\n") // unknown fields only: nothing to dispatch
	f.Fuzz(func(t *testing.T, stream string) {
		events, err := readAll(t, stream, 1<<16)
		if err != nil {
			return
		}
		// What Vigie re-emits reads back the same.
		for _, e := range events {
			again, err := readAll(t, string(Encode(e)), 1<<17)
			if err != nil || len(again) != 1 {
				t.Fatalf("re-encoded event unreadable: %q -> %v %d", stream, err, len(again))
			}
			a := again[0]
			if a.Data != strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(e.Data) || a.ID != e.ID || a.HasID != e.HasID || a.Type != e.Type || a.HasData != e.HasData {
				t.Fatalf("round trip changed the event: %+v -> %+v", e, a)
			}
		}
	})
}
