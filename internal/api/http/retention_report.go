package http

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/retention"
)

// retentionReportResponse is the wire shape of GET /v1/_retention (RFC BM). It
// reports the data-retention sweeper's effective configuration plus — for an
// admin — a per-def-type count of currently-purgeable retired versions.
//
// The purgeable counts + the export_dir are ADMIN-ONLY: the counts span every
// tenant (the retention purge is an operator-global function, not tenant-scoped),
// so exposing them to a substrate:tenant operator would be a cross-tenant oracle;
// export_dir is a host filesystem path (infra detail). A tenant operator still
// sees the config knobs (so its UI can show that retention exists + how it's
// tuned), mirroring /v1/_routing's admin-vs-tenant split.
type retentionReportResponse struct {
	Admin         bool   `json:"admin"`
	Enabled       bool   `json:"enabled"`
	IntervalMS    int64  `json:"interval_ms"`
	DefsMode      string `json:"defs_mode"`
	DefsMaxAgeMS  int64  `json:"defs_max_age_ms"`
	DefsKeepLastN int    `json:"defs_keep_last_n"`
	// ChatsMode / ChatsMaxAgeMS are the RFC BM Phase 2 aged-chat archiver knobs
	// (config only — tenant-readable). ChatsMode reflects the effective value
	// after the legacy LOOMCYCLE_USAGE_RUN_RETENTION_* alias in config.Load.
	ChatsMode     string `json:"chats_mode"`
	ChatsMaxAgeMS int64  `json:"chats_max_age_ms"`
	ExportDir     string `json:"export_dir,omitempty"`
	// Purgeable is the per-def-type count of versions the CURRENT age + keep-last-N
	// settings would purge right now, plus an aged-chat-session count under the
	// "chats" key (both regardless of mode — a preview). Admin-only.
	Purgeable map[string]int `json:"purgeable,omitempty"`
}

// handleRetentionReport serves GET /v1/_retention — the read-only view of the
// RFC BM data-retention sweeper. Tenant-readable config (see requiredScopeFor);
// the purgeable counts + export_dir are stripped for a non-admin caller. 503 when
// the server has no store (an empty/disabled report).
func (s *Server) handleRetentionReport(w http.ResponseWriter, r *http.Request) {
	admin := true
	if p, ok := auth.PrincipalFromContext(r.Context()); ok {
		admin = auth.HasScope(p.Scopes, auth.ScopeAdmin)
	}

	// Effective config, applying the same fallbacks retention.New does, so the
	// report shows what the sweeper actually runs with.
	interval := s.cfg.Env.RetentionInterval
	if interval <= 0 {
		interval = time.Hour
	}
	mode := s.cfg.Env.RetentionDefsMode
	if mode == "" {
		mode = "off"
	}
	chatsMode := s.cfg.Env.RetentionChatsMode
	if chatsMode == "" {
		chatsMode = "off"
	}
	resp := retentionReportResponse{
		Admin:         admin,
		Enabled:       s.cfg.Env.RetentionEnabled,
		IntervalMS:    interval.Milliseconds(),
		DefsMode:      mode,
		DefsMaxAgeMS:  s.cfg.Env.RetentionDefsMaxAge.Milliseconds(),
		DefsKeepLastN: s.cfg.Env.RetentionDefsKeepLastN,
		ChatsMode:     chatsMode,
		ChatsMaxAgeMS: s.cfg.Env.RetentionChatsMaxAge.Milliseconds(),
	}

	if s.store == nil {
		// Read-only, degraded: report config only (no store → nothing to purge).
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	// Admin-only global detail: the export dir (infra path) + the cross-tenant
	// purgeable counts.
	if admin {
		resp.ExportDir = s.cfg.Env.RetentionExportDir
		sw := retention.New(s.store, retention.Config{
			DefsMode:      mode,
			DefsMaxAge:    s.cfg.Env.RetentionDefsMaxAge,
			DefsKeepLastN: s.cfg.Env.RetentionDefsKeepLastN,
			ChatsMode:     chatsMode,
			ChatsMaxAge:   s.cfg.Env.RetentionChatsMaxAge,
			ExportDir:     s.cfg.Env.RetentionExportDir,
		})
		counts, err := sw.DryRunCounts(r.Context())
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		resp.Purgeable = counts
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
