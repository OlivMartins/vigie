// Package sse reads and writes Server-Sent Events streams, as the MCP
// Streamable HTTP transport uses them, after the WHATWG specification
// (https://html.spec.whatwg.org/multipage/server-sent-events.html).
//
// The reader keeps what a relay needs to re-emit an event faithfully: its id,
// type and retry fields, even when it has no data (the priming event of the
// 2025-11-25 revision), and its comment lines (keep-alives).
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
)

// ErrEventTooLarge reports an event larger than the reader's limit.
var ErrEventTooLarge = errors.New("server-sent event exceeds the size limit")

// Event is one block of the stream, up to the blank line that ends it.
type Event struct {
	// Comments holds comment lines, without their leading colon.
	Comments []string
	// ID is the event id, set when HasID.
	ID    string
	HasID bool
	// Type is the event type; empty means "message".
	Type string
	// Data is the event data: its data lines joined with "\n".
	Data    string
	HasData bool
	// Retry is the reconnection delay in milliseconds, as written.
	Retry string
}

// IsMessage reports whether the event carries a message for the default
// "message" handler, the only one MCP clients listen to.
func (e *Event) IsMessage() bool { return e.HasData && (e.Type == "" || e.Type == "message") }

// Reader reads events from a stream.
type Reader struct {
	br      *bufio.Reader
	max     int
	skipLF  bool // the last line ended with a CR: a LF right after belongs to it
	started bool
}

// NewReader returns a Reader that refuses events larger than maxBytes.
func NewReader(r io.Reader, maxBytes int) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, 32<<10), max: maxBytes}
}

// Next returns the next event. It returns io.EOF when the stream ends; an
// event cut short by the end of the stream is discarded, as the
// specification requires.
func (r *Reader) Next() (*Event, error) {
	var e Event
	var data strings.Builder
	size, empty := 0, true
	for {
		line, err := r.readLine()
		if err != nil {
			return nil, err
		}
		if len(line) == 0 {
			if empty {
				continue
			}
			if !e.HasID && !e.HasData && e.Type == "" && e.Retry == "" && len(e.Comments) == 0 {
				// Only unknown or invalid fields: nothing to dispatch.
				e, empty, size = Event{}, true, 0
				continue
			}
			e.Data = data.String()
			return &e, nil
		}
		size += len(line)
		if r.max > 0 && size > r.max {
			return nil, ErrEventTooLarge
		}
		empty = false
		if line[0] == ':' {
			e.Comments = append(e.Comments, string(line[1:]))
			continue
		}
		field, value := line, []byte(nil)
		if i := bytes.IndexByte(line, ':'); i >= 0 {
			field, value = line[:i], line[i+1:]
			value = bytes.TrimPrefix(value, []byte(" "))
		}
		switch string(field) {
		case "data":
			if e.HasData {
				data.WriteByte('\n')
			}
			data.Write(value)
			e.HasData = true
		case "event":
			e.Type = string(value)
		case "id":
			if bytes.IndexByte(value, 0) < 0 {
				e.ID, e.HasID = string(value), true
			}
		case "retry":
			if len(value) > 0 && isDigits(value) {
				e.Retry = string(value)
			}
		}
	}
}

// readLine returns the next line without its terminator: CRLF, LF or CR.
func (r *Reader) readLine() ([]byte, error) {
	if !r.started {
		r.started = true
		if b, err := r.br.Peek(3); err == nil && bytes.Equal(b, []byte("\xEF\xBB\xBF")) {
			_, _ = r.br.Discard(3)
		}
	}
	if r.skipLF {
		r.skipLF = false
		if b, err := r.br.Peek(1); err == nil && b[0] == '\n' {
			_, _ = r.br.Discard(1)
		}
	}
	var line []byte
	for {
		if _, err := r.br.Peek(1); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.EOF
			}
			return nil, err
		}
		buf, _ := r.br.Peek(r.br.Buffered())
		if i := bytes.IndexAny(buf, "\r\n"); i >= 0 {
			line = append(line, buf[:i]...)
			r.skipLF = buf[i] == '\r'
			_, _ = r.br.Discard(i + 1)
			return line, nil
		}
		line = append(line, buf...)
		_, _ = r.br.Discard(len(buf))
		if r.max > 0 && len(line) > r.max {
			return nil, ErrEventTooLarge
		}
	}
}

func isDigits(b []byte) bool {
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Encode returns the wire form of an event, ending with the blank line that
// dispatches it. Data lines are split on line breaks.
func Encode(e *Event) []byte {
	var b bytes.Buffer
	for _, c := range e.Comments {
		b.WriteString(":")
		b.WriteString(c)
		b.WriteByte('\n')
	}
	if e.HasID {
		b.WriteString("id: ")
		b.WriteString(e.ID)
		b.WriteByte('\n')
	}
	if e.Type != "" {
		b.WriteString("event: ")
		b.WriteString(e.Type)
		b.WriteByte('\n')
	}
	if e.Retry != "" {
		b.WriteString("retry: ")
		b.WriteString(e.Retry)
		b.WriteByte('\n')
	}
	if e.HasData {
		for _, line := range splitLines(e.Data) {
			b.WriteString("data: ")
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	b.WriteByte('\n')
	return b.Bytes()
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.Split(s, "\n")
}
