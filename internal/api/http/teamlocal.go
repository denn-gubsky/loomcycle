package http

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
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
// How a name resolves inside a scope depends on who wrote it (nameSource):
//
//	a state of the team definition   ./N → the team's own agent; any other
//	                                 name → a global agent, never shadowed
//	the Agent tool, at run time      ./N or <team>/N → the team's own; a bare
//	                                 N → per bareNameShadows
//	a run's or session's own agent   <team>/N → the team's own; else global
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

// nameSource is where an agent name came from, which decides how it may
// resolve inside a team. The three have different authors, and the rules
// differ because of who the author is.
type nameSource int

const (
	// nameOfRun is the name a run or a session already carries — its own
	// agent's. A team's own agent is recorded as "<team>/<name>", so that
	// spelling finds it; a bare name is a global agent's and stays one.
	nameOfRun nameSource = iota
	// nameFromDefinition is an agent field of the team definition itself (a
	// state's agent, agents, consolidator, or a starter's fan-out). The author
	// chose the agent in writing: "./N" is the team's own and NOTHING else is.
	// A bare name is a global agent and is never shadowed, so a team that
	// gains a local agent does not change which agent an existing state runs.
	nameFromDefinition
	// nameFromAgentTool is a name a model wrote at run time, in the Agent
	// tool. "./N" and "<team>/N" find the team's own agent; whether a bare
	// name does is bareNameShadows.
	nameFromAgentTool
)

// bareNameShadows is THE rule for a bare name written at run time (the Agent
// tool): does it resolve to the team's own agent of that name, ahead of a
// global agent of the same name?
//
// callerIsTeamLocal says who is asking: one of the team's own agents (true),
// or a global agent running somewhere in the team's spawn tree (false). Today
// both shadow. The second case is the delicate one — a team's author thereby
// chooses which agent a GLOBAL agent's own bare name starts — so the decision
// is here, alone, and reads its caller: to stop shadowing for global callers,
// return callerIsTeamLocal.
func bareNameShadows(callerIsTeamLocal bool) bool {
	return true
}

