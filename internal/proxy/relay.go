package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/OlivMartins/vigie/internal/config"
	"github.com/OlivMartins/vigie/internal/jsonrpc"
)

// Errors returned when a stream cannot be written to anymore.
var (
	ErrClientGone = errors.New("client stream closed")
	ErrServerGone = errors.New("server stream closed")
)

// Relay carries a Conversation over the stdio transport: one frame per line,
// in both directions. Handle* methods are called from one goroutine per
// direction.
type Relay struct {
	*Conversation
	toClient *jsonrpc.Writer
	toServer *jsonrpc.Writer

	mu      sync.Mutex
	garbage int // unparseable frames from the server, for log throttling
}

// NewRelay returns a relay writing to the given client and server streams.
func NewRelay(opts Options, toClient, toServer *jsonrpc.Writer) *Relay {
	return &Relay{Conversation: NewConversation(opts), toClient: toClient, toServer: toServer}
}

// HandleClientFrame processes one frame read from the client. It returns an
// error only when a stream cannot be written to anymore.
func (r *Relay) HandleClientFrame(line []byte) error {
	frame, err := jsonrpc.ParseFrame(line)
	if err != nil {
		return r.unparseableFromClient(line, err)
	}
	var forward, replies []*jsonrpc.Message
	for _, m := range frame.Messages {
		fwd, reply := r.FromClient(m)
		if fwd {
			forward = append(forward, m)
		}
		if reply != nil {
			replies = append(replies, reply)
		}
	}
	if err := send(r.toServer, frame, forward); err != nil {
		return fmt.Errorf("%w: %w", ErrServerGone, err)
	}
	if err := send(r.toClient, &jsonrpc.Frame{Batch: frame.Batch}, replies); err != nil {
		return fmt.Errorf("%w: %w", ErrClientGone, err)
	}
	return nil
}

// HandleServerFrame processes one frame read from the server.
func (r *Relay) HandleServerFrame(line []byte) error {
	frame, err := jsonrpc.ParseFrame(line)
	if err != nil {
		r.unparseableFromServer(line, err)
		if r.Mode() == config.ModeEnforce {
			// What Vigie cannot read, it cannot filter: a lenient client
			// might read a tool list in it.
			return nil
		}
		if err := r.toClient.Write(line); err != nil {
			return fmt.Errorf("%w: %w", ErrClientGone, err)
		}
		return nil
	}
	keep := make([]*jsonrpc.Message, 0, len(frame.Messages))
	for _, m := range frame.Messages {
		if r.FromServer(m) {
			keep = append(keep, m)
		}
	}
	if err := send(r.toClient, frame, keep); err != nil {
		return fmt.Errorf("%w: %w", ErrClientGone, err)
	}
	return nil
}

// RejectOversized answers the client when one of its frames was too large to
// be read: the frame was dropped, its id is unknown.
func (r *Relay) RejectOversized(limit int) error {
	r.log.Error("message from client dropped: too large", "limit_bytes", limit)
	return r.toClient.WriteMessage(jsonrpc.NewErrorResponse(jsonrpc.NullID, jsonrpc.CodeInvalidRequest,
		fmt.Sprintf("message dropped by Vigie: larger than %d bytes", limit)))
}

// DropOversized records a server frame too large to be read.
func (r *Relay) DropOversized(limit int) {
	r.log.Error("message from server dropped: too large", "limit_bytes", limit)
}

func (r *Relay) unparseableFromClient(line []byte, err error) error {
	r.log.Warn("message from client is not valid JSON-RPC", "error", err, "bytes", len(line))
	if r.Mode() == config.ModeAudit {
		if err := r.toServer.Write(line); err != nil {
			return fmt.Errorf("%w: %w", ErrServerGone, err)
		}
		return nil
	}
	// Enforce mode: what Vigie cannot read, it cannot check. A lenient
	// server might read it differently, so it is not relayed.
	if err := r.toClient.WriteMessage(jsonrpc.NewErrorResponse(jsonrpc.NullID, jsonrpc.CodeParseError,
		"message rejected by Vigie: not valid JSON-RPC")); err != nil {
		return fmt.Errorf("%w: %w", ErrClientGone, err)
	}
	return nil
}

func (r *Relay) unparseableFromServer(line []byte, err error) {
	r.mu.Lock()
	r.garbage++
	n := r.garbage
	r.mu.Unlock()
	// Servers that print logs on standard output would flood the log.
	if n <= 3 || n%100 == 0 {
		r.log.Warn("server wrote something that is not JSON-RPC on standard output",
			"error", err, "bytes", len(line), "occurrences", n, "relayed", r.Mode() == config.ModeAudit)
	}
}

// send writes the messages of a frame that survived processing, as a batch
// when the frame was one.
func send(w *jsonrpc.Writer, frame *jsonrpc.Frame, msgs []*jsonrpc.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	b, err := Encode(frame, msgs)
	if err != nil {
		return err
	}
	return w.Write(b)
}

// Encode returns the bytes of the messages of a frame that survived
// processing: the original bytes when nothing was dropped or changed, a batch
// when the frame was one.
func Encode(frame *jsonrpc.Frame, msgs []*jsonrpc.Message) ([]byte, error) {
	if !frame.Batch {
		return msgs[0].Bytes()
	}
	if len(msgs) == len(frame.Messages) {
		return frame.Bytes()
	}
	parts := make([]json.RawMessage, 0, len(msgs))
	for _, m := range msgs {
		b, err := m.Bytes()
		if err != nil {
			return nil, err
		}
		parts = append(parts, b)
	}
	return jsonrpc.Marshal(parts)
}
