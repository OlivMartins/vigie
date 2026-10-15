// Package audit writes Vigie's decisions to a local file, one JSON object per
// line, for teams without an observability backend and for evidence that
// outlives trace retention. The file rotates by size; several Vigie
// processes can share it.
package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/OlivMartins/vigie/internal/filelock"
)

// Events recorded.
const (
	// EventToolCall is the decision on a tools/call request, recorded for
	// every call, allowed or not.
	EventToolCall = "tool.call"
	// EventToolList is a tools/list result in which a rule matched.
	EventToolList = "tool.list"
	// EventToolResult is a tools/call result in which a rule matched.
	EventToolResult = "tool.result"
)

// Defaults.
const (
	DefaultMaxSize  = 100 << 20
	DefaultMaxFiles = 5
)

// Finding is one rule that matched. It never holds the detected value.
type Finding struct {
	Rule       string `json:"rule"`
	Action     string `json:"action"`
	Tool       string `json:"tool,omitempty"`
	Location   string `json:"location,omitempty"`
	SecretType string `json:"secret_type,omitempty"`
	Reason     string `json:"reason"`
}

// Entry is one line of the audit log.
type Entry struct {
	Time      time.Time `json:"time"`
	Event     string    `json:"event"`
	Server    string    `json:"server"`
	Transport string    `json:"transport"`
	// Session is a hash of the MCP session id, never the id itself: it can
	// be enough to take over a session.
	Session   string    `json:"session,omitempty"`
	Client    string    `json:"client,omitempty"`
	Method    string    `json:"method"`
	Tool      string    `json:"tool,omitempty"`
	Decision  string    `json:"decision"`
	Mode      string    `json:"mode"`
	Rule      string    `json:"rule,omitempty"`
	PinStatus string    `json:"pin_status,omitempty"`
	TraceID   string    `json:"trace_id,omitempty"`
	SpanID    string    `json:"span_id,omitempty"`
	Findings  []Finding `json:"findings,omitempty"`
}

// Options configures a Log.
type Options struct {
	Path string
	// MaxSize is the size in bytes beyond which the file is rotated.
	MaxSize int64
	// MaxFiles is how many rotated files are kept: path.1 to path.MaxFiles.
	MaxFiles int
}

// Log appends entries to the audit file.
type Log struct {
	opts Options

	mu sync.Mutex
	f  *os.File
}

// Open opens (or creates) the audit file.
func Open(opts Options) (*Log, error) {
	if opts.Path == "" {
		return nil, errors.New("audit log: no file given")
	}
	if opts.MaxSize <= 0 {
		opts.MaxSize = DefaultMaxSize
	}
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = DefaultMaxFiles
	}
	l := &Log{opts: opts}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

// Path returns the file written to.
func (l *Log) Path() string { return l.opts.Path }

// Write appends an entry. The line is written in one call, under a lock
// shared with the other processes writing the same file.
func (l *Log) Write(e Entry) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	unlock, err := filelock.Lock(l.opts.Path)
	if err != nil {
		return err
	}
	defer unlock()
	if err := l.follow(); err != nil {
		return err
	}
	if info, err := l.f.Stat(); err == nil && info.Size() > 0 && info.Size()+int64(buf.Len()) > l.opts.MaxSize {
		if err := l.rotate(); err != nil {
			return fmt.Errorf("rotating %s: %w", l.opts.Path, err)
		}
	}
	_, err = l.f.Write(buf.Bytes())
	return err
}

// Close closes the file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

func (l *Log) open() error {
	if err := os.MkdirAll(filepath.Dir(l.opts.Path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(l.opts.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	l.f = f
	return nil
}

// follow reopens the file when another process rotated it.
func (l *Log) follow() error {
	if l.f == nil {
		return l.open()
	}
	onDisk, err := os.Stat(l.opts.Path)
	if errors.Is(err, fs.ErrNotExist) {
		l.f.Close()
		return l.open()
	}
	if err != nil {
		return err
	}
	mine, err := l.f.Stat()
	if err != nil || !os.SameFile(mine, onDisk) {
		l.f.Close()
		return l.open()
	}
	return nil
}

// rotate shifts path.N-1 to path.N... path to path.1, and starts a new file.
func (l *Log) rotate() error {
	l.f.Close()
	l.f = nil
	oldest := fmt.Sprintf("%s.%d", l.opts.Path, l.opts.MaxFiles)
	if err := os.Remove(oldest); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for i := l.opts.MaxFiles - 1; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", l.opts.Path, i)
		if err := os.Rename(from, fmt.Sprintf("%s.%d", l.opts.Path, i+1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(l.opts.Path, l.opts.Path+".1"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return l.open()
}
