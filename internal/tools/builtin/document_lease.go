package builtin

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// document_lease.go — a named, expiring lease on one document scope (RFC FP §13
// R6). A pass that maintains a scope's statements must be the only one doing so,
// and the consolidator's cursor_lease cannot serve: it is one lease per memory
// target and carries the consolidation watermark, so a links pass holding it
// would stall consolidation of the same target and the other way round. A NAMED
// lease in the scope's own SQL database lets each kind of pass exclude only its
// own kind.
//
// The table lives in the per-scope DDL (docSchemaDDL), not in a store migration:
// the lease is per scope because the per-scope database is.

const documentLeasesDDL = `CREATE TABLE IF NOT EXISTS document_leases (
		name TEXT PRIMARY KEY, owner TEXT NOT NULL, expires_at BIGINT NOT NULL)`

const (
	// Mirrors cursor_lease's bounds: a runaway ttl cannot wedge a scope far into
	// the future, and the lease always expires on its own.
	defaultDocumentLeaseTTL = 60 * time.Second
	maxDocumentLeaseTTL     = time.Hour
)

var documentLeaseNameRE = regexp.MustCompile(`^[a-z0-9_.:/-]{1,64}$`)

// documentLeaseAllowed is the lease ops' grant. It fails closed: today only the
// operator plane holds a lease. RFC FP §13 R1 (the statement_methods grant) widens
// it to a code-js agent holding that grant — the lease exists for the driver that
// maintains statements, and no model-driven agent should be able to hold a pass
// off a scope for up to an hour.
func documentLeaseAllowed(ctx context.Context) bool {
	return authorIsOperatorPlane(ctx)
}

// documentLeaseOwner is the holder's identity: the run id when present, else the
// agent name. Never caller-supplied, so one run cannot release another's lease.
func documentLeaseOwner(ctx context.Context) string {
	if id := tools.RunID(ctx); id != "" {
		return id
	}
	if n := tools.AgentName(ctx); n != "" {
		return "agent:" + n
	}
	return "operator"
}

type documentLeaseResult struct {
	Name      string `json:"name"`
	Acquired  bool   `json:"acquired"`
	HeldBy    string `json:"held_by,omitempty"`
	ExpiresAt int64  `json:"expires_at"`
}

func (d *Document) leaseOp(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	name, refuse := documentLeaseInput("lease", ctx, in)
	if refuse != nil {
		return *refuse, nil
	}
	if in.TTLMs < 0 {
		return errValidation("lease: ttl_ms must not be negative", "Omit it for the default of 60 seconds."), nil
	}
	ttl := defaultDocumentLeaseTTL
	if in.TTLMs > 0 {
		ttl = time.Duration(in.TTLMs) * time.Millisecond
		if ttl > maxDocumentLeaseTTL {
			ttl = maxDocumentLeaseTTL
		}
	}
	res, err := d.acquireDocumentLease(ctx, key, name, documentLeaseOwner(ctx), time.Now(), ttl)
	if err != nil {
		return errFrom("lease: "+err.Error(), err), nil
	}
	return jsonResult(res)
}

func (d *Document) releaseOp(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	name, refuse := documentLeaseInput("release", ctx, in)
	if refuse != nil {
		return *refuse, nil
	}
	released, err := d.releaseDocumentLease(ctx, key, name, documentLeaseOwner(ctx))
	if err != nil {
		return errFrom("release: "+err.Error(), err), nil
	}
	return jsonResult(map[string]any{"name": name, "released": released})
}

func documentLeaseInput(op string, ctx context.Context, in docInput) (string, *tools.Result) {
	if !documentLeaseAllowed(ctx) {
		r := errPermission(op+": a scope lease is held only by a statement-maintaining driver or the operator",
			"Run the pass from an agent granted statement_methods, or from the console.")
		return "", &r
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		r := errValidation(op+": missing required field: name", "Name the lease, e.g. \"linker\".")
		return "", &r
	}
	if !documentLeaseNameRE.MatchString(name) {
		r := errValidation(op+": name must be 1–64 characters of a-z 0-9 _ . : / -", "")
		return "", &r
	}
	return name, nil
}

// acquireDocumentLease takes name for owner until now+ttl, or reports who holds it.
// The owner already holding it extends it. Acquisition is decided by one
// conditional UPDATE's row count, so two replicas racing for an expired lease
// cannot both win; the INSERT before it only makes sure the row exists (its
// failure under a concurrent insert of the same name is expected and ignored —
// the UPDATE decides).
//
// A lease expiring further out than maxDocumentLeaseTTL from now was not written
// by this op — the scope's SQL database is also reachable through Memory sql_exec
// by an agent holding sql_scopes — so it counts as expired: no row can hold a
// pass off a scope for longer than the cap.
func (d *Document) acquireDocumentLease(ctx context.Context, key sqlmem.ScopeKey, name, owner string, now time.Time, ttl time.Duration) (documentLeaseResult, error) {
	exp := now.Add(ttl).UnixNano()
	insErr := d.exec(ctx, key, `INSERT INTO document_leases (name, owner, expires_at)
		SELECT ?, '', 0 WHERE NOT EXISTS (SELECT 1 FROM document_leases WHERE name = ?)`, name, name)
	n, err := d.execCount(ctx, key, `UPDATE document_leases SET owner = ?, expires_at = ?
		WHERE name = ? AND (expires_at <= ? OR expires_at > ? OR owner = ?)`,
		owner, exp, name, now.UnixNano(), now.Add(maxDocumentLeaseTTL).UnixNano(), owner)
	if err != nil {
		if insErr != nil {
			return documentLeaseResult{}, insErr
		}
		return documentLeaseResult{}, err
	}
	if n == 1 {
		return documentLeaseResult{Name: name, Acquired: true, ExpiresAt: exp}, nil
	}
	res, err := d.query(ctx, key, `SELECT owner, expires_at FROM document_leases WHERE name = ?`, name)
	if err != nil {
		return documentLeaseResult{}, err
	}
	if len(res.Rows) == 0 {
		// The row is missing although nothing deletes one: the INSERT failed for a
		// reason other than a concurrent insert.
		if insErr != nil {
			return documentLeaseResult{}, insErr
		}
		return documentLeaseResult{Name: name}, nil
	}
	return documentLeaseResult{Name: name, HeldBy: asStr(res.Rows[0][0]), ExpiresAt: asInt64Val(res.Rows[0][1])}, nil
}

// releaseDocumentLease ends owner's lease on name. Releasing a lease one does not
// hold (expired and taken over, or never held) is not an error: it reports false.
func (d *Document) releaseDocumentLease(ctx context.Context, key sqlmem.ScopeKey, name, owner string) (bool, error) {
	n, err := d.execCount(ctx, key, `UPDATE document_leases SET owner = '', expires_at = 0
		WHERE name = ? AND owner = ?`, name, owner)
	return n == 1, err
}

// execCount is exec that also reports the affected row count.
func (d *Document) execCount(ctx context.Context, key sqlmem.ScopeKey, stmt string, args ...any) (int64, error) {
	res, err := d.SqlMem.Exec(ctx, key, d.SqlMem.Rebind(stmt), args, 0)
	if schemaMissing(err) && d.reensureSchema(ctx, key) == nil {
		res, err = d.SqlMem.Exec(ctx, key, d.SqlMem.Rebind(stmt), args, 0)
	}
	if err != nil || res == nil {
		return 0, err
	}
	return res.RowsAffected, nil
}
