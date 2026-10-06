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
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
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

	// MaxDefinitionBytes caps the serialised definition JSON, the team's own
	// agents included (LOOMCYCLE_TEAM_DEF_MAX_DEFINITION_BYTES). 0 = no cap.
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

	// Skills is the SkillDef tool whose gates a team's own skills pass at
	// create and fork (see teamdef_local.go). nil = a definition declaring
	// local skills is refused, rather than stored unchecked.
	Skills *SkillDef

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

	// ArmWalkTriggers starts what a walk carries that wakes it from inside
	// the team — today the team's own schedules — on walkCtx, and returns
	// the disarm. op=run calls it once every refusal is behind it, and calls
	// the disarm first thing when the walk ends, on every path, before the
	// walk's run is recorded as over: a team's own trigger is alive only
	// while a walk of the team is, and nothing it does may land on a walk
	// already finished. The disarm must not return until every trigger has
	// stopped.
	//
	// An error refuses the walk (a trigger that cannot run would leave the
	// walk waiting on something that never comes). nil = a team that
	// declares a trigger of its own is refused: the walk could not be woken.
	ArmWalkTriggers func(walkCtx context.Context, def teamgraph.Definition) (disarm func(), err error)

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

	// LiveChildren bounds the children each run has alive at once — the
	// registry the Agent tool admits against. A walk run in poll mode is one
	// of its caller's children and holds a slot until it ends. nil admits
	// everything.
	LiveChildren *tools.LiveChildren

	// PollWaitCapMs bounds how long one poll with wait "any" or "all" may
	// block — the Agent poll cap (LOOMCYCLE_AGENT_POLL_WAIT_CAP_MS). 0 =
	// DefaultPollWaitCapMs.
	PollWaitCapMs int
}

// teamWalkLabel prefixes the label a walk's run is filed under (team:<name>),
// which is also how a poll-mode walk is named in its caller's background
// table and in the notes that report it.
const teamWalkLabel = "team:"

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
	`addressed while it runs — mode=detach returns that id immediately instead of waiting for the walk. ` +
	`mode=poll (inside an agent's run) also returns {run_id, state:"running"} at once, and runs the walk as a background ` +
	`child of your run: you keep working, are told when it ends, and read its answer — the fields a waited-for run returns — ` +
	`with poll (run_ids, wait none|any|all, wait_ms). It is cancelled with your run, and your run does not end while it is ` +
	`still running; cancel (run_ids) ends it sooner. create and fork PREFLIGHT a definition's channel references — a channel the team's own ACL ` +
	`does not grant, or one that is not declared at all, is refused with the exact block to add, rather than ` +
	`failing later at the state that needed it. verify reports the content hash AND sweeps what the stored ` +
	`definition references but does not contain (channels deleted, ACL gaps, members retired) as issues[] with ` +
	`a runnable flag. verify with an overlay checks an UNSAVED draft without writing anything: it runs every check ` +
	`create or fork would, plus that sweep, and returns ALL the problems at once — valid (a save would be accepted), ` +
	`runnable, content_sha256 and issues[], each with a severity (refused | unrunnable | advisory) and the JSON path of ` +
	`the value at fault. It checks the draft as the save would (a fork when the name has a version, else a create); ` +
	`as:"create"|"fork" overrides that. A starter state dispatches a wave of agent runs, one per work item, and publishes each result to its sink ` +
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
	`declare is refused before anything runs. A definition may also declare agents of its own (local: {agents: {name: <the overlay AgentDef create takes>}}) ` +
	`and run one from a state as ./<name>; any other name in a state is a global agent, even when the team declares one of that name, and a ./<name> the team does not declare is refused. Such an agent ` +
	`exists only in the team, runs as <team>/<name>, cannot be started outside a walk of the team, and passes every check a new agent does ` +
	`(your agent-authoring grant, your own tools as its ceiling) at create and at fork; its full name must not be an existing agent's. ` +
	`The team may declare skills of its own the same way (local: {skills: {name: {body, description, tools}}}); one of the team's own agents ` +
	`is granted one by listing ./<name> in its skills (only that exact spelling grants it — no pattern does), and loads it with the Skill tool as ./<name>. ` +
	`Each skill passes the checks a new skill does, as <team>/<name> (your skills allowlist, your own tools as its ceiling), and its tools must be ` +
	`within the tools of every team agent granted it. ` +
	`It may declare channels of its own too (local: {channels: {name: {scope: tenant|user, default_ttl?, max_messages?, hold?, semantic?, description?}}}) ` +
	`and name one as ./<name> in a starter's source or sink, a channel state, or an input state's publish. The team publishes to and reads its own ` +
	`channels without a channels ACL entry; nothing outside the team can reach them, and of the agents inside it only the team's own, each ` +
	`listing ./<name> in its own channels (publish / subscribe). A tenant-scoped one is shared by every walk of the team in the tenant, a user-scoped one is per user. ` +
	`It may declare schedules of its own (local: {schedules: {name: {schedule, channel, payload?}}}): while a walk of the team runs, each publishes ` +
	`into one of the team's own channels (channel: ./<name>) on its cadence — five-field cron or a descriptor like @every 1m, at most every 10s — ` +
	`the payload if set, else a tick naming the schedule and when it fired. They start with each walk and stop when it ends; a team with no walk ` +
	`running has none ticking, and two walks at once each tick. No agent, prompt, credentials or max_fires: a starter reading the channel is what the tick wakes. ` +
	`It may declare webhooks of its own (local: {webhooks: {name: {auth, channel, payload_mapping?}}}): while a walk of the team runs, a signed POST to ` +
	`/v1/_teams/<tenant>/<team>/webhooks/<name> (/v1/_teams/<team>/webhooks/<name> for the shared tenant) publishes its body, once, into one of the team's own ` +
	`channels (channel: ./<name>). auth is a webhook definition's (hmac with signing_secret_env by default, bearer with bearer_token_env, none only where the ` +
	`operator allows it; secrets are env-var names). payload_mapping may map only user_id (a JSONPath), the user the message is attributed to, and must for a ` +
	`user-scoped channel. With no walk running it answers 404 like an unknown webhook. No agent, delivery, credentials, sync_response or on_complete. ` +
	`Declaring one needs the authority to create a webhook definition, which an agent inside a run does not have. ` +
	`run may also set breakpoints on starter states to step a fan-out wave: the walk pauses ` +
	`before dispatching (showing each composed prompt) and asks a human to release all, release n, or abort. ` +
	`run may also set review on starter, agent or parallel states (not a consolidator): ` +
	`each member run is held when it finishes, for an operator to approve, send back with feedback, or reject; a rejected ` +
	`starter member reaches the sink as status "rejected", and a rejected agent or parallel member fails like any failed member. retire soft-retires one version; delete ` +
	`hard-removes a whole team by name (all versions + active pointer), scoped to your tenant. Operations: ` +
	`create, fork, get, list, retire, delete, promote, verify, render_diagram, run, poll, cancel.`

