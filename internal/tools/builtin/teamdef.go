package builtin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TeamDef is the RFC AP Phase 2 built-in tool that lets operators + tenant
// agents author, fork, promote, retire, and inspect TEAM workflow definitions
// at runtime. Structural mirror of the SkillDef tool — same six+verify
// operations, same content-addressed lineage model, same append-only storage —
// but the `definition` payload is a teamgraph workflow graph (states +
// transitions), not a skill body.
//
// Differences from SkillDef (all deliberate):
//   - No static-config layer. There is no cfg.Teams, so create never refuses a
//     "static" name and fork resolves parents ONLY from the store (pin def_id →
//     own-tenant active → shared "" active). There is no static bootstrap path.
//   - No per-agent scope gate. TeamDef authoring is gated at the HTTP route
//     (ScopeTenant) + the MCP meta-tool authz list; the tool itself does NOT
//     sub-gate by name. MVP simplification — a per-agent `team_def` capability
//     (mirroring *_def_scopes) could be layered on later without changing this
//     tool's storage or wire shape.
//   - No tools ceiling. A team has no `tools` field to narrow/widen.
//
// The graph is validated (teamgraph.Validate) at create/fork BEFORE any write —
// an invalid graph is refused with an errResult and persists nothing. The
// content_sha256 is teamgraph.Sign (name + graph, colours excluded), so two
// tenants forking the same workflow share a hash and recolouring never forks a
// def's identity.
//
// Server-stamped fields: created_at, created_by_agent_id (from
// tools.RunIdentity), tenant_id (from the authoritative principal on ctx). The
// model NEVER supplies these.
type TeamDef struct {
	// Store is the persistence backend. Required.
	Store store.Store

	// MaxDefinitionBytes caps the serialised definition JSON
	// (mirrors AgentDef/ScheduleDef). 0 = no cap.
	MaxDefinitionBytes int

	// MaxDescriptionBytes caps the free-text description field. 0 = no cap.
	MaxDescriptionBytes int

	// Spawn runs one of a team's agents and returns its output. It mirrors the
	// Agent tool's SubAgentRunner exactly, so op=run reuses the existing
	// sub-agent machinery (tenant/identity inheritance, cancel registry). Wired
	// by the server via SetTeamDefTool; nil = op=run refuses with "not configured
	// for execution" (authoring ops still work).
	Spawn teamrun.SpawnFunc

	// Admit, if set, gates op=run once before the walk: op=run is a run-trigger
	// that does NOT pass through RunOnce, so without this a DIRECT HTTP/MCP call
	// would spawn a team's agents with no RFC AW token-budget check, no RFC AX
	// operator-key restriction, and no agent-depth bound. Admit enforces those
	// and returns a ctx enriched with the restriction + an incremented depth for
	// the walk (used for every spawned agent), or an error that aborts the run.
	// nil = no admission (unit tests / authoring-only wiring).
	Admit func(ctx context.Context) (context.Context, error)

	// OperatorKeyGate reports whether the deployment restricts the operator's
	// provider key (LOOMCYCLE_OPERATOR_KEY_RESTRICTION), read at each promote
	// so the promoter's confinement is captured under the live setting. Wired
	// by SetTeamDefTool; nil = the gate is off.
	OperatorKeyGate func() bool

	// Channels, when set, is what a `starter` state reads and what a `channel`
	// state publishes to — the fifth injected collaborator, alongside Spawn,
	// Admit and Board. It is built PER RUN from the definition, because the
	// team's own channel ACL is part of that definition; hence a factory rather
	// than a value. nil means a definition containing a starter cannot run, and
	// says so at the state rather than skipping it silently.
	Channels func(ctx context.Context, d teamgraph.Definition) teamrun.ChannelIO

	// Documents, when set, is what a `starter` state with source.kind=document
	// reads its sections from. A value rather than a per-run factory like
	// Channels: nothing about the read comes from the definition beyond its
	// path, and its authority is the walk ctx it is called with. nil means
	// such a state cannot run, and says so at the state.
	Documents teamrun.DocumentReader

	// WaveContext, when set, returns a ctx carrying the wave a spawn belongs to,
	// for the seam that stamps it on the spawned run. nil only means the
	// correlation is not recorded.
	WaveContext func(ctx context.Context, walkID, waveID string, index int) context.Context

	// WalkContext, when set, returns a ctx carrying the walk state (walk id,
	// state id, state visit) every member a state spawns belongs to, for the
	// same seam. nil only means the correlation is not recorded.
	WalkContext func(ctx context.Context, walkID, state string, visit int) context.Context

	// MaxWave is the DEPLOYMENT's ceiling on one Starter wave's width — the
	// operator's bound on a spawn amplifier whose definition anyone with def
	// authority can author. 0 disables the check.
	MaxWave int

	// Board, if set, lets an op=run OPTIONALLY bind to a Document task board: when
	// the caller passes board_chunk_id, the walk persists its position onto that
	// chunk's status (chunk.status = the current team state) on every transition
	// and RESUMES from the persisted status on the next run — durable, resumable
	// progress. Satisfied by *Document (same package). nil = board binding is
	// unavailable (board_chunk_id is then refused). Wired by SetTeamDefTool.
	Board teamBoard

	// ChannelCatalog, if set, reads the operator's DECLARED channel set so
	// create/fork can refuse a definition naming a channel that does not exist,
	// and verify can report one deleted after the def was written.
	//
	// It reads the config + runtime channel rows directly rather than the ctx
	// channel POLICY, deliberately: the policy is the caller's grant, and the
	// question here is whether the channel exists at all. Reading the grant
	// would make the preflight refuse different definitions on different
	// transports. nil = the declared-set check is SKIPPED (never failed) — a
	// tool with no catalog cannot tell "undeclared" from "I have no list".
	ChannelCatalog func(ctx context.Context) map[string]tools.ChannelDef

	// Agents is the AgentDef tool whose gates a team's own agents pass at
	// create and fork (see teamdef_local.go). nil = a definition declaring
	// local agents is refused, rather than stored unchecked.
	Agents *AgentDef

	// AgentExists, if set, reports whether an agent name resolves, so verify can
	// report a member retired after the def was written. nil = the agent sweep
	// is omitted from the report rather than reported as failing.
	AgentExists func(ctx context.Context, name string) bool

	// WalkRun, if set, gives an op=run walk its OWN run: a session, a `runs`
	// row, a run id on ctx, and the Interruption policy the pause machinery
	// needs. It returns the walk's ctx, the run id, and the finish to call when
	// the walk ends — with how it ended (WalkEnd), which becomes the walk run's
	// result and status.
	//
	// WHY A WALK IS A RUN. Everything that makes a walk observable or
	// controllable from outside is addressed by run id — the breakpoint set,
	// the Interruption ask a pause is answered through, cancel, the event
	// stream. A walk that had no run had no handle, so none of them could reach
	// it. That was not a gap in any one of them; it was the walk not being a
	// first-class unit of work.
	//
	// It also fixes the thing no amount of endpoint-adding could: over HTTP,
	// op=run is synchronous, so a caller learned nothing until the walk was
	// over. With a run and `mode:"detach"` the run id comes back immediately
	// and the caller holds a handle WHILE the walk runs, which is what a
	// debugger needs and what live status will need next.
	//
	// nil = the walk runs under the caller's own ctx (an in-band agent run
	// already has a run id; a direct API call gets none, and its breakpoints
	// are unaddressable — the behaviour before this existed).
	WalkRun func(ctx context.Context, spec WalkRunSpec) (walkCtx context.Context, runID string, finish func(WalkEnd), err error)

	// LiveBreakpoints, if set, opens the MUTABLE armed set for this run's walk,
	// seeded with the run argument, and returns it plus the release to call when
	// the walk ends.
	//
	// It is what makes ad-hoc debugging possible. A breakpoint set captured at
	// dispatch only serves an operator who already knows the workflow is broken;
	// the common case is starting a run that you expect to work, watching a wave
	// go wrong, and wanting to stop before the next one. There is nothing to
	// pass at that moment, so the walk has to read an armed set something else
	// can still write to.
	//
	// targets refuses a spec this walk's definition cannot arm; the set runs it
	// on every live re-arm, so a typo is refused there as it is at the start
	// rather than arming a state the walk will never reach.
	//
	// reviewTTL is the run argument's review deadline, seeded into the set
	// beside the specs so it can be changed live too.
	//
	// nil = breakpoints are dispatch-time only (the run argument still works,
	// and nothing can arm a state once the walk has started).
	LiveBreakpoints func(ctx context.Context, seed []string, reviewTTL time.Duration, targets func(spec string) error) (teamrun.BreakpointSource, func(), error)

	// AskHuman, if set, escalates an iteration-cap overflow to a human instead of
	// aborting: when the caller passes interrupt_on_cap, a capped state raises an
	// Interruption `ask` (this closure blocks until answered/timed-out/cancelled)
	// and the answer decides continue / reroute:<state> / abort. It mirrors the
	// Spawn/Admit injection — wired by the server to the Interruption tool. nil (or
	// a run that omits interrupt_on_cap) = a cap returns the iteration_cap outcome.
	AskHuman func(ctx context.Context, question string) (answer string, err error)
}

// teamBoard is the minimal Document-board surface a board-bound op=run needs: read
// a chunk's status to resume, and set it to persist each transition. An interface
// (satisfied by *Document, same package) keeps op=run testable with a fake and
// documents exactly the two operations the walk performs against a board.
type teamBoard interface {
	GetChunkStatus(ctx context.Context, scope, chunkID string) (status string, ok bool, err error)
	SetChunkStatus(ctx context.Context, scope, chunkID, status string) error
}

