package http

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
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

// documentFeedPolling is a reader stream's timing and its credential re-check.
// Injected so a test can drive the access window without waiting on it.
type documentFeedPolling struct {
	changeFeedPolling
	ttl time.Duration
	now func() time.Time
	// reauth authenticates the stream's request again, as every request is
	// authenticated, and returns the principal with its CURRENT rules.
	reauth func(*http.Request) (auth.Principal, error)
	// expiry is when the request's credential stops being valid (zero: no
	// fixed end), which the stream does not outlive.
	expiry func(*http.Request) time.Time
}

// The ways a re-check ends a stream. Each is sent as an `error` frame code.
var (
	errFeedCredentialRevoked = errors.New("credential_revoked")
	errFeedAccessUnavailable = errors.New("access_unavailable")
)

func (s *Server) handleDocumentFeed(w http.ResponseWriter, r *http.Request) {
	s.serveDocumentFeed(w, r, s.defaultDocumentFeedPolling())
}

func (s *Server) defaultDocumentFeedPolling() documentFeedPolling {
	return documentFeedPolling{
		changeFeedPolling: changeFeedPolling{base: memoryChangesPollInterval, max: memoryChangesMaxBackoff, sleep: sleepCtx},
		ttl:               documentFeedAccessTTL,
		now:               time.Now,
		reauth:            s.reauthenticateFeed,
		expiry:            s.credentialExpiry,
	}
}

// reauthenticateFeed is authMiddleware's admission, asked again of a stream's
// request: the credential still resolves (unrevoked, unexpired, its user
// active and approved), the route still admits it, and its route question
// still passes under its CURRENT rules. An ACL set that cannot be read fails
// closed here, unlike enrichPrincipal's per-request fallback: a stream that
// cannot re-check its reader stops rather than keep sending.
func (s *Server) reauthenticateFeed(r *http.Request) (auth.Principal, error) {
	ctx := r.Context()
	p, ui, ok := s.authenticateRequest(r)
	if !ok || (ui != nil && ui.pending) || !RouteAdmits(p, r.Method, r.URL.Path) {
		return auth.Principal{}, errFeedCredentialRevoked
	}
	if s.aclResolver != nil {
		var err error
		if p, err = s.aclResolver.Resolve(ctx, p); err != nil {
			return auth.Principal{}, fmt.Errorf("%w: %v", errFeedAccessUnavailable, err)
		}
	}
	if surface, governed := authz.HTTPSurface(r.Method, r.URL.Path); governed {
		res := authz.EvaluateSurface(ctx, p.Effective, surface, true)
		if res.Unavailable {
			return auth.Principal{}, errFeedAccessUnavailable
		}
		if s.gateReply(ctx, p, surface, res) != nil {
			return auth.Principal{}, errFeedCredentialRevoked
		}
	}
	return p, nil
}

// credentialExpiry is when the request's bearer token stops being valid — its
// fixed lifetime, or the end of a retirement's grace window — zero for a
// credential with no fixed end (a session, the legacy token, open mode), whose
// end the periodic re-check finds instead.
func (s *Server) credentialExpiry(r *http.Request) time.Time {
	bearer, ok := extractBearer(r, s.cfg().ResolvedAuth.Strict())
	if !ok || s.store == nil {
		return time.Time{}
	}
	h, err := s.hasher()
	if err != nil {
		return time.Time{}
	}
	row, err := s.store.OperatorTokenDefGetByTokenHash(r.Context(), h.Hash(bearer))
	if err != nil || (row.ExpiresAt.IsZero() && row.RetiredAt.IsZero()) {
		return time.Time{}
	}
	return row.ValidUntil()
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

	// Only a request that carries a principal has a credential to outlive.
	var deadline time.Time
	if _, ok := auth.PrincipalFromContext(r.Context()); ok {
		deadline = p.expiry(r)
	}

	stream, ok := newSSE(w)
	if !ok {
		http.Error(w, "server does not support streaming on this transport", http.StatusInternalServerError)
		return
	}
	stream.start()
	// Only whether changes are captured: enabled:false means the client falls
	// back to reloading. Not `since` (a reader holds no cursor), and not the
	// operator feed's embedder health — provider, model and failure counts are
	// the deployment's infrastructure state, not a member's to read.
	stream.sendRaw("feed", map[string]any{"enabled": s.changeFeedEnabled()})
	stream.startKeepalive(r.Context(), s.cfg().Env.SSEKeepaliveInterval)
	defer stream.end()

	s.tailDocumentFeed(r, stream, view, open, cursor, deadline, p)
}

// tailDocumentFeed polls the change table past cursor, sending what view lets
// through, until the request's ctx is done. Every ttl it authenticates the
// request again and opens a fresh view under the caller's current rules, so no
// credential or access answer outlives the window, and it ends at deadline
// (the credential's expiry; zero: none). Each of those ends the stream with an
// `error` frame — it fails closed, and the client's resync re-asks.
func (s *Server) tailDocumentFeed(r *http.Request, stream *sse, view *builtin.DocumentFeedView,
	open func(context.Context) (*builtin.DocumentFeedView, error), cursor int64, deadline time.Time, p documentFeedPolling) {
	ctx := r.Context()
	opened := p.now()
	delay := p.base
	failures := 0
	for {
		if !deadline.IsZero() && !p.now().Before(deadline) {
			stream.sendRaw("error", map[string]string{
				"code":  "credential_expired",
				"error": "the credential this stream was opened with has expired",
			})
			return
		}
		if p.now().Sub(opened) >= p.ttl {
			fresh, err := s.reopenDocumentFeed(r, open, p)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				code := errFeedAccessUnavailable.Error()
				msg := "the caller's access could not be re-checked; reconnect to resync"
				if errors.Is(err, errFeedCredentialRevoked) {
					code, msg = errFeedCredentialRevoked.Error(), "the credential this stream was opened with is no longer valid"
				}
				slog.WarnContext(ctx, "document feed: the re-check refused the stream; closing it", "code", code, "err", err)
				stream.sendRaw("error", map[string]string{"code": code, "error": msg})
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

// reopenDocumentFeed authenticates the stream's request again and opens a view
// under the result. Open mode (no principal) has no credential to re-check.
func (s *Server) reopenDocumentFeed(r *http.Request, open func(context.Context) (*builtin.DocumentFeedView, error), p documentFeedPolling) (*builtin.DocumentFeedView, error) {
	ctx := r.Context()
	if _, ok := auth.PrincipalFromContext(ctx); ok {
		fresh, err := p.reauth(r)
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
