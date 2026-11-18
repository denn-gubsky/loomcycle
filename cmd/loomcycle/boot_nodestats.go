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

// startNodeStatsSweeper starts the node statistics sweeper (RFC GM P2.1): on each
// tick, one replica walks every tenant scope with a tenant ontology and recomputes
// the taxonomy nodes whose inputs moved. It reports whether it started one.
//
// OFF BY DEFAULT: memory.node_stats must be enabled with a positive interval.
// Otherwise no goroutine starts and no lock is taken. It needs no embedder and no
// vector store: without them it keeps member counts only, as Document
// op=node_stats does. It makes no model call.
//
// audience is the access-control resolver: the pass counts only documents every
// member of the tenant may read (RFC GM C8). nil reads no rules.
func startNodeStatsSweeper(ctx context.Context, cfg *config.Config, st store.Store, mgr *sqlmem.Manager,
	emb providers.Embedder, audience builtin.MemberAudience, lock *coord.AdvisoryLock) bool {
	s := cfg.DocumentNodeStats()
	switch {
	case !s.Enabled:
		return false
	case s.Interval <= 0:
		slog.Info("node statistics: no sweeper (no interval); Document op=node_stats computes them on demand")
		return false
	case st == nil || mgr == nil:
		slog.Warn("node statistics: sweeper disabled (no SQL Memory, so no documents)")
		return false
	}
	doc := &builtin.Document{Store: st, SqlMem: mgr, Embedder: emb, Cfg: cfg, Audience: audience}
	goAdvisoryGatedSweeper(ctx, s.Interval, lock, coord.LockKeyNodeStatsSweeper, "node_stats",
		func(ctx context.Context) { nodeStatsSweep(ctx, doc, mgr) })
	slog.Info("node statistics: sweeper enabled", "interval", s.Interval)
	return true
}

// nodeStatsSweep runs one compute pass over every tenant scope. A scope without
// document tables or without an ontology is skipped before anything is written
// (NodeStatsScopeKey).
func nodeStatsSweep(ctx context.Context, doc *builtin.Document, mgr *sqlmem.Manager) {
	scopes, err := mgr.ListScopes(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "node statistics: list scopes failed", "err", err)
		return
	}
	for _, sk := range scopes {
		if sk.Scope != "tenant" {
			continue
		}
		rep, ran, err := doc.NodeStatsScopeKey(ctx, sk)
		if err != nil {
			// Per scope: one unreadable scope must not stop the sweep.
			slog.ErrorContext(ctx, "node statistics: scope pass failed", "tenant", sk.Tenant, "err", err)
			continue
		}
		if ran && (rep.Computed > 0 || rep.Removed > 0) {
			slog.InfoContext(ctx, "node statistics: recomputed nodes", "tenant", sk.Tenant,
				"computed", rep.Computed, "removed", rep.Removed, "vectors", rep.Vectors)
		}
	}
}
