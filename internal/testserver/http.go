package testserver

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// HTTPServer serves a Server over the Streamable HTTP transport, in both
// revisions: sessions, SSE responses, a GET stream and resumption for
// 2025-03-26 to 2025-11-25; per-request metadata for 2026-07-28.
type HTTPServer struct {
	*Server
	// SSE answers requests with an event stream instead of a JSON body.
	SSE bool
	// Sessions assigns a session id at initialize and requires it after.
	Sessions bool
	// Script, when set, handles a request instead of the server: it returns
	// false to let the server handle it.
	Script func(w http.ResponseWriter, r *http.Request, body []byte) bool

	mu       sync.Mutex
	sessions map[string]bool
	requests []Recorded
	events   int
	stream   chan string
}

// Recorded is a request as the server received it.
type Recorded struct {
	Method string
	Header http.Header
	Body   []byte
}

// NewHTTP returns an HTTP server for s.
func NewHTTP(s *Server) *HTTPServer {
	return &HTTPServer{Server: s, sessions: make(map[string]bool), stream: make(chan string, 16)}
}

// Requests returns the requests received.
func (h *HTTPServer) Requests() []Recorded {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Recorded(nil), h.requests...)
}

// Push queues raw SSE text for the GET stream.
func (h *HTTPServer) Push(event string) { h.stream <- event }

// NextEventID returns a fresh SSE event id.
func (h *HTTPServer) NextEventID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events++
	return fmt.Sprintf("ev-%d", h.events)
}

func (h *HTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if !h.NoHistory {
		h.mu.Lock()
		h.requests = append(h.requests, Recorded{Method: r.Method, Header: r.Header.Clone(), Body: body})
		h.mu.Unlock()
	}
	if h.Script != nil && h.Script(w, r, body) {
		return
	}
	switch r.Method {
	case http.MethodPost:
		h.post(w, r, body)
	case http.MethodGet:
		h.get(w, r)
	case http.MethodDelete:
		if !h.Sessions {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		h.mu.Lock()
		delete(h.sessions, r.Header.Get("Mcp-Session-Id"))
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *HTTPServer) post(w http.ResponseWriter, r *http.Request, body []byte) {
	var batch []map[string]json.RawMessage
	var msgs []map[string]json.RawMessage
	isBatch := json.Unmarshal(body, &batch) == nil
	if isBatch {
		msgs = batch
	} else {
		var one map[string]json.RawMessage
		if err := json.Unmarshal(body, &one); err != nil {
			http.Error(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`, http.StatusBadRequest)
			return
		}
		msgs = []map[string]json.RawMessage{one}
	}
	session := r.Header.Get("Mcp-Session-Id")
	initialize := false
	for _, m := range msgs {
		if string(m["method"]) == `"initialize"` {
			initialize = true
		}
	}
	if h.Sessions && !initialize {
		h.mu.Lock()
		ok := h.sessions[session]
		h.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}
	var responses []map[string]any
	for _, m := range msgs {
		if resp := h.process(m); resp != nil {
			responses = append(responses, resp)
		}
	}
	if h.Sessions && initialize {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		session = hex.EncodeToString(b)
		h.mu.Lock()
		h.sessions[session] = true
		h.mu.Unlock()
		w.Header().Set("Mcp-Session-Id", session)
	}
	if len(responses) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if h.SSE {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "id: %s\ndata: \n\n", h.NextEventID())
		for _, resp := range responses {
			data, _ := json.Marshal(resp)
			fmt.Fprintf(w, ": keep-alive\nid: %s\ndata: %s\n\n", h.NextEventID(), data)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if isBatch {
		_ = json.NewEncoder(w).Encode(responses)
		return
	}
	_ = json.NewEncoder(w).Encode(responses[0])
}

func (h *HTTPServer) get(w http.ResponseWriter, r *http.Request) {
	if !h.Sessions {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-h.stream:
			if _, err := io.WriteString(w, ev); err != nil {
				return
			}
			_ = rc.Flush()
		}
	}
}
