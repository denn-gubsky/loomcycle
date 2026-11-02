package http

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// document_feed.go serves the member-safe document change feed (RFC FV):
//
//	GET /v1/_document/feed?scope=user|tenant[&scope_id=…][&tenant=…]
//
// The operator feed (GET /v1/_document/changes) is tenant-wide observability
// and stays the operator's: its frames name every chunk the tenant writes, and
// its seq cursor counts, by its gaps, writes to documents a member may not
// list. This route is the reader's: one scope the caller names and may read,
// gated and resolved exactly like POST /v1/_document, and only changes to
// documents the caller can list (builtin.DocumentFeedView). It has no cursor:
// it starts at the current head, never sends a seq, and a reconnect is a
// resync — the client reloads its view and opens a new stream.

// documentFeedAccessTTL is how long one stream trusts its access answers
// (RFC FV G6): a grant revoked mid-stream stops frames within this window.
const documentFeedAccessTTL = 30 * time.Second

// documentFeedPolling is a reader stream's timing and its principal refresh.
// Injected so a test can drive the access window without waiting on it.
type documentFeedPolling struct {
	changeFeedPolling
	ttl time.Duration
	now func() time.Time
	// resolve re-reads a principal's CURRENT access-control set.
	resolve func(context.Context, auth.Principal) (auth.Principal, error)
}

func (s *Server) handleDocumentFeed(w http.ResponseWriter, r *http.Request) {
	s.serveDocumentFeed(w, r, documentFeedPolling{
		changeFeedPolling: changeFeedPolling{base: memoryChangesPollInterval, max: memoryChangesMaxBackoff, sleep: sleepCtx},
		ttl:               documentFeedAccessTTL,
		now:               time.Now,
		resolve:           s.currentPrincipal,
	})
}

// currentPrincipal is p with its access-control set read again: the rules as
// they stand now, not as they stood when the stream connected. No resolver
// wired: access control is not configured, and p is all there is.
func (s *Server) currentPrincipal(ctx context.Context, p auth.Principal) (auth.Principal, error) {
	if s.aclResolver == nil {
		return p, nil
	}
	return s.aclResolver.Resolve(ctx, p)
}

func (s *Server) serveDocumentFeed(w http.ResponseWriter, r *http.Request, p documentFeedPolling) {
	if s.store == nil || s.sqlMem == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable", "the document feed requires a persistence backend and SQL Memory")
		return
	}
	scope := r.URL.Query().Get("scope")
	if scope != "user" && scope != "tenant" {
		writeJSONError(w, http.StatusBadRequest, "bad_request", builtin.ErrDocumentFeedScope.Error())
		return
	}
	release, ok := s.documentFeeds.acquire(documentFeedOwner(r.Context()), s.cfg().Env.DocumentFeedMaxStreams)
	if !ok {
		writeJSONError(w, http.StatusTooManyRequests, "too_many_streams",
			"this principal already holds the most document feed streams allowed (LOOMCYCLE_DOCUMENT_FEED_MAX_STREAMS); close one first")
		return
	}
	defer release()

	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	browse := s.substrateBrowseCtxFn(r)
	open := func(ctx context.Context) (*builtin.DocumentFeedView, error) {
		return doc.OpenDocumentFeedView(browse(ctx), scope)
	}
	view, err := open(r.Context())
	if err != nil {
		status := http.StatusForbidden
		if errors.Is(err, builtin.ErrDocumentFeedScope) {
			status = http.StatusBadRequest
		}
		writeJSONError(w, status, "scope_refused", err.Error())
		return
	}
	cursor, err := s.store.MemoryChangesHead(r.Context(), view.Tenant())
	if err != nil {
		slog.WarnContext(r.Context(), "document feed: reading the change head failed", "tenant", view.Tenant(), "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable", changeFeedStoreUnavailable)
		return
	}

	stream, ok := newSSE(w)
	if !ok {
		http.Error(w, "server does not support streaming on this transport", http.StatusInternalServerError)
		return
	}
	stream.start()
	// The operator feed's opening frame without `since`: a reader holds no cursor.
	// enabled:false means nothing is captured and the client falls back to reloading.
	stream.sendRaw("feed", map[string]any{
		"enabled":  s.changeFeedEnabled(),
		"embedder": s.embedderHealth(),
	})
	stream.startKeepalive(r.Context(), s.cfg().Env.SSEKeepaliveInterval)
	defer stream.end()

	s.tailDocumentFeed(r.Context(), stream, view, open, cursor, p)
}

