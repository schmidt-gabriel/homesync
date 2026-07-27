package api

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// heartbeatInterval keeps idle SSE connections alive through proxies and NAT
// tables that would otherwise drop a silent stream.
const heartbeatInterval = 25 * time.Second

// broadcaster fans revision bumps out to every connected client.
//
// The payload is only the new revision number, never the change itself: the
// client always follows up with GET /v1/changes, so a dropped or coalesced
// event can never cause a missed update. The stream is a hint to look, not a
// channel that has to be reliable.
type broadcaster struct {
	mu   sync.Mutex
	subs map[chan int64]struct{}
}

func newBroadcaster() *broadcaster {
	return &broadcaster{subs: make(map[chan int64]struct{})}
}

func (b *broadcaster) subscribe() chan int64 {
	// Buffered so a slow reader cannot block the write path.
	ch := make(chan int64, 8)

	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[ch] = struct{}{}
	return ch
}

func (b *broadcaster) unsubscribe(ch chan int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[ch]; ok {
		delete(b.subs, ch)
		close(ch)
	}
}

func (b *broadcaster) publish(rev int64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for ch := range b.subs {
		select {
		case ch <- rev:
		default:
			// Subscriber is behind. Dropping is safe: it will still call
			// /v1/changes and catch up in one request.
		}
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Tell nginx not to buffer, which would defeat the whole point.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ch := s.events.subscribe()
	defer s.events.unsubscribe(ch)

	// Send the current revision immediately so a client that reconnects knows
	// straight away whether it missed anything.
	if rev, err := s.index.CurrentRev(r.Context()); err == nil {
		fmt.Fprintf(w, "event: rev\ndata: {\"rev\":%d}\n\n", rev)
		flusher.Flush()
	}

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case rev, open := <-ch:
			if !open {
				return
			}
			// Drain anything that piled up: only the newest revision matters,
			// since the client fetches everything since its own position.
			for drained := true; drained; {
				select {
				case newer, stillOpen := <-ch:
					if !stillOpen {
						return
					}
					rev = newer
				default:
					drained = false
				}
			}
			fmt.Fprintf(w, "event: rev\ndata: {\"rev\":%d}\n\n", rev)
			flusher.Flush()

		case <-ticker.C:
			// A comment line is a valid no-op event in SSE.
			fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}