const teamDefInputSchema = `{
  "type": "object",
  "properties": {
    "op":            {"type": "string", "enum": ["create","fork","get","list","retire","delete","promote","verify","render_diagram","run","poll","cancel"], "description": "Operation to perform."},
    "name":          {"type": "string", "description": "Team name (required for create/fork/list/verify/delete). A new name is one segment of A-Z a-z 0-9 _ -, at most 64 characters: no \"/\", \":\", \".\" or spaces."},
    "def_id":        {"type": "string", "description": "Existing def_id (required for get/retire/promote)."},
    "parent_def_id": {"type": "string", "description": "Fork parent (optional for fork — when absent, forks the active def of the name in your tenant, falling back to the shared \"\" base). verify with an overlay takes it too, to check the draft as a fork of that version."},
    "overlay": {
      "type": "object",
      "description": "Team workflow graph. For create/fork, top-level fields are merged per-field over the parent (slices replace wholesale); server-set fields (def_id, version, parent_def_id, created_*) are ignored if supplied. For render_diagram, supplying an overlay renders a DRY-RUN preview of the unsaved graph (syntax-checked, not persisted) instead of resolving a stored def. For verify, supplying an overlay checks the unsaved draft — exactly what create or fork would be sent — without writing it.",
      "properties": {
        "entry":          {"type": "string", "description": "The entry state id."},
        "max_iterations": {"type": "integer", "description": "Per-state cycle cap (0 = default). On fork, omit it to keep the parent's cap; send 0 to go back to the default."},
        "states":         {"type": "array", "items": {"type": "object"}, "description": "State nodes: each is {state, handler:{kind, agent|agents, wait?, consolidator?, ...}}. An agent name is a global agent, or \"./<name>\" for one the team declares under local.agents. A starter handler instead carries source ({channel} or {kind:\"document\", path, scope?}), fanout ({agent|agents, per: message|chunk|once, max}), prompt, sink and binds. An input state may set publish ({channel}) to publish the walk's input to that channel as a JSON value; a channel state may set payload:\"raw\" to publish its input as a JSON value instead of the {state, output} envelope. Replaces the parent's states wholesale."},
        "transitions":    {"type": "array", "items": {"type": "object"}, "description": "Edges: each is {from, to, on}. Replaces the parent's transitions wholesale."},
        "colors":         {"type": "object", "description": "Presentation-only fills/edge colours. Excluded from the content hash."},
        "hooks":          {"type": "object", "description": "The walk's own hooks: {run_end: [entry, ...]}, fired when the walk ends. A state's handler may also carry hooks / tool_hooks, added to every run it starts."},
        "local":          {"type": "object", "properties": {"agents": {"type": "object", "additionalProperties": {"type": "object"}, "description": "The team's own agents, at most 64: name (one segment of A-Z a-z 0-9 _ -, at most 64 characters) → the overlay AgentDef create takes (tier or provider/model, system_prompt, tools, skills, ...). A state runs one as \"./<name>\". Each passes the checks a new agent does, as <team>/<name>; a code-js one must carry code_body. A fork that sends agents replaces the whole list; {} declares none."}, "skills": {"type": "object", "additionalProperties": {"type": "object", "properties": {"body": {"type": "string"}, "description": {"type": "string"}, "tools": {"type": "array", "items": {"type": "string"}}}, "required": ["body"], "additionalProperties": false}, "description": "The team's own skills, at most 64: name (same grammar as an agent's) → {body, description, tools}, what SkillDef create takes. Only the team's own agents can use one, each granted it by listing \"./<name>\" in its skills; they load it with the Skill tool as \"./<name>\". Each passes the checks a new skill does, as <team>/<name>, and its tools must be within each granted agent's tools. A fork that sends skills replaces the whole list and leaves agents alone; {} declares none."}, "channels": {"type": "object", "additionalProperties": {"type": "object"}, "description": "The team's own channels, at most 64: name (one segment of A-Z a-z 0-9 _ -, at most 64 characters) → {scope: tenant|user, default_ttl?, max_messages?, hold?, semantic?, description?} (a ChannelDef's fields; no publisher, period or hooks). Named as \"./<name>\" in channel fields and in the team's own agents' channels. A fork that sends channels replaces the whole list; {} declares none."}, "schedules": {"type": "object", "additionalProperties": {"type": "object", "properties": {"schedule": {"type": "string", "description": "Five-field cron, or a descriptor such as @every 1m or @hourly; at most every 10s. A time zone goes in the expression: CRON_TZ=Europe/Berlin 0 9 * * *."}, "channel": {"type": "string", "description": "One of the team's own channels, as ./<name>."}, "payload": {"description": "Any JSON value, published as the message on every tick (at most 16 KiB). Omitted: a tick {schedule_name, fired_at, delivery, payload: null}."}}, "required": ["schedule", "channel"], "additionalProperties": false}, "description": "The team's own schedules, at most 16: name (same grammar as an agent's) → {schedule, channel, payload?}. Each ticks only while a walk of the team runs — started with the walk, stopped when it ends, skipped while the runtime is paused. No other field is taken (no agent, prompt, credentials, max_fires). A fork that sends schedules replaces the whole list; {} declares none."}, "webhooks": {"type": "object", "additionalProperties": {"type": "object", "properties": {"auth": {"type": "object", "properties": {"kind": {"type": "string", "enum": ["hmac", "bearer", "none"]}, "signing_secret_env": {"type": "string"}, "bearer_token_env": {"type": "string"}, "header": {"type": "string"}, "algorithm": {"type": "string"}, "delivery_id_header": {"type": "string"}}, "additionalProperties": false, "description": "A webhook definition's auth: hmac (the default) needs signing_secret_env, bearer needs bearer_token_env, none is honoured only where the operator allows unauthenticated webhooks. Secrets are env-var NAMES, resolved under the receiver's allowlist."}, "channel": {"type": "string", "description": "One of the team's own channels, as ./<name>."}, "payload_mapping": {"type": "object", "properties": {"user_id": {"type": "string"}}, "additionalProperties": false, "description": "Only user_id: a JSONPath ($, .key and [N]) to the user the message is attributed to. Required when the channel is user-scoped (the message is filed under that user)."}}, "required": ["auth", "channel"], "additionalProperties": false}, "description": "The team's own webhooks, at most 16: name (same grammar as an agent's) → {auth, channel, payload_mapping?}. Each answers POST /v1/_teams/<tenant>/<team>/webhooks/<name> only while a walk of the team runs, publishing the request body once into the channel; otherwise 404. No other field is taken (no agent, delivery, credentials, sync_response, on_complete, tenant_id). Declaring one needs the authority to create a webhook definition. A fork that sends webhooks replaces the whole list; {} declares none."}}, "additionalProperties": false, "description": "What the team declares for itself: agents, skills, channels, schedules and webhooks; any other key is refused."},
        "vars":           {"type": "object", "additionalProperties": {"type": "string"}, "description": "The team's variables: name → default value, read in a state's prompts as ${var.<name>}. Every walk starts with these defaults; run may set a declared one with its own vars. A default is literal text (never expanded; no {{ or }}), at most 4096 bytes, at most 64 variables. A fork that sends vars replaces the whole list; {} declares none."}
      },
      "additionalProperties": true
    },
    "description":    {"type": "string", "description": "Free-text rationale for create/fork."},
    "promote":        {"type": "boolean", "description": "create defaults true, fork defaults false."},
    "retired":        {"type": "boolean", "description": "Required for retire — set true to retire, false to un-retire."},
    "content_sha256": {"type": "string", "description": "Input for op=verify — the local content hash to compare against the active row. verify also returns runnable + issues[]: what the stored definition references but does not contain (an undeclared channel, a team-ACL gap, a retired member). Optional — verify reports the sweep with or without it. Not with overlay: a draft's hash is computed."},
    "as":             {"type": "string", "enum": ["create","fork"], "description": "verify with an overlay (optional): check the draft as a create or as a fork. Omit to check it as a save would: a fork of the active version when the name has one, else a create."},
    "format":         {"type": "string", "enum": ["mermaid","d2"], "description": "render_diagram output format (default mermaid; d2 is deferred)."},
    "highlight_state": {"type": "string", "description": "render_diagram: optionally mark this state (e.g. a chunk's current state) with a bold outline."},
    "input":          {"type": "string", "description": "run: the initial input handed to the entry state's agent (the task/prompt the team works on)."},
    "vars":           {"type": "object", "additionalProperties": {"type": "string"}, "description": "run (optional): values for this walk's variables, name → text, read by the team's prompts as ${var.<name>}. Only a name the team's definition declares in its vars is accepted; any other is refused before anything runs, with the declared names. A value given here replaces the team's default for this walk; a vars state, a capture or a starter's binds may still overwrite it as the walk runs. Literal text: never expanded, no {{ or }}, at most 4096 bytes."},
    "board_chunk_id": {"type": "string", "description": "run (optional): bind the walk to a Document chunk task board. Each state transition persists chunk.status = the current team state (durable progress), and a later run RESUMES from the persisted status. Omit for an ephemeral run (default)."},
    "board_scope":    {"type": "string", "enum": ["agent","user"], "description": "run (optional): the Document scope of board_chunk_id (default user)."},
    "interrupt_on_cap": {"type": "boolean", "description": "run (optional): when a state hits its iteration cap, ask a human (Interruption) whether to continue / reroute:<state> / abort instead of returning the iteration_cap outcome. An unanswered/timed-out/declined ask aborts (still terminates). Default false."},
    "mode":             {"type": "string", "enum": ["detach","poll"], "description": "run (optional): omit to wait for the walk and get its trace. \"detach\" returns {run_id, status:\"running\"} immediately and the walk continues in the background, outside your run — use it when you need a handle WHILE the walk runs, to arm a breakpoint, answer a pause, or watch progress. \"poll\" (inside an agent's run) returns {run_id, state:\"running\"} immediately and the walk runs as a background child of your run: you keep working, are told when it ends, read it with op=poll, and your run does not end while it is still running; it is cancelled with your run. Either way the response carries run_id."},
    "notify":           {"type": "boolean", "description": "run with mode poll (optional, default true): add a short note to your next turn when the walk ends. false = no note; you poll."},
    "on_parent_end":    {"type": "string", "enum": ["wait","cancel"], "description": "run with mode poll (optional). wait (default): ending your turn waits for the walk. cancel: the walk is cancelled when you end your turn."},
    "run_ids":          {"type": "array", "items": {"type": "string"}, "description": "poll (optional): walks you ran with mode poll, by the run_id each returned. Omit to get every such walk whose answer you have not read yet. cancel (required): the walks to end."},
    "wait":             {"type": "string", "enum": ["none","any","all"], "description": "poll (optional). none (default): answer at once. any: wait until one of the named walks that is still running ends. all: wait until all of them have. Bounded by wait_ms."},
    "wait_ms":          {"type": "integer", "description": "poll with wait any or all (optional): the longest to wait, in ms; capped by the runtime (60000 unless the operator set another). Absent = the cap."},
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
	// Poll is the run's mode when the walk runs as a background child of the
	// calling run; RunID is then the id the caller was handed before the
	// walk's run existed, which the run must be created under.
	Poll  bool
	RunID string
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
	Mode           string          `json:"mode,omitempty"`               // run: "" (wait for the walk) | "detach" (return the run id now) | "poll" (a background child of the calling run)
	As             string          `json:"as,omitempty"`                 // verify with overlay: check the draft as a "create" or a "fork" ("" = as a save would)
	Notify         *bool           `json:"notify,omitempty"`             // run, mode poll: note the walk's end (default true)
	OnParentEnd    string          `json:"on_parent_end,omitempty"`      // run, mode poll: "wait" | "cancel"

	// poll: walks run in poll mode by run id (none = every unread one), and
	// how long to wait for them.
	RunIDs []string `json:"run_ids,omitempty"`
	Wait   string   `json:"wait,omitempty"`
	WaitMs int      `json:"wait_ms,omitempty"`

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
	case "poll":
		return t.execPoll(ctx, in)
	case "cancel":
		return t.execCancel(ctx, in)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: create, fork, get, list, retire, delete, promote, verify, render_diagram, run, poll, cancel)", in.Op)), nil
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
	// Every gate BEFORE any write — an invalid graph must never reach storage
	// (a broken team is silent orchestration corruption). The same gates
	// verify runs on a draft; a save refuses with the first.
	if first := firstRefusal(t.authoringIssues(ctx, "create", in.Name, defJSON, def, nil, in.Description)); first != nil {
		return errResult("create: " + first.Detail), nil
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

	ident := tools.RunIdentity(ctx)
	tenantID := ident.TenantID
	parent, err := t.resolveForkParent(ctx, in.Name, in.ParentDefID)
	if err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	parentDefID := parent.DefID

	defJSON, err := t.buildDefinition(string(parent.Definition), in.Overlay)
	if err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	def, err := teamgraph.Parse(defJSON)
	if err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	// The parent's own agents, for the one gate that compares against them. A
	// parent that no longer parses was already refused by buildDefinition.
	parentDef, _ := teamgraph.Parse(parent.Definition)
	if first := firstRefusal(t.authoringIssues(ctx, "fork", in.Name, defJSON, def, &parentDef, in.Description)); first != nil {
		return errResult("fork: " + first.Detail), nil
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

// errNoForkParent: the name has no version a fork could start from.
type errNoForkParent struct{ name string }

func (e *errNoForkParent) Error() string {
	return fmt.Sprintf("no parent — name %q has no DB version to fork (own tenant or shared \"\")", e.name)
}

// errForkParent: the pinned parent_def_id cannot be forked under this name —
// unknown, another tenant's (the same opaque not-found), or another team's.
type errForkParent struct{ msg string }

func (e *errForkParent) Error() string { return e.msg }

// resolveForkParent finds the version a fork of name starts from — fork's
// lookup, and verify's when it checks a draft as a fork, so the two cannot
// resolve different parents. From the STORE only (no static bootstrap — there
// is no cfg.Teams). Three paths, mirroring SkillDef minus the static branch:
//  1. parent_def_id supplied → pin
//  2. parent_def_id empty + own-tenant active pointer → use it
//  3. neither → fall back to the shared ("") active base, else
//     *errNoForkParent.
//
// RFC N: resolved within the caller's own tenant (from the authoritative run
// identity, never tool input).
func (t *TeamDef) resolveForkParent(ctx context.Context, name, parentDefID string) (store.TeamDefRow, error) {
	tenantID := tools.RunIdentity(ctx).TenantID
	var nf *store.ErrNotFound
	if parentDefID != "" {
		row, err := t.Store.TeamDefGet(ctx, parentDefID)
		notFound := &errForkParent{fmt.Sprintf("parent_def_id %q not found", parentDefID)}
		if err != nil {
			if errors.As(err, &nf) {
				return store.TeamDefRow{}, notFound
			}
			return store.TeamDefRow{}, err
		}
		// Allow forking the SHARED ("") base or the caller's own tenant (the fork
		// lands under the caller's tenant); refuse another specific tenant's
		// private def unless the caller is substrate:admin (crosses tenants, RFC L).
		if !forkParentVisible(ctx, row.TenantID, tenantID) {
			return store.TeamDefRow{}, notFound
		}
		if row.Name != name {
			return store.TeamDefRow{}, &errForkParent{fmt.Sprintf("parent_def_id %q has name %q, refusing to fork under name %q", parentDefID, row.Name, name)}
		}
		return row, nil
	}
	row, err := t.Store.TeamDefGetActive(ctx, tenantID, name)
	if err == nil {
		return row, nil
	}
	if !errors.As(err, &nf) {
		return store.TeamDefRow{}, err
	}
	// No own-tenant active pointer. Fall back to the SHARED ("") base so a
	// per-tenant principal can fork a name seeded under the legacy "" tenant.
	// Skip when tenantID is already "" (identical lookup).
	if tenantID != "" {
		shared, serr := t.Store.TeamDefGetActive(ctx, "", name)
		if serr == nil {
			return shared, nil
		}
		if !errors.As(serr, &nf) {
			return store.TeamDefRow{}, serr
		}
	}
	return store.TeamDefRow{}, &errNoForkParent{name: name}
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
	// Un-retiring puts this version's own agents back in play.
	if !*in.Retired {
		if err := t.checkLocalNamesFree(ctx, row); err != nil {
			return errResult(fmt.Sprintf("retire: %s", err)), nil
		}
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
// hash, and the tool reports whether it matches the deployed active def. With
// an overlay instead, it checks that unsaved draft (verifyDraft).
func (t *TeamDef) execVerify(ctx context.Context, in teamDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("verify: missing required field: name"), nil
	}
	if len(in.Overlay) > 0 {
		return t.verifyDraft(ctx, in)
	}
	if in.As != "" || in.ParentDefID != "" || in.Description != "" {
		return errResult("verify: as, parent_def_id and description describe a draft — they need an overlay"), nil
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
		_, runnable := verdict(issues)
		out["runnable"] = runnable
		if len(issues) > 0 {
			out["issues"] = issueMaps(issues)
		}
	}
	return okJSON(out)
}

// verifyDraft is verify with an overlay: the pre-save check. It builds the
// definition a create or fork of name with this overlay would store, runs
// every gate that save would run plus the reference sweep, and writes nothing.
//
// It reports instead of refusing — the op's contract — so a draft that cannot
// even be built is an answer with one issue, not an error. Each step that
// cannot run without the one before it stops the report there; every other
// problem is collected, so an author fixing a hand-written team sees all of
// them at once.
//
// As a create or fork: `as` picks one; unset, whichever a save would do — a
// fork when there is a version to fork, else a create.
func (t *TeamDef) verifyDraft(ctx context.Context, in teamDefInput) (tools.Result, error) {
	if in.ContentSHA256 != "" {
		return errResult("verify: pass content_sha256 or overlay, not both — with an overlay, verify computes the draft's hash itself"), nil
	}
	as := in.As
	switch as {
	case "", "create", "fork":
	default:
		return errResult(fmt.Sprintf("verify: invalid as %q (want create or fork)", as)), nil
	}
	if as == "create" && in.ParentDefID != "" {
		return errResult("verify: parent_def_id names a fork's parent; it cannot be checked as a create"), nil
	}

	out := map[string]any{"name": in.Name, "deployed": false, "matches": false,
		"current_sha256": "", "current_def_id": "", "version": 0}
	// The deployed version, for `matches`: is this draft what is in force now?
	active, err := t.Store.TeamDefGetActive(ctx, tools.RunIdentity(ctx).TenantID, in.Name)
	var nf *store.ErrNotFound
	switch {
	case err == nil:
		out["deployed"], out["current_sha256"], out["current_def_id"], out["version"] = true, active.ContentSHA256, active.DefID, active.Version
	case !errors.As(err, &nf):
		return errResult(fmt.Sprintf("verify: %s", err)), nil
	}

	report := func(issues []teamIssue) (tools.Result, error) {
		valid, runnable := verdict(issues)
		out["valid"], out["runnable"], out["issues"] = valid, runnable, issueMaps(issues)
		return okJSON(out)
	}

	var issues []teamIssue
	// The name rule create and fork both apply. A name that fails it is
	// reported and the rest is still checked under it.
	if err := t.checkNewName(ctx, in.Name); err != nil {
		issues = append(issues, refused(teamIssueNameInvalid, "", err.Error()))
	}

	var parent store.TeamDefRow
	if as != "create" {
		p, err := t.resolveForkParent(ctx, in.Name, in.ParentDefID)
		var none *errNoForkParent
		var bad *errForkParent
		switch {
		case err == nil:
			parent, as = p, "fork"
		case errors.As(err, &none) && as == "":
			as = "create" // nothing to fork: a save of a new name is a create
		case errors.As(err, &none), errors.As(err, &bad):
			out["checked_as"] = "fork"
			return report(append(issues, refused(teamIssueParentNotFound, "parent_def_id", err.Error())))
		default:
			return errResult(fmt.Sprintf("verify: %s", err)), nil
		}
	}
	out["checked_as"] = as
	if as == "fork" {
		out["parent_def_id"] = parent.DefID
	}

	defJSON, err := t.buildDefinition(string(parent.Definition), in.Overlay)
	if err != nil {
		return report(append(issues, refused(teamIssueOverlayInvalid, "", err.Error())))
	}
	def, err := teamgraph.Parse(defJSON)
	if err != nil {
		return report(append(issues, refused(teamIssueOverlayInvalid, "", err.Error())))
	}
	sha := teamgraph.Sign(in.Name, def)
	out["content_sha256"] = sha
	out["matches"] = active.DefID != "" && sha == active.ContentSHA256

	var parentDef *teamgraph.Definition
	if as == "fork" {
		pd, _ := teamgraph.Parse(parent.Definition)
		parentDef = &pd
	}
	issues = append(issues, t.authoringIssues(ctx, as, in.Name, defJSON, def, parentDef, in.Description)...)
	return report(withoutRepeats(issues, t.sweepReferences(ctx, def)))
}

// sweepReferences reports what a definition names that does not resolve. Each
// issue carries the state that names the thing and its JSON path — the only
// part anyone can act on. None of these stops a save: a stored team that
// references a deleted channel or a retired member is unrunnable, not invalid,
// except an advisory, which stops nothing.
func (t *TeamDef) sweepReferences(ctx context.Context, def teamgraph.Definition) []teamIssue {
	var issues []teamIssue
	unrunnable := func(i teamIssue) {
		i.Severity = severityUnrunnable
		issues = append(issues, i)
	}
	catalog := t.channelCatalog(ctx)
	for _, ref := range teamgraph.ChannelRefs(def) {
		// The team's own channels need neither an ACL entry nor a declaration
		// outside it: checked against the definition itself below.
		if name, isLocal := teamgraph.LocalRef(ref.Channel); isLocal {
			if _, ok := def.LocalChannel(name); !ok {
				unrunnable(teamIssue{
					Kind: teamIssueLocalChannelMissing, Path: ref.Path, State: ref.State, Field: ref.Field, Channel: ref.Channel,
					Detail: fmt.Sprintf("%q names a channel the team does not declare under local.channels", ref.Channel),
				})
			}
			continue
		}
		// The team ACL is def-internal, so this is checkable on every plane and
		// is the failure that actually strands workflows: a def promoted before
		// the ACL existed still validates.
		if !channelAllowed(ref.Channel, def.GrantList(ref.Side)) {
			unrunnable(teamIssue{
				Kind: teamIssueACLMissing, Path: ref.Path, State: ref.State, Field: ref.Field,
				Channel: ref.Channel, Side: string(ref.Side),
				Detail: fmt.Sprintf("the team's ACL does not grant %s on %q", ref.Side, ref.Channel),
			})
		}
		// Skipped, not reported as broken, when no catalog is wired — the same
		// reason the preflight skips it.
		if catalog == nil {
			continue
		}
		if _, ok := catalog[ref.Channel]; !ok {
			unrunnable(teamIssue{
				Kind: teamIssueChannelUndeclared, Path: ref.Path, State: ref.State, Field: ref.Field, Channel: ref.Channel,
				Detail: fmt.Sprintf("channel %q is no longer declared", ref.Channel),
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
			unrunnable(teamIssue{
				Kind: teamIssueAgentMissing, Path: ref.Path, State: ref.State, Field: ref.Field, Agent: ref.Agent,
				Detail: fmt.Sprintf("agent %q does not resolve in this tenant", ref.Agent),
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
			unrunnable(teamIssue{
				Kind: teamIssueLocalAgentMissing, Path: ref.Path, State: ref.State, Field: ref.Field, Agent: ref.Agent,
				Detail: fmt.Sprintf("%q names an agent the team does not declare under local.agents", ref.Agent),
			})
		}
	}
	for _, name := range teamgraph.UnreferencedLocalAgents(def) {
		issues = append(issues, teamIssue{
			Kind: teamIssueLocalAgentUnreferenced, Severity: severityAdvisory,
			Path: teamgraph.PathKey("local.agents", name), Agent: teamgraph.LocalRefPrefix + name,
			Detail: fmt.Sprintf("the team declares its own agent %q and no state runs it "+
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

	// A stored body may not have been through create/fork (a restore, a row
	// from before the rule). An undeclared "./x" must stop the walk here: once
	// qualified below it is an ordinary name, and would run a GLOBAL agent
	// called "<team>/x" if one existed — an agent the author never named.
	if err := teamgraph.CheckLocalRefs(def); err != nil {
		return errResult(fmt.Sprintf("run: %s", err)), nil
	}
	if err := teamgraph.CheckLocalRunNames(def, row.Name); err != nil {
		return errResult(fmt.Sprintf("run: %s", err)), nil
	}
	// A team's own agents are its tenant's. An admin may run another tenant's
	// team by def_id, and the walk then runs in the ADMIN's tenant — where
	// those agents are not resolvable, by design (a run reaches a team's agents
	// only when the team is its own tenant's). Said here rather than at the
	// first state that needs one.
	if len(def.LocalAgentNames()) > 0 && row.TenantID != tools.RunIdentity(ctx).TenantID {
		return errResult(fmt.Sprintf("run: team %q declares agents of its own and belongs to another tenant; "+
			"a team's own agents run only in the team's tenant — run it as that tenant", row.Name)), nil
	}
	// A team's own schedules and webhooks publish into its own channels, which
	// are its tenant's, and are started by whatever arms a walk's triggers.
	// Either missing, the walk would wait on a clock that never ticks or an
	// endpoint that never answers.
	for _, kind := range []struct {
		what  string
		names []string
	}{{"schedules", def.LocalScheduleNames()}, {"webhooks", def.LocalWebhookNames()}} {
		if len(kind.names) == 0 {
			continue
		}
		if t.ArmWalkTriggers == nil {
			return errResult(fmt.Sprintf("run: team %q declares %s of its own, and this server cannot run them", row.Name, kind.what)), nil
		}
		if row.TenantID != tools.RunIdentity(ctx).TenantID {
			return errResult(fmt.Sprintf("run: team %q declares %s of its own and belongs to another tenant; "+
				"a team's own %s run only in the team's tenant — run it as that tenant", row.Name, kind.what, kind.what)), nil
		}
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

	// mode "poll" makes the walk a background child of the calling run, so it
	// needs that run's table, and the iteration to collect it in, before
	// anything is admitted. notify and on_parent_end are poll mode's only.
	pollArg := ""
	if in.Mode == "poll" {
		pollArg = "poll"
	}
	pm, r, ok := pollModeOf(agentInput{Mode: pollArg, Notify: in.Notify, OnParentEnd: in.OnParentEnd})
	if !ok {
		return r, nil
	}
	var bg *tools.Background
	if pm.poll {
		if bg, r, ok = backgroundFor(ctx, teamPollNoTable, teamPollLastIteration); !ok {
			return r, nil
		}
		if t.WalkRun == nil {
			return errResult("run: mode=poll requires run tracking, which is not wired on this server"), nil
		}
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
	// Everything this walk starts — its members, their sub-agents, and theirs
	// — belongs to THIS version of the team, and may name its own agents. A
	// walk started from inside another team's walk replaces that scope: its
	// runs are this team's.
	walkCtx = store.WithTeamScope(walkCtx, store.TeamScope{Tenant: row.TenantID, Team: row.Name, DefID: row.DefID})

	detach := in.Mode == "detach"
	if in.Mode != "" && !detach && !pm.poll {
		return errResult(fmt.Sprintf("run: unknown mode %q (expected \"detach\" or \"poll\")", in.Mode)), nil
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
	// A poll-mode walk is a background child of the calling run: admitted
	// against the run's live children, filed in its table under an id minted
	// now — the caller is handed it before the walk's run row exists — and run
	// on the ctx the table hands out: the call's values, cancelled with the
	// calling RUN (or by its cancel of the walk) rather than with this call,
	// which returns at once. withdraw undoes this for a walk refused below.
	pollRunID := ""
	releaseLive := func() {}
	withdraw := func() {}
	if pm.poll {
		live, lerr := t.LiveChildren.Admit(tools.RunID(ctx), 1)
		if lerr != nil {
			return liveLimitResult(lerr), nil
		}
		releaseLive = live[0]
		pollRunID = store.NewRunID()
		cctx, serr := bg.Start(walkCtx, tools.ChildSpec{
			RunID: pollRunID, Agent: teamWalkLabel + row.Name, Index: -1, Kind: tools.ChildKindTeam,
			Notify: pm.notify, CancelOnParentEnd: pm.cancelOnEnd,
		})
		if serr != nil {
			releaseLive()
			return errBusiness(serr.Error(), ""), nil
		}
		bg.SetState(pollRunID, tools.ChildRunning)
		walkCtx = cctx
		withdraw = func() {
			bg.Withdraw(pollRunID)
			releaseLive()
		}
	}
	// held reports a person holding a poll-mode walk — a breakpoint or cap
	// question it asked, or a member held for a review verdict — on its row
	// in the caller's table: "held" while any hold is open, "running" again
	// once none is.
	held := func(bool) {}
	if pm.poll {
		var mu sync.Mutex
		open := 0
		held = func(on bool) {
			mu.Lock()
			defer mu.Unlock()
			if on {
				open++
			} else {
				open--
			}
			state := tools.ChildRunning
			if open > 0 {
				state = tools.ChildHeld
			}
			bg.SetState(pollRunID, state)
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
			Poll:             pm.poll,
			RunID:            pollRunID,
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
			withdraw()
			return errResult(fmt.Sprintf("run: %s", werr)), nil
		}
		// Fail closed: a poll-mode walk the caller would read under one id
		// while its run lives under another could be neither polled nor
		// cancelled through its run.
		if pm.poll && runID != pollRunID {
			finishRun(WalkEnd{Err: errors.New("the walk's run was not created under the id it was handed")})
			withdraw()
			return errResult("run: internal: the walk's run was not created under the id it was handed"), nil
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
			held(true)
			answer, aerr := t.AskHuman(c, q)
			held(false)
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
		withdraw()
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
				held(true)
				answer, aerr := t.AskHuman(c, formatBreakpoint(row.Name, bp))
				held(false)
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
	// The walk names its members by the name they RUN under, so that is what a
	// step, an envelope and a sink message carry. Starting one, the name goes
	// back to what the definition wrote: "./x" for the team's own agent, and
	// any other name exactly as written — a global agent, which a local agent
	// of the same bare name must not replace.
	ownAgents := teamgraph.LocalRunNames(def, row.Name)
	spawn := func(ctx context.Context, agent string, p teamrun.Prompt, defID string) (teamrun.SpawnResult, error) {
		if ref, own := ownAgents[agent]; own {
			agent = ref
		}
		if pm.poll {
			var endHold func()
			ctx, endHold = memberHolds(ctx, held)
			defer endHold()
		}
		return t.Spawn(ctx, agent, p, defID)
	}
	runner := teamrun.NewAgentRunner(spawn, runnerOpts...)
	// Armed last, once nothing can refuse the walk, and on the walk's own ctx.
	// The disarm runs first in finishRun, so it covers every way a walk ends —
	// completed, failed, cancelled — and nothing a trigger does lands after the
	// walk's run is recorded as over.
	if t.ArmWalkTriggers != nil {
		disarm, aerr := t.ArmWalkTriggers(walkCtx, def)
		if aerr != nil {
			releaseBreakpoints()
			return refuse(fmt.Sprintf("run: %s", aerr))
		}
		closeRun := finishRun
		finishRun = func(end WalkEnd) {
			disarm()
			closeRun(end)
		}
	}
	// walkCancelled is whether the walk's ctx had been cancelled when it
	// ended, read before finishRun releases that ctx.
	walkCancelled := false
	walk := func() ([]teamrun.StepRecord, error) {
		defer releaseBreakpoints()
		// The walk runs on the definition with each "./name" written out as the
		// name that agent runs under, so every member it starts, and every
		// step, envelope and sink message that names one, carries it.
		trace, werr := teamrun.Walk(walkCtx, teamgraph.QualifyLocalRefs(def, row.Name), task, runner, opts...)
		end := WalkEnd{FinalText: walkFinalOutput(trace), Err: werr}
		if werr == nil {
			end.Terminal = walkTerminal(def, task)
		}
		walkCancelled = walkCtx.Err() != nil
		finishRun(end)
		return trace, werr
	}

	// answer is what op=run answers once the walk is over: the trace and the
	// end state for a walk that completed or stopped at an iteration cap (a
	// first-class outcome, not a tool fault), or the error that failed it.
	// Built here so a poll-mode walk is answered exactly as a waited-for one.
	answer := func(trace []teamrun.StepRecord, walkErr error) (map[string]any, error) {
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
				return annotate(map[string]any{
					"name":            row.Name,
					"def_id":          row.DefID,
					"status":          "iteration_cap",
					"capped_state":    capErr.State,
					"max_iterations":  capErr.Max,
					"iteration_count": capErr.Count,
					"steps":           steps,
				}), nil
			}
			return nil, walkErr
		}

		return annotate(map[string]any{
			"name":         row.Name,
			"def_id":       row.DefID,
			"status":       "completed",
			"final_state":  task.State,
			"final_output": task.Input, // Walk threads the last handler's output here
			"steps":        steps,
		}), nil
	}

	if pm.poll {
		// The caller gets the walk's id NOW and goes on working. The walk is a
		// child of the caller's run (see above): its end is filed in the run's
		// table with the answer a waited-for walk returns, and the run is told.
		// Like a detached walk it no longer holds the caller up, so whatever it
		// waits on must not pause the caller's clock.
		walkCtx = providers.WithRunClock(walkCtx, nil)
		go finishPollWalk(bg, pollRunID, releaseLive, func() walkEnding {
			trace, werr := walk()
			return pollWalkResult(answer, trace, werr, walkCancelled, context.Cause(walkCtx))
		})
		return okJSON(map[string]any{
			"name":   row.Name,
			"def_id": row.DefID,
			"run_id": runID,
			"state":  tools.ChildRunning,
		})
	}

	if detach {
		// The caller gets its handle NOW and the walk continues behind it. This
		// is the whole point of the mode: a synchronous op=run tells the caller
		// nothing until it is over, so there is no moment at which it can arm a
		// breakpoint, read a pause, or watch progress.
		//
		// walkCtx is already detached from the request by WalkRun, so the walk
		// survives this handler returning; only cancel stops it.
		//
		// A detached walk no longer holds the caller up, so whatever it waits
		// on must not pause the caller's clock.
		walkCtx = providers.WithRunClock(walkCtx, nil)
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

	endWait := providers.BeginWait(ctx) // the caller's run is parked while the team works
	defer endWait()                     // idempotent; closes the wait on every exit
	trace, walkErr := walk()
	endWait()

	out, failed := answer(trace, walkErr)
	if failed != nil {
		return errResult(fmt.Sprintf("run: %s", failed)), nil
	}
	return okJSON(out)
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
		// max_iterations' zero value IS a value — "use the default" — so the
		// set-if-non-zero merge above could never put a capped team back on
		// the default: the editor deletes the key, the fork keeps the
		// parent's cap, and nothing says so. A SENT 0 or null clears it; an
		// absent key keeps the parent's.
		var keys map[string]json.RawMessage
		if json.Unmarshal(overlay, &keys) == nil {
			if _, sent := keys["max_iterations"]; sent && ov.MaxIterations == 0 {
				base.MaxIterations = 0
			}
		}
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
// teamChannelAuthorityIssues enforces trust rule 4 on `Definition.Channels`:
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
func teamChannelAuthorityIssues(ctx context.Context, def teamgraph.Definition) []teamIssue {
	if def.Channels == nil {
		return nil
	}
	var issues []teamIssue
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
		for i, ch := range side.want {
			if !channelAllowed(ch, granted) {
				issues = append(issues, teamIssue{
					Kind: teamIssueChannelAuthority, Severity: severityRefused,
					Path: fmt.Sprintf("channels.%s[%d]", side.name, i), Channel: ch, Side: side.name,
					Detail: fmt.Sprintf("channels.%s: %q is not in the authoring principal's own %s allowlist — "+
						"a team ACL may only narrow what its author holds, never widen it", side.name, ch, side.name),
				})
			}
		}
	}
	return issues
}

func applyTeamOverlay(base *teamgraph.Definition, ov teamgraph.Definition) {
	if ov.Entry != "" {
		base.Entry = ov.Entry
	}
	// A sent 0 or null (back to the default) is applied by buildDefinition,
	// which can see whether the key was sent: ov alone cannot tell it from absent.
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
		if ov.Local.Skills != nil {
			base.Local.Skills = ov.Local.Skills
		}
		if ov.Local.Channels != nil {
			base.Local.Channels = ov.Local.Channels
		}
		if ov.Local.Schedules != nil {
			base.Local.Schedules = ov.Local.Schedules
		}
		if ov.Local.Webhooks != nil {
			base.Local.Webhooks = ov.Local.Webhooks
		}
	}
}

func (t *TeamDef) sizeCapIssues(defJSON []byte, description string) []teamIssue {
	var issues []teamIssue
	if t.MaxDefinitionBytes > 0 && len(defJSON) > t.MaxDefinitionBytes {
		issues = append(issues, refused(teamIssueSizeCap, "",
			fmt.Sprintf("definition (%d bytes) exceeds max %d", len(defJSON), t.MaxDefinitionBytes)))
	}
	if t.MaxDescriptionBytes > 0 && len(description) > t.MaxDescriptionBytes {
		issues = append(issues, refused(teamIssueSizeCap, "",
			fmt.Sprintf("description (%d bytes) exceeds max %d", len(description), t.MaxDescriptionBytes)))
	}
	return issues
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
// agent, skill, channel, schedule and webhook the team declares for itself. A
// snapshot restore calls it before writing a body. The graph itself is not
// re-validated — a team dials nothing of its own — and neither are the
// caller-dependent checks: channel authority, and the gates a local agent,
// skill or webhook passes when it is authored (see ValidateAgentDefBody for
// why a restore has no authoring caller).
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
	// And its own skills to what a restored skill def is.
	for _, name := range def.LocalSkillNames() {
		sk, _ := def.LocalSkill(name)
		body, err := json.Marshal(localSkillDefinition(sk))
		if err == nil {
			err = ValidateSkillDefBody(body)
		}
		if err != nil {
			return fmt.Errorf("local.skills[%q]: %w", name, err)
		}
	}
	// And its own channels to what create accepts: a restored body reaches
	// the walks and the channel writer through the same decoder.
	for _, name := range def.LocalChannelNames() {
		if _, err := decodeLocalChannel(def.Local.Channels[name]); err != nil {
			return fmt.Errorf("local.channels[%q]: %w", name, err)
		}
	}
	// And its own schedules: a cadence the walk's timer could not parse, or a
	// channel the team does not declare, would only be found by a walk.
	if err := teamgraph.CheckLocalSchedules(def); err != nil {
		return err
	}
	// And its own webhooks, by the rules their authoring is held to — all but
	// the authoring gate, which needs a caller.
	for _, name := range def.LocalWebhookNames() {
		if _, err := LocalWebhookOf(def, name); err != nil {
			return fmt.Errorf("local.webhooks[%q]: %w", name, err)
		}
	}
	return teamgraph.ValidateHooks(def)
}
