package builtin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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

	// Board, if set, lets an op=run OPTIONALLY bind to a Document task board: when
	// the caller passes board_chunk_id, the walk persists its position onto that
	// chunk's status (chunk.status = the current team state) on every transition
	// and RESUMES from the persisted status on the next run — durable, resumable
	// progress. Satisfied by *Document (same package). nil = board binding is
	// unavailable (board_chunk_id is then refused). Wired by SetTeamDefTool.
	Board teamBoard

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
	`(success to advance, pushback to loop back for rework) — output threads to the next state, until a ` +
	`terminal state. run may OPTIONALLY bind to a Document chunk board (board_chunk_id) so progress persists ` +
	`as chunk.status and resumes across runs, and may escalate an iteration cap to a human (interrupt_on_cap) ` +
	`instead of aborting. retire soft-retires one version; delete ` +
	`hard-removes a whole team by name (all versions + active pointer), scoped to your tenant. Operations: ` +
	`create, fork, get, list, retire, delete, promote, verify, render_diagram, run.`

const teamDefInputSchema = `{
  "type": "object",
  "properties": {
    "op":            {"type": "string", "enum": ["create","fork","get","list","retire","delete","promote","verify","render_diagram","run"], "description": "Operation to perform."},
    "name":          {"type": "string", "description": "Team name (required for create/fork/list/verify/delete)."},
    "def_id":        {"type": "string", "description": "Existing def_id (required for get/retire/promote)."},
    "parent_def_id": {"type": "string", "description": "Fork parent (optional for fork — when absent, forks the active def of the name in your tenant, falling back to the shared \"\" base)."},
    "overlay": {
      "type": "object",
      "description": "Team workflow graph. For create/fork, top-level fields are merged per-field over the parent (slices replace wholesale); server-set fields (def_id, version, parent_def_id, created_*) are ignored if supplied. For render_diagram, supplying an overlay renders a DRY-RUN preview of the unsaved graph (syntax-checked, not persisted) instead of resolving a stored def.",
      "properties": {
        "entry":          {"type": "string", "description": "The entry state id."},
        "max_iterations": {"type": "integer", "description": "Per-state cycle cap (0 = default)."},
        "states":         {"type": "array", "items": {"type": "object"}, "description": "State nodes: each is {state, handler:{kind, agent|agents, wait?, consolidator?, ...}}. Replaces the parent's states wholesale."},
        "transitions":    {"type": "array", "items": {"type": "object"}, "description": "Edges: each is {from, to, on}. Replaces the parent's transitions wholesale."},
        "colors":         {"type": "object", "description": "Presentation-only fills/edge colours. Excluded from the content hash."}
      },
      "additionalProperties": true
    },
    "description":    {"type": "string", "description": "Free-text rationale for create/fork."},
    "promote":        {"type": "boolean", "description": "create defaults true, fork defaults false."},
    "retired":        {"type": "boolean", "description": "Required for retire — set true to retire, false to un-retire."},
    "content_sha256": {"type": "string", "description": "Input for op=verify — the local content hash to compare against the active row."},
    "format":         {"type": "string", "enum": ["mermaid","d2"], "description": "render_diagram output format (default mermaid; d2 is deferred)."},
    "highlight_state": {"type": "string", "description": "render_diagram: optionally mark this state (e.g. a chunk's current state) with a bold outline."},
    "input":          {"type": "string", "description": "run: the initial input handed to the entry state's agent (the task/prompt the team works on)."},
    "board_chunk_id": {"type": "string", "description": "run (optional): bind the walk to a Document chunk task board. Each state transition persists chunk.status = the current team state (durable progress), and a later run RESUMES from the persisted status. Omit for an ephemeral run (default)."},
    "board_scope":    {"type": "string", "enum": ["agent","user"], "description": "run (optional): the Document scope of board_chunk_id (default user)."},
    "interrupt_on_cap": {"type": "boolean", "description": "run (optional): when a state hits its iteration cap, ask a human (Interruption) whether to continue / reroute:<state> / abort instead of returning the iteration_cap outcome. An unanswered/timed-out/declined ask aborts (still terminates). Default false."}
  },
  "required": ["op"]
}`

