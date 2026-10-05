package http

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// teamlocal.go — resolving an agent name inside a team.
//
// A team may declare agents of its own (teamgraph.Local). They are stored only
// in the team's definition, so the global lookup chain (lookup.Agent) never
// finds them: this file is the ONE place a name can turn into one, and it only
// does so for a run that carries that team's scope (store.TeamScope) — which a
// walk sets, every run below it inherits, and a run records so that a resume
// or a continuation reads the same team version again.
//
// Inside a scope, for a name N:
//
//	./N        → the team's own agent N; an error if it declares none
//	N          → the team's own agent N if it declares one, else the global N
//	<team>/N   → the same, by the name the agent runs under
//
// Outside every scope "./N" is refused and no spelling reaches a team's own
// agent: POST /v1/runs, a schedule, a webhook and an A2A call all resolve
// through here with no scope, and so see only global agents.

// errAgentNotFound is a name that resolves to no agent, as opposed to one that
// could not be resolved (a store fault, a team version that is gone).
var errAgentNotFound = errors.New("agent not found")

// teamVersionGoneError is a team scope whose definition is no longer there to
// read: the version was deleted, or the row is not the team the scope names.
// A resume treats it as "the definition is gone" and fails the run; a store
// fault is a different error and leaves the run to be tried again.
type teamVersionGoneError struct{ msg string }

func (e *teamVersionGoneError) Error() string { return e.msg }

// resolveAgentName turns an agent name into its definition and the name the
// agent RUNS under, honouring the team scope on ctx. The returned name is what
// the run row, the session, events and agent-scoped state must carry: for a
// team's own agent that is "<team>/<name>" however the caller spelled it; for
// a global agent it is the name as given.
//
// tenantID is the run's authoritative tenant, used for the global chain. A
// team's own agent is read from the team version the scope names, whose tenant
// the scope carries.
func (s *Server) resolveAgentName(ctx context.Context, tenantID, name string) (config.AgentDef, string, error) {
	sc, inTeam := store.TeamScopeFromContext(ctx)
	local, dotted := teamgraph.LocalRef(name)
	if !inTeam {
		if dotted {
			return config.AgentDef{}, "", fmt.Errorf("%q names a team's own agent, which can only be run from inside a walk of that team", name)
		}
		return s.globalAgent(ctx, tenantID, name)
	}
	// Which local name, if any, this spelling could mean. A name with another
	// prefix cannot be one of this team's agents, so it costs no team read.
	candidate := local
	if !dotted {
		switch rest, qualified := strings.CutPrefix(name, sc.Team+"/"); {
		case !strings.Contains(name, "/"):
			candidate = name
		case qualified:
			candidate = rest
		}
	}
	if candidate != "" && teamgraph.ValidateLocalName(candidate) == nil {
		def, found, err := s.teamLocalAgent(ctx, sc, candidate)
		if err != nil {
			// Never the global agent instead: a store fault must not be how a
			// run comes to execute a different definition under the same name.
			return config.AgentDef{}, "", err
		}
		if found {
			runName := teamgraph.QualifiedLocalName(sc.Team, candidate)
			if err := s.refuseSharedName(ctx, tenantID, sc, runName); err != nil {
				return config.AgentDef{}, "", err
			}
			return def, runName, nil
		}
	}
	if dotted {
		return config.AgentDef{}, "", fmt.Errorf("team %q declares no agent of its own named %q", sc.Team, local)
	}
	return s.globalAgent(ctx, tenantID, name)
}

