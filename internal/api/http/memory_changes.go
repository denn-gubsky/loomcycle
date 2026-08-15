package http

import (
	"net/http"
	"strconv"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// memory_changes.go serves the RFC CD Part C change-data-capture feed as SSE.
//
// GET /v1/_memory/changes?since=<seq>   — memory.* changes
// GET /v1/_document/changes?since=<seq> — document.* changes
//
// Value-free: each frame names WHAT changed; the subscriber pulls the current
// value via the data API. Tenant-scoped (the feed is read for the caller's OWN
// tenant only) and — per the locked decision — substrate:tenant, NOT
// member-accessible (see memberCarveOut). The table is only ever populated when
// LOOMCYCLE_MEMORY_CHANGES_ENABLED; with the feed off this stream just idles.

const memoryChangesPollInterval = 250 * time.Millisecond

func (s *Server) handleMemoryChanges(w http.ResponseWriter, r *http.Request) {
	s.streamMemoryChanges(w, r, false)
}

func (s *Server) handleDocumentChanges(w http.ResponseWriter, r *http.Request) {
	s.streamMemoryChanges(w, r, true)
}

// streamMemoryChanges tails the change feed for the caller's tenant, emitting
// only the family it was asked for (documents vs memory). The cursor advances
// over EVERY row so the two families stay consistent; filtering happens on the
// way out.
func (s *Server) streamMemoryChanges(w http.ResponseWriter, r *http.Request, documents bool) {
	if s.store == nil {
		http.Error(w, "change feed requires a persistence backend", http.StatusServiceUnavailable)
		return
	}
	tenantID := tenantFromCtx(r.Context())

	var cursor int64
	if q := r.URL.Query().Get("since"); q != "" {
		if n, err := strconv.ParseInt(q, 10, 64); err == nil && n >= 0 {
			cursor = n
		}
	}

	stream, ok := newSSE(w)
	if !ok {
		http.Error(w, "server does not support streaming on this transport", http.StatusInternalServerError)
		return
	}
	stream.start()
	stream.startKeepalive(r.Context(), s.cfg.Env.SSEKeepaliveInterval)

	ticker := time.NewTicker(memoryChangesPollInterval)
	defer ticker.Stop()
	for {
		// Drain everything past the cursor, then wait for the next tick.
		for {
			changes, err := s.store.GetMemoryChangesSince(r.Context(), tenantID, cursor, 500)
			if err != nil {
				break
			}
			for _, ch := range changes {
				cursor = ch.Seq
				if isDocumentChange(ch.Type) != documents {
					continue
				}
				stream.sendRaw("change", ch)
			}
			if len(changes) < 500 {
				break
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func isDocumentChange(t store.MemoryChangeType) bool {
	return t == store.DocumentChangeUpdated || t == store.DocumentChangeDeleted
}
