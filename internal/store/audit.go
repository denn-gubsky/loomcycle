package store

import (
	"context"
	"time"
)

// AuditEventRow is one row of the store-backed audit stream (RFC FZ Part E):
// a per-tenant hash chain every replica appends to. The store persists the
// row and serializes the append; the hashing and signing live in package
// audit, which hands the store a seal function to run inside the append.
type AuditEventRow struct {
	// TenantID names the chain. "" is the operator chain: events that belong
	// to no tenant (an operator-plane change with no tenant target).
	TenantID string
	// Seq is the row's position in its tenant's chain, from 1, with no gaps.
	Seq int64
	// TS is when the event was recorded, kept to the nanosecond on both
	// backends so the hash reproduces exactly.
	TS      time.Time
	Actor   string
	Action  string
	Subject string
	// Payload is the whole audit.Event as JSON.
	Payload string
	// PrevHash is the Hash of the row before (the anchor's after a prune);
	// "" for the first row of a chain.
	PrevHash string
	Hash     string
	// MAC signs Hash under the deployment's audit key, so a database writer
	// without the key cannot recompute a chain over rewritten rows.
	MAC string
}

// AuditHead is a tenant chain's head: where the next row goes, and the anchor
// a retention prune leaves behind so the remaining rows still verify.
type AuditHead struct {
	TenantID string
	// Seq / Hash / MAC are the last appended row's position and hash, and a
	// MAC over them in the head's own domain.
	Seq  int64
	Hash string
	MAC  string
	// AnchorSeq / AnchorHash are the last pruned row's position and hash (0
	// and "" when nothing was pruned); AnchorMAC signs them, so removing old
	// rows without the key reads as a gap rather than a prune.
	AnchorSeq  int64
	AnchorHash string
	AnchorMAC  string
	// AnchorState is what the pruned rows held that verification still
	// needs, carried forward by each prune and signed with the anchor: for
	// the operator chain, which tenant chains were opened before it.
	AnchorState string
}

// AuditSealFunc fills row.Hash and row.MAC for a row whose Seq and PrevHash
// the store has set from the locked head, and returns the head's new MAC. The
// store calls it inside the append transaction, so what it signs is exactly
// what is written.
type AuditSealFunc func(row *AuditEventRow) (headMAC string, err error)

// AuditAnchorSealFunc returns the state and MAC of the anchor a prune leaves
// at (seq, hash), given the previous anchor's state and the pruned rows whose
// action is the prune's carryAction.
type AuditAnchorSealFunc func(tenantID string, seq int64, hash, prevState string, carried []AuditEventRow) (state, mac string)

// AuditStore persists the audit stream.
type AuditStore interface {
	// AuditAppend appends row to its tenant's chain. Within one transaction
	// it locks the tenant's head (creating it on the first append), sets
	// row.Seq = head.Seq+1 and row.PrevHash = head.Hash, calls seal, inserts
	// the row and moves the head. Two appenders on one tenant — on one
	// replica or two — are serialized by the head lock, so the chain never
	// forks. It returns the row as written.
	//
	// When this append creates the tenant's head and opened is non-nil,
	// opened is appended first, in the same transaction, to the chain it
	// names (the operator chain): a signed record that this tenant's chain
	// exists, so deleting the chain outright reads as a break rather than as
	// a tenant that never had one. The tenant's head is always locked before
	// the operator chain's, so two such appends cannot deadlock.
	AuditAppend(ctx context.Context, row AuditEventRow, seal AuditSealFunc, opened *AuditEventRow) (AuditEventRow, error)
	// AuditHeadGet returns a tenant's head, or *ErrNotFound when the tenant
	// has no chain.
	AuditHeadGet(ctx context.Context, tenantID string) (AuditHead, error)
	// AuditEventsList returns a tenant's rows with Seq > afterSeq, ascending
	// by Seq, at most limit (a non-positive limit takes a default).
	AuditEventsList(ctx context.Context, tenantID string, afterSeq int64, limit int) ([]AuditEventRow, error)
	// AuditTenants lists every tenant that has a chain, sorted.
	AuditTenants(ctx context.Context) ([]string, error)
	// AuditPrune removes a tenant's rows up to the last one recorded before
	// `before` (rows are numbered in commit order and stamped by each
	// replica's clock, so a few rows newer than `before` can go with them),
	// and moves the head's anchor to that row, in one transaction under the
	// head lock. seal is handed the pruned rows whose action is carryAction
	// ("" hands none) to carry forward in the anchor's state. It returns the
	// rows removed.
	AuditPrune(ctx context.Context, tenantID string, before time.Time, carryAction string, seal AuditAnchorSealFunc) (int, error)
}