// refuseSharedName refuses to run a team's own agent while a GLOBAL agent of
// the same full name resolves in the run's tenant. Agent-scoped memory and
// channel cursors key on the agent name, so the two would read and write each
// other's state; inside the team the local one would run, outside it the
// global one, under one name.
//
// Authoring refuses to create that pair (builtin.TeamLocalAgentCollision,
// TeamDef.checkLocalAgents), but those checks are made once, by one writer,
// and cannot see a static agent added to the config later, rows a restore
// wrote, or two writers racing. This one is made every time, where the name
// is about to be used, so it is the guarantee and they are the early refusal.
// It costs the global lookup only when a team's own agent is being started.
func (s *Server) refuseSharedName(ctx context.Context, tenantID string, sc store.TeamScope, runName string) error {
	if _, _, err := s.globalAgent(ctx, tenantID, runName); err != nil {
		return nil // no global agent of that name
	}
	log.Printf("team %q (version %s): refusing to run its own agent %q — a global agent of the same name exists in tenant %q; rename one",
		sc.Team, sc.DefID, runName, tenantID)
	return fmt.Errorf("team %q's own agent %q cannot run: another agent named %q exists, and the two would share "+
		"agent-scoped memory and channel cursors. Rename one of them", sc.Team, runName, runName)
}

// globalAgent is the lookup chain every agent outside a team resolves through.
func (s *Server) globalAgent(ctx context.Context, tenantID, name string) (config.AgentDef, string, error) {
	// nil-store guard at the boundary so the lookup package can type-assert an
	// interface receiver. The lookup package treats "no store" identically to
	// "store didn't have the name" — both fall through to (zero, false).
	var (
		def config.AgentDef
		ok  bool
	)
	if s.store == nil {
		def, ok = lookup.Agent(ctx, nil, s.cfg(), tenantID, name)
	} else {
		def, ok = lookup.Agent(ctx, s.store, s.cfg(), tenantID, name)
	}
	if !ok {
		return config.AgentDef{}, "", fmt.Errorf("%w: %s", errAgentNotFound, name)
	}
	return def, name, nil
}

// teamLocalAgent reads the team's own agent `local` from the team version the
// scope names. found=false means that version declares no such agent.
//
// The definition is built by the code a stored agent version goes through —
// builtin.LocalAgentDefinition (AgentDef create's own overlay decoder and
// merge), then lookup.AgentFromDefRow — so a local agent and a global one
// written from the same overlay are the same runtime definition. Authorship
// and the owning tenant come from the TEAM ROW: a local agent has no row of
// its own, and its body cannot claim either.
//
// A retired version still resolves: a run keeps the team version it started
// under, as a run keeps a retired agent version. Starting a new walk of a
// retired team is refused where walks start.
func (s *Server) teamLocalAgent(ctx context.Context, sc store.TeamScope, local string) (config.AgentDef, bool, error) {
	if s.store == nil {
		return config.AgentDef{}, false, fmt.Errorf("team %q: its own agents need a store to be read from", sc.Team)
	}
	row, err := s.store.TeamDefGet(ctx, sc.DefID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return config.AgentDef{}, false, &teamVersionGoneError{fmt.Sprintf(
				"team %q: the version this run belongs to (%s) no longer exists, so its agents cannot be resolved", sc.Team, sc.DefID)}
		}
		return config.AgentDef{}, false, fmt.Errorf("team %q: read version %s: %w", sc.Team, sc.DefID, err)
	}
	if row.Name != sc.Team || row.TenantID != sc.Tenant {
		return config.AgentDef{}, false, &teamVersionGoneError{fmt.Sprintf(
			"team %q: version %s is not a version of that team", sc.Team, sc.DefID)}
	}
	def, err := teamgraph.Parse(row.Definition)
	if err != nil {
		return config.AgentDef{}, false, &teamVersionGoneError{fmt.Sprintf("team %q version %s: %v", sc.Team, sc.DefID, err)}
	}
	body, declared := def.LocalAgent(local)
	if !declared {
		return config.AgentDef{}, false, nil
	}
	merged, err := builtin.LocalAgentDefinition(body)
	if err != nil {
		return config.AgentDef{}, false, &teamVersionGoneError{fmt.Sprintf("team %q: its agent %q is unreadable: %v", sc.Team, local, err)}
	}
	agentDef, ok := lookup.AgentFromDefRow(store.AgentDefRow{
		Definition:       merged,
		OperatorAuthored: row.OperatorAuthored,
		TenantID:         row.TenantID,
	})
	if !ok {
		return config.AgentDef{}, false, &teamVersionGoneError{fmt.Sprintf("team %q: its agent %q is unreadable", sc.Team, local)}
	}
	agentDef.TeamDefID = row.DefID
	return agentDef, true, nil
}

