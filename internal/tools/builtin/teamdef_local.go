package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// teamdef_local.go — a team's own agents, on the authoring side.
//
// A local agent is stored only inside its team's definition, but it RUNS like
// any agent: with tools, scopes, hooks and possibly code. TeamDef authoring is
// gated at the route and has no per-agent gates of its own, so without the
// checks here a team would be a way to mint an agent past every gate AgentDef
// create applies. Each local agent therefore passes exactly those gates
// (AgentDef.gateNewDef), judged under its full name "<team>/<name>" so an
// operator's grant like `named:sdlc/**` covers team sdlc's own agents.

// codeJSProvider is the provider id of an agent that runs code instead of a
// model.
const codeJSProvider = "code-js"

// LocalAgentDefinition turns a team-local agent's stored body — the overlay
// AgentDef create takes — into the definition JSON an agent_defs row holds,
// through the same decoder and merge AgentDef create uses. A reader builds the
// runtime definition from it with lookup.AgentFromDefRow, exactly as it would
// for a stored agent version.
func LocalAgentDefinition(body json.RawMessage) (json.RawMessage, error) {
	ov, err := decodeAgentOverlay(body)
	if err != nil {
		return nil, err
	}
	var def mergedDef
	def.applyOverlay(ov)
	def.normalize()
	return json.Marshal(def)
}

// localAgentIssues holds every local agent a definition declares to the gates
// a new agent passes, and refuses one whose full name is already an agent.
// Called at create AND fork, on the merged definition: a fork is an authoring
// act by whoever forks, so the agents it carries over are judged under the
// forker's authority like the ones it adds — there is no parent lineage to
// inherit a ceiling from. parent is the definition being forked (nil at
// create); it only lets a caller inside a run keep the hooks a local agent
// already had.
//
// Every agent is judged; one that fails a gate is not judged further (the
// AgentDef gates after a refusal assume it passed), and the next agent is.
func (t *TeamDef) localAgentIssues(ctx context.Context, op, team string, def teamgraph.Definition, parent *teamgraph.Definition) []teamIssue {
	names := def.LocalAgentNames()
	if len(names) == 0 {
		return nil
	}
	var issues []teamIssue
	// A local agent is named "<team>/<name>", which must split one way. A team
	// named before the one-segment rule may hold a "/" or ":" and keeps
	// working as it did, but cannot declare agents of its own. Nothing below
	// can be judged under a name that does not split.
	if err := teamgraph.ValidateName(team); err != nil {
		return append(issues, refused(teamIssueNameInvalid, "local.agents", fmt.Sprintf(
			"local: a team that declares its own agents needs a name of one segment "+
				"(A-Z a-z 0-9 _ -), because each is named \"<team>/<name>\": %v", err)))
	}
	if t.Agents == nil || t.Agents.Cfg == nil {
		return append(issues, refused(teamIssueUncheckable, "local.agents",
			"local: this server cannot check a team's own agents (no agent-definition tool is wired), so a definition declaring them is refused"))
	}
	issues = append(issues, graphIssues(teamgraph.CheckLocalRunNamesAll(def, team))...)
	policy := tools.AgentDefPolicy(ctx)
	tenantID := tools.RunIdentity(ctx).TenantID
	for _, name := range names {
		full := teamgraph.QualifiedLocalName(team, name)
		where := fmt.Sprintf("local.agents[%q]", name)
		path := teamgraph.PathKey("local.agents", name)
		agent := teamgraph.LocalRefPrefix + name
		refuse := func(kind, subpath, detail string) {
			issues = append(issues, teamIssue{Kind: kind, Severity: severityRefused, Path: path + subpath, Agent: agent, Detail: detail})
		}
		// The scope gate first, although gateNewDef applies it again below: a
		// caller with no authority over this name must not learn from the
		// collision check whether an agent of that name exists.
		if err := t.Agents.checkScopeForName(policy, full, ""); err != nil {
			refuse(teamIssueLocalAgentAuthority, "", fmt.Sprintf("%s: %v", where, err))
			continue
		}
		// The global chain a run of that name would resolve through: the
		// tenant's own agents, the operator's static ones, the shared ones.
		//
		// Read with faults reported: a store that cannot answer has not said
		// the name is free.
		_, exists, err := lookup.AgentChecked(ctx, t.Store, t.Agents.Cfg, tenantID, full)
		if err != nil {
			refuse(teamIssueUncheckable, "", fmt.Sprintf("%s: could not check that no agent is named %q: %v", where, full, err))
			continue
		}
		if exists {
			refuse(teamIssueLocalAgentCollision, "", fmt.Sprintf("%s: an agent named %q already exists. A team's own agent and another agent of the same "+
				"full name would share agent-scoped memory and channel cursors — rename one of them", where, full))
			continue
		}
		var parentDef json.RawMessage
		if parent != nil {
			if body, ok := parent.LocalAgent(name); ok {
				// An unreadable parent body compares as "no hooks", which can
				// only refuse more.
				parentDef, _ = LocalAgentDefinition(body)
			}
		}
		merged, _, err := t.Agents.gateNewDef(ctx, policy, op, full, def.Local.Agents[name], parentDef)
		if err != nil {
			refuse(teamIssueLocalAgentInvalid, "", fmt.Sprintf("%s: %v", where, err))
			continue
		}
		if merged.Provider == codeJSProvider && merged.Code == "" {
			refuse(teamIssueLocalAgentInvalid, ".code_body", fmt.Sprintf("%s: a team's own code-js agent must carry its source inline as code_body — "+
				"there is no agent_code directory for it", where))
			continue
		}
		// skill.tools ⊆ agent.tools for every team skill it is granted — the
		// rule the Skill tool applies when one is loaded, checked here so a
		// team cannot be stored with a grant that would only ever be refused.
		// Validate has already refused a grant of an undeclared skill.
		for _, granted := range teamgraph.LocalSkillGrants(merged.Skills) {
			sk, ok := def.LocalSkill(granted)
			if !ok {
				continue
			}
			if widening := skillToolsExceedingAgent(sk.Tools, merged.Tools, merged.Tools); len(widening) > 0 {
				issues = append(issues, teamIssue{
					Kind: teamIssueLocalAgentInvalid, Severity: severityRefused, Path: path + ".skills",
					Agent: agent, Skill: teamgraph.LocalRefPrefix + granted,
					Detail: fmt.Sprintf("%s: its skill %q requires tools %v this agent is not granted — a skill cannot widen its agent's tools",
						where, teamgraph.LocalRefPrefix+granted, widening),
				})
			}
		}
	}
	return issues
}