// tailDocumentFeed polls the change table past cursor, sending what view lets
// through, until ctx is done. Every ttl it re-reads the caller's rules and opens
// a fresh view, so no access answer outlives the window. A rule set that can no
// longer be read, or a scope the caller may no longer open, ends the stream
// with an `error` frame: it fails closed, and the client's resync re-asks.
func (s *Server) tailDocumentFeed(ctx context.Context, stream *sse, view *builtin.DocumentFeedView,
	open func(context.Context) (*builtin.DocumentFeedView, error), cursor int64, p documentFeedPolling) {
	opened := p.now()
	delay := p.base
	failures := 0
	for {
		if p.now().Sub(opened) >= p.ttl {
			fresh, err := s.reopenDocumentFeed(ctx, open, p)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				slog.WarnContext(ctx, "document feed: the caller's access could not be re-read; closing the stream", "err", err)
				stream.sendRaw("error", map[string]string{
					"code":  "access_unavailable",
					"error": "the caller's access could not be re-checked; reconnect to resync",
				})
				return
			}
			view, opened = fresh, p.now()
		}
		err := s.drainDocumentFeed(ctx, stream, view, &cursor)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			failures++
			if failures == 1 {
				slog.WarnContext(ctx, "document feed: read failed, retrying with backoff", "after_seq", cursor, "err", err)
				stream.sendRaw("error", map[string]string{
					"code":  "store_unavailable",
					"error": changeFeedStoreUnavailable,
				})
			}
			if delay *= 2; delay > p.max {
				delay = p.max
			}
		} else {
			failures, delay = 0, p.base
		}
		if !p.sleep(ctx, delay) {
			return
		}
	}
}

// reopenDocumentFeed opens a view under the caller's CURRENT rules.
func (s *Server) reopenDocumentFeed(ctx context.Context, open func(context.Context) (*builtin.DocumentFeedView, error), p documentFeedPolling) (*builtin.DocumentFeedView, error) {
	if pr, ok := auth.PrincipalFromContext(ctx); ok {
		fresh, err := p.resolve(ctx, pr)
		if err != nil {
			return nil, err
		}
		ctx = auth.WithPrincipal(ctx, fresh)
	}
	return open(ctx)
}

// drainDocumentFeed sends everything past *cursor the view lets through, page
// by page. The cursor moves past a page only once its frames are decided: a
// page whose access check failed is read again, never sent unchecked.
func (s *Server) drainDocumentFeed(ctx context.Context, stream *sse, view *builtin.DocumentFeedView, cursor *int64) error {
	for {
		rows, err := s.store.GetMemoryChangesSince(ctx, view.Tenant(), *cursor, changeFeedBatch)
		if err != nil {
			return err
		}
		frames, err := view.Frames(ctx, rows)
		if err != nil {
			return err
		}
		for _, f := range frames {
			stream.sendRaw("change", f)
		}
		if len(rows) > 0 {
			*cursor = rows[len(rows)-1].Seq
		}
		if len(rows) < changeFeedBatch {
			return nil
		}
	}
}

// documentFeedOwner keys the per-principal stream cap. No principal (open
// mode): one shared key, the whole deployment being one operator.
func documentFeedOwner(ctx context.Context) string {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return ""
	}
	return p.TenantID + "\x00" + p.Subject
}

// documentFeedStreams counts each principal's open reader streams on this
// replica. The zero value is ready to use.
type documentFeedStreams struct {
	mu   sync.Mutex
	open map[string]int
}

// acquire takes a stream slot for owner under limit (≤ 0 = unlimited) and
// returns its release, or false when owner already holds limit streams.
func (c *documentFeedStreams) acquire(owner string, limit int) (func(), bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if limit > 0 && c.open[owner] >= limit {
		return nil, false
	}
	if c.open == nil {
		c.open = map[string]int{}
	}
	c.open[owner]++
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.open[owner]--; c.open[owner] <= 0 {
				delete(c.open, owner)
			}
		})
	}, true
}