const teamDefDescription = `Author, fork, promote, retire, and inspect team workflow definitions at runtime (see Context op=help topic=agent-teams). ` +
	`The definition is a state-machine graph (states with agent/parallel/consolidator/terminal handlers + ` +
	`transitions gated by success/pushback/conditional). The graph is validated before any write — an invalid ` +
	`graph (dangling transition, parallel without a consolidator, unreachable state, …) is refused and persists ` +
	`nothing. Colours are presentation-only and excluded from the content hash. Promotion is explicit — selection ` +
	`is policy, not runtime. render_diagram generates a Mermaid stateDiagram-v2 (with the colour ` +
	`scheme applied) for a stored team, or — when given an inline overlay — a dry-run preview of an unsaved ` +
	`graph (syntax-checked, not persisted). run walks a team's graph for a given input via the sub-agent ` +
	`machinery — a single agent, or a parallel fan-out whose consolidator agent selects the next edge ` +
	`(success to advance, pushback to loop back for rework) — output threads to the next state ` +
	`(a state's input_template replaces it, unless it contains {{thread.output}}, which is filled with it as data ` +
	`that is never expanded; not allowed in system_prompt), until a ` +
	`terminal state. run may OPTIONALLY bind to a Document chunk board (board_chunk_id) so progress persists ` +
	`as chunk.status and resumes across runs, and may escalate an iteration cap to a human (interrupt_on_cap) ` +
	`instead of aborting. A run is a first-class unit of work: it gets its own run_id (returned either way), so it can be ` +
	`addressed while it runs — mode=detach returns that id immediately instead of waiting for the walk. create and fork PREFLIGHT a definition's channel references — a channel the team's own ACL ` +
	`does not grant, or one that is not declared at all, is refused with the exact block to add, rather than ` +
	`failing later at the state that needed it. verify reports the content hash AND sweeps what the stored ` +
	`definition references but does not contain (channels deleted, ACL gaps, members retired) as issues[] with ` +
	`a runnable flag. A starter state dispatches a wave of agent runs, one per work item, and publishes each result to its sink ` +
	`channel. Its source is a channel (source.channel) or a document (source: {kind:"document", path:"/specs/x", ` +
	`scope:"user"|"tenant"}), whose top-level sections are the items: read once when the wave dispatches, one run per ` +
	`section with fanout.per:"chunk" (max required; more sections than max fails the walk) or one run holding them all ` +
	`with per:"once". Each run gets {document_id, chunk_id, index, title, markdown} in {{starter.message}} ` +
	`({{starter.messages}} for once), as data that is never expanded. A document source takes no ack, wait, n, wait_ms ` +
	`or batch, and is never started automatically. The entry starter may instead read the walk's own input ` +
	`(source: {kind:"input"}): a JSON array is one item per element, any other JSON value one item, and text that is ` +
	`not JSON the item {"text": <input>}; per:"message" (max required; more items than max fails the walk) or per:"once". ` +
	`It takes no channel, path, scope, select, ack, wait, n, wait_ms or batch, may carry the run form's schema, is ` +
	`never started automatically, and nothing may lead back into it (no transition, no cap reroute): route a retry to ` +
	`a later state. When the entry (an input state, or that starter) carries a schema, run checks the input against ` +
	`it before anything runs — its top-level type, its required fields, and each present property's type — and ` +
	`refuses a bad input naming the field. A definition may declare variables with defaults (vars: name → default text), ` +
	`read in prompts as ${var.<name>}; run may set a declared one for that walk (vars), and a name the team does not ` +
	`declare is refused before anything runs. run may also set breakpoints on starter states to step a fan-out wave: the walk pauses ` +
	`before dispatching (showing each composed prompt) and asks a human to release all, release n, or abort. ` +
	`run may also set review on starter, agent or parallel states (not a consolidator): ` +
	`each member run is held when it finishes, for an operator to approve, send back with feedback, or reject; a rejected ` +
	`starter member reaches the sink as status "rejected", and a rejected agent or parallel member fails like any failed member. retire soft-retires one version; delete ` +
	`hard-removes a whole team by name (all versions + active pointer), scoped to your tenant. Operations: ` +
	`create, fork, get, list, retire, delete, promote, verify, render_diagram, run.`

const teamDefInputSchema = `{
  "type": "object",
  "properties": {
    "op":            {"type": "string", "enum": ["create","fork","get","list","retire","delete","promote","verify","render_diagram","run"], "description": "Operation to perform."},
    "name":          {"type": "string", "description": "Team name (required for create/fork/list/verify/delete). A new name is one segment of A-Z a-z 0-9 _ -, at most 64 characters: no \"/\", \":\", \".\" or spaces."},
    "def_id":        {"type": "string", "description": "Existing def_id (required for get/retire/promote)."},
    "parent_def_id": {"type": "string", "description": "Fork parent (optional for fork — when absent, forks the active def of the name in your tenant, falling back to the shared \"\" base)."},
    "overlay": {
      "type": "object",
      "description": "Team workflow graph. For create/fork, top-level fields are merged per-field over the parent (slices replace wholesale); server-set fields (def_id, version, parent_def_id, created_*) are ignored if supplied. For render_diagram, supplying an overlay renders a DRY-RUN preview of the unsaved graph (syntax-checked, not persisted) instead of resolving a stored def.",
      "properties": {
        "entry":          {"type": "string", "description": "The entry state id."},
        "max_iterations": {"type": "integer", "description": "Per-state cycle cap (0 = default)."},
        "states":         {"type": "array", "items": {"type": "object"}, "description": "State nodes: each is {state, handler:{kind, agent|agents, wait?, consolidator?, ...}}. A starter handler instead carries source ({channel} or {kind:\"document\", path, scope?}), fanout ({agent|agents, per: message|chunk|once, max}), prompt, sink and binds. An input state may set publish ({channel}) to publish the walk's input to that channel as a JSON value; a channel state may set payload:\"raw\" to publish its input as a JSON value instead of the {state, output} envelope. Replaces the parent's states wholesale."},
        "transitions":    {"type": "array", "items": {"type": "object"}, "description": "Edges: each is {from, to, on}. Replaces the parent's transitions wholesale."},
        "colors":         {"type": "object", "description": "Presentation-only fills/edge colours. Excluded from the content hash."},
        "hooks":          {"type": "object", "description": "The walk's own hooks: {run_end: [entry, ...]}, fired when the walk ends. A state's handler may also carry hooks / tool_hooks, added to every run it starts."},
        "vars":           {"type": "object", "additionalProperties": {"type": "string"}, "description": "The team's variables: name → default value, read in a state's prompts as ${var.<name>}. Every walk starts with these defaults; run may set a declared one with its own vars. A default is literal text (never expanded; no {{ or }}), at most 4096 bytes, at most 64 variables. A fork that sends vars replaces the whole list; {} declares none."}
      },
      "additionalProperties": true
    },
    "description":    {"type": "string", "description": "Free-text rationale for create/fork."},
    "promote":        {"type": "boolean", "description": "create defaults true, fork defaults false."},
    "retired":        {"type": "boolean", "description": "Required for retire — set true to retire, false to un-retire."},
    "content_sha256": {"type": "string", "description": "Input for op=verify — the local content hash to compare against the active row. verify also returns runnable + issues[]: what the stored definition references but does not contain (an undeclared channel, a team-ACL gap, a retired member). Optional — verify reports the sweep with or without it."},
    "format":         {"type": "string", "enum": ["mermaid","d2"], "description": "render_diagram output format (default mermaid; d2 is deferred)."},
    "highlight_state": {"type": "string", "description": "render_diagram: optionally mark this state (e.g. a chunk's current state) with a bold outline."},
    "input":          {"type": "string", "description": "run: the initial input handed to the entry state's agent (the task/prompt the team works on)."},
    "vars":           {"type": "object", "additionalProperties": {"type": "string"}, "description": "run (optional): values for this walk's variables, name → text, read by the team's prompts as ${var.<name>}. Only a name the team's definition declares in its vars is accepted; any other is refused before anything runs, with the declared names. A value given here replaces the team's default for this walk; a vars state, a capture or a starter's binds may still overwrite it as the walk runs. Literal text: never expanded, no {{ or }}, at most 4096 bytes."},
    "board_chunk_id": {"type": "string", "description": "run (optional): bind the walk to a Document chunk task board. Each state transition persists chunk.status = the current team state (durable progress), and a later run RESUMES from the persisted status. Omit for an ephemeral run (default)."},
    "board_scope":    {"type": "string", "enum": ["agent","user"], "description": "run (optional): the Document scope of board_chunk_id (default user)."},
    "interrupt_on_cap": {"type": "boolean", "description": "run (optional): when a state hits its iteration cap, ask a human (Interruption) whether to continue / reroute:<state> / abort instead of returning the iteration_cap outcome. An unanswered/timed-out/declined ask aborts (still terminates). Default false."},
    "mode":             {"type": "string", "enum": ["detach"], "description": "run (optional): omit to wait for the walk and get its trace. \"detach\" returns {run_id, status:\"running\"} immediately and the walk continues in the background — use it when you need a handle WHILE the walk runs, to arm a breakpoint, answer a pause, or watch progress. Either way the response carries run_id."},
    "breakpoints":      {"type": "array", "items": {"type": "string"}, "description": "run (optional): debug mode. Each entry is a starter state id — \"wave\" (or \"wave:before_dispatch\") pauses the state before it dispatches: the wave is composed and nothing has run. Each pause asks a human (Interruption) to reply 'continue' (release all), 'release:<n>' (release n and pause again), or 'abort'. An unanswered/declined ask aborts. A run-time argument, never part of the definition: debugging a team must not change what the team IS. \"<state>:review\" arms review instead (see review), and may be set here or live."},
    "review":           {"type": "array", "items": {"type": "string"}, "description": "run (optional): starter, agent or parallel state ids whose member runs are held for an operator's verdict when they finish (a consolidator is not: it judges the work, it is not the work). A person approves each one, rejects it with feedback it revises from, or rejects it. A rejected starter member reaches the sink as status \"rejected\" and does not count toward the wave's wait; a rejected agent or parallel member counts as failed. Can also be armed while the walk runs, as the breakpoint \"<state>:review\". A run-time argument, never part of the definition."},
    "review_ttl_seconds": {"type": "integer", "minimum": 0, "description": "run (optional): with review, end a member hold nobody rules on within this many seconds as rejected. Omit for no deadline."}
  },
  "required": ["op"]
}`

