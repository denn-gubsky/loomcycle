// Package fallback wraps a primary memory.Backend with a graceful
// degradation path: every op tries the primary first and, on error,
// retries on the fallback backend (the in-process default in practice).
//
// This is the runtime half of RFC I Decision 9's `fallback_on_error:
// inprocess` — the MemoryBackendDef declares the intent and the tool layer
// constructs this wrapper (primary = a remote backend, fallback = the
// operator-default in-process backend) so an agent whose remote deployment
// is unreachable keeps reading/writing memory locally instead of failing
// the run.
//
// NOTE: this package currently has NO production constructor. The only kind
// that ever wrapped itself in it was the external mem9 backend, removed once
// the in-process backend became a native memory layer. It is kept because it
// is deliberately backend-AGNOSTIC — the degradation policy lives here rather
// than inside a backend, so a remote backend reports its real error and the
// wrapper alone decides whether to degrade. Whether to keep carrying it is an
// open call for whoever adds the next external backend.
package fallback

import (
	"context"
	"encoding/json"
	"errors"

	memory "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Backend implements memory.Backend by trying primary then fallback.
//
// The fallback fires on ANY primary error. We intentionally do NOT try
// to distinguish "transient network failure" from "permanent
// misconfiguration": the whole point of fallback_on_error is graceful
// degradation, and a misconfigured remote backend that always errors
// should still let the agent keep working against local memory rather
// than failing every op. Operators see the degradation in the logs +
// OTEL spans (Decision 12) and fix the config out of band.
type Backend struct {
	primary  memory.Backend
	fallback memory.Backend
	// logf logs the degradation. NEVER receives a credential — a primary
	// backend must construct its errors without its API key (the contract
	// every backend owes this wrapper), so %v on the error is secret-free.
	logf func(format string, args ...any)
}

// New builds a fallback wrapper. logf may be nil (degradation then logs
// nothing — used by tests that assert behavior without log capture).
func New(primary, fallback memory.Backend, logf func(format string, args ...any)) *Backend {
	return &Backend{primary: primary, fallback: fallback, logf: logf}
}

// degrade logs the primary failure and reports that the fallback should
// serve. The log line never carries a secret: the err comes from the
// primary backend, which is contractually forbidden from embedding the
// API key in its errors.
func (b *Backend) degrade(op string, err error) {
	if b.logf != nil {
		b.logf("memory backend %T failed on %s, falling back to in-process: %v", b.primary, op, err)
	}
}

// Get tries primary then fallback.
func (b *Backend) Get(ctx context.Context, scope store.MemoryScope, scopeID, key string) (store.MemoryEntry, error) {
	entry, err := b.primary.Get(ctx, scope, scopeID, key)
	if err != nil {
		// A genuine "not found" from the primary is NOT a backend
		// failure — it's a valid answer. Falling back on it would mask
		// a deletion (the row is gone remotely but lingers locally).
		// Only degrade on non-NotFound errors.
		if isNotFound(err) {
			return entry, err
		}
		b.degrade("get", err)
		return b.fallback.Get(ctx, scope, scopeID, key)
	}
	return entry, nil
}

// Set tries primary then fallback.
func (b *Backend) Set(ctx context.Context, scope store.MemoryScope, scopeID, key string, value json.RawMessage, opts memory.SetOptions) (memory.SetResult, error) {
	res, err := b.primary.Set(ctx, scope, scopeID, key, value, opts)
	if err != nil {
		b.degrade("set", err)
		return b.fallback.Set(ctx, scope, scopeID, key, value, opts)
	}
	return res, nil
}

// Delete tries primary then fallback.
func (b *Backend) Delete(ctx context.Context, scope store.MemoryScope, scopeID, key string) (bool, error) {
	existed, err := b.primary.Delete(ctx, scope, scopeID, key)
	if err != nil {
		b.degrade("delete", err)
		return b.fallback.Delete(ctx, scope, scopeID, key)
	}
	return existed, nil
}

// List tries primary then fallback.
func (b *Backend) List(ctx context.Context, scope store.MemoryScope, scopeID, prefix string, limit int) ([]store.MemoryEntry, bool, error) {
	entries, truncated, err := b.primary.List(ctx, scope, scopeID, prefix, limit)
	if err != nil {
		b.degrade("list", err)
		return b.fallback.List(ctx, scope, scopeID, prefix, limit)
	}
	return entries, truncated, nil
}

// Search tries primary then fallback.
func (b *Backend) Search(ctx context.Context, scope store.MemoryScope, scopeID string, q memory.SearchQuery, rank memory.RankConfig, dedup memory.DedupConfig) (memory.SearchResult, error) {
	res, err := b.primary.Search(ctx, scope, scopeID, q, rank, dedup)
	if err != nil {
		b.degrade("search", err)
		return b.fallback.Search(ctx, scope, scopeID, q, rank, dedup)
	}
	return res, nil
}

// Stats tries primary then fallback.
func (b *Backend) Stats(ctx context.Context, scope store.MemoryScope) (store.MemoryEmbedStats, error) {
	stats, err := b.primary.Stats(ctx, scope)
	if err != nil {
		b.degrade("stats", err)
		return b.fallback.Stats(ctx, scope)
	}
	return stats, nil
}

// isNotFound reports whether err is a store.ErrNotFound. Get uses this
// to avoid masking a real "key absent" answer with the fallback's copy.
func isNotFound(err error) bool {
	var nf *store.ErrNotFound
	return errors.As(err, &nf)
}

var _ memory.Backend = (*Backend)(nil)
