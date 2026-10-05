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

// checkLocalAgents holds every local agent a definition declares to the gates
// a new agent passes, and refuses one whose full name is already an agent.
// Called at create AND fork, on the merged definition: a fork is an authoring
// act by whoever forks, so the agents it carries over are judged under the
// forker's authority like the ones it adds — there is no parent lineage to
// inherit a ceiling from. parent is the definition being forked (nil at
// create); it only lets a caller inside a run keep the hooks a local agent
// already had.
func (t *TeamDef) checkLocalAgents(ctx context.Context, op, team string, def teamgraph.Definition, parent *teamgraph.Definition) error {
	names := def.LocalAgentNames()
	if len(names) == 0 {
		return nil
	}
	// A local agent is named "<team>/<name>", which must split one way. A team
	// named before the one-segment rule may hold a "/" or ":" and keeps
	// working as it did, but cannot declare agents of its own.
	if err := teamgraph.ValidateName(team); err != nil {
		return fmt.Errorf("local: a team that declares its own agents needs a name of one segment "+
			"(A-Z a-z 0-9 _ -), because each is named \"<team>/<name>\": %w", err)
	}
	if t.Agents == nil || t.Agents.Cfg == nil {
		return fmt.Errorf("local: this server cannot check a team's own agents (no agent-definition tool is wired), so a definition declaring them is refused")
	}
	policy := tools.AgentDefPolicy(ctx)
	tenantID := tools.RunIdentity(ctx).TenantID
	for _, name := range names {
		full := teamgraph.QualifiedLocalName(team, name)
		where := fmt.Sprintf("local.agents[%q]", name)
		// The scope gate first, although gateNewDef applies it again below: a
		// caller with no authority over this name must not learn from the
		// collision check whether an agent of that name exists.
		if err := t.Agents.checkScopeForName(policy, full, ""); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		// The global chain a run of that name would resolve through: the
		// tenant's own agents, the operator's static ones, the shared ones.
		if _, exists := lookup.Agent(ctx, t.Store, t.Agents.Cfg, tenantID, full); exists {
			return fmt.Errorf("%s: an agent named %q already exists. A team's own agent and another agent of the same "+
				"full name would share agent-scoped memory and channel cursors — rename one of them", where, full)
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
			return fmt.Errorf("%s: %w", where, err)
		}
		if merged.Provider == codeJSProvider && merged.Code == "" {
			return fmt.Errorf("%s: a team's own code-js agent must carry its source inline as code_body — "+
				"there is no agent_code directory for it", where)
		}
	}
	return nil
}

// checkLocalNamesFree refuses to make a version ACTIVE AND LIVE while one of
// its local agents shares its full name with an existing agent. create and
// fork check this on what they write; promote and un-retire can bring back an
// older version after such an agent appeared. (A version that is neither is
// still runnable by def_id, and a restore writes rows directly: what stops a
// clash there is the check made each time a local agent is resolved.)
//
// A version that declares agents is refused when the check cannot be made,
// as checkLocalAgents refuses to store one. A name the caller holds no
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
		if _, exists := lookup.Agent(ctx, t.Store, t.Agents.Cfg, row.TenantID, full); exists {
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
// Called wherever a global agent name can come to resolve: AgentDef create,
// fork, promote and un-retire, and register_agent. It reads the ACTIVE team
// version only, and nothing here can see a static agent added to the
// operator's config or rows written by a restore, so it is the early, readable
// refusal and not the guarantee: that is the check made when a team's own
// agent is resolved for a run, which refuses while both exist.
func TeamLocalAgentCollision(ctx context.Context, st teamDefActiveGetter, tenantID, name string) error {
	team, local, two := strings.Cut(name, "/")
	if !two || strings.Contains(local, "/") {
		return nil
	}
	if st == nil {
		return fmt.Errorf("could not check team %q for an agent of its own named %q: no store", team, local)
	}
	row, err := st.TeamDefGetActive(ctx, tenantID, team)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return nil
		}
		// Refused rather than waved through: a store fault must not be the
		// way two agents come to share a name.
		return fmt.Errorf("could not check team %q for an agent of its own named %q: %w", team, local, err)
	}
	if row.Retired {
		return nil
	}
	def, err := teamgraph.Parse(row.Definition)
	if err != nil {
		return nil // op=run parses first and refuses, so its agents cannot run
	}
	if _, declared := def.LocalAgent(local); declared {
		return fmt.Errorf("team %q declares its own agent %q, which runs as %q. Another agent of that name would share "+
			"its agent-scoped memory and channel cursors — choose a different name", team, local, name)
	}
	return nil
}