// WalkRunSpec is what one op=run walk started with: the definition version it
// resolved to, the caller's input and the run arguments. WalkRun records it on
// the walk's run when it creates the row, so a viewer can tell which version
// ran and what it was given after the team is forked, promoted or deleted —
// the name alone resolves to whatever is active NOW.
//
// The input is the caller's raw text; whatever persists it decides how much
// of it to keep and masks it first.
type WalkRunSpec struct {
	Name          string
	DefID         string
	Version       int
	ContentSHA256 string
	// DefTenant is the definition row's tenant ("" = shared), which can differ
	// from the walk's own when an admin runs another tenant's def by id.
	DefTenant string
	// ResolvedBy is "def_id" when the caller pinned a version and "name" when
	// it took the active one.
	ResolvedBy string
	Input      string
	// Vars are the variable values the caller supplied at start, and only
	// those: the team's defaults are in the definition this spec names.
	Vars map[string]string
	// Detach is the run's mode: the caller got the run id back at once.
	Detach bool
	// Board is the Document-board binding, nil when the walk has none.
	Board            *WalkBoard
	Breakpoints      []string
	Review           []string
	ReviewTTLSeconds int
	InterruptOnCap   bool
}

// WalkEnd is how one op=run walk ended, as its run records it.
type WalkEnd struct {
	// FinalText is the walk's answer: the last output any of its states
	// produced.
	FinalText string
	// Terminal is the id of the end state the walk reached; "" when it did
	// not reach one (Err is set).
	Terminal string
	// Err is why the walk stopped short of an end state; nil when it reached
	// one.
	Err error
}

// WalkBoard is a walk's board binding as it started. ResumedFrom is the state
// the board's persisted status resumed it from; "" when it started at entry.
type WalkBoard struct {
	Scope       string
	ChunkID     string
	ResumedFrom string
}

type teamDefInput struct {
	Op             string          `json:"op"`
	Name           string          `json:"name,omitempty"`
	DefID          string          `json:"def_id,omitempty"`
	ParentDefID    string          `json:"parent_def_id,omitempty"`
	Overlay        json.RawMessage `json:"overlay,omitempty"`
	Description    string          `json:"description,omitempty"`
	Promote        *bool           `json:"promote,omitempty"`
	Retired        *bool           `json:"retired,omitempty"`
	ContentSHA256  string          `json:"content_sha256,omitempty"`     // input for op: verify
	Format         string          `json:"format,omitempty"`             // render_diagram: mermaid (default) | d2
	HighlightState string          `json:"highlight_state,omitempty"`    // render_diagram: mark a state
	Input          string          `json:"input,omitempty"`              // run: initial input to the entry state
	BoardChunkID   string          `json:"board_chunk_id,omitempty"`     // run: bind the walk to a Document chunk board
	BoardScope     string          `json:"board_scope,omitempty"`        // run: board_chunk_id's Document scope (agent|user, default user)
	InterruptOnCap bool            `json:"interrupt_on_cap,omitempty"`   // run: escalate an iteration cap to a human instead of aborting
	Breakpoints    []string        `json:"breakpoints,omitempty"`        // run: starter states to pause at (debug mode)
	Review         []string        `json:"review,omitempty"`             // run: starter/agent/parallel states whose member runs are held for a verdict
	ReviewTTL      int             `json:"review_ttl_seconds,omitempty"` // run: end an unreviewed member hold as rejected after this long
	Mode           string          `json:"mode,omitempty"`               // run: "" (wait for the walk) | "detach" (return the run id now)

	// Vars — run: values for variables the team declares, name → text.
	Vars map[string]string `json:"vars,omitempty"`
}

// Name implements tools.Tool.
func (t *TeamDef) Name() string { return "TeamDef" }

// Description implements tools.Tool.
func (t *TeamDef) Description() string { return teamDefDescription }

// InputSchema implements tools.Tool.
func (t *TeamDef) InputSchema() json.RawMessage { return json.RawMessage(teamDefInputSchema) }

// Execute implements tools.Tool.
func (t *TeamDef) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if t.Store == nil {
		return errResult("TeamDef tool: not configured (no Store backend)"), nil
	}
	var in teamDefInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult(fmt.Sprintf("invalid input JSON: %s", err)), nil
	}
	switch in.Op {
	case "create":
		return t.execCreate(ctx, in)
	case "fork":
		return t.execFork(ctx, in)
	case "get":
		return t.execGet(ctx, in)
	case "list":
		return t.execList(ctx, in)
	case "retire":
		return t.execRetire(ctx, in)
	case "delete":
		return t.execDelete(ctx, in)
	case "promote":
		return t.execPromote(ctx, in)
	case "verify":
		return t.execVerify(ctx, in)
	case "render_diagram":
		return t.execRenderDiagram(ctx, in)
	case "run":
		return t.execRun(ctx, in)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: create, fork, get, list, retire, delete, promote, verify, render_diagram, run)", in.Op)), nil
	}
}

// ---- create ----

func (t *TeamDef) execCreate(ctx context.Context, in teamDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("create: missing required field: name"), nil
	}
	if err := t.checkNewName(ctx, in.Name); err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	defJSON, err := t.buildDefinition("", in.Overlay)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	def, err := teamgraph.Parse(defJSON)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	// Validate the merged graph BEFORE any write — an invalid graph must
	// never reach storage (a broken team is silent orchestration corruption).
	if err := teamgraph.Validate(def); err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	if err := checkTeamChannelAuthority(ctx, def); err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	// Preflight AFTER the authority check: "you may not grant this" is a
	// harder refusal than "this will not work", and reporting the softer one
	// first would send the author to fix a def they are not allowed to write.
	if err := t.preflightChannels(ctx, def); err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	if err := t.checkLocalAgents(ctx, "create", in.Name, def, nil); err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	if err := t.checkSizeCaps(defJSON, in.Description); err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}

	ident := tools.RunIdentity(ctx)
	// RFC N: the tenant comes from the authoritative run identity in ctx, never
	// from tool input. "" = shared/legacy tenant. Used for the row stamp + the
	// promote — both scoped to the team's own tenant.
	tenantID := ident.TenantID
	row := store.TeamDefRow{
		DefID:            mintTeamDefID(),
		Name:             in.Name,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
		ContentSHA256:    teamgraph.Sign(in.Name, def),
		TenantID:         tenantID,
		// Who wrote this team, from the CTX rather than from anything the
		// caller supplied. CreatedByAgentID above is a run's own claim about
		// itself; this is the runtime's. A team's node prompts are expanded
		// under this flag, so it must not be assertable by the body it gates.
		OperatorAuthored: tools.IsSubstrateOperator(ctx),
	}
	created, err := t.Store.TeamDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	promote := true
	if in.Promote != nil {
		promote = *in.Promote
	}
	if promote {
		if err := t.Store.TeamDefSetActive(ctx, tenantID, in.Name, created.DefID, ident.AgentID, t.promoter(ctx)); err != nil {
			return errResult(fmt.Sprintf("create: promote: %s", err)), nil
		}
	}
	return okJSON(teamDefRowResponse(created, promote))
}

// checkNewName holds a name to the team-name grammar unless the caller's tenant
// already has a version under it. Names were only checked for non-empty before
// the grammar existed, so a team named outside it must stay writable — a new
// version under the name it already has — while no new such name is admitted.
func (t *TeamDef) checkNewName(ctx context.Context, name string) error {
	nameErr := teamgraph.ValidateName(name)
	if nameErr == nil {
		return nil
	}
	rows, err := t.Store.TeamDefListByName(ctx, name)
	if err != nil {
		return err
	}
	// TeamDefListByName returns every tenant's rows. Match the caller's tenant
	// EXACTLY, admin included: the row is written there, and another tenant's
	// team of the same name neither makes this one exist nor may be revealed.
	tenantID := tools.RunIdentity(ctx).TenantID
	for _, r := range rows {
		if r.TenantID == tenantID {
			return nil
		}
	}
	return nameErr
}

// ---- fork ----

func (t *TeamDef) execFork(ctx context.Context, in teamDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("fork: missing required field: name"), nil
	}
	// A fork lands under the CALLER's tenant, so forking the shared ("") base —
	// or, for an admin, another tenant's def — can bring a name into a tenant
	// that never held it. That is a new name there, held to the same rule.
	if err := t.checkNewName(ctx, in.Name); err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}

	// Resolve the parent from the STORE only (no static bootstrap — there is no
	// cfg.Teams). Three paths, mirroring SkillDef minus the static branch:
	//   1. parent_def_id supplied → pin
	//   2. parent_def_id empty + own-tenant active pointer → use it
	//   3. neither → fall back to the shared ("") active base, else refuse.
	// RFC N: fork resolves + stamps within the team's own tenant (from the
	// authoritative run identity, never tool input).
	ident := tools.RunIdentity(ctx)
	tenantID := ident.TenantID

	parentDefID := in.ParentDefID
	var parent store.TeamDefRow
	if parentDefID != "" {
		row, err := t.Store.TeamDefGet(ctx, parentDefID)
		if err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				return errResult(fmt.Sprintf("fork: parent_def_id %q not found", parentDefID)), nil
			}
			return errResult(fmt.Sprintf("fork: %s", err)), nil
		}
		// Allow forking the SHARED ("") base or the caller's own tenant (the fork
		// lands under the caller's tenant); refuse another specific tenant's
		// private def unless the caller is substrate:admin (crosses tenants, RFC L).
		if !forkParentVisible(ctx, row.TenantID, tenantID) {
			return errResult(fmt.Sprintf("fork: parent_def_id %q not found", parentDefID)), nil
		}
		if row.Name != in.Name {
			return errResult(fmt.Sprintf("fork: parent_def_id %q has name %q, refusing to fork under name %q", parentDefID, row.Name, in.Name)), nil
		}
		parent = row
	} else {
		row, err := t.Store.TeamDefGetActive(ctx, tenantID, in.Name)
		if err == nil {
			parent = row
			parentDefID = row.DefID
		} else {
			var nf *store.ErrNotFound
			if !errors.As(err, &nf) {
				return errResult(fmt.Sprintf("fork: %s", err)), nil
			}
			// No own-tenant active pointer. Fall back to the SHARED ("") base so a
			// per-tenant principal can fork a name seeded under the legacy "" tenant.
			// Skip when tenantID is already "" (identical lookup).
			if tenantID != "" {
				if shared, serr := t.Store.TeamDefGetActive(ctx, "", in.Name); serr == nil {
					parent = shared
					parentDefID = shared.DefID
				} else if !errors.As(serr, &nf) {
					return errResult(fmt.Sprintf("fork: %s", serr)), nil
				}
			}
			if parentDefID == "" {
				return errResult(fmt.Sprintf("fork: no parent — name %q has no DB version to fork (own tenant or shared \"\")", in.Name)), nil
			}
		}
	}

	defJSON, err := t.buildDefinition(string(parent.Definition), in.Overlay)
	if err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	def, err := teamgraph.Parse(defJSON)
	if err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	if err := teamgraph.Validate(def); err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	if err := checkTeamChannelAuthority(ctx, def); err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	if err := t.preflightChannels(ctx, def); err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	// The parent's own agents, for the one gate that compares against them. A
	// parent that no longer parses was already refused by buildDefinition.
	parentDef, _ := teamgraph.Parse(parent.Definition)
	if err := t.checkLocalAgents(ctx, "fork", in.Name, def, &parentDef); err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	if err := t.checkSizeCaps(defJSON, in.Description); err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}

	row := store.TeamDefRow{
		DefID:            mintTeamDefID(),
		Name:             in.Name,
		ParentDefID:      parentDefID,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
		ContentSHA256:    teamgraph.Sign(in.Name, def),
		TenantID:         tenantID,
		// The FORKER's ctx, never the parent row's flag. A fork may rewrite
		// every node prompt, so inheriting would launder an operator-authored
		// team into an agent-authored one that kept the authority.
		OperatorAuthored: tools.IsSubstrateOperator(ctx),
	}
	created, err := t.Store.TeamDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	promote := false
	if in.Promote != nil {
		promote = *in.Promote
	}
	if promote {
		if err := t.Store.TeamDefSetActive(ctx, tenantID, in.Name, created.DefID, ident.AgentID, t.promoter(ctx)); err != nil {
			return errResult(fmt.Sprintf("fork: promote: %s", err)), nil
		}
	}
	return okJSON(teamDefRowResponse(created, promote))
}

