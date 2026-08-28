package http

import (
	"net/http"
	"strconv"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"

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

// changeFeedCapturer is implemented by the CDC store decorator, which is in the
// write path exactly when LOOMCYCLE_MEMORY_CHANGES_ENABLED. Asking the store is
// what makes the answer below true by construction rather than by a second
// reading of the env var — the two could disagree, and the one that matters is
// whether writes are actually being captured.
type changeFeedCapturer interface{ CapturesChanges() bool }

// changeFeedEnabled reports whether writes to this store land in the feed.
func (s *Server) changeFeedEnabled() bool {
	c, ok := s.store.(changeFeedCapturer)
	return ok && c.CapturesChanges()
}

// embedderHealthReporter is implemented by providers.ObservedEmbedder.
type embedderHealthReporter interface {
	Health() providers.EmbedderHealth
}

// embedderHealth reports what the embedder has actually been doing, for the
// opening frame.
//
// WHY THE CHANGE FEED CARRIES THIS. An embedding failure on a content write is
// deliberately not fatal — the body is stored and the embedding skipped with a log
// line, because losing an author's text to an unreachable embedder is worse than
// losing its searchability. So an embedder outage produces no error anywhere a
// caller can see: writes succeed, change frames keep arriving, and search quietly
// stops finding everything written during the outage. A reader watching this feed
// to answer "is the memory pipeline healthy" would see a perfectly busy feed and
// conclude yes.
//
// `state: failing` plus a failure COUNT is what makes it actionable: the count is
// how many rows need `backfill_embeddings`, which is the remedy that already
// exists.
//
// A deployment with no embedder reports `absent` rather than nothing at all — the
// difference between "not configured" and "configured and broken" is the whole
// point, and an omitted field would collapse them.
func (s *Server) embedderHealth() providers.EmbedderHealth {
	if s.embedder == nil {
		return providers.EmbedderHealth{State: providers.EmbedderAbsent}
	}
	if r, ok := s.embedder.(embedderHealthReporter); ok {
		return r.Health()
	}
	// An embedder that is configured but not wrapped (a test fixture, or a future
	// call site that forgets ObserveEmbedder). Report what is knowable and do NOT
	// claim health: `untried` says "configured, nothing observed", which is exactly
	// true here.
	return providers.EmbedderHealth{
		State:    providers.EmbedderUntried,
		Provider: s.embedder.Provider(),
		Model:    s.embedder.Model(),
	}
}

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

	// THE OPENING FRAME SAYS WHETHER THE FEED IS ON, and it is here because of what
	// this stream does otherwise. With LOOMCYCLE_MEMORY_CHANGES_ENABLED unset the
	// table is never written, so the connection succeeds, keepalives flow, and no
	// change frame ever arrives — which is indistinguishable from a healthy feed over
	// a quiet store. A reader watching for "is the pass doing anything" would wait
	// forever on a misconfiguration and conclude nothing is happening.
	//
	// Sent on the channel that would otherwise idle rather than added to a separate
	// report, so a client cannot hold a stale answer while connected, and so the
	// statement comes from the code that would be doing the capturing.
	//
	// Additive: an existing subscriber switches on the change TYPE and ignores an
	// event name it does not know (the payload deliberately carries no `type`, so a
	// client that backfills from the event name sees `feed`).
	stream.sendRaw("feed", map[string]any{
		"enabled": s.changeFeedEnabled(),
		"since":   cursor,
		// Reported alongside `enabled`, never folded into it: capture being on and
		// the embedder working are independent failures, and a single boolean would
		// make "capturing but unsearchable" indistinguishable from healthy.
		"embedder": s.embedderHealth(),
	})

	stream.startKeepalive(r.Context(), s.cfg().Env.SSEKeepaliveInterval)

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