// agentRunName is the name an agent the caller wrote as `name` runs under, for
// the Agent tool: everything it reports about a child — its result envelope,
// the spawn ledger, the hooks it fires — then names a team's own agent the way
// its run row does. A name that resolves to nothing comes back unchanged, so
// the spawn itself reports it the way it always has.
func (s *Server) agentRunName(ctx context.Context, name string) (string, error) {
	_, runName, err := s.resolveAgentName(ctx, tenantFromCtx(ctx), name)
	if errors.Is(err, errAgentNotFound) {
		return name, nil
	}
	return runName, err
}

// runTeamScopeCtx returns ctx carrying the team scope `run` recorded, and none
// if it recorded none: whoever acts on a run resolves its agent in the run's
// own scope, never in their own.
func runTeamScopeCtx(ctx context.Context, run store.Run) context.Context {
	rc, _ := decodeRunConfig(run.RunConfig)
	return rc.withTeamScope(ctx)
}

// sessionTeamScopeCtx is runTeamScopeCtx for a session: the scope its most
// recent run recorded. A session that began inside a team — a member's, or a
// sub-agent's below one — is continued inside that team, on the version it
// started under; that is the only way a team's own agent is reachable from
// outside a walk, and it reaches only the session that was already its own.
//
// A store fault leaves no scope. For a session of a team's own agent the
// continuation then finds no such agent and is refused, which is the safe
// reading of "could not tell".
func (s *Server) sessionTeamScopeCtx(ctx context.Context, sessionID string) context.Context {
	ctx = store.WithTeamScope(ctx, store.TeamScope{})
	if s.store == nil {
		return ctx
	}
	runs, err := s.store.RunsForSession(ctx, sessionID)
	if err != nil {
		log.Printf("team scope: session %s: could not read its runs, continuing outside any team: %v", sessionID, err)
		return ctx
	}
	if len(runs) == 0 {
		return ctx
	}
	return runTeamScopeCtx(ctx, runs[len(runs)-1]) // oldest first
}

// LogTeamLocalNameClashes reports, once at boot, every active team whose own
// agent shares its full name with a global agent — in practice a static agent
// added to the operator's configuration after the team was written, which no
// authoring check could have seen. It only reports: the team's agent is
// refused when a walk tries to start it (refuseSharedName), and this line is
// how an operator learns why before a walk does. Returns how many it found.
func (s *Server) LogTeamLocalNameClashes(ctx context.Context) int {
	if s.store == nil {
		return 0
	}
	names, err := s.store.TeamDefListNames(ctx)
	if err != nil {
		log.Printf("team local agents: could not list teams to check their agents' names: %v", err)
		return 0
	}
	found := 0
	for _, n := range names {
		if n.ActiveDefID == "" || n.ActiveRetired {
			continue
		}
		row, err := s.store.TeamDefGet(ctx, n.ActiveDefID)
		if err != nil {
			continue
		}
		def, err := teamgraph.Parse(row.Definition)
		if err != nil {
			continue
		}
		for _, local := range def.LocalAgentNames() {
			full := teamgraph.QualifiedLocalName(row.Name, local)
			if _, _, gerr := s.globalAgent(ctx, row.TenantID, full); gerr != nil {
				continue
			}
			found++
			log.Printf("team %q (tenant %q): its own agent %q and a global agent of the same name both exist. "+
				"The team's agent will be refused when a walk starts it — rename one of them", row.Name, row.TenantID, full)
		}
	}
	return found
}