// ---- get / list ----

func (t *TeamDef) execGet(ctx context.Context, in teamDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("get: missing required field: def_id"), nil
	}
	row, err := t.Store.TeamDefGet(ctx, in.DefID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("get: def_id %q not found", in.DefID)), nil
		}
		return errResult(fmt.Sprintf("get: %s", err)), nil
	}
	// RFC N: def_id is a global handle but a def is owned by exactly one tenant.
	// A caller in tenant T cannot read another tenant's def — return the SAME
	// opaque not-found a missing def returns (never leak existence/body).
	if !defCallerIsAdmin(ctx) && row.TenantID != tools.RunIdentity(ctx).TenantID {
		return errResult(fmt.Sprintf("get: def_id %q not found", in.DefID)), nil
	}
	return okJSON(teamDefRowResponse(row, false))
}

func (t *TeamDef) execList(ctx context.Context, in teamDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("list: missing required field: name"), nil
	}
	rows, err := t.Store.TeamDefListByName(ctx, in.Name)
	if err != nil {
		return errResult(fmt.Sprintf("list: %s", err)), nil
	}
	// RFC N: TeamDefListByName returns rows across ALL tenants for a name.
	// Filter to the caller's own tenant so a tenant lists only its own versions.
	tenantID := tools.RunIdentity(ctx).TenantID
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		if !defCallerIsAdmin(ctx) && r.TenantID != tenantID {
			continue
		}
		out = append(out, teamDefRowResponseMap(r))
	}
	return okJSONCount(map[string]any{"name": in.Name, "versions": out}, len(out))
}

// ---- retire / promote ----

func (t *TeamDef) execRetire(ctx context.Context, in teamDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("retire: missing required field: def_id"), nil
	}
	if in.Retired == nil {
		return errResult("retire: missing required field: retired (true|false)"), nil
	}
	row, err := t.Store.TeamDefGet(ctx, in.DefID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("retire: def_id %q not found", in.DefID)), nil
		}
		return errResult(fmt.Sprintf("retire: %s", err)), nil
	}
	// RFC N: refuse cross-tenant retire. TeamDefSetRetired is a global
	// by-def_id mutation; opaque not-found — don't leak existence.
	if !defCallerIsAdmin(ctx) && row.TenantID != tools.RunIdentity(ctx).TenantID {
		return errResult(fmt.Sprintf("retire: def_id %q not found", in.DefID)), nil
	}
	if err := t.Store.TeamDefSetRetired(ctx, in.DefID, *in.Retired); err != nil {
		return errResult(fmt.Sprintf("retire: %s", err)), nil
	}
	out := map[string]any{"def_id": in.DefID, "retired": *in.Retired}
	// Name the channels this version subscribed to.
	//
	// A retired workflow stops being swept, so nothing keeps reading its
	// source — but the operator cannot SEE that from a bare {def_id, retired}.
	// An unattended subscription is the failure mode this phase is most likely
	// to produce, and the cheapest guard against it is telling the person who
	// retired the team exactly which wires it was on, so they can check that
	// nothing is still accumulating there.
	if def, perr := teamgraph.Parse(row.Definition); perr == nil {
		var sources []string
		seen := map[string]bool{}
		for _, ref := range teamgraph.ChannelRefs(def) {
			if ref.Side != teamgraph.SideSubscribe || seen[ref.Channel] {
				continue
			}
			seen[ref.Channel] = true
			sources = append(sources, ref.Channel)
		}
		if len(sources) > 0 {
			sort.Strings(sources)
			// `released` when retiring, `resumed` when un-retiring: the same
			// list means opposite things, and a caller reading one field name
			// for both would have to know the verb to interpret it.
			key := "sources_released"
			if !*in.Retired {
				key = "sources_resumed"
			}
			out[key] = sources
		}
	}
	return okJSON(out)
}

// execDelete hard-deletes a team by name (all versions + active pointer). Teams
// are runtime-only, so an operator needs to remove an obsolete/test team, not
// just retire a version. RFC N: scoped to the caller's tenant (mirrors
// DynamicAgentDelete) — a principal can't delete another tenant's team.
func (t *TeamDef) execDelete(ctx context.Context, in teamDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("delete: missing required field: name"), nil
	}
	tenantID := tools.RunIdentity(ctx).TenantID
	deleted, err := t.Store.TeamDefDelete(ctx, tenantID, in.Name)
	if err != nil {
		return errResult(fmt.Sprintf("delete: %s", err)), nil
	}
	if !deleted {
		return errResult(fmt.Sprintf("delete: team %q not found", in.Name)), nil
	}
	return okJSON(map[string]any{"name": in.Name, "deleted": true})
}

func (t *TeamDef) execPromote(ctx context.Context, in teamDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("promote: missing required field: def_id"), nil
	}
	row, err := t.Store.TeamDefGet(ctx, in.DefID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("promote: def_id %q not found", in.DefID)), nil
		}
		return errResult(fmt.Sprintf("promote: %s", err)), nil
	}
	// RFC N: refuse cross-tenant promote (opaque not-found). Belt-and-suspenders
	// with TeamDefSetActive, which also refuses when ident.TenantID ≠ row.TenantID.
	if !defCallerIsAdmin(ctx) && row.TenantID != tools.RunIdentity(ctx).TenantID {
		return errResult(fmt.Sprintf("promote: def_id %q not found", in.DefID)), nil
	}
	if err := t.checkLocalNamesFree(ctx, row); err != nil {
		return errResult(fmt.Sprintf("promote: %s", err)), nil
	}
	ident := tools.RunIdentity(ctx)
	if err := t.Store.TeamDefSetActive(ctx, ident.TenantID, row.Name, row.DefID, ident.AgentID, t.promoter(ctx)); err != nil {
		return errResult(fmt.Sprintf("promote: %s", err)), nil
	}
	return okJSON(map[string]any{"def_id": row.DefID, "name": row.Name, "promoted": true})
}

// promoter captures the confinement of whoever is promoting through ctx — the
// principal and the calling run, most restrictive wins — for the active
// pointer. A promoted team whose entry is a Starter is driven by the
// subscription sweep with no caller at all, so this is what its walks are
// confined by. Every path that sets the pointer (create, fork, promote) goes
// through here: one that did not would arm a team unrestricted.
func (t *TeamDef) promoter(ctx context.Context) store.TeamDefPromoter {
	gateOn := t.OperatorKeyGate != nil && t.OperatorKeyGate()
	return store.TeamDefPromoter{
		OperatorKeyRestricted: tools.AuthorOperatorKeyRestricted(ctx, gateOn),
		Isolated:              tools.AuthorIsolated(ctx),
	}
}

// execVerify compares a caller-supplied content_sha256 against the active row's
// (same shape as SkillDef verify): the caller passes name + a locally-computed
// hash, and the tool reports whether it matches the deployed active def.
func (t *TeamDef) execVerify(ctx context.Context, in teamDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("verify: missing required field: name"), nil
	}
	// RFC N: verify against the team's own tenant active pointer.
	row, err := t.Store.TeamDefGetActive(ctx, tools.RunIdentity(ctx).TenantID, in.Name)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return okJSON(map[string]any{
				"matches":        false,
				"current_sha256": "",
				"current_def_id": "",
				"version":        0,
				"name":           in.Name,
				"deployed":       false,
			})
		}
		return errResult(fmt.Sprintf("verify: %s", err)), nil
	}
	out := map[string]any{
		"matches":        in.ContentSHA256 != "" && in.ContentSHA256 == row.ContentSHA256,
		"current_sha256": row.ContentSHA256,
		"current_def_id": row.DefID,
		"version":        row.Version,
		"name":           row.Name,
		"deployed":       true,
	}
	// The hash answers "is this the def I wrote". It cannot answer "will it
	// still run" — a matching def whose channel was deleted or whose member was
	// retired is byte-identical and broken. So verify also sweeps what the def
	// REFERENCES but does not contain.
	//
	// The sweep NEVER turns verify into an error: the op's contract is a report,
	// and a caller comparing hashes across deployments must not start failing
	// because the far side is missing a channel. Findings ride the response.
	if def, perr := teamgraph.Parse(row.Definition); perr == nil {
		issues := t.sweepReferences(ctx, def)
		// `runnable` is reported either way — present-and-true is an answer,
		// where an absent field would be indistinguishable from a sweep that
		// did not run. `issues` appears only when there are some, so a healthy
		// team's response stays the shape callers already parse.
		//
		// An advisory issue is worth reading and stops nothing, so it does not
		// make the team unrunnable.
		runnable := true
		for _, issue := range issues {
			if advisory, _ := issue["advisory"].(bool); !advisory {
				runnable = false
			}
		}
		out["runnable"] = runnable
		if len(issues) > 0 {
			out["issues"] = issues
		}
	}
	return okJSON(out)
}

