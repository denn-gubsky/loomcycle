package providers

// embedder_observed.go — an Embedder decorator that records what happened, so a
// reader can be TOLD the embedder is failing rather than inferring it from
// results that look fine.
//
// WHY THIS IS NEEDED AT ALL, and it is not the obvious reason. An embedding
// failure on a content write is deliberately NOT fatal: the chunk body is stored
// and the embedding is skipped with one log line, because losing an author's text
// to an unreachable embedder would be far worse than losing its searchability
// (see internal/tools/builtin/document.go writeBody, and the admin re-embed
// surface that exists to recover such a scope). The consequence is that an
// embedder outage is INVISIBLE at the call site: every write succeeds, search
// quietly stops finding the rows written during the outage, and the only trace is
// a server log line an operator is not watching.
//
// So the thing worth reporting is not "can I reach the embedder" — a probe would
// cost a model call per reader and still only describe one instant. It is what
// the embedder ACTUALLY DID for the traffic that already went through it. Asking
// the decorator is free at read time and cannot disagree with reality, the same
// argument as cdc.Store.CapturesChanges().

import (
	"context"
	"errors"
	"sync"
	"time"
)

// EmbedderState is the coarse answer a reader acts on.
type EmbedderState string

const (
	// EmbedderAbsent — no embedder is configured. Nothing will ever be embedded,
	// and no amount of waiting changes that.
	EmbedderAbsent EmbedderState = "absent"
	// EmbedderUntried — configured, but nothing has been embedded since boot. We
	// genuinely do not know whether it works. Reported as its own state rather
	// than folded into "ok", because claiming health we have not observed is the
	// same class of lie as a disabled change feed reading as a quiet one.
	EmbedderUntried EmbedderState = "untried"
	// EmbedderOK — the most recent call succeeded.
	EmbedderOK EmbedderState = "ok"
	// EmbedderFailing — the most recent call failed. Rows written since then are
	// stored but unsearchable; `backfill_embeddings` is the recovery.
	EmbedderFailing EmbedderState = "failing"
)

// EmbedderHealth is what an observed embedder reports. Counts are SINCE BOOT and
// process-local — this is an operator hint, not an SLO.
//
// Provider and Model are included because the capabilities report already
// discloses exactly those (provider/model/dimension, "none of the three is a
// secret or an address"). The base URL is NOT here and must not be added: it maps
// the operator's network. Neither is the error TEXT — a dial error carries an
// address — so failures are reported as a classified kind.
type EmbedderHealth struct {
	State        EmbedderState `json:"state"`
	Provider     string        `json:"provider,omitempty"`
	Model        string        `json:"model,omitempty"`
	Calls        uint64        `json:"calls"`
	Failures     uint64        `json:"failures"`
	LastFailKind string        `json:"last_failure_kind,omitempty"`
	// LastOKUnix / LastFailUnix are seconds, 0 when never. A reader shows "failing
	// for 20 minutes" from these; an absolute stamp travels better than an age
	// computed against the server's clock.
	LastOKUnix   int64 `json:"last_ok_unix,omitempty"`
	LastFailUnix int64 `json:"last_failure_unix,omitempty"`
}

// ObservedEmbedder wraps an Embedder and records each call's outcome.
type ObservedEmbedder struct {
	inner Embedder

	mu       sync.Mutex
	calls    uint64
	failures uint64
	// lastFailed is the OUTCOME OF THE MOST RECENT CALL, recorded rather than
	// derived. Deriving it from `lastFail.After(lastOK)` looks equivalent and is
	// not: two calls inside one clock tick make the comparison false, so a failure
	// immediately after a success reports as ok — which is precisely the unchecked
	// claim this type exists to prevent. It showed up as a test that passed alone
	// and failed in the full suite, i.e. as flake, which is how a clock-derived
	// state usually announces itself.
	lastFailed   bool
	lastOK       time.Time
	lastFail     time.Time
	lastFailKind string
}

var _ Embedder = (*ObservedEmbedder)(nil)

// ObserveEmbedder decorates e. A nil embedder returns nil so the caller's
// "no embedder configured" checks (`if d.Embedder == nil`) keep working — wrapping
// nil into a non-nil interface holding a nil pointer is exactly how that check
// silently stops firing.
func ObserveEmbedder(e Embedder) *ObservedEmbedder {
	if e == nil {
		return nil
	}
	return &ObservedEmbedder{inner: e}
}

func (o *ObservedEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	vecs, err := o.inner.Embed(ctx, texts)
	o.mu.Lock()
	o.calls++
	if err != nil {
		o.failures++
		o.lastFailed = true
		o.lastFail = time.Now().UTC()
		o.lastFailKind = classifyEmbedError(err)
	} else {
		o.lastFailed = false
		o.lastOK = time.Now().UTC()
	}
	o.mu.Unlock()
	return vecs, err
}

func (o *ObservedEmbedder) Model() string    { return o.inner.Model() }
func (o *ObservedEmbedder) Provider() string { return o.inner.Provider() }
func (o *ObservedEmbedder) Dimension() int   { return o.inner.Dimension() }

// Unwrap exposes the underlying embedder for a call site that type-asserts a
// concrete driver. Mirrors cdc.Store.Unwrap.
func (o *ObservedEmbedder) Unwrap() Embedder { return o.inner }

// Health reports the observed state.
func (o *ObservedEmbedder) Health() EmbedderHealth {
	o.mu.Lock()
	defer o.mu.Unlock()
	h := EmbedderHealth{
		Provider:     o.inner.Provider(),
		Model:        o.inner.Model(),
		Calls:        o.calls,
		Failures:     o.failures,
		LastFailKind: o.lastFailKind,
	}
	if !o.lastOK.IsZero() {
		h.LastOKUnix = o.lastOK.Unix()
	}
	if !o.lastFail.IsZero() {
		h.LastFailUnix = o.lastFail.Unix()
	}
	switch {
	case o.calls == 0:
		h.State = EmbedderUntried
	case o.lastFailed:
		h.State = EmbedderFailing
	default:
		h.State = EmbedderOK
	}
	return h
}

// classifyEmbedError buckets a failure WITHOUT quoting it. The error text is the
// one thing that cannot be forwarded: a transport failure reads like
// `dial tcp 192.168.0.77:11434: connect: connection refused`, which is a map of
// the operator's network — the same reason the capabilities report withholds the
// base URL.
//
// Classified by ERRORS.IS where a sentinel exists, and left as "other" where one
// does not, rather than sniffing substrings. A wrong bucket derived from string
// matching would be a confident wrong answer, and "other" is an honest one. The
// store's ErrEmbedderNotImplemented is deliberately NOT special-cased: it lives in
// internal/store, this package must not depend on it, and an embedder that cannot
// embed fails every call and therefore already reports as failing.
func classifyEmbedError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "other"
	}
}
