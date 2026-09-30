package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Snapshot read/restore for the webhook, A2A, memory-backend and
// document-source def tables (RFC DP P3, P4).
//
// These def tables share one column set and so do their active-pointer
// tables, so each restore goes through one helper parameterised by table
// name. The names are compile-time constants below, never caller input.

const (
	tblWebhookDefs            = "webhook_defs"
	tblWebhookDefActive       = "webhook_def_active"
	tblA2AAgentDefs           = "a2a_agent_defs"
	tblA2AAgentDefActive      = "a2a_agent_def_active"
	tblA2AServerCardDefs      = "a2a_server_card_defs"
	tblA2AServerCardDefActive = "a2a_server_card_def_active"
	tblMemoryBackendDefs      = "memory_backend_defs"
	tblMemoryBackendDefActive = "memory_backend_def_active"
	tblDocSourceDefs          = "document_source_defs"
	tblDocSourceDefActive     = "document_source_def_active"
)

// snapshotDefCols is the column set the def tables share.
type snapshotDefCols struct {
	DefID, Name, ParentDefID, Description string
	Version                               int
	Definition                            json.RawMessage
	CreatedAt                             time.Time
	CreatedByAgentID, CreatedByRunID      string
	Retired, BootstrappedFromStatic       bool
	TenantID                              string
}

// snapshotActiveCols is the column set the active-pointer tables share.
type snapshotActiveCols struct {
	TenantID, Name, DefID string
	PromotedAt            time.Time
	PromotedByAgentID     string
}