// sweepReferences reports what a stored definition names that no longer
// resolves. Each issue is a map so a canvas can render it and a human can read
// it, and each carries the state that names the thing — the only part anyone
// can act on.
func (t *TeamDef) sweepReferences(ctx context.Context, def teamgraph.Definition) []map[string]any {
	var issues []map[string]any
	catalog := t.channelCatalog(ctx)
	for _, ref := range teamgraph.ChannelRefs(def) {
		// The team ACL is def-internal, so this is checkable on every plane and
		// is the failure that actually strands workflows: a def promoted before
		// the ACL existed still validates.
		if !channelAllowed(ref.Channel, def.GrantList(ref.Side)) {
			issues = append(issues, map[string]any{
				"kind": "acl_missing", "state": ref.State, "field": ref.Field,
				"channel": ref.Channel, "side": string(ref.Side),
				"detail": fmt.Sprintf("the team's ACL does not grant %s on %q", ref.Side, ref.Channel),
			})
		}
		// Skipped, not reported as broken, when no catalog is wired — the same
		// reason the preflight skips it.
		if catalog == nil {
			continue
		}
		if _, ok := catalog[ref.Channel]; !ok {
			issues = append(issues, map[string]any{
				"kind": "channel_undeclared", "state": ref.State, "field": ref.Field,
				"channel": ref.Channel,
				"detail":  fmt.Sprintf("channel %q is no longer declared", ref.Channel),
			})
		}
	}
	if t.AgentExists != nil {
		seen := map[string]bool{}
		for _, ref := range teamgraph.AgentRefs(def) {
			if _, isLocal := teamgraph.LocalRef(ref.Agent); isLocal {
				continue // the team's own: checked against the definition below
			}
			if seen[ref.Agent] || t.AgentExists(ctx, ref.Agent) {
				seen[ref.Agent] = true
				continue
			}
			seen[ref.Agent] = true
			issues = append(issues, map[string]any{
				"kind": "agent_missing", "state": ref.State, "field": ref.Field,
				"agent":  ref.Agent,
				"detail": fmt.Sprintf("agent %q does not resolve in this tenant", ref.Agent),
			})
		}
	}
	// The team's own agents need no store: a reference is checked against the
	// definition that carries it. create and fork refuse an undeclared one, so
	// this only finds a body stored some other way.
	for _, ref := range teamgraph.AgentRefs(def) {
		name, isLocal := teamgraph.LocalRef(ref.Agent)
		if !isLocal {
			continue
		}
		if _, ok := def.LocalAgent(name); !ok {
			issues = append(issues, map[string]any{
				"kind": "local_agent_missing", "state": ref.State, "field": ref.Field,
				"agent":  ref.Agent,
				"detail": fmt.Sprintf("%q names an agent the team does not declare under local.agents", ref.Agent),
			})
		}
	}
	for _, name := range teamgraph.UnreferencedLocalAgents(def) {
		issues = append(issues, map[string]any{
			"kind": "local_agent_unreferenced", "agent": teamgraph.LocalRefPrefix + name, "advisory": true,
			"detail": fmt.Sprintf("the team declares its own agent %q and no state runs it "+
				"(an agent of the team may still start it with the Agent tool)", name),
		})
	}
	return issues
}

// execRenderDiagram generates a diagram for a team — by def_id (a specific
// version) or by name (the caller's-tenant active version). Read-only; RFC N
// tenant isolation applies (a cross-tenant def_id is an opaque not-found). Only
// Mermaid is supported today; format=d2 is deferred (RFC AP).
func (t *TeamDef) execRenderDiagram(ctx context.Context, in teamDefInput) (tools.Result, error) {
	if in.Format != "" && in.Format != "mermaid" {
		return errResult(fmt.Sprintf("render_diagram: format %q is not supported (only mermaid; d2 is deferred)", in.Format)), nil
	}

	// Dry-run preview: when an inline overlay is supplied, render (and
	// syntax-check) the UNSAVED definition without any store write. This backs
	// the Web UI editor's "refresh diagram" — an operator previews edits before
	// persisting them via create/fork. No def is read (the overlay carries the
	// whole graph), so no tenant/def_id resolution or store access is needed;
	// the same Parse+Validate create runs is applied so the check matches.
	if len(in.Overlay) > 0 {
		defJSON, err := t.buildDefinition("", in.Overlay)
		if err != nil {
			return errResult(fmt.Sprintf("render_diagram: %s", err)), nil
		}
		def, err := teamgraph.Parse(defJSON)
		if err != nil {
			return errResult(fmt.Sprintf("render_diagram: %s", err)), nil
		}
		if err := teamgraph.Validate(def); err != nil {
			return errResult(fmt.Sprintf("render_diagram: %s", err)), nil
		}
		name := in.Name
		if name == "" {
			name = "team"
		}
		return okJSON(map[string]any{
			"name":    name,
			"def_id":  "",
			"format":  "mermaid",
			"diagram": teamgraph.RenderMermaid(name, def, in.HighlightState),
			"preview": true,
		})
	}

	var row store.TeamDefRow
	var err error
	switch {
	case in.DefID != "":
		row, err = t.Store.TeamDefGet(ctx, in.DefID)
		if err == nil && !defCallerIsAdmin(ctx) && row.TenantID != tools.RunIdentity(ctx).TenantID {
			return errResult(fmt.Sprintf("render_diagram: def_id %q not found", in.DefID)), nil
		}
	case in.Name != "":
		row, err = t.Store.TeamDefGetActive(ctx, tools.RunIdentity(ctx).TenantID, in.Name)
	default:
		return errResult("render_diagram: provide `name` (active version) or `def_id`"), nil
	}
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult("render_diagram: team not found"), nil
		}
		return errResult(fmt.Sprintf("render_diagram: %s", err)), nil
	}

	def, err := teamgraph.Parse(row.Definition)
	if err != nil {
		return errResult(fmt.Sprintf("render_diagram: %s", err)), nil
	}
	diagram := teamgraph.RenderMermaid(row.Name, def, in.HighlightState)
	return okJSON(map[string]any{
		"name":    row.Name,
		"def_id":  row.DefID,
		"format":  "mermaid",
		"diagram": diagram,
	})
}

// ---- run ----

