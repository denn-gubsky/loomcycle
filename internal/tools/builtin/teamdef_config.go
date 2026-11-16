package builtin

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// teamdef_config.go — the yaml's teams written into the store (RFC GS P2).
//
// A team in the configuration is not a tier runs read. The boot writes each
// one into the shared ("") tier as an ordinary version, so a team's own agents,
// walk records, resume, subscriptions and snapshots — everything keyed on a
// stored def_id — need nothing new. The version is marked
// bootstrapped_from_static (RFC GS D8: for a team it means "written from the
// configuration"), is operator-authored, and is promoted with the posture a
// static schedule runs under: not operator-key restricted, not isolated, no
// access-control owner.

// configTeamAuthor is the created_by / promoted_by label of a version the
// configuration wrote. Nothing resolves it; it says where the version came from.
const configTeamAuthor = "config"

// ConfigTeamsReport is what one reconcile did, by team name.
type ConfigTeamsReport struct {
	// Written are the teams given a new version (new, or changed in the yaml).
	Written []string
	// Restored are teams whose config version was retired and is in the yaml
	// again unchanged: it is in service again.
	Restored []string
	// Unchanged are teams whose active version is already the yaml's.
	Unchanged []string
	// Retired are config-written teams no longer in the yaml (RFC GS D5).
	Retired []string
	// Conflicts are names the shared tier already holds as an operator's own
	// team, made over the API: the boot does not take them over (RFC GS D3).
	Conflicts []ConfigTeamProblem
	// Refused are teams a create would refuse here — a check that needs the
	// deployment (a team's own agents against the AgentDef rules, channel or
	// decision authority). They are not written.
	Refused []ConfigTeamProblem
}

// ConfigTeamProblem is a team the reconcile left alone, and why.
type ConfigTeamProblem struct {
	Name   string
	Detail string
}

// configTeamCtx pins where and as whom a configured team is written: the
// operator's own call in the shared tier, labelled as the configuration. The
// grants the create gates read (a team's own agents' agent_def_scopes, the
// tools ceiling, channels) are the operator plane's, which the caller has put
// on ctx (Server.ReconcileConfigTeams, through the substrate admin ctx). With
// no principal on ctx the promoter captures no restriction and no
// access-control owner.
func configTeamCtx(ctx context.Context) context.Context {
	ctx = tools.WithCallerTier(ctx, tools.TierOperatorPlane)
	return tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: configTeamAuthor})
}

// ReconcileConfigTeams makes the shared tier hold the yaml's teams. For each:
// no active version, or an active config-written version with other content →
// a new version, made active; the same content → nothing (a restart writes
// nothing); an active version an operator made over the API → left alone and
// reported. Each config-written active version whose name left the yaml is
// retired, never deleted: walks and member runs keep its def_id.
//
// Only the shared tier is read or written. A tenant's own versions of a name
// are never touched.
//
// A team is held to every gate a create applies — through the create path's
// own buildDefinition and authoringIssues — so the yaml cannot write a team
// the API would refuse. ctx must carry the operator plane's grants and no
// principal; Server.ReconcileConfigTeams is the caller that stamps them. The
// error is a store fault; what is refused or in conflict is in the report.
func (t *TeamDef) ReconcileConfigTeams(ctx context.Context, teams map[string]config.Team) (ConfigTeamsReport, error) {
	var rep ConfigTeamsReport
	if t.Store == nil {
		return rep, errors.New("config teams: no store")
	}
	ctx = configTeamCtx(ctx)
	names := make([]string, 0, len(teams))
	for name := range teams {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := t.reconcileConfigTeam(ctx, name, teams[name], &rep); err != nil {
			return rep, fmt.Errorf("config team %q: %w", name, err)
		}
	}
	if err := t.retireRemovedConfigTeams(ctx, teams, &rep); err != nil {
		return rep, err
	}
	return rep, nil
}

