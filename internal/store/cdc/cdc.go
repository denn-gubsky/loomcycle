// Package cdc wraps a store.Store to emit a value-free change-data-capture row
// after each successful memory/document write (RFC CD Part C).
//
// It is applied ONLY when LOOMCYCLE_MEMORY_CHANGES_ENABLED. When the feed is
// off the raw store is used directly and this package is never in the path, so
// the default deployment pays nothing and is byte-identical to before.
//
// The decorator embeds store.Store, so every method it does NOT override is
// promoted unchanged — it overrides only the content-write methods, delegates
// to the wrapped store, and on success appends a change row. Emit is
// BEST-EFFORT: an append failure is logged, never returned, so a lagging feed
// can never fail the write it describes (the feed is advisory; a subscriber
// reconciles by re-reading via the data API).
package cdc

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// docChunkPrefix is the reserved key prefix under which Document chunk bodies
// live in the memory keyspace. Kept in sync with memory.DocumentChunkKeyPrefix
// (pinned by TestDocChunkPrefix_MatchesMemory) so this package need not import
// the whole memory package just for a constant.
const docChunkPrefix = "doc.chunk:"

// Store decorates a store.Store with CDC emission on writes.
type Store struct {
	store.Store
	logf func(format string, args ...any)
}

// Wrap returns a CDC-emitting store over base. logf may be nil (append
// failures then go unlogged — used by tests).
func Wrap(base store.Store, logf func(format string, args ...any)) *Store {
	return &Store{Store: base, logf: logf}
}

var _ store.Store = (*Store)(nil)

// CapturesChanges reports that writes through this store land in the change
// feed. It exists so a reader of the feed can be TOLD the feed is on, rather
// than inferring it from an empty stream.
//
// Asked of the store rather than re-read from LOOMCYCLE_MEMORY_CHANGES_ENABLED,
// because this decorator IS the mechanism: it is in the path exactly when the
// feed is on, so its presence cannot disagree with reality the way a second
// reading of an env var can. A plain store answers false by not implementing
// the method at all.
func (c *Store) CapturesChanges() bool { return true }

// Unwrap returns the wrapped store. Call sites that need a CONCRETE backend
// (e.g. the postgres cluster-coordination wiring type-asserts *postgres.Store)
// must unwrap through this — see cmd/loomcycle's asPostgresStore.
func (c *Store) Unwrap() store.Store { return c.Store }

func (c *Store) MemorySet(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, key string, value json.RawMessage, ttl time.Duration) error {
	err := c.Store.MemorySet(ctx, tenantID, scope, scopeID, key, value, ttl)
	if err == nil {
		c.emitKey(ctx, tenantID, scope, scopeID, key, false)
	}
	return err
}

func (c *Store) MemorySetProvenance(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, key string, value json.RawMessage, ttl time.Duration, prov store.MemoryProvenance) error {
	err := c.Store.MemorySetProvenance(ctx, tenantID, scope, scopeID, key, value, ttl, prov)
	if err == nil {
		c.emitKey(ctx, tenantID, scope, scopeID, key, false)
	}
	return err
}

func (c *Store) MemoryIncrement(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, key string, delta int64, ttl time.Duration) (int64, error) {
	v, err := c.Store.MemoryIncrement(ctx, tenantID, scope, scopeID, key, delta, ttl)
	if err == nil {
		c.emitKey(ctx, tenantID, scope, scopeID, key, false)
	}
	return v, err
}

func (c *Store) MemoryDelete(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, key string) (bool, error) {
	existed, err := c.Store.MemoryDelete(ctx, tenantID, scope, scopeID, key)
	if err == nil && existed {
		c.emitKey(ctx, tenantID, scope, scopeID, key, true)
	}
	return existed, err
}

func (c *Store) MemoryDeleteScope(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID string) (int, error) {
	n, err := c.Store.MemoryDeleteScope(ctx, tenantID, scope, scopeID)
	if err == nil && n > 0 {
		c.emit(ctx, store.MemoryChange{
			TenantID: tenantID, Type: store.MemoryChangeScopeDeleted, Scope: scope, ScopeID: scopeID,
		})
	}
	return n, err
}

func (c *Store) MemoryAtomicUpdate(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, key string, ttl time.Duration, reducer func(current json.RawMessage) (json.RawMessage, error)) (json.RawMessage, error) {
	v, err := c.Store.MemoryAtomicUpdate(ctx, tenantID, scope, scopeID, key, ttl, reducer)
	if err == nil {
		c.emitKey(ctx, tenantID, scope, scopeID, key, false)
	}
	return v, err
}

// emitKey classifies a keyed write as a document-chunk change (reserved
// doc.chunk: prefix) or a plain memory change, and appends it.
func (c *Store) emitKey(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, key string, isDelete bool) {
	ch := store.MemoryChange{TenantID: tenantID, Scope: scope, ScopeID: scopeID}
	if chunkID, ok := strings.CutPrefix(key, docChunkPrefix); ok {
		ch.ChunkID = chunkID
		if isDelete {
			ch.Type = store.DocumentChangeDeleted
		} else {
			ch.Type = store.DocumentChangeUpdated
		}
	} else {
		ch.Key = key
		if isDelete {
			ch.Type = store.MemoryChangeDelete
		} else {
			ch.Type = store.MemoryChangeSet
		}
	}
	c.emit(ctx, ch)
}

func (c *Store) emit(ctx context.Context, ch store.MemoryChange) {
	if err := c.Store.AppendMemoryChange(ctx, ch); err != nil && c.logf != nil {
		// The write already succeeded; a failed append only lags the feed.
		c.logf("cdc: append %s change failed (feed lag, write succeeded): %v", ch.Type, err)
	}
}