type teamDefInput struct {
	Op             string          `json:"op"`
	Name           string          `json:"name,omitempty"`
	DefID          string          `json:"def_id,omitempty"`
	ParentDefID    string          `json:"parent_def_id,omitempty"`
	Overlay        json.RawMessage `json:"overlay,omitempty"`
	Description    string          `json:"description,omitempty"`
	Promote        *bool           `json:"promote,omitempty"`
	Retired        *bool           `json:"retired,omitempty"`
	ContentSHA256  string          `json:"content_sha256,omitempty"`   // input for op: verify
	Format         string          `json:"format,omitempty"`           // render_diagram: mermaid (default) | d2
	HighlightState string          `json:"highlight_state,omitempty"`  // render_diagram: mark a state
	Input          string          `json:"input,omitempty"`            // run: initial input to the entry state
	BoardChunkID   string          `json:"board_chunk_id,omitempty"`   // run: bind the walk to a Document chunk board
	BoardScope     string          `json:"board_scope,omitempty"`      // run: board_chunk_id's Document scope (agent|user, default user)
	InterruptOnCap bool            `json:"interrupt_on_cap,omitempty"` // run: escalate an iteration cap to a human instead of aborting
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
		if err := t.Store.TeamDefSetActive(ctx, tenantID, in.Name, created.DefID, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("create: promote: %s", err)), nil
		}
	}
	return okJSON(teamDefRowResponse(created, promote))
}

// ---- fork ----

func (t *TeamDef) execFork(ctx context.Context, in teamDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("fork: missing required field: name"), nil
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
		if row.Name != in.Name {
			return errResult(fmt.Sprintf("fork: parent_def_id %q has name %q, refusing to fork under name %q", parentDefID, row.Name, in.Name)), nil
		}
		// Allow forking the SHARED ("") base or the caller's own tenant (the fork
		// lands under the caller's tenant); refuse another specific tenant's
		// private def unless the caller is substrate:admin (crosses tenants, RFC L).
		if row.TenantID != "" && row.TenantID != tenantID && !defCallerIsAdmin(ctx) {
			return errResult(fmt.Sprintf("fork: parent_def_id %q belongs to another tenant, refusing", parentDefID)), nil
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
		if err := t.Store.TeamDefSetActive(ctx, tenantID, in.Name, created.DefID, ident.AgentID); err != nil {
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
	return okJSON(map[string]any{"name": in.Name, "versions": out})
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
	return okJSON(map[string]any{"def_id": in.DefID, "retired": *in.Retired})
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
	ident := tools.RunIdentity(ctx)
	if err := t.Store.TeamDefSetActive(ctx, ident.TenantID, row.Name, row.DefID, ident.AgentID); err != nil {
		return errResult(fmt.Sprintf("promote: %s", err)), nil
	}
	return okJSON(map[string]any{"def_id": row.DefID, "name": row.Name, "promoted": true})
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
	return okJSON(map[string]any{
		"matches":        in.ContentSHA256 != "" && in.ContentSHA256 == row.ContentSHA256,
		"current_sha256": row.ContentSHA256,
		"current_def_id": row.DefID,
		"version":        row.Version,
		"name":           row.Name,
		"deployed":       true,
	})
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

	task := &teamrun.Task{Input: in.Input}

	// Assemble walk options. When neither feature is used, opts is empty and Walk
	// runs with no options → the ephemeral Phase-1 behaviour is byte-identical.
	var opts []teamrun.Option
	resumedFrom := ""
	if boardBound {
		// Resume: continue from the chunk's persisted status when it names a state
		// still in the current graph (a graph edit that dropped that state falls
		// back to the entry — start over rather than resume into a hole).
		status, ok, gerr := t.Board.GetChunkStatus(walkCtx, boardScope, in.BoardChunkID)
		if gerr != nil {
			return errResult(fmt.Sprintf("run: board: %s", gerr)), nil
		}
		if ok && status != "" {
			if _, known := teamgraph.StateByID(def, status); known {
				task.State = status
				resumedFrom = status
			}
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

	trace, walkErr := teamrun.Walk(walkCtx, def, task, teamrun.NewAgentRunner(t.Spawn), opts...)

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
