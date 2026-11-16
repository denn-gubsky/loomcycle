package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// teamdef_local_locks.go — the one-way locks of a team's own agent.
//
// An agent stored on its own keeps three locks across its versions, and only
// an operator lifts one: allow_overrides: false (admitOverrideLockChange),
// retention: none (admitNoTraceChange) and a read tool mode
// (admitToolModesChange). A team's own agent has no versions of its own — a
// TeamDef fork replaces local.agents wholesale — so its lineage is projected
// from the team's: the local agent of the same name in the team's active
// version, and in the newest team version that declares it. Exactly as for a
// stored agent, the shared ("") tier's team counts too when the tenant's
// active team does not declare the agent, which is what a tenant's first
// version of a shared team is, and what a team that dropped the agent is.
//
// Dropping a local agent is therefore not a way round its locks: the agent
// is not running while no active version declares it, and whichever version
// declares it again is checked against the newest version that last did.

// localAgentLockActions checks the version of team's own agent local that def
// is against the locks the team's lineage holds for it, and returns the
// no-trace changes an operator is making, which recordLocalAgentLockLifts
// records. A refusal is an error. Reading the lineage only, it is what verify
// reports as well.
func (t *TeamDef) localAgentLockActions(ctx context.Context, op, tenant, team, local string, def mergedDef) ([]string, error) {
	defs, err := localAgentLineage(ctx, t.Store, tenant, team, local)
	if err != nil {
		return nil, fmt.Errorf("%s: reading the locks earlier versions of team %q hold on its agent %q: %w", op, team, local, err)
	}
	if len(defs) == 0 {
		return nil, nil
	}
	full := teamgraph.QualifiedLocalName(team, local)
	var actions []string
	for _, d := range defs {
		if !defIsNoTrace(d) {
			continue
		}
		// The hooks compared are those of the version the agent runs with
		// now, defs[0], as noTraceLineage's current.
		if actions, err = noTraceLiftActions(ctx, op, full, def, defs[0]); err != nil {
			return nil, err
		}
		break
	}
	var held map[string]string
	for _, d := range defs {
		m, err := defToolModes(d)
		if err != nil {
			return nil, fmt.Errorf("%s: reading the tool modes of %q: %w", op, full, err)
		}
		held = unionToolModes(held, m)
	}
	if err := refuseDroppedReadModes(ctx, op, full, def, held); err != nil {
		return nil, err
	}
	// admitOverrideLockChange's rule, over this lineage.
	if (def.AllowOverrides != nil && !*def.AllowOverrides) || tools.IsOperatorPlane(ctx) {
		return actions, nil
	}
	for _, d := range defs {
		if defLocksOverrides(d) {
			return nil, fmt.Errorf("%s: %q locks its per-run settings (allow_overrides: false), and only an operator may take that off; keep \"allow_overrides\": false in this version", op, full)
		}
	}
	return actions, nil
}

// recordLocalAgentLockLifts checks every agent the team version def declares
// against its locks and records each no-trace change an operator makes, as
// AgentDef does; it refuses when a record cannot be written. create and fork
// call it once their checks have passed (localAgentIssues refused the rest,
// and verify must record nothing); promote calls it as its check.
func (t *TeamDef) recordLocalAgentLockLifts(ctx context.Context, op, tenant, team string, def teamgraph.Definition) error {
	if t.Agents == nil {
		return nil // a definition declaring agents was refused for this already
	}
	for _, local := range def.LocalAgentNames() {
		body, _ := def.LocalAgent(local)
		merged, err := localAgentMergedDef(body)
		if err != nil {
			return fmt.Errorf("%s: local.agents[%q]: %w", op, local, err)
		}
		actions, err := t.localAgentLockActions(ctx, op, tenant, team, local, merged)
		if err != nil {
			return err
		}
		if err := t.Agents.recordNoTraceLift(ctx, op, tenant, teamgraph.QualifiedLocalName(team, local), actions); err != nil {
			return err
		}
	}
	return nil
}

// localAgentMergedDef is a team's own agent body as the definition AgentDef
// create would store, decoded.
func localAgentMergedDef(body json.RawMessage) (mergedDef, error) {
	raw, err := LocalAgentDefinition(body)
	if err != nil {
		return mergedDef{}, err
	}
	var def mergedDef
	err = json.Unmarshal(raw, &def)
	return def, err
}

// localAgentLineage is the definitions of team's own agent local whose locks
// hold over its next version, in the shape an agent_defs row holds: the one in
// the tenant's active team version and the one in its newest team version
// that declares it; and, when the tenant's active team does not declare it,
// the shared ("") tier's the same way. The version the agent runs with now
// comes first. Empty when no version declares it.
func localAgentLineage(ctx context.Context, st store.Store, tenant, team, local string) ([]json.RawMessage, error) {
	defs, declaredActive, err := localAgentLineageIn(ctx, st, tenant, team, local)
	if err != nil || declaredActive || tenant == "" {
		return defs, err
	}
	shared, _, err := localAgentLineageIn(ctx, st, "", team, local)
	if err != nil {
		return nil, err
	}
	return append(defs, shared...), nil
}

// localAgentLineageIn is localAgentLineage for one tenant's team rows, and
// whether that tenant's active team declares the agent.
//
// A team version that does not parse is passed over: it cannot run, so
// neither can its agents (TeamDeclaresLocalAgent reads it the same way), and
// failing on it would refuse every later version of the team for good. A
// local agent body that does not parse in a version that does is an error.
func localAgentLineageIn(ctx context.Context, st store.Store, tenant, team, local string) (defs []json.RawMessage, declaredActive bool, err error) {
	declared := func(teamDef json.RawMessage) (json.RawMessage, bool, error) {
		d, perr := teamgraph.Parse(teamDef)
		if perr != nil {
			return nil, false, nil
		}
		body, ok := d.LocalAgent(local)
		if !ok {
			return nil, false, nil
		}
		def, derr := LocalAgentDefinition(body)
		if derr != nil {
			return nil, false, fmt.Errorf("its agent %q does not parse: %w", local, derr)
		}
		return def, true, nil
	}
	var nf *store.ErrNotFound
	active, err := st.TeamDefGetActive(ctx, tenant, team)
	switch {
	case err == nil:
		def, ok, derr := declared(active.Definition)
		if derr != nil {
			return nil, false, derr
		}
		if ok {
			defs, declaredActive = append(defs, def), true
		}
	case !errors.As(err, &nf):
		return nil, false, err
	}
	rows, err := st.TeamDefListByName(ctx, team) // newest first
	if err != nil {
		return nil, false, err
	}
	for _, r := range rows {
		if r.TenantID != tenant {
			continue
		}
		def, ok, derr := declared(r.Definition)
		if derr != nil {
			return nil, false, derr
		}
		if ok {
			return append(defs, def), declaredActive, nil
		}
	}
	return defs, declaredActive, nil
}

// defLocksOverrides reports whether a stored agent definition says
// allow_overrides: false. A definition that does not parse cannot be shown to
// be open, so it counts as locked (overrideLockedIn reads it the same way).
func defLocksOverrides(def json.RawMessage) bool {
	var d struct {
		AllowOverrides *bool `json:"allow_overrides"`
	}
	return json.Unmarshal(def, &d) != nil || (d.AllowOverrides != nil && !*d.AllowOverrides)
}
