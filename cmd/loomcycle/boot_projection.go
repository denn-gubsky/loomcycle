package main

import (
	"context"
	"log/slog"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/coord"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// startProjectionSweeper starts the document projection sweeper (RFC FQ P2): on
// each tick, one replica walks every scope with document chunk vectors, fits its
// 3D map where due and places the chunks still missing from it. It reports whether
// it started one.
//
// OFF BY DEFAULT: memory.projection must be enabled with a positive interval (or
// LOOMCYCLE_DOCUMENT_PROJECTION_INTERVAL set). Otherwise no goroutine starts and no
// lock is taken. The pass makes no model call — it reads stored vectors — so it is
// bounded by its own place limit, not by provider lanes.
func startProjectionSweeper(ctx context.Context, cfg *config.Config, st store.Store, mgr *sqlmem.Manager,
	emb providers.Embedder, lock *coord.AdvisoryLock) bool {
	s := cfg.DocumentProjection()
	switch {
	case !s.Enabled:
		return false
	case s.Interval <= 0:
		slog.Info("document projection: no sweeper (no interval); POST /v1/_document/project places chunks on demand")
		return false
	case st == nil || mgr == nil:
		slog.Warn("document projection: sweeper disabled (no SQL Memory, so no documents to place)")
		return false
	case emb == nil:
		slog.Warn("document projection: sweeper disabled (no memory.embedder, so no model whose vectors to place)")
		return false
	case !st.SupportsVectors():
		slog.Warn("document projection: sweeper disabled (the store has no vector index)")
		return false
	}
	doc := &builtin.Document{Store: st, SqlMem: mgr, Embedder: emb, Cfg: cfg}
	go runAdvisoryGatedSweeper(ctx, s.Interval, lock, coord.LockKeyProjectionSweeper, "projection",
		func(ctx context.Context) { projectionSweep(ctx, doc, mgr) })
	slog.Info("document projection: sweeper enabled", "interval", s.Interval)
	return true
}

// projectionSweep runs one pass over every durable scope. A scope with no chunk
// vectors is skipped before its SQL is touched (ProjectScopeKey).
func projectionSweep(ctx context.Context, doc *builtin.Document, mgr *sqlmem.Manager) {
	scopes, err := mgr.ListScopes(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "document projection: list scopes failed", "err", err)
		return
	}
	for _, sk := range scopes {
		// A run scope is dropped whole at run end: a map of it has no viewer.
		ms, ok := deadLinkMemoryScope(sk.Scope)
		if !ok {
			continue
		}
		rep, err := doc.ProjectScopeKey(ctx, sk, ms, builtin.ProjectionOptions{})
		if err != nil {
			// Per scope: one unreadable scope must not stop the sweep.
			slog.ErrorContext(ctx, "document projection: scope pass failed",
				"tenant", sk.Tenant, "scope", sk.Scope, "scope_id", sk.ScopeID, "err", err)
			continue
		}
		if rep.Placed > 0 || rep.Refitted {
			slog.InfoContext(ctx, "document projection: placed chunks", "tenant", sk.Tenant, "scope", sk.Scope,
				"scope_id", sk.ScopeID, "model", rep.Model, "basis_version", rep.BasisVersion,
				"placed", rep.Placed, "refitted", rep.Refitted, "more", rep.More)
		}
	}
}