// localSkillDefinition is a team-local skill in the shape a skill_defs row
// holds, which every SkillDef check reads.
func localSkillDefinition(sk teamgraph.LocalSkill) skillDefOverlay {
	return skillDefOverlay{Body: sk.Body, Description: sk.Description, Tools: sk.Tools}
}

// localSkillIssues holds every skill a definition declares to the gates a new
// skill passes (SkillDef.gateNewSkill), under its full name "<team>/<name>".
// Called at create AND fork, on the merged definition, for the reason
// localAgentIssues gives: a fork is an authoring act by the forker, so the
// skills it carries over are judged under the forker's authority — its
// `skills:` allowlist and its own tools — like the ones it adds.
//
// There is no name-collision check, unlike for agents: a team's skill keeps
// no state of its own, and it is reached only as "./<name>" by the team's own
// agents, so a global skill of the same full name is never ambiguous with it.
func (t *TeamDef) localSkillIssues(ctx context.Context, op, team string, def teamgraph.Definition) []teamIssue {
	names := def.LocalSkillNames()
	if len(names) == 0 {
		return nil
	}
	if err := teamgraph.ValidateName(team); err != nil {
		return []teamIssue{refused(teamIssueNameInvalid, "local.skills", fmt.Sprintf(
			"local: a team that declares its own skills needs a name of one segment "+
				"(A-Z a-z 0-9 _ -), because each is named \"<team>/<name>\": %v", err))}
	}
	if t.Skills == nil {
		return []teamIssue{refused(teamIssueUncheckable, "local.skills",
			"local: this server cannot check a team's own skills (no skill-definition tool is wired), so a definition declaring them is refused")}
	}
	var issues []teamIssue
	policy := tools.SkillPolicy(ctx)
	for _, name := range names {
		sk, _ := def.LocalSkill(name)
		if _, err := t.Skills.gateNewSkill(ctx, policy, op, teamgraph.QualifiedLocalName(team, name), localSkillDefinition(sk)); err != nil {
			issues = append(issues, teamIssue{
				Kind: teamIssueLocalSkillInvalid, Severity: severityRefused,
				Path: teamgraph.PathKey("local.skills", name), Skill: teamgraph.LocalRefPrefix + name,
				Detail: fmt.Sprintf("local.skills[%q]: %v", name, err),
			})
		}
	}
	return issues
}

