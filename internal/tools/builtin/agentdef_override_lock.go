package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// admitOverrideLockChange refuses a version that would run open
// (allow_overrides unset or true) when the name's lineage is locked
// (allow_overrides: false), unless the caller is the operator plane. def is
// the version being created, forked or promoted.
//
// It is checked against the lineage on create, fork and promote, as the
// no-trace mode and the read tool modes are, because buildDefinition only
// sees the version a fork names as its parent: forking a version from before
// the lock (pinned by parent_def_id), promoting one, or creating the name
// anew would otherwise make an open version active or newest. A static
// agent's lock is also kept at read time (lookup), but counted here too so a
// fork of it does not store a version that only reads as locked while the
// config says so.
//
// Only the operator plane may lift it (tools.IsOperatorPlane: the operator's
// own off-run call, or a tenant operator's in its own tenant — never a run),
// the same rule as lifting a read mode. An operator's lift is recorded in the
// audit log, as taking a no-trace mode off is, and refused when the record
// cannot be written: the lock is what keeps a measured agent's settings fixed,
// so its removal must leave a trace of who did it.
func (a *AgentDef) admitOverrideLockChange(ctx context.Context, op, tenant, name string, def mergedDef) error {
	if def.AllowOverrides != nil && !*def.AllowOverrides {
		return nil
	}
	locked, err := overrideLockedLineage(ctx, a.Store, a.Cfg, tenant, name)
	if err != nil {
		return fmt.Errorf("%s: reading whether %q locks its per-run settings: %w", op, name, err)
	}
	if !locked {
		return nil
	}
	if !tools.IsOperatorPlane(ctx) {
		return fmt.Errorf("%s: %q locks its per-run settings (allow_overrides: false), and only an operator may take that off; keep \"allow_overrides\": false in this version", op, name)
	}
	subject := ""
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		subject = p.Subject
	}
	const action = "allow_overrides_lock_removed"
	if a.Audit != nil {
		if err := a.Audit.Record(audit.Event{
			Action: action, TargetTenant: tenant, TargetName: name,
			ActorTenant: tenant, ActorSubject: subject,
		}); err != nil {
			return fmt.Errorf("%s: %q was not changed: the audit record of taking its override lock off could not be written: %w", op, name, err)
		}
	}
	slog.WarnContext(ctx, "agentdef: override lock taken off",
		"op", op, "subject", subject, "agent_name", name, "target_tenant", tenant, "action", action)
	return nil
}

// overrideLockedLineage reports whether name, in tenant, is locked: its
// static definition, its active version or its newest version says
// allow_overrides: false. When the tenant has no active version of its own,
// the shared ("") tier's version is what its runs resolve (lookup.Agent), so
// that lineage counts as well: a tenant's first version of the name would
// otherwise replace a locked shared one without the lock.
func overrideLockedLineage(ctx context.Context, st store.Store, cfg *config.Config, tenant, name string) (bool, error) {
	if cfg != nil {
		if static, ok := cfg.Agents[name]; ok && static.AllowOverrides != nil && !*static.AllowOverrides {
			return true, nil
		}
	}
	locked, hasActive, err := overrideLockedIn(ctx, st, tenant, name)
	if err != nil || locked || hasActive || tenant == "" {
		return locked, err
	}
	locked, _, err = overrideLockedIn(ctx, st, "", name)
	return locked, err
}

// overrideLockedIn is overrideLockedLineage for one tenant's rows, and
// whether that tenant has an active version. A definition that does not parse
// cannot be shown to be open, so it counts as locked.
func overrideLockedIn(ctx context.Context, st store.Store, tenant, name string) (locked, hasActive bool, err error) {
	isLocked := func(def json.RawMessage) bool {
		var d struct {
			AllowOverrides *bool `json:"allow_overrides"`
		}
		return json.Unmarshal(def, &d) != nil || (d.AllowOverrides != nil && !*d.AllowOverrides)
	}
	var nf *store.ErrNotFound
	active, err := st.AgentDefGetActive(ctx, tenant, name)
	switch {
	case err == nil:
		hasActive = true
		locked = isLocked(active.Definition)
	case !errors.As(err, &nf):
		return false, false, err
	}
	rows, err := st.AgentDefListByName(ctx, name) // newest first
	if err != nil {
		return false, false, err
	}
	for _, r := range rows {
		if r.TenantID != tenant {
			continue
		}
		locked = locked || isLocked(r.Definition)
		break
	}
	return locked, hasActive, nil
}