// execRun walks a team's graph for a given input: it resolves the active (or
// pinned) def in the caller's tenant, then runs each state's handler via the
// injected Spawn (the exact sub-agent machinery), threading output → input,
// until a terminal state. Handlers may be a single agent, a parallel fan-out, or
// a consolidator that selects the outgoing edge (enabling success/pushback
// routing) — see teamrun.NewAgentRunner.
//
// Two opt-in additions (RFC AP/BD), both additive — a run that sets neither is
// byte-identical to the ephemeral Phase-1 path (no board, cap → iteration_cap):
//   - board_chunk_id binds the walk to a Document chunk task board. Each state
//     transition upserts the chunk's status to the current state (durable
//     progress), and a later run RESUMES from the persisted status. The board
//     tracks POSITION only — the threaded intermediate output is NOT persisted, so
//     a resumed walk re-seeds the entry input from the caller's `input` (each
//     state re-reads its working material from the chunk the agents co-author).
//   - interrupt_on_cap escalates an iteration-cap overflow to a human via the
//     Interruption machinery (continue / reroute:<state> / abort) instead of
//     returning the iteration_cap outcome. An unanswered/declined ask aborts, so
//     the termination guarantee holds.
func (t *TeamDef) execRun(ctx context.Context, in teamDefInput) (tools.Result, error) {
	if t.Spawn == nil {
		return errResult("run: this TeamDef tool is not configured for execution (no runner wired)"), nil
	}

	// Board binding is opt-in; if requested it MUST be wired (a SQL-Memory-backed
	// Document tool), else fail loud rather than silently dropping durability.
	boardBound := in.BoardChunkID != ""
	if boardBound && t.Board == nil {
		return errResult("run: board_chunk_id set but this TeamDef tool has no Document board wired (requires SQL Memory)"), nil
	}
	boardScope := in.BoardScope
	if boardScope == "" {
		boardScope = "user"
	}

	var row store.TeamDefRow
	var err error
	switch {
	case in.DefID != "":
		row, err = t.Store.TeamDefGet(ctx, in.DefID)
		if err == nil && !defCallerIsAdmin(ctx) && row.TenantID != tools.RunIdentity(ctx).TenantID {
			return errResult(fmt.Sprintf("run: def_id %q not found", in.DefID)), nil
		}
	case in.Name != "":
		row, err = t.Store.TeamDefGetActive(ctx, tools.RunIdentity(ctx).TenantID, in.Name)
	default:
		return errResult("run: provide `name` (active version) or `def_id`"), nil
	}
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult("run: team not found"), nil
		}
		return errResult(fmt.Sprintf("run: %s", err)), nil
	}
	if row.Retired {
		return errResult(fmt.Sprintf("run: team %q is retired", row.Name)), nil
	}

	def, err := teamgraph.Parse(row.Definition)
	if err != nil {
		return errResult(fmt.Sprintf("run: %s", err)), nil
	}

	// Checked before admission and before the walk's run exists, like the
	// input below: a name the team does not declare, or a value it could not
	// have declared, is the caller's to fix and must cost neither a row nor a
	// model call. Supplied values are untrusted text and take the path every
	// other variable does from here — the task's map, then prompt assembly.
	if err := teamgraph.CheckStartVars(def, in.Vars); err != nil {
		return errValidation(fmt.Sprintf("run: %s", err),
			"send `vars` naming only variables the team's definition declares in its own `vars`, with literal text values, and run again"), nil
	}

	// Run admission (op=run bypasses RunOnce): enforce the token budget +
	// operator-key restriction + agent-depth bound, and walk under the enriched
	// ctx so every spawned agent inherits them. A refusal (over budget / too
	// deep) aborts before any agent is spawned.
	walkCtx := ctx
	if t.Admit != nil {
		walkCtx, err = t.Admit(ctx)
		if err != nil {
			return errResult(fmt.Sprintf("run: %s", err)), nil
		}
	}

	detach := in.Mode == "detach"
	if in.Mode != "" && !detach {
		return errResult(fmt.Sprintf("run: unknown mode %q (only \"detach\")", in.Mode)), nil
	}
	// Where a board-bound walk resumes is read BEFORE its run exists: the run
	// records it when the row is created, and a board that cannot be read
	// refuses the walk before a row is minted. Resume continues from the
	// chunk's persisted status when it names a state still in the current
	// graph (a graph edit that dropped that state falls back to the entry —
	// start over rather than resume into a hole).
	resumedFrom := ""
	if boardBound {
		status, ok, gerr := t.Board.GetChunkStatus(walkCtx, boardScope, in.BoardChunkID)
		if gerr != nil {
			return errResult(fmt.Sprintf("run: board: %s", gerr)), nil
		}
		if ok && status != "" {
			if _, known := teamgraph.StateByID(def, status); known {
				resumedFrom = status
			}
		}
	}
	// The input is checked against the entry's form HERE, the one point every
	// transport's op=run passes through, and before the walk's run row exists:
	// a form missing a field is the caller's to fix, so it must cost neither a
	// row nor a model call. A walk resumed past the entry hands the input to a
	// later state, which the entry's form does not describe.
	if resumedFrom == "" || resumedFrom == def.Entry {
		if err := teamgraph.CheckInput(def, in.Input); err != nil {
			return errValidation(fmt.Sprintf("run: %s", err),
				"send an input that matches the team's input form (the `schema` on its entry state) and run again"), nil
		}
	}
	// The walk becomes a RUN — after admission, so a refused request never
	// mints a row. From here on walkCtx carries the run id, which is what makes
	// the walk addressable: breakpoints, the Interruption ask a pause is
	// answered through, and cancel all key on it.
	runID := ""
	finishRun := func(WalkEnd) {}
	if t.WalkRun != nil {
		var werr error
		// The walk's own hooks go to whatever opens its run; an operator's
		// definition is what may let them count as the operator's.
		walkCtx = teamrun.WithWalkHooks(walkCtx, teamrun.WalkHooks{Hooks: def.Hooks, OperatorAuthored: row.OperatorAuthored, Tenant: row.TenantID})
		spec := WalkRunSpec{
			Name:             row.Name,
			DefID:            row.DefID,
			Version:          row.Version,
			ContentSHA256:    row.ContentSHA256,
			DefTenant:        row.TenantID,
			ResolvedBy:       "name",
			Input:            in.Input,
			Vars:             in.Vars,
			Detach:           detach,
			Breakpoints:      in.Breakpoints,
			Review:           in.Review,
			ReviewTTLSeconds: in.ReviewTTL,
			InterruptOnCap:   in.InterruptOnCap,
		}
		if in.DefID != "" {
			spec.ResolvedBy = "def_id"
		}
		if boardBound {
			spec.Board = &WalkBoard{Scope: boardScope, ChunkID: in.BoardChunkID, ResumedFrom: resumedFrom}
		}
		walkCtx, runID, finishRun, werr = t.WalkRun(walkCtx, spec)
		if werr != nil {
			return errResult(fmt.Sprintf("run: %s", werr)), nil
		}
	} else if detach {
		// Refused, not silently run inline: a caller that asked for a handle
		// and got a completed walk instead has no way to notice.
		return errResult("run: mode=detach requires run tracking, which is not wired on this server"), nil
	}

	task := &teamrun.Task{Input: in.Input}
	// ONE HANDLE FOR THE WHOLE WALK. teamrun mints a walk id only when the task
	// does not carry one, so the walk's own run id becomes the correlation key
	// stamped on every run it spawns.
	//
	// Without this there were two ids and no way to get from one to the other:
	// a caller held the run_id that mode=detach returned, while the spawned
	// runs carried an internal `wlk_…` nobody had ever seen. Watching a walk
	// meant knowing an id the API never handed out. Now `run_id` is the answer
	// to "which walk is this" everywhere — the response, the run row, the
	// parent_context of every agent the walk starts, and the stream filter.
	if runID != "" {
		task.WalkID = runID
	}
	// What the caller supplied goes on the task first; the walk then fills in
	// the team's default for every declared name still missing, and anything a
	// state binds while it runs overwrites both.
	for name, value := range in.Vars {
		task.SetVar(name, value)
	}

	// Assemble walk options. When neither feature is used, opts is empty and Walk
	// runs with no options → the ephemeral Phase-1 behaviour is byte-identical.
	var opts []teamrun.Option
	if boardBound {
		// Tag every handler run this walk spawns with the board task, so a client
		// folding the run-state stream can pin the live agent to this chunk's card
		// (RFC BT P4). Only the DIRECT handler run is tagged — the sub-agent spawn
		// path clears it so a handler's own sub-agents aren't pinned onto the card.
		walkCtx = store.WithBoardTask(walkCtx, store.BoardTask{Scope: boardScope, ChunkID: in.BoardChunkID})
		if resumedFrom != "" {
			task.State = resumedFrom
		}
		opts = append(opts, teamrun.OnEnterState(func(c context.Context, state string) error {
			if serr := t.Board.SetChunkStatus(c, boardScope, in.BoardChunkID, state); serr != nil {
				return fmt.Errorf("board: %w", serr)
			}
			return nil
		}))
	}

	interruptions := 0
	lastDecision := ""
	if in.InterruptOnCap && t.AskHuman != nil {
		opts = append(opts, teamrun.OnCap(func(c context.Context, capErr *teamrun.ErrIterationCap) (teamrun.CapDecision, error) {
			interruptions++
			q := fmt.Sprintf(
				"Team %q: state %q hit its iteration cap (%d entries > max %d). "+
					"Reply `continue` to grant another %d-iteration window, `reroute:<state>` to jump to another state, or `abort` to stop.",
				row.Name, capErr.State, capErr.Count, capErr.Max, capErr.Max)
			answer, aerr := t.AskHuman(c, q)
			if aerr != nil {
				// Interruption unavailable / timed out / cancelled / declined →
				// abort. Preserves the termination guarantee (a failed escalation
				// never loops).
				lastDecision = "abort"
				return teamrun.CapDecision{Action: teamrun.CapAbort}, nil
			}
			dec := parseCapAnswer(answer)
			// A reroute to an unknown state degrades to abort (fail safe) so a
			// human typo can't send the walk into a non-existent state.
			if dec.Action == teamrun.CapReroute {
				if _, known := teamgraph.StateByID(def, dec.Reroute); !known {
					lastDecision = "abort"
					return teamrun.CapDecision{Action: teamrun.CapAbort}, nil
				}
			}
			lastDecision = capActionLabel(dec)
			return dec, nil
		}))
	}

	// Breakpoints are a RUN argument, deliberately: a `debug: true` in a
	// definition would change its content hash, so turning the debugger on would
	// fork the workflow — and the thing being debugged would not be the thing
	// that runs in production.
	breaks := 0
	lastBreak := ""
	var runnerOpts []teamrun.RunnerOption
	// review is sugar for "<state>:review" in the same armed set, so arming it
	// at start and arming it live are one mechanism with one reader.
	seed := append([]string(nil), in.Breakpoints...)
	for _, id := range in.Review {
		seed = append(seed, id+":"+string(teamrun.Review))
	}
	// Only a debug PAUSE needs a human to ask. Review does not: its verdict
	// arrives through each member run's own review verb.
	needsAsk := false
	for _, bp := range in.Breakpoints {
		if _, phase, ok := teamrun.ParseBreakpoint(bp); ok && phase != teamrun.Review {
			needsAsk = true
		}
	}
	// The walk's run is already open, so a refusal from here on closes it: a
	// refused walk otherwise stayed running, its heartbeat beating, until the
	// stale sweeper failed it.
	refuse := func(msg string) (tools.Result, error) {
		finishRun(WalkEnd{Err: errors.New(msg)})
		return errResult(msg), nil
	}
	if len(seed) > 0 {
		if err := teamrun.ValidateBreakpoints(seed); err != nil {
			return refuse(fmt.Sprintf("run: %s", err))
		}
		if err := teamrun.CheckBreakpointTargets(def, row.Name, seed); err != nil {
			return refuse(fmt.Sprintf("run: %s", err))
		}
		// REFUSED rather than degraded. interrupt_on_cap may silently fall back
		// to aborting because the fallback is still safe; a breakpoint's whole
		// job is to hold work back, so running at full speed because nobody can
		// be asked is the opposite of what the caller requested.
		if needsAsk && t.AskHuman == nil {
			return refuse("run: breakpoints require the Interruption machinery, which is not wired on this server")
		}
	}
	// The armed set is opened for EVERY run that could be asked, not only one
	// that passed `breakpoints`. A run that starts with none is exactly the run
	// an operator later wants to debug, and if the set only existed when it was
	// seeded there would be nothing for them to arm.
	//
	// An empty set answers false for every state, so a walk nobody arms takes
	// the same path it took before any of this existed.
	releaseBreakpoints := func() {}
	var armedSet teamrun.BreakpointSource
	// Review arming lives in the same set, so the set opens for a run that can
	// be armed live (LiveBreakpoints) or that armed anything at start — review
	// included, in either spelling — not only for one that can pause.
	if t.AskHuman != nil || t.LiveBreakpoints != nil || len(seed) > 0 {
		// Opened on the WALK ctx, which now carries the run id — that is the key
		// the arming endpoint addresses. Opening it on the caller's ctx would
		// register the set under whatever run the CALLER is in, or under none.
		targets := func(spec string) error {
			return teamrun.CheckBreakpointTargets(def, row.Name, []string{spec})
		}
		reviewTTL := time.Duration(in.ReviewTTL) * time.Second
		src, release, serr := t.openBreakpoints(walkCtx, seed, reviewTTL, targets)
		if serr != nil {
			return refuse(fmt.Sprintf("run: %s", serr))
		}
		armedSet = src
		// NOT a defer: a detached walk outlives this function, and releasing
		// here would unregister the armed set the moment the caller got its run
		// id back — leaving a running walk nobody could arm.
		releaseBreakpoints = release
		runnerOpts = append(runnerOpts, teamrun.WithMemberReview(src, reviewTTL))
	}
	if armedSet != nil && t.AskHuman != nil {
		runnerOpts = append(runnerOpts, teamrun.WithBreakpoints(armedSet,
			func(c context.Context, bp teamrun.Breakpoint) (teamrun.BreakDecision, error) {
				breaks++
				answer, aerr := t.AskHuman(c, formatBreakpoint(row.Name, bp))
				if aerr != nil {
					// Unavailable / timed out / cancelled / declined → abort.
					// A debugger nobody can answer must not release the wave.
					lastBreak = "abort"
					return teamrun.BreakDecision{Action: teamrun.BreakAbort}, nil
				}
				dec := parseBreakAnswer(answer)
				lastBreak = breakActionLabel(dec)
				return dec, nil
			}))
	}
	if t.Channels != nil {
		// Built per run: the executor closes over THIS definition's ACL, which
		// is what makes the team the ACL subject for its source and sink.
		if io := t.Channels(walkCtx, def); io != nil {
			runnerOpts = append(runnerOpts, teamrun.WithChannels(io))
		}
	}
	if t.Documents != nil {
		runnerOpts = append(runnerOpts, teamrun.WithDocuments(t.Documents))
	}
	if t.WaveContext != nil {
		runnerOpts = append(runnerOpts, teamrun.WithWaveContext(t.WaveContext))
	}
	if t.WalkContext != nil {
		runnerOpts = append(runnerOpts, teamrun.WithWalkContext(t.WalkContext))
	}
	if t.MaxWave > 0 {
		runnerOpts = append(runnerOpts, teamrun.WithMaxWave(t.MaxWave))
	}
	// From the DEF ROW, not from ctx and not from the definition body. The row
	// is where the runtime recorded who wrote this team; the body is the thing
	// the flag gates, so a team that could assert its own authorship would be
	// asserting its own authority.
	runnerOpts = append(runnerOpts, teamrun.WithOperatorAuthored(row.OperatorAuthored),
		teamrun.WithTeamSource(row.Name, row.TenantID))
	runner := teamrun.NewAgentRunner(t.Spawn, runnerOpts...)
	walk := func() ([]teamrun.StepRecord, error) {
		defer releaseBreakpoints()
		trace, werr := teamrun.Walk(walkCtx, def, task, runner, opts...)
		end := WalkEnd{FinalText: walkFinalOutput(trace), Err: werr}
		if werr == nil {
			end.Terminal = walkTerminal(def, task)
		}
		finishRun(end)
		return trace, werr
	}

	if detach {
		// The caller gets its handle NOW and the walk continues behind it. This
		// is the whole point of the mode: a synchronous op=run tells the caller
		// nothing until it is over, so there is no moment at which it can arm a
		// breakpoint, read a pause, or watch progress.
		//
		// walkCtx is already detached from the request by WalkRun, so the walk
		// survives this handler returning; only cancel stops it.
		go func() {
			if _, werr := walk(); werr != nil {
				log.Printf("teamdef: detached walk %q (run %s): %v", row.Name, runID, werr)
			}
		}()
		return okJSON(map[string]any{
			"name":   row.Name,
			"def_id": row.DefID,
			"run_id": runID,
			"status": "running",
		})
	}

	trace, walkErr := walk()

	steps := make([]map[string]any, 0, len(trace))
	for _, s := range trace {
		steps = append(steps, map[string]any{
			"state":  s.State,
			"agent":  s.Agent,
			"edge":   s.Edge,
			"next":   s.Next,
			"output": s.Output,
		})
	}

	// annotate adds the opt-in board/interruption fields to a response ONLY when
	// the relevant feature was used, keeping the default ephemeral response shape
	// byte-identical for existing callers.
	annotate := func(m map[string]any) map[string]any {
		if runID != "" {
			// Reported on the synchronous path as well: a caller holding a
			// second connection can still arm this walk while it runs, and the
			// id is what every run surface keys on afterwards.
			m["run_id"] = runID
		}
		if boardBound {
			m["board_chunk_id"] = in.BoardChunkID
			m["board_scope"] = boardScope
			if resumedFrom != "" {
				m["resumed_from"] = resumedFrom
			}
		}
		if in.InterruptOnCap {
			m["interruptions"] = interruptions
			if lastDecision != "" {
				m["cap_decision"] = lastDecision
			}
		}
		// Reported when the walk actually paused — not when `breakpoints` was
		// passed. A walk armed mid-run passed nothing and still paused, and a
		// walk that was armed but never reached the state paused nothing.
		if breaks > 0 {
			m["breakpoints_hit"] = breaks
			if lastBreak != "" {
				m["break_decision"] = lastBreak
			}
		}
		return m
	}

	if walkErr != nil {
		var capErr *teamrun.ErrIterationCap
		if errors.As(walkErr, &capErr) {
			// Cap overflow is a first-class outcome, not a tool fault — report it
			// with the trace so the caller sees how far the walk got. (When
			// interrupt_on_cap escalated, this is the human's abort / a failed
			// escalation; continue/reroute keep the walk going and don't land here.)
			return okJSON(annotate(map[string]any{
				"name":            row.Name,
				"def_id":          row.DefID,
				"status":          "iteration_cap",
				"capped_state":    capErr.State,
				"max_iterations":  capErr.Max,
				"iteration_count": capErr.Count,
				"steps":           steps,
			}))
		}
		return errResult(fmt.Sprintf("run: %s", walkErr)), nil
	}

	return okJSON(annotate(map[string]any{
		"name":         row.Name,
		"def_id":       row.DefID,
		"status":       "completed",
		"final_state":  task.State,
		"final_output": task.Input, // Walk threads the last handler's output here
		"steps":        steps,
	}))
}