// checkLocalNamesFree refuses to make a version ACTIVE AND LIVE while one of
// its local agents shares its full name with an existing agent. create and
// fork check this on what they write; promote and un-retire can bring back an
// older version after such an agent appeared. (A version that is neither is
// still runnable by def_id, and a restore writes rows directly: what stops a
// clash there is the check made each time a local agent is resolved.)
//
// A version that declares agents is refused when the check cannot be made,
// as localAgentIssues refuses to store one. A name the caller holds no
// agent-authoring grant over is NOT checked: refusing on it would tell the
// caller whether an agent of that name exists.
func (t *TeamDef) checkLocalNamesFree(ctx context.Context, row store.TeamDefRow) error {
	def, err := teamgraph.Parse(row.Definition)
	if err != nil {
		return nil // op=run parses first and refuses, so its agents cannot run
	}
	names := def.LocalAgentNames()
	if len(names) == 0 {
		return nil
	}
	if t.Agents == nil || t.Agents.Cfg == nil {
		return fmt.Errorf("this version declares its own agents, and this server cannot check them (no agent-definition tool is wired)")
	}
	policy := tools.AgentDefPolicy(ctx)
	for _, name := range names {
		full := teamgraph.QualifiedLocalName(row.Name, name)
		if t.Agents.checkScopeForName(policy, full, "") != nil {
			continue
		}
		_, exists, err := lookup.AgentChecked(ctx, t.Store, t.Agents.Cfg, row.TenantID, full)
		if err != nil {
			return fmt.Errorf("could not check that no agent is named %q: %w", full, err)
		}
		if exists {
			return fmt.Errorf("this version declares its own agent %q, and an agent named %q exists now. They would share "+
				"agent-scoped memory and channel cursors — rename or remove one first", name, full)
		}
	}
	return nil
}

// teamDefActiveGetter is the one store read TeamLocalAgentCollision needs.
type teamDefActiveGetter interface {
	TeamDefGetActive(ctx context.Context, tenantID, name string) (store.TeamDefRow, error)
}

// TeamLocalAgentCollision refuses a NEW global agent name "<team>/<name>" when
// the tenant's active, non-retired team <team> declares its own agent <name>:
// inside that team the two would be one name, and they would share
// agent-scoped memory and channel cursors, which key on the agent name.
//
// Only a name of exactly two segments can collide, so every other name costs
// nothing; a two-segment one costs a single read.
//
// Checked wherever a global agent name can come to resolve: AgentDef create,
// fork, promote and un-retire call this, register_agent the predicate under
// it (TeamDeclaresLocalAgent). It reads the ACTIVE team
// version only, and nothing here can see a static agent added to the
// operator's config or rows written by a restore, so it is the early, readable
// refusal and not the guarantee: that is the check made when a team's own
// agent is resolved for a run, which refuses while both exist.
//
// The refusal names what the team declares, so it is for a caller already
// authorised for the name (AgentDef's paths check agent_def_scopes first). A
// caller with no per-name grant must not learn a team's declarations from it,
// so it asks TeamDeclaresLocalAgent and refuses in its own words.
func TeamLocalAgentCollision(ctx context.Context, st teamDefActiveGetter, tenantID, name string) error {
	declared, err := TeamDeclaresLocalAgent(ctx, st, tenantID, name)
	if err != nil {
		return err
	}
	if declared {
		team, local, _ := strings.Cut(name, "/")
		return fmt.Errorf("team %q declares its own agent %q, which runs as %q. Another agent of that name would share "+
			"its agent-scoped memory and channel cursors — choose a different name", team, local, name)
	}
	return nil
}

// TeamDeclaresLocalAgent reports whether name is "<team>/<local>" for an
// active, non-retired team of the tenant that declares its own agent <local>
// (see TeamLocalAgentCollision). A store fault is an error, never false: it
// must not be the way two agents come to share a name.
func TeamDeclaresLocalAgent(ctx context.Context, st teamDefActiveGetter, tenantID, name string) (bool, error) {
	team, local, two := strings.Cut(name, "/")
	if !two || strings.Contains(local, "/") {
		return false, nil
	}
	if st == nil {
		return false, fmt.Errorf("could not check team %q for an agent of its own named %q: no store", team, local)
	}
	row, err := st.TeamDefGetActive(ctx, tenantID, team)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return false, nil
		}
		return false, fmt.Errorf("could not check team %q for an agent of its own named %q: %w", team, local, err)
	}
	if row.Retired {
		return false, nil
	}
	def, err := teamgraph.Parse(row.Definition)
	if err != nil {
		return false, nil // op=run parses first and refuses, so its agents cannot run
	}
	_, declared := def.LocalAgent(local)
	return declared, nil
}
