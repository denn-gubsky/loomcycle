package builtin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A run-triggering def also carries the USER its runs execute as: a schedule's
// user_id, or a webhook's user_id mapping (each delivery names its own). That
// run reads and writes that user's memory, documents and history, so naming
// another user is acting as them. A member (auth.OwnRowsOnly, isolated or not)
// reads and acts on only its own conversations, so the triggers it writes run
// as itself; a tenant operator, an admin and the unauthenticated operator may
// name any user of their tenant.

// refuseRunAsAnotherUser returns a refusal when a member would store a
// schedule whose runs execute as someone other than itself. userID is the
// MERGED value (overlay over parent), so one check covers an overlay that names
// another user and a fork that inherits one. With defaultToCaller (create and
// fork), an unset user_id becomes the member's own subject, so a member's
// schedule runs as the member; without it (a hook edit, which re-mints the
// parent's body) an unset user_id — a tenant-level run — is refused too, since
// that run is not the member's either.
func refuseRunAsAnotherUser(ctx context.Context, op string, userID *string, defaultToCaller bool) (tools.Result, bool) {
	self, tenantLevelOK, confined := triggerRunUser(ctx)
	if !confined {
		return tools.Result{}, false
	}
	if *userID == "" && defaultToCaller && self != "" {
		*userID = self
	}
	// Only an ownerless tenant-level run (self "") may keep writing
	// tenant-level schedules; a member, on or off a run, owns none.
	if *userID == self && (self != "" || tenantLevelOK) {
		return tools.Result{}, false
	}
	if defaultToCaller {
		return errValidation(
			fmt.Sprintf("%s: the schedule's user_id names another user (set in the overlay, or inherited from the parent on fork) — a schedule you write runs as you", op),
			"omit user_id, or set it to your own user id; a tenant operator can schedule runs as another user"), true
	}
	return errValidation(
		fmt.Sprintf("%s: this schedule's runs do not execute as you, so only a tenant operator may change it", op),
		"ask a tenant operator, or create a schedule of your own"), true
}

// triggerRunUser reports whether the caller may write only triggers whose runs
// execute as one user (confined), and which user that is (self).
//
// A principal on ctx decides it: a member (auth.OwnRowsOnly) is held to its
// own subject. With no principal, an off-run call is the unauthenticated
// operator. A run with no principal (a scheduler fire, a webhook delivery, a
// resumed run, an A2A run) is held to the user its recorded owner IS when that
// owner is a member or isolated, whatever user the run itself executes as: a
// member's schedule stored before this rule may run as a colleague, and its
// runs must not inherit that reach. A tenant operator's or an admin's run is
// not held. A run whose owner is not recorded is held to its own user, and
// only such a run with no user (tenantLevelOK) may write tenant-level
// triggers: an agent inside a run is never the operator, whoever started it.
func triggerRunUser(ctx context.Context) (self string, tenantLevelOK, confined bool) {
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		return p.Subject, false, auth.OwnRowsOnly(p, ok)
	}
	if tools.RunID(ctx) == "" {
		return "", false, false
	}
	if acl := tools.PolicyOf(ctx).ACL; acl != nil {
		if aclres.IsOperatorClass(acl.Subject.Class) {
			return "", false, false
		}
		return acl.Subject.ID, false, true
	}
	return tools.RunIdentity(ctx).UserID, true, true
}

// refuseMappedRunUser returns a refusal when a member would store a webhook
// that maps user_id: every delivery would then run as whatever user its body
// names, and the member, holding the webhook's secret, chooses the body.
func refuseMappedRunUser(ctx context.Context, op string, mapping map[string]string) (tools.Result, bool) {
	if _, mapped := mapping["user_id"]; !mapped {
		return tools.Result{}, false
	}
	if _, _, confined := triggerRunUser(ctx); !confined {
		return tools.Result{}, false
	}
	return errValidation(
		fmt.Sprintf("%s: payload_mapping maps user_id (set in the overlay, or inherited from the parent on fork), so each delivery would run as the user its body names — only a tenant operator may write such a webhook", op),
		"remove the user_id mapping; a tenant operator can author a webhook whose deliveries run as their users"), true
}

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
//
// "Admin" here is operator authority (callerHasOperatorAuthority): an open-mode
// or stdio operator authenticates nobody, so it has no principal to carry
// substrate:admin, and is still the operator.
func refuseForeignExecTenant(ctx context.Context, op, execTenant string) (tools.Result, bool) {
	own := tools.RunIdentity(ctx).TenantID
	if execTenant == "" || execTenant == own || callerHasOperatorAuthority(ctx) {
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