// resolveAgentName turns an agent name into its definition and the name the
// agent RUNS under, honouring the team scope on ctx. The returned name is what
// the run row, the session, events and agent-scoped state must carry: for a
// team's own agent that is "<team>/<name>" however the caller spelled it; for
// a global agent it is the name as given.
//
// tenantID is the run's authoritative tenant. It is used for the global
// chain, and a team's own agents are reachable only when the scope's team
// belongs to that same tenant (see teamVersion).
func (s *Server) resolveAgentName(ctx context.Context, tenantID, name string, src nameSource) (config.AgentDef, string, error) {
	sc, inTeam := store.TeamScopeFromContext(ctx)
	local, dotted := teamgraph.LocalRef(name)
	// "./N" is a spelling for choosing an agent, never the name of a run.
	if dotted && (!inTeam || src == nameOfRun) {
		return config.AgentDef{}, "", fmt.Errorf("%q names a team's own agent, which can only be run from inside a walk of that team", name)
	}
	if !inTeam {
		return s.globalAgent(ctx, tenantID, name)
	}
	// Which local name, if any, this spelling could mean. Most names can mean
	// none, and those cost no team read.
	candidate, bare := "", false
	switch rest, qualified := strings.CutPrefix(name, sc.Team+"/"); {
	case dotted:
		candidate = local
	case src == nameFromDefinition:
		// Only "./N", handled above.
	case qualified:
		candidate = rest
	case src == nameFromAgentTool && !strings.Contains(name, "/"):
		candidate, bare = name, true
	}
	if candidate != "" && teamgraph.ValidateLocalName(candidate) == nil {
		def, found, err := s.teamLocalAgent(ctx, tenantID, sc, candidate)
		if err != nil {
			// Never the global agent instead: a store fault must not be how a
			// run comes to execute a different definition under the same name.
			return config.AgentDef{}, "", err
		}
		if found && bare && !bareNameShadows(s.callerIsTeamLocal(ctx, tenantID, sc)) {
			found = false
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

// callerIsTeamLocal reports whether the run asking is one of the scope's own
// agents: it runs as "<team>/<name>" and the team version declares <name>.
// (No global agent can hold that name while it runs; see refuseSharedName.)
func (s *Server) callerIsTeamLocal(ctx context.Context, tenantID string, sc store.TeamScope) bool {
	local, ok := strings.CutPrefix(tools.AgentName(ctx), sc.Team+"/")
	if !ok || teamgraph.ValidateLocalName(local) != nil {
		return false
	}
	_, def, err := s.teamVersion(ctx, tenantID, sc)
	if err != nil {
		return false
	}
	_, declared := def.LocalAgent(local)
	return declared
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
//
// Being the guarantee, it FAILS CLOSED: if the stores cannot be read, whether
// a global agent holds the name is unknown, and the agent does not start.
func (s *Server) refuseSharedName(ctx context.Context, tenantID string, sc store.TeamScope, runName string) error {
	held, err := s.globalNameHeld(ctx, tenantID, runName)
	if err != nil {
		return fmt.Errorf("team %q's own agent %q was not started: could not check that no other agent holds its name: %w", sc.Team, runName, err)
	}
	if !held {
		return nil
	}
	log.Printf("team %q (version %s): refusing to run its own agent %q — a global agent of the same name exists in tenant %q; rename one",
		sc.Team, sc.DefID, runName, tenantID)
	return fmt.Errorf("team %q's own agent %q cannot run: another agent named %q exists, and the two would share "+
		"agent-scoped memory and channel cursors. Rename one of them", sc.Team, runName, runName)
}

// globalNameHeld reports whether a global agent named `name` resolves in
// tenantID, with a store fault reported as one rather than read as "no".
func (s *Server) globalNameHeld(ctx context.Context, tenantID, name string) (bool, error) {
	if s.store == nil {
		_, held := lookup.StaticAgent(s.cfg(), name)
		return held, nil
	}
	_, held, err := lookup.AgentChecked(ctx, s.store, s.cfg(), tenantID, name)
	return held, err
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

// teamVersion reads the team version a scope names: its row, and its parsed
// definition.
//
// The ROW is read from the store every time. Its body never changes, but its
// flags do — `retired`, and who authored it — and a decision made on a stale
// one would be wrong in the direction that matters. The PARSE is what costs
// (it is paid by every name resolved inside a walk, global agents' included),
// so that alone is kept, in a small bounded cache, and is used only when the
// row just read carries byte-for-byte the definition that was parsed.
//
// The team must belong to runTenant, the tenant of the run that is asking. The
// scope is written only by the runtime, but it travels in run records and
// those travel in snapshots: a record that pointed a run at another tenant's
// team must not hand it that tenant's agents. (A walk never legitimately
// crosses tenants with agents of its own: op=run refuses an admin's by-def_id
// run of another tenant's team when that team declares any.)
func (s *Server) teamVersion(ctx context.Context, runTenant string, sc store.TeamScope) (store.TeamDefRow, teamgraph.Definition, error) {
	if s.store == nil {
		return store.TeamDefRow{}, teamgraph.Definition{}, fmt.Errorf("team %q: its own agents need a store to be read from", sc.Team)
	}
	if sc.Tenant != runTenant {
		return store.TeamDefRow{}, teamgraph.Definition{}, &teamVersionGoneError{fmt.Sprintf(
			"team %q (version %s) belongs to another tenant than the run, so its own agents are not this run's to use", sc.Team, sc.DefID)}
	}
	row, err := s.store.TeamDefGet(ctx, sc.DefID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return store.TeamDefRow{}, teamgraph.Definition{}, &teamVersionGoneError{fmt.Sprintf(
				"team %q: the version this run belongs to (%s) no longer exists, so its agents cannot be resolved", sc.Team, sc.DefID)}
		}
		return store.TeamDefRow{}, teamgraph.Definition{}, fmt.Errorf("team %q: read version %s: %w", sc.Team, sc.DefID, err)
	}
	if row.Name != sc.Team || row.TenantID != sc.Tenant {
		return store.TeamDefRow{}, teamgraph.Definition{}, &teamVersionGoneError{fmt.Sprintf(
			"team %q: version %s is not a version of that team", sc.Team, sc.DefID)}
	}
	def, err := s.teamDefs.parsed(row.DefID, row.Definition)
	if err != nil {
		return store.TeamDefRow{}, teamgraph.Definition{}, &teamVersionGoneError{fmt.Sprintf("team %q version %s: %v", sc.Team, sc.DefID, err)}
	}
	return row, def, nil
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
// A retired version still resolves HERE: a run in flight keeps the team
// version it started under, as it keeps a retired agent version — a paused run
// resumes on it, and a walk still running goes on spawning from it. What a
// retired version refuses is anything NEW: a walk (op=run), and a continuation
// of a session that ran inside one (continuationTeamScopeCtx).
func (s *Server) teamLocalAgent(ctx context.Context, runTenant string, sc store.TeamScope, local string) (config.AgentDef, bool, error) {
	row, def, err := s.teamVersion(ctx, runTenant, sc)
	if err != nil {
		return config.AgentDef{}, false, err
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

// maxCachedTeamDefs bounds the parsed-definition cache. A definition is at
// most the team cap (1 MiB by default) raw and a few times that parsed, so
// this is the cache's memory ceiling; a deployment walking more teams than
// this at once re-parses the least recently used.
const maxCachedTeamDefs = 32

// teamDefCache keeps the parsed form of the team versions most recently
// resolved through, keyed by def id (the layout codehook.Runner's program
// cache uses). The zero value is ready to use.
//
// An entry is used only for a row whose definition bytes equal the bytes it
// was parsed from. Rows are immutable, so that is nearly always true; it is
// checked anyway because a def id can be written again by a restore, and the
// comparison costs far less than the parse it guards.
type teamDefCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	lru     *list.List // of *cachedTeamDef, most recently used first
}

type cachedTeamDef struct {
	defID string
	raw   []byte
	def   teamgraph.Definition
}

func (c *teamDefCache) parsed(defID string, raw []byte) (teamgraph.Definition, error) {
	c.mu.Lock()
	if el, ok := c.entries[defID]; ok {
		if e := el.Value.(*cachedTeamDef); bytes.Equal(e.raw, raw) {
			c.lru.MoveToFront(el)
			c.mu.Unlock()
			return e.def, nil
		}
	}
	c.mu.Unlock()
	// Parsed outside the lock: two callers may both parse a version nobody has
	// cached yet, which costs one extra parse and blocks no one.
	def, err := teamgraph.Parse(raw)
	if err != nil {
		return teamgraph.Definition{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries, c.lru = make(map[string]*list.Element), list.New()
	}
	if el, ok := c.entries[defID]; ok {
		c.lru.Remove(el)
	}
	c.entries[defID] = c.lru.PushFront(&cachedTeamDef{defID: defID, raw: bytes.Clone(raw), def: def})
	for c.lru.Len() > maxCachedTeamDefs {
		oldest := c.lru.Back()
		c.lru.Remove(oldest)
		delete(c.entries, oldest.Value.(*cachedTeamDef).defID)
	}
	return def, nil
}

// agentRunName is the name an agent the caller wrote as `name` runs under, for
// the Agent tool: everything it reports about a child — its result envelope,
// the spawn ledger, the hooks it fires — then names a team's own agent the way
// its run row does. A name that resolves to nothing comes back unchanged, so
// the spawn itself reports it the way it always has.
func (s *Server) agentRunName(ctx context.Context, name string) (string, error) {
	_, runName, err := s.resolveAgentName(ctx, tenantFromCtx(ctx), name, nameFromAgentTool)
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

// sessionTeamScope is the team scope a session's most recent run recorded;
// ok=false when it recorded none.
//
// It reads the session's runs through RunsForSession, the one read of a
// session's runs the store has. A "latest run of a session" read would be a
// new Store method on both backends for a list that is one row per turn, on a
// path — a continuation — that already loads the session's whole transcript.
func (s *Server) sessionTeamScope(ctx context.Context, sessionID string) (store.TeamScope, bool, error) {
	if s.store == nil {
		return store.TeamScope{}, false, nil
	}
	runs, err := s.store.RunsForSession(ctx, sessionID)
	if err != nil {
		return store.TeamScope{}, false, err
	}
	if len(runs) == 0 {
		return store.TeamScope{}, false, nil
	}
	rc, _ := decodeRunConfig(runs[len(runs)-1].RunConfig) // oldest first
	sc, ok := store.TeamScopeFromContext(rc.withTeamScope(ctx))
	return sc, ok, nil
}

// sessionTeamScopeCtx is runTeamScopeCtx for a session, for a caller that only
// READS on the session's behalf (a recap). A store fault leaves no scope: a
// session of a team's own agent then finds no such agent, which is the safe
// reading of "could not tell".
func (s *Server) sessionTeamScopeCtx(ctx context.Context, sessionID string) context.Context {
	sc, _, err := s.sessionTeamScope(ctx, sessionID)
	if err != nil {
		log.Printf("team scope: session %s: could not read its runs, reading it outside any team: %v", sessionID, err)
	}
	return store.WithTeamScope(ctx, sc) // the zero scope clears
}

// errContinuationRefused is a continuation that may not start because of the
// team its session belongs to — as opposed to one that could not be checked.
var errContinuationRefused = errors.New("continuation refused")

// continuationTeamScopeCtx is the scope a CONTINUATION of a session runs in: a
// new run on a session that began inside a team is inside that team, on the
// version the session started under. That is the one way a team's own agent is
// reachable once its walk is over, and it reaches only the session that was
// already its own.
//
// A continuation is a NEW run, so it is held to what a new run is: a session
// whose team version has been RETIRED cannot be continued, exactly as a
// session of a retired agent cannot. (A run already in flight is different —
// see teamLocalAgent.) Neither can one whose version is gone, or belongs to
// another tenant than the session.
//
// It returns errContinuationRefused for those, and any other error when the
// answer could not be read: a continuation must not start on a guess, in
// either direction — not outside the team it belongs to, nor inside a version
// nobody could confirm is still in service.
func (s *Server) continuationTeamScopeCtx(ctx context.Context, sess store.Session) (context.Context, error) {
	sc, ok, err := s.sessionTeamScope(ctx, sess.ID)
	if err != nil {
		return ctx, fmt.Errorf("read the session's team: %w", err)
	}
	if !ok {
		return store.WithTeamScope(ctx, store.TeamScope{}), nil
	}
	row, _, err := s.teamVersion(ctx, sess.TenantID, sc)
	var gone *teamVersionGoneError
	switch {
	case errors.As(err, &gone):
		return ctx, fmt.Errorf("%w: %s", errContinuationRefused, err)
	case err != nil:
		return ctx, err
	case row.Retired:
		return ctx, fmt.Errorf("%w: this session ran inside team %q version %d, which has been retired", errContinuationRefused, sc.Team, row.Version)
	}
	return store.WithTeamScope(ctx, sc), nil
}

// LogTeamLocalNameClashes reports, once at boot, every active team whose own
// agent shares its full name with a global agent — in practice a static agent
// added to the operator's configuration after the team was written, which no
// authoring check could have seen. It only reports: the team's agent is
// refused when a walk tries to start it (refuseSharedName), and this line is
// how an operator learns why before a walk does. Returns how many it found.
//
// It reads every active team and makes one lookup per agent they declare, so
// its caller runs it off the boot path, under a deadline: ctx ending stops it.
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
		if ctx.Err() != nil {
			log.Printf("team local agents: name check stopped early: %v", ctx.Err())
			return found
		}
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
			if held, herr := s.globalNameHeld(ctx, row.TenantID, full); herr != nil || !held {
				continue
			}
			found++
			log.Printf("team %q (tenant %q): its own agent %q and a global agent of the same name both exist. "+
				"The team's agent will be refused when a walk starts it — rename one of them", row.Name, row.TenantID, full)
		}
	}
	return found
}
