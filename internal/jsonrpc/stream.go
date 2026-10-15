package jsonrpc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
)

// DefaultMaxFrameBytes bounds a single frame. Tool results carrying images or
// documents can weigh several megabytes; anything beyond this is refused rather
// than buffered without limit.
const DefaultMaxFrameBytes = 64 << 20

// ErrFrameTooLarge reports a frame larger than the reader's limit. The frame is
// discarded and the reader stays usable.
var ErrFrameTooLarge = errors.New("frame exceeds the size limit")

// Reader splits a stdio stream into frames: one JSON-RPC message (or batch)
// per line, as the MCP stdio transport defines it.
type Reader struct {
	r   *bufio.Reader
	max int
}

// NewReader returns a Reader that refuses frames larger than maxBytes.
func NewReader(r io.Reader, maxBytes int) *Reader {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxFrameBytes
	}
	return &Reader{r: bufio.NewReaderSize(r, 64<<10), max: maxBytes}
}

// Next returns the next non-empty line, without its line terminator. It
// returns io.EOF once the stream ends; a final line without terminator is
// still returned first.
func (r *Reader) Next() ([]byte, error) {
	for {
		line, err := r.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) && len(bytes.TrimSpace(line)) > 0 {
				return line, nil
			}
			return nil, err
		}
		if len(bytes.TrimSpace(line)) > 0 {
			return line, nil
		}
	}
}

func (r *Reader) readLine() ([]byte, error) {
	var buf []byte
	tooLarge := false
	for {
		chunk, err := r.r.ReadSlice('\n')
		if !tooLarge {
			if len(buf)+len(chunk) > r.max+2 { // +2: the "\r\n" terminator does not count
				tooLarge = true
				buf = nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case err == nil:
			if tooLarge {
				return nil, fmt.Errorf("%w (%d bytes)", ErrFrameTooLarge, r.max)
			}
			return bytes.TrimRight(buf, "\r\n"), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			if tooLarge {
				return nil, err
			}
			return bytes.TrimRight(buf, "\r\n"), err
		}
	}
}

// Writer writes frames, one per line. It is safe for concurrent use: a proxy
// answers some requests itself while relaying others on the same stream.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

// NewWriter returns a Writer on w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Write writes one frame followed by a newline, in a single call so that
// concurrent frames never interleave.
func (w *Writer) Write(frame []byte) error {
	line := make([]byte, 0, len(frame)+1)
	line = append(line, frame...)
	line = append(line, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err := w.w.Write(line)
	return err
}

// WriteMessage encodes and writes one message.
func (w *Writer) WriteMessage(m *Message) error {
	b, err := m.Bytes()
	if err != nil {
		return err
	}
	return w.Write(b)
}