// parseCapAnswer maps a human's free-text cap answer to a walk decision. Only an
// explicit "continue" or "reroute:<state>" proceeds; everything else — "abort",
// empty, or an unrecognised reply — is abort, so the default is always to stop
// (the termination guarantee). The reroute target keeps its original case (it's a
// state id); only the keyword match is case-insensitive.
func parseCapAnswer(answer string) teamrun.CapDecision {
	trimmed := strings.TrimSpace(answer)
	switch {
	case strings.EqualFold(trimmed, "continue"):
		return teamrun.CapDecision{Action: teamrun.CapContinue}
	case strings.HasPrefix(strings.ToLower(trimmed), "reroute"):
		target := strings.TrimSpace(trimmed[len("reroute"):])
		target = strings.TrimSpace(strings.TrimPrefix(target, ":"))
		if target == "" {
			return teamrun.CapDecision{Action: teamrun.CapAbort}
		}
		return teamrun.CapDecision{Action: teamrun.CapReroute, Reroute: target}
	default:
		return teamrun.CapDecision{Action: teamrun.CapAbort}
	}
}

// capActionLabel is the human-readable decision recorded on the run response.
func capActionLabel(d teamrun.CapDecision) string {
	switch d.Action {
	case teamrun.CapContinue:
		return "continue"
	case teamrun.CapReroute:
		return "reroute:" + d.Reroute
	default:
		return "abort"
	}
}

// openBreakpoints returns the armed set the walk will consult, plus the release
// to call when it ends.
//
// With LiveBreakpoints wired the set is MUTABLE and addressable while the walk
// runs; without it the run argument still works exactly as before, fixed at
// dispatch. Degrading rather than refusing is right here — the dispatch-time
// behaviour is the feature this replaces, not a broken half of it.
func (t *TeamDef) openBreakpoints(ctx context.Context, seed []string, reviewTTL time.Duration, targets func(spec string) error) (teamrun.BreakpointSource, func(), error) {
	if t.LiveBreakpoints != nil {
		return t.LiveBreakpoints(ctx, seed, reviewTTL, targets)
	}
	src, err := teamrun.NewStaticBreakpoints(seed)
	if err != nil {
		return nil, nil, err
	}
	return src, func() {}, nil
}

// maxBreakPreview bounds ONE previewed prompt or output in the question text.
// An agent's output can be a whole document; a human deciding "release this or
// not" needs enough to recognise it, and an Interruption ask that carries a
// megabyte helps nobody.
const maxBreakPreview = 600

// formatBreakpoint renders a pause as the question a human answers.
//
// The wave's width is stated on every pause so "3 of 8" means the same thing at
// each step of a staged release, and each pending entry is listed with its wave
// index — the index is what the operator matches against the sink messages and
// the run list afterwards.
func formatBreakpoint(team string, bp teamrun.Breakpoint) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Team %q: state %q paused BEFORE dispatching wave %s (%d runs in the wave, %d pending).\n",
		team, bp.State, bp.Wave, bp.WaveSize, bp.Pending)
	for _, p := range bp.Prompts {
		fmt.Fprintf(&b, "[%d] %s ← %s\n", p.Index, p.Agent, truncPreview(p.Message))
	}
	fmt.Fprintf(&b, "Reply `continue` to dispatch all %d, `release:<n>` to dispatch the first n and pause again, or `abort` to stop the walk.",
		bp.Pending)
	return b.String()
}

// truncPreview bounds one previewed body and says so, rather than silently
// handing a human a prefix they would read as the whole thing.
func truncPreview(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(empty)"
	}
	if len(s) <= maxBreakPreview {
		return s
	}
	return s[:maxBreakPreview] + fmt.Sprintf("… (+%d bytes)", len(s)-maxBreakPreview)
}