func (t *TeamDef) reconcileConfigTeam(ctx context.Context, name string, team config.Team, rep *ConfigTeamsReport) error {
	refuse := func(detail string) {
		rep.Refused = append(rep.Refused, ConfigTeamProblem{Name: name, Detail: detail})
	}
	defJSON, err := t.buildDefinition("", team.Definition)
	if err != nil {
		refuse(err.Error())
		return nil
	}
	def, err := teamgraph.Parse(defJSON)
	if err != nil {
		refuse(err.Error())
		return nil
	}
	if first := firstRefusal(t.authoringIssues(ctx, "create", name, defJSON, def, nil, team.Description)); first != nil {
		refuse(first.Detail)
		return nil
	}
	sha := teamgraph.Sign(name, def)

	var parent string
	active, err := t.Store.TeamDefGetActive(ctx, "", name)
	switch {
	case err == nil && !active.BootstrappedFromStatic:
		rep.Conflicts = append(rep.Conflicts, ConfigTeamProblem{Name: name, Detail: fmt.Sprintf(
			"the shared tier's active version %s of %q was made over the API, not by the configuration; "+
				"it is not replaced — rename the team in teams: or the stored one", active.DefID, name)})
		return nil
	case err == nil && active.ContentSHA256 == sha && active.Description == team.Description:
		if !active.Retired {
			rep.Unchanged = append(rep.Unchanged, name)
			return nil
		}
		if err := t.Store.TeamDefSetRetired(ctx, active.DefID, false); err != nil {
			return err
		}
		rep.Restored = append(rep.Restored, name)
		return nil
	case err == nil:
		parent = active.DefID
	case !isNotFound(err):
		return err
	}

	// As create does, last before the write: an operator's lift of a local
	// agent's no-trace mode, which authoringIssues has admitted.
	if err := t.recordLocalAgentLockLifts(ctx, "create", "", name, def); err != nil {
		return err
	}
	created, err := t.Store.TeamDefCreate(ctx, store.TeamDefRow{
		DefID:                  mintTeamDefID(),
		Name:                   name,
		ParentDefID:            parent,
		Definition:             defJSON,
		Description:            team.Description,
		CreatedByAgentID:       configTeamAuthor,
		ContentSHA256:          sha,
		BootstrappedFromStatic: true,
		OperatorAuthored:       true,
	})
	if err != nil {
		return err
	}
	if err := t.Store.TeamDefSetActive(ctx, "", name, created.DefID, configTeamAuthor, t.promoter(ctx)); err != nil {
		return err
	}
	rep.Written = append(rep.Written, name)
	return nil
}

// retireRemovedConfigTeams retires the shared tier's config-written active
// versions whose names the yaml no longer holds. Retiring disarms a
// subscription and refuses new walks; the active pointer stays, so a name put
// back in the yaml unchanged is simply restored.
func (t *TeamDef) retireRemovedConfigTeams(ctx context.Context, teams map[string]config.Team, rep *ConfigTeamsReport) error {
	summaries, err := t.Store.TeamDefListNames(ctx)
	if err != nil {
		return fmt.Errorf("config teams: list names: %w", err)
	}
	for _, s := range summaries {
		if s.TenantID != "" || s.ActiveDefID == "" || s.ActiveRetired {
			continue
		}
		if _, configured := teams[s.Name]; configured {
			continue
		}
		active, err := t.Store.TeamDefGet(ctx, s.ActiveDefID)
		if err != nil {
			return fmt.Errorf("config teams: read %q: %w", s.Name, err)
		}
		if !active.BootstrappedFromStatic {
			continue
		}
		if err := t.Store.TeamDefSetRetired(ctx, active.DefID, true); err != nil {
			return fmt.Errorf("config teams: retire %q: %w", s.Name, err)
		}
		rep.Retired = append(rep.Retired, s.Name)
	}
	sort.Strings(rep.Retired)
	return nil
}

// configTeamGuard refuses an edit (create, fork, promote, retire, delete) of a
// name in the shared tier while the configuration holds it — its active
// version there is one the configuration wrote and is live. The yaml is where
// such a team changes: an edit over the API would be undone by the next boot,
// or would turn the name into a conflict the boot then refuses to resolve.
// This is the rule create already applies to a static agent or skill name.
//
// Only the shared tier is guarded. A tenant forks a configured team into its
// own tenant like any shared team. A config-written version that is retired is
// one whose name has left the yaml: the name is the operator's again. Verify,
// get, list and run are not edits and are never refused.
func (t *TeamDef) configTeamGuard(ctx context.Context, op, tenantID, name string) error {
	if tenantID != "" || name == "" {
		return nil
	}
	active, err := t.Store.TeamDefGetActive(ctx, "", name)
	switch {
	case isNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("%s: %w", op, err)
	case active.BootstrappedFromStatic && !active.Retired:
		return fmt.Errorf("%s: team %q comes from the configuration (teams.%s); change it there", op, name, name)
	}
	return nil
}
