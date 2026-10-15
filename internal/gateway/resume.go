package gateway

import (
	"sync"
	"time"

	"github.com/OlivMartins/vigie/internal/jsonrpc"
	"github.com/OlivMartins/vigie/internal/proxy"
)

// Bounds of the resumption registry.
const (
	resumeTTL        = 10 * time.Minute
	resumeMaxEntries = 10000
)

// registry remembers the requests of interrupted streams, for revisions
// 2025-03-26 to 2025-11-25: a client resumes such a stream with a GET and a
// Last-Event-ID header, and the server replays the responses there. Vigie
// must still check them (a replayed tools/list is filtered like any other),
// so it keeps, for a while, what each pending request asked for.
//
// The registry lives in memory: a stream resumed on another instance finds
// nothing, and its responses are dropped in enforce mode, the client
// retrying. Revision 2026-07-28 has no resumption.
type registry struct {
	mu      sync.Mutex
	entries map[resumeKey]resumeEntry
}

type resumeKey struct{ server, session, id string }

type resumeEntry struct {
	pending proxy.Pending
	expires time.Time
}

func newRegistry() *registry { return &registry{entries: make(map[resumeKey]resumeEntry)} }

func (r *registry) put(server, session string, pending []proxy.Pending) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if len(r.entries)+len(pending) > resumeMaxEntries {
		for k, e := range r.entries {
			if now.After(e.expires) {
				delete(r.entries, k)
			}
		}
		if len(r.entries)+len(pending) > resumeMaxEntries {
			return // full of live entries: those responses will be dropped
		}
	}
	for _, p := range pending {
		r.entries[resumeKey{server, sessionHash(session), p.ID.Key()}] = resumeEntry{pending: p, expires: now.Add(resumeTTL)}
	}
}

func (r *registry) take(server, session string, id jsonrpc.ID) (proxy.Pending, bool) {
	key := resumeKey{server, sessionHash(session), id.Key()}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[key]
	if !ok {
		return proxy.Pending{}, false
	}
	delete(r.entries, key)
	if time.Now().After(e.expires) {
		return proxy.Pending{}, false
	}
	return e.pending, true
}