// parseBreakAnswer maps a human's free-text breakpoint answer to a decision.
// Mirrors parseCapAnswer: only an explicit continue or release proceeds, and
// everything else — "abort", empty, an unrecognised reply — stops. A debugger
// that defaulted to releasing would be a debugger you cannot trust to hold.
func parseBreakAnswer(answer string) teamrun.BreakDecision {
	trimmed := strings.TrimSpace(answer)
	switch {
	case strings.EqualFold(trimmed, "continue"), strings.EqualFold(trimmed, "all"):
		return teamrun.BreakDecision{Action: teamrun.BreakContinue}
	case strings.HasPrefix(strings.ToLower(trimmed), "release"):
		rest := strings.TrimSpace(trimmed[len("release"):])
		rest = strings.TrimSpace(strings.TrimPrefix(rest, ":"))
		if rest == "" {
			// "release" with no number is one step — the same reading the
			// runner gives an N below 1.
			return teamrun.BreakDecision{Action: teamrun.BreakRelease, N: 1}
		}
		n, err := strconv.Atoi(rest)
		if err != nil || n < 1 {
			return teamrun.BreakDecision{Action: teamrun.BreakAbort}
		}
		return teamrun.BreakDecision{Action: teamrun.BreakRelease, N: n}
	default:
		return teamrun.BreakDecision{Action: teamrun.BreakAbort}
	}
}

// breakActionLabel is the human-readable decision recorded on the run response.
func breakActionLabel(d teamrun.BreakDecision) string {
	switch d.Action {
	case teamrun.BreakContinue:
		return "continue"
	case teamrun.BreakRelease:
		return fmt.Sprintf("release:%d", d.N)
	default:
		return "abort"
	}
}

// ---- helpers ----

// buildDefinition parses the base definition (parent's JSON for fork; empty for
// create) into a teamgraph.Definition, applies the overlay per top-level field,
// and returns the merged definition marshalled back to JSON. Slices replace
// wholesale — a team graph is cohesive; states/transitions are NOT
// element-merged. The returned bytes are exactly what create/fork parse +
// validate + persist (validate-what-you-store).
func (t *TeamDef) buildDefinition(parentJSON string, overlay json.RawMessage) (json.RawMessage, error) {
	base := teamgraph.Definition{}
	if parentJSON != "" {
		parsed, err := teamgraph.Parse([]byte(parentJSON))
		if err != nil {
			return nil, fmt.Errorf("parse parent definition: %w", err)
		}
		base = parsed
	}
	if len(overlay) > 0 {
		ov, err := teamgraph.Parse(overlay)
		if err != nil {
			return nil, fmt.Errorf("parse overlay: %w", err)
		}
		applyTeamOverlay(&base, ov)
	}
	merged, err := json.Marshal(base)
	if err != nil {
		return nil, fmt.Errorf("marshal merged definition: %w", err)
	}
	return merged, nil
}

// applyTeamOverlay merges ov over base per top-level field. Scalars set-if-set;
// slices/maps replace wholesale (never element-merged) since the graph is a
// cohesive unit.
// checkTeamChannelAuthority enforces trust rule 4 on `Definition.Channels`:
// the workflow's ACL may only NARROW what the authoring principal already
// holds. Inherit, never widen.
//
// This is the half of the team ACL that matters. The field itself is only data;
// what makes it authority is that the Starter reads and publishes under it
// rather than under each agent's own grants, so an author who could write a
// channel into it that they cannot reach themselves would have escalated by
// authoring a definition. Checked at create AND fork, because a fork is an
// authoring act by whoever forks, not by whoever wrote the parent.
//
// An author with NO channel policy at all can declare no channels — the same
// default-deny every other channel surface applies, rather than "no policy
// means no limit".
func checkTeamChannelAuthority(ctx context.Context, def teamgraph.Definition) error {
	if def.Channels == nil {
		return nil
	}
	pol := tools.ChannelPolicy(ctx)
	for _, side := range []struct {
		name string
		want []string
	}{
		{"publish", def.Channels.Publish},
		{"subscribe", def.Channels.Subscribe},
	} {
		all, granted := pol.GrantsFor(side.name)
		if all {
			continue // the plane holds every channel; there is nothing to narrow from
		}
		for _, ch := range side.want {
			if !channelAllowed(ch, granted) {
				return fmt.Errorf("channels.%s: %q is not in the authoring principal's own %s allowlist — "+
					"a team ACL may only narrow what its author holds, never widen it", side.name, ch, side.name)
			}
		}
	}
	return nil
}

func applyTeamOverlay(base *teamgraph.Definition, ov teamgraph.Definition) {
	if ov.Entry != "" {
		base.Entry = ov.Entry
	}
	if ov.MaxIterations != 0 {
		base.MaxIterations = ov.MaxIterations
	}
	if ov.States != nil {
		base.States = ov.States
	}
	if ov.Transitions != nil {
		base.Transitions = ov.Transitions
	}
	if ov.Colors != nil {
		base.Colors = ov.Colors
	}
	// Layout is how a canvas persists node positions, so a missing case here is
	// not a cosmetic gap: the fork succeeds and every position is silently gone.
	// Presentation, like Colors — and like Colors it is excluded from the content
	// hash, so a layout-only fork keeps the parent's identity.
	if ov.Layout != nil {
		base.Layout = ov.Layout
	}
	// Every Definition field needs a case here or a fork returns 200, mints a
	// version and silently drops it — which is exactly what happened to Layout
	// when it was added. Channels is the ACL, so dropping it would hand a
	// forked workflow no channel authority and fail at run time instead.
	if ov.Channels != nil {
		base.Channels = ov.Channels
	}
	if ov.Hooks != nil {
		base.Hooks = ov.Hooks
	}
	// Wholesale, like every map here: a fork that sends `vars` states the whole
	// declared list, so `{}` is how one declares none.
	if ov.Vars != nil {
		base.Vars = ov.Vars
	}
	// Per KIND, wholesale within one: a fork that sends `local.agents` states
	// the team's whole list of agents (`{}` declares none), and one that sends
	// a `local` block without that kind leaves the parent's agents alone — so
	// a kind added later is replaced independently of this one.
	if ov.Local != nil {
		if base.Local == nil {
			base.Local = &teamgraph.Local{}
		}
		if ov.Local.Agents != nil {
			base.Local.Agents = ov.Local.Agents
		}
	}
}

func (t *TeamDef) checkSizeCaps(defJSON []byte, description string) error {
	if t.MaxDefinitionBytes > 0 && len(defJSON) > t.MaxDefinitionBytes {
		return fmt.Errorf("definition (%d bytes) exceeds max %d", len(defJSON), t.MaxDefinitionBytes)
	}
	if t.MaxDescriptionBytes > 0 && len(description) > t.MaxDescriptionBytes {
		return fmt.Errorf("description (%d bytes) exceeds max %d", len(description), t.MaxDescriptionBytes)
	}
	return nil
}

// teamDefRowResponse + Map shape the tool's reply envelope (mirror of
// skillDefRowResponse). bootstrapped_from_static is always false for teams
// (no static layer) but included so the response shape matches the sibling
// def-family tools + the Library UI.
func teamDefRowResponse(row store.TeamDefRow, promoted bool) map[string]any {
	m := teamDefRowResponseMap(row)
	m["promoted"] = promoted
	return m
}

func teamDefRowResponseMap(row store.TeamDefRow) map[string]any {
	return map[string]any{
		"def_id":                   row.DefID,
		"name":                     row.Name,
		"version":                  row.Version,
		"parent_def_id":            row.ParentDefID,
		"description":              row.Description,
		"created_at":               row.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		"created_by_agent_id":      row.CreatedByAgentID,
		"retired":                  row.Retired,
		"bootstrapped_from_static": row.BootstrappedFromStatic,
		"content_sha256":           row.ContentSHA256,
		"definition":               row.Definition,
	}
}

// mintTeamDefID returns a fresh opaque ID for a new row. Same 64-bit-entropy
// shape as mintSkillDefID but with the "tdf_" prefix so team defs never collide
// with agent/skill defs in logs / grep output.
func mintTeamDefID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "tdf_" + hex.EncodeToString(b[:])
}

// walkFinalOutput is what a walk answered: the last output any of its states
// produced. A walk ends at a terminal state, which produces nothing itself, so
// the answer is the output that was threaded INTO it.
func walkFinalOutput(trace []teamrun.StepRecord) string {
	for i := len(trace) - 1; i >= 0; i-- {
		if trace[i].Output != "" {
			return trace[i].Output
		}
	}
	return ""
}

// walkTerminal names the end state a walk that returned without error stopped
// at. Walk leaves task.State there, including for a walk that started at it
// and so has no step to read it from. It is checked against the definition
// rather than trusted, so a state that is not a terminal is never reported as
// the walk's end.
func walkTerminal(def teamgraph.Definition, task *teamrun.Task) string {
	if st, ok := teamgraph.StateByID(def, task.State); ok && st.Handler.Kind == teamgraph.HandlerTerminal {
		return st.ID
	}
	return ""
}

// ValidateTeamDefBody re-runs, over a stored team def body, the authoring
// checks on its hooks: it parses as a team definition, and the walk's and each
// state's hooks are well formed (inline webhooks included), and so is each
// agent the team declares for itself. A snapshot restore calls it before
// writing a body. The graph itself is not re-validated — a team dials nothing
// of its own — and neither are the caller-dependent checks: channel authority,
// and the gates a local agent passes when it is authored (see
// ValidateAgentDefBody for why a restore has no authoring caller).
func ValidateTeamDefBody(body json.RawMessage) error {
	def, err := teamgraph.Parse(body)
	if err != nil {
		return err
	}
	// The team's own agents are agent definitions and are held to what a
	// restored agent def is: it decodes, and its hooks are well formed.
	for _, name := range def.LocalAgentNames() {
		agentDef, err := LocalAgentDefinition(def.Local.Agents[name])
		if err == nil {
			err = ValidateAgentDefBody(agentDef)
		}
		if err != nil {
			return fmt.Errorf("local.agents[%q]: %w", name, err)
		}
	}
	return teamgraph.ValidateHooks(def)
}
