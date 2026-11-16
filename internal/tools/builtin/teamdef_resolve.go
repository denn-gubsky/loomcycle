package builtin

import (
	"context"
	"errors"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// foreignTeamRunRefusal is op=run's refusal for a team that declares agents,
// schedules or webhooks of its own (what) and is not the caller's tenant's.
// Those run only in the team's own tenant. For a shared team that tenant is
// the shared tier, which a tenant caller cannot run as — so it is told the
// way that does work: fork the team into its own tenant, where the fork's
// own agents are that tenant's.
func foreignTeamRunRefusal(row store.TeamDefRow, what string) string {
	if row.TenantID == "" {
		return fmt.Sprintf("run: team %q is a shared team that declares %s of its own; a team's own %s run only in "+
			"the team's tenant, so a shared one runs only for the shared tier — fork it into your tenant "+
			"(TeamDef op=fork name=%q) to run it here", row.Name, what, what, row.Name)
	}
	return fmt.Sprintf("run: team %q declares %s of its own and belongs to another tenant; "+
		"a team's own %s run only in the team's tenant — run it as that tenant", row.Name, what, what)
}

// ActiveTeam resolves a team NAME the way every by-name read of a team does:
// the caller tenant's active version, else the shared ("") tier's.
//
// The shared tier is readable by every tenant (defRowReadable) and a run by
// def_id already accepts a shared version, so a name must reach it too —
// before this, a shared team was listed and forkable from a tenant but "not
// found" when run by name (RFC GS P0). A tenant's own pointer always wins,
// even a retired one: retiring is how a tenant takes its version out of
// service, and falling through to the shared one would undo that.
//
// The returned error is the tenant read's *store.ErrNotFound when neither
// tier has the name, so callers keep their own not-found wording; any other
// store fault is returned as it is.
func ActiveTeam(ctx context.Context, st teamDefActiveGetter, tenantID, name string) (store.TeamDefRow, error) {
	row, err := st.TeamDefGetActive(ctx, tenantID, name)
	var nf *store.ErrNotFound
	if err == nil || !errors.As(err, &nf) || tenantID == "" {
		return row, err
	}
	shared, serr := st.TeamDefGetActive(ctx, "", name)
	if errors.As(serr, &nf) {
		return store.TeamDefRow{}, err
	}
	return shared, serr
}
