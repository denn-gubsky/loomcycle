package builtin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A run-triggering def (ScheduleDef, WebhookDef) carries TWO tenants: the
// row's OWNING tenant (always the author's, stamped from the principal) and
// the EXECUTION tenant inside the definition JSON — the tenant the fired run
// resolves its agents / skills / MCP servers in and writes its memory and runs
// into. The execution tenant used to be settable from the overlay by any
// author (and inherited from any forkable parent), so a tenant operator could
// author a trigger whose runs execute inside ANOTHER tenant. Only an admin may
// do that; everyone else's triggers execute in their own tenant.

// refuseForeignExecTenant returns a refusal when a non-admin caller would store
// a def whose runs execute in a tenant other than its own. execTenant is the
// MERGED value (overlay over parent, after the empty-means-mine default), so
// one check covers an overlay that names another tenant and a fork that
// inherits one from its parent. Refusing the inherited case (rather than
// silently re-stamping it to the forker's tenant) keeps a fork from quietly
// changing where the parent's runs execute; the caller re-homes it explicitly
// by setting tenant_id to its own tenant.
//
// An empty execTenant passes: create and fork stamp the author's tenant over
// it before this check, so it reaches here only on a schedule hook edit of a
// row written before that stamp existed, which the edit carries over as is.
func refuseForeignExecTenant(ctx context.Context, op, execTenant string) (tools.Result, bool) {
	own := tools.RunIdentity(ctx).TenantID
	if execTenant == "" || execTenant == own || defCallerIsAdmin(ctx) {
		return tools.Result{}, false
	}
	// The foreign tenant's name is deliberately not echoed: on an inherited
	// fork it comes from a parent the caller may not be allowed to read.
	msg := fmt.Sprintf("%s: the definition's tenant_id names another tenant (set in the overlay, or inherited from the parent on fork) — only an admin can make a trigger's runs execute outside the author's own tenant", op)
	fix := "omit tenant_id, and fork a definition whose runs execute in another tenant only as an admin"
	if own != "" {
		fix = fmt.Sprintf("omit tenant_id (runs then execute in your tenant) or set it to %q, your own tenant", own)
	}
	return errValidation(msg, fix), true
}

// execTenantRow is the part of a stored trigger def version the audit reads.
type execTenantRow struct {
	defID, name, owningTenant string
	version                   int
	definition                []byte
	bootstrapped              bool
}

// foreignExecTenantWarnings returns one line per stored version whose runs
// execute in a tenant other than the one that owns the row. Before the
// non-admin guard any author could write such a row, so each one is a row an
// operator should confirm an admin meant; nothing is changed. Skipped: rows
// bootstrapped from the operator's yaml (the yaml may name any tenant), and an
// empty execution tenant (a row written before create stamped the author's
// tenant, which no overlay can produce).
func foreignExecTenantWarnings(kind string, rows []execTenantRow, active map[string]bool) []string {
	var out []string
	for _, r := range rows {
		if r.bootstrapped {
			continue
		}
		var body struct {
			TenantID string `json:"tenant_id"`
		}
		if json.Unmarshal(r.definition, &body) != nil || body.TenantID == "" || body.TenantID == r.owningTenant {
			continue
		}
		state := "inactive"
		if active[r.defID] {
			state = "ACTIVE"
		}
		out = append(out, fmt.Sprintf("%s %q v%d (def_id %s, %s) is owned by tenant %q but its runs execute in tenant %q — confirm an admin authored it, else retire it",
			kind, r.name, r.version, r.defID, state, r.owningTenant, body.TenantID))
	}
	return out
}

// ForeignExecTenantWarnings audits every stored WebhookDef version (all
// tenants, read-only); see foreignExecTenantWarnings.
func (s *WebhookDef) ForeignExecTenantWarnings(ctx context.Context) ([]string, error) {
	names, err := s.Store.WebhookDefListNames(ctx)
	if err != nil {
		return nil, err
	}
	active := map[string]bool{}
	seen := map[string]bool{}
	var rows []execTenantRow
	for _, n := range names {
		active[n.ActiveDefID] = true
		if seen[n.Name] {
			continue // ListByName already returns every tenant's versions
		}
		seen[n.Name] = true
		versions, err := s.Store.WebhookDefListByName(ctx, n.Name)
		if err != nil {
			return nil, err
		}
		for _, v := range versions {
			rows = append(rows, execTenantRow{v.DefID, v.Name, v.TenantID, v.Version, v.Definition, v.BootstrappedFromStatic})
		}
	}
	return foreignExecTenantWarnings("webhook", rows, active), nil
}

// ForeignExecTenantWarnings audits every stored ScheduleDef version (all
// tenants, read-only); see foreignExecTenantWarnings.
func (s *ScheduleDef) ForeignExecTenantWarnings(ctx context.Context) ([]string, error) {
	names, err := s.Store.ScheduleDefListNames(ctx)
	if err != nil {
		return nil, err
	}
	active := map[string]bool{}
	seen := map[string]bool{}
	var rows []execTenantRow
	for _, n := range names {
		active[n.ActiveDefID] = true
		if seen[n.Name] {
			continue // ListByName already returns every tenant's versions
		}
		seen[n.Name] = true
		versions, err := s.Store.ScheduleDefListByName(ctx, n.Name)
		if err != nil {
			return nil, err
		}
		for _, v := range versions {
			rows = append(rows, execTenantRow{v.DefID, v.Name, v.TenantID, v.Version, v.Definition, v.BootstrappedFromStatic})
		}
	}
	return foreignExecTenantWarnings("schedule", rows, active), nil
}