// snapshotRestoreDef inserts one def keeping every column. The conflict
// target is def_id ONLY, never INSERT OR IGNORE: that would also swallow a
// DIFFERENT live row on (tenant_id, name, version) — the target's own yaml
// bootstrap — which the restore must report instead.
func (s *Store) snapshotRestoreDef(ctx context.Context, table string, r snapshotDefCols) (bool, error) {
	if r.DefID == "" || r.Name == "" {
		return false, fmt.Errorf("snapshot restore %s: def_id and name required", table)
	}
	createdNs := r.CreatedAt.UnixNano()
	if r.CreatedAt.IsZero() {
		createdNs = time.Now().UnixNano()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO `+table+`(
			def_id, name, version, parent_def_id, definition, description,
			created_at, created_by_agent_id, created_by_run_id,
			retired, bootstrapped_from_static, tenant_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(def_id) DO NOTHING`,
		r.DefID, r.Name, r.Version, nilIfEmpty(r.ParentDefID),
		string(r.Definition), nilIfEmpty(r.Description),
		createdNs, nilIfEmpty(r.CreatedByAgentID), nilIfEmpty(r.CreatedByRunID),
		boolToInt(r.Retired), boolToInt(r.BootstrappedFromStatic), r.TenantID,
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore %s: %w", table, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// snapshotRestoreActive inserts one active pointer; a live pointer on
// (tenant_id, name) stands.
func (s *Store) snapshotRestoreActive(ctx context.Context, table string, e snapshotActiveCols) (bool, error) {
	if e.Name == "" || e.DefID == "" {
		return false, fmt.Errorf("snapshot restore %s: name and def_id required", table)
	}
	promotedNs := e.PromotedAt.UnixNano()
	if e.PromotedAt.IsZero() {
		promotedNs = time.Now().UnixNano()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO `+table+`(tenant_id, name, def_id, promoted_at, promoted_by_agent_id) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, name) DO NOTHING`,
		e.TenantID, e.Name, e.DefID, promotedNs, nilIfEmpty(e.PromotedByAgentID),
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore %s: %w", table, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// snapshotReadActive returns every pointer of an active-pointer table.
func (s *Store) snapshotReadActive(ctx context.Context, table string) ([]snapshotActiveCols, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, def_id, promoted_at, promoted_by_agent_id, tenant_id FROM `+table+`
		 ORDER BY tenant_id ASC, name ASC`)
	if err != nil {
		return nil, fmt.Errorf("snapshot read %s: %w", table, err)
	}
	defer rows.Close()
	var out []snapshotActiveCols
	for rows.Next() {
		var (
			e          snapshotActiveCols
			promotedNs int64
			promoter   sql.NullString
		)
		if err := rows.Scan(&e.Name, &e.DefID, &promotedNs, &promoter, &e.TenantID); err != nil {
			return nil, fmt.Errorf("scan %s: %w", table, err)
		}
		e.PromotedAt = time.Unix(0, promotedNs)
		e.PromotedByAgentID = promoter.String
		out = append(out, e)
	}
	return out, rows.Err()
}

const snapshotDefOrder = ` ORDER BY tenant_id ASC, name ASC, version ASC`

// ---- webhook_defs ----

func (s *Store) SnapshotReadWebhookDefs(ctx context.Context) ([]store.WebhookDefRow, error) {
	rows, err := s.db.QueryContext(ctx, webhookDefSelect+snapshotDefOrder)
	if err != nil {
		return nil, fmt.Errorf("snapshot read webhook_defs: %w", err)
	}
	defer rows.Close()
	return s.scanWebhookDefRows(rows)
}

func (s *Store) SnapshotReadWebhookDefActive(ctx context.Context) ([]store.WebhookDefActiveEntry, error) {
	cols, err := s.snapshotReadActive(ctx, tblWebhookDefActive)
	if err != nil {
		return nil, err
	}
	out := make([]store.WebhookDefActiveEntry, 0, len(cols))
	for _, c := range cols {
		out = append(out, store.WebhookDefActiveEntry{Name: c.Name, DefID: c.DefID, PromotedAt: c.PromotedAt, PromotedByAgentID: c.PromotedByAgentID, TenantID: c.TenantID})
	}
	return out, nil
}

func (s *Store) SnapshotRestoreWebhookDef(ctx context.Context, r store.WebhookDefRow) (bool, error) {
	return s.snapshotRestoreDef(ctx, tblWebhookDefs, snapshotDefCols{
		DefID: r.DefID, Name: r.Name, Version: r.Version, ParentDefID: r.ParentDefID, Definition: r.Definition,
		Description: r.Description, CreatedAt: r.CreatedAt, CreatedByAgentID: r.CreatedByAgentID, CreatedByRunID: r.CreatedByRunID,
		Retired: r.Retired, BootstrappedFromStatic: r.BootstrappedFromStatic, TenantID: r.TenantID,
	})
}

func (s *Store) SnapshotRestoreWebhookDefActive(ctx context.Context, e store.WebhookDefActiveEntry) (bool, error) {
	return s.snapshotRestoreActive(ctx, tblWebhookDefActive, snapshotActiveCols{
		TenantID: e.TenantID, Name: e.Name, DefID: e.DefID, PromotedAt: e.PromotedAt, PromotedByAgentID: e.PromotedByAgentID,
	})
}

// ---- a2a_agent_defs ----

func (s *Store) SnapshotReadA2AAgentDefs(ctx context.Context) ([]store.A2AAgentDefRow, error) {
	rows, err := s.db.QueryContext(ctx, a2aAgentDefSelect+snapshotDefOrder)
	if err != nil {
		return nil, fmt.Errorf("snapshot read a2a_agent_defs: %w", err)
	}
	defer rows.Close()
	return s.scanA2AAgentDefRows(rows)
}

func (s *Store) SnapshotReadA2AAgentDefActive(ctx context.Context) ([]store.A2AAgentDefActiveEntry, error) {
	cols, err := s.snapshotReadActive(ctx, tblA2AAgentDefActive)
	if err != nil {
		return nil, err
	}
	out := make([]store.A2AAgentDefActiveEntry, 0, len(cols))
	for _, c := range cols {
		out = append(out, store.A2AAgentDefActiveEntry{Name: c.Name, DefID: c.DefID, PromotedAt: c.PromotedAt, PromotedByAgentID: c.PromotedByAgentID, TenantID: c.TenantID})
	}
	return out, nil
}

func (s *Store) SnapshotRestoreA2AAgentDef(ctx context.Context, r store.A2AAgentDefRow) (bool, error) {
	return s.snapshotRestoreDef(ctx, tblA2AAgentDefs, snapshotDefCols{
		DefID: r.DefID, Name: r.Name, Version: r.Version, ParentDefID: r.ParentDefID, Definition: r.Definition,
		Description: r.Description, CreatedAt: r.CreatedAt, CreatedByAgentID: r.CreatedByAgentID, CreatedByRunID: r.CreatedByRunID,
		Retired: r.Retired, BootstrappedFromStatic: r.BootstrappedFromStatic, TenantID: r.TenantID,
	})
}

func (s *Store) SnapshotRestoreA2AAgentDefActive(ctx context.Context, e store.A2AAgentDefActiveEntry) (bool, error) {
	return s.snapshotRestoreActive(ctx, tblA2AAgentDefActive, snapshotActiveCols{
		TenantID: e.TenantID, Name: e.Name, DefID: e.DefID, PromotedAt: e.PromotedAt, PromotedByAgentID: e.PromotedByAgentID,
	})
}

// ---- a2a_server_card_defs ----

func (s *Store) SnapshotReadA2AServerCardDefs(ctx context.Context) ([]store.A2AServerCardDefRow, error) {
	rows, err := s.db.QueryContext(ctx, a2aServerCardDefSelect+snapshotDefOrder)
	if err != nil {
		return nil, fmt.Errorf("snapshot read a2a_server_card_defs: %w", err)
	}
	defer rows.Close()
	return s.scanA2AServerCardDefRows(rows)
}

func (s *Store) SnapshotReadA2AServerCardDefActive(ctx context.Context) ([]store.A2AServerCardDefActiveEntry, error) {
	cols, err := s.snapshotReadActive(ctx, tblA2AServerCardDefActive)
	if err != nil {
		return nil, err
	}
	out := make([]store.A2AServerCardDefActiveEntry, 0, len(cols))
	for _, c := range cols {
		out = append(out, store.A2AServerCardDefActiveEntry{Name: c.Name, DefID: c.DefID, PromotedAt: c.PromotedAt, PromotedByAgentID: c.PromotedByAgentID, TenantID: c.TenantID})
	}
	return out, nil
}

func (s *Store) SnapshotRestoreA2AServerCardDef(ctx context.Context, r store.A2AServerCardDefRow) (bool, error) {
	return s.snapshotRestoreDef(ctx, tblA2AServerCardDefs, snapshotDefCols{
		DefID: r.DefID, Name: r.Name, Version: r.Version, ParentDefID: r.ParentDefID, Definition: r.Definition,
		Description: r.Description, CreatedAt: r.CreatedAt, CreatedByAgentID: r.CreatedByAgentID, CreatedByRunID: r.CreatedByRunID,
		Retired: r.Retired, BootstrappedFromStatic: r.BootstrappedFromStatic, TenantID: r.TenantID,
	})
}

func (s *Store) SnapshotRestoreA2AServerCardDefActive(ctx context.Context, e store.A2AServerCardDefActiveEntry) (bool, error) {
	return s.snapshotRestoreActive(ctx, tblA2AServerCardDefActive, snapshotActiveCols{
		TenantID: e.TenantID, Name: e.Name, DefID: e.DefID, PromotedAt: e.PromotedAt, PromotedByAgentID: e.PromotedByAgentID,
	})
}

// ---- memory_backend_defs ----

func (s *Store) SnapshotReadMemoryBackendDefs(ctx context.Context) ([]store.MemoryBackendDefRow, error) {
	rows, err := s.db.QueryContext(ctx, memoryBackendDefSelect+snapshotDefOrder)
	if err != nil {
		return nil, fmt.Errorf("snapshot read memory_backend_defs: %w", err)
	}
	defer rows.Close()
	return s.scanMemoryBackendDefRows(rows)
}

func (s *Store) SnapshotReadMemoryBackendDefActive(ctx context.Context) ([]store.MemoryBackendDefActiveEntry, error) {
	cols, err := s.snapshotReadActive(ctx, tblMemoryBackendDefActive)
	if err != nil {
		return nil, err
	}
	out := make([]store.MemoryBackendDefActiveEntry, 0, len(cols))
	for _, c := range cols {
		out = append(out, store.MemoryBackendDefActiveEntry{Name: c.Name, DefID: c.DefID, PromotedAt: c.PromotedAt, PromotedByAgentID: c.PromotedByAgentID, TenantID: c.TenantID})
	}
	return out, nil
}

func (s *Store) SnapshotRestoreMemoryBackendDef(ctx context.Context, r store.MemoryBackendDefRow) (bool, error) {
	return s.snapshotRestoreDef(ctx, tblMemoryBackendDefs, snapshotDefCols{
		DefID: r.DefID, Name: r.Name, Version: r.Version, ParentDefID: r.ParentDefID, Definition: r.Definition,
		Description: r.Description, CreatedAt: r.CreatedAt, CreatedByAgentID: r.CreatedByAgentID, CreatedByRunID: r.CreatedByRunID,
		Retired: r.Retired, BootstrappedFromStatic: r.BootstrappedFromStatic, TenantID: r.TenantID,
	})
}

func (s *Store) SnapshotRestoreMemoryBackendDefActive(ctx context.Context, e store.MemoryBackendDefActiveEntry) (bool, error) {
	return s.snapshotRestoreActive(ctx, tblMemoryBackendDefActive, snapshotActiveCols{
		TenantID: e.TenantID, Name: e.Name, DefID: e.DefID, PromotedAt: e.PromotedAt, PromotedByAgentID: e.PromotedByAgentID,
	})
}

// ---- document_source_defs ----

func (s *Store) SnapshotReadDocumentSourceDefs(ctx context.Context) ([]store.DocumentSourceDefRow, error) {
	rows, err := s.db.QueryContext(ctx, documentSourceDefSelect+snapshotDefOrder)
	if err != nil {
		return nil, fmt.Errorf("snapshot read document_source_defs: %w", err)
	}
	defer rows.Close()
	return s.scanDocumentSourceDefRows(rows)
}

func (s *Store) SnapshotReadDocumentSourceDefActive(ctx context.Context) ([]store.DocumentSourceDefActiveEntry, error) {
	cols, err := s.snapshotReadActive(ctx, tblDocSourceDefActive)
	if err != nil {
		return nil, err
	}
	out := make([]store.DocumentSourceDefActiveEntry, 0, len(cols))
	for _, c := range cols {
		out = append(out, store.DocumentSourceDefActiveEntry{Name: c.Name, DefID: c.DefID, PromotedAt: c.PromotedAt, PromotedByAgentID: c.PromotedByAgentID, TenantID: c.TenantID})
	}
	return out, nil
}

func (s *Store) SnapshotRestoreDocumentSourceDef(ctx context.Context, r store.DocumentSourceDefRow) (bool, error) {
	return s.snapshotRestoreDef(ctx, tblDocSourceDefs, snapshotDefCols{
		DefID: r.DefID, Name: r.Name, Version: r.Version, ParentDefID: r.ParentDefID, Definition: r.Definition,
		Description: r.Description, CreatedAt: r.CreatedAt, CreatedByAgentID: r.CreatedByAgentID, CreatedByRunID: r.CreatedByRunID,
		Retired: r.Retired, BootstrappedFromStatic: r.BootstrappedFromStatic, TenantID: r.TenantID,
	})
}

func (s *Store) SnapshotRestoreDocumentSourceDefActive(ctx context.Context, e store.DocumentSourceDefActiveEntry) (bool, error) {
	return s.snapshotRestoreActive(ctx, tblDocSourceDefActive, snapshotActiveCols{
		TenantID: e.TenantID, Name: e.Name, DefID: e.DefID, PromotedAt: e.PromotedAt, PromotedByAgentID: e.PromotedByAgentID,
	})
}
