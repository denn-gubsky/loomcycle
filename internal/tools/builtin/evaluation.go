package builtin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Evaluation is the v0.8.5 built-in tool that lets agents emit and
// query scores attached to (run, def) pairs. Forms the "selection"
// half of the self-evolution substrate (paired with the AgentDef
// tool's "mutation" half).
//
// Five operations dispatched off the `op` field:
//
//	submit         — record a score against a run. emitter_role is
//	                  DERIVED server-side from the emitter's ctx vs
//	                  the target run's identity (self / sibling /
//	                  parent / external / unrelated). The model
//	                  NEVER supplies emitter_role.
//	get            — fetch one row by eval_id.
//	list_for_run   — evaluations targeting one run.
//	list_for_def   — evaluations targeting one def (denormalised
//	                  def_id; captures which version the run was
//	                  running against at submit time).
//	aggregate      — mean / median / min / max / latest of scores
//	                  for a def, plus per-dimension and per-emitter-
//	                  role breakdowns. Optional include_lineage
//	                  walks parent_def_id chain.
//
// Capability gate: per-agent yaml `evaluation_scopes` (multi-select)
// closes the surface based on the derived emitter_role:
//
//	"submit_self"        → may submit when emitter_role == "self"
//	"submit_siblings"    → RESERVED. deriveEmitterRole cannot derive
//	                        "sibling" today because RunIdentityValue
//	                        does not carry emitter ParentAgentID;
//	                        sibling cases collapse to "unrelated"
//	                        and this scope is currently inert. Plumb
//	                        emitter ParentAgentID through ctx in a
//	                        future PR to activate it.
//	"submit_descendants" → may submit when target run is in this
//	                        agent's spawn tree
//	"submit_any"         → unrestricted submit (also the only path
//	                        that permits emitter_role == "external"
//	                        and the current escape hatch for sibling
//	                        cross-rating)
//	"read_any"           → may call get / list / aggregate
//
// Default-deny: empty scopes blocks every op.
//
// Loomcycle does NOT auto-promote based on score. Selection (max /
// GA / PPO / RLHF / etc.) is policy and lives in user agents — they
// call Evaluation.aggregate then AgentDef.promote as their policy
// dictates.
type Evaluation struct {
	// Store is the persistence backend. Required.
	Store store.Store

	// MaxJudgementBytes caps a single submit's judgement JSON
	// (LOOMCYCLE_EVALUATION_MAX_JUDGEMENT_BYTES). 0 = no cap.
	MaxJudgementBytes int

	// MaxRationaleBytes caps a single submit's rationale text
	// (LOOMCYCLE_EVALUATION_MAX_RATIONALE_BYTES). 0 = no cap.
	MaxRationaleBytes int
}

const evaluationDescription = `Submit and query scores attached to (run, def) pairs. ` +
	`The score scalar is required; dimensions/judgement/rationale optional. ` +
	`Server derives emitter_role (self/sibling/parent/external/unrelated) from ` +
	`the caller's context vs the target run's identity; the model never supplies it. ` +
	`Operations: submit, get, list_for_run, list_for_def, aggregate.`

const evaluationInputSchema = `{
  "type": "object",
  "properties": {
    "op":               {"type": "string", "enum": ["submit","get","list_for_run","list_for_def","aggregate"], "description": "Operation to perform."},
    "run_id":           {"type": "string", "description": "Target run id (submit / list_for_run)."},
    "eval_id":          {"type": "string", "description": "Evaluation id (get)."},
    "def_id":           {"type": "string", "description": "Definition id (list_for_def / aggregate)."},
    "score":            {"type": "number", "description": "submit only — REQUIRED. Convention [-1,1] or [0,1] (operator chooses)."},
    "dimensions":       {"type": "object", "description": "submit only — optional map of named axes to numeric values, e.g. {\"correctness\": 0.8, \"speed\": 0.6}."},
    "judgement":        {"description": "submit only — optional structured payload, free-form."},
    "rationale":        {"type": "string", "description": "submit only — optional natural-language reasoning."},
    "include_lineage":  {"type": "boolean", "description": "aggregate only — when true, walks parent_def_id and includes ancestors' evaluations."},
    "limit":            {"type": "integer", "description": "list_for_* only — max rows (default 100)."}
  },
  "required": ["op"]
}`

type evaluationInput struct {
	Op             string             `json:"op"`
	RunID          string             `json:"run_id,omitempty"`
	EvalID         string             `json:"eval_id,omitempty"`
	DefID          string             `json:"def_id,omitempty"`
	Score          *float64           `json:"score,omitempty"`
	Dimensions     map[string]float64 `json:"dimensions,omitempty"`
	Judgement      json.RawMessage    `json:"judgement,omitempty"`
	Rationale      string             `json:"rationale,omitempty"`
	IncludeLineage bool               `json:"include_lineage,omitempty"`
	Limit          int                `json:"limit,omitempty"`
}

// Name implements tools.Tool.
func (e *Evaluation) Name() string { return "Evaluation" }

// Description implements tools.Tool.
func (e *Evaluation) Description() string { return evaluationDescription }

// InputSchema implements tools.Tool.
func (e *Evaluation) InputSchema() json.RawMessage { return json.RawMessage(evaluationInputSchema) }

// Execute implements tools.Tool.
func (e *Evaluation) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if e.Store == nil {
		return errResult("Evaluation tool: not configured (no Store backend)"), nil
	}
	// Pre-unmarshal size guard. The per-field caps in execSubmit run
	// AFTER json.Unmarshal copies the whole raw payload into heap, so
	// without this guard a 50 MB judgement allocates 50 MB before
	// being rejected. Use the sum of the two field caps + headroom
	// for the JSON envelope (op, run_id, etc.) as the upper bound.
	// Headroom of 4 KB easily fits the other small fields.
	if cap := totalRawBudget(e.MaxJudgementBytes, e.MaxRationaleBytes); cap > 0 && len(raw) > cap {
		return errResult(fmt.Sprintf("Evaluation tool: input (%d bytes) exceeds max %d", len(raw), cap)), nil
	}
	var in evaluationInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult(fmt.Sprintf("invalid input JSON: %s", err)), nil
	}
	policy := tools.EvaluationPolicy(ctx)

	switch in.Op {
	case "submit":
		return e.execSubmit(ctx, policy, in)
	case "get":
		return e.execGet(ctx, policy, in)
	case "list_for_run":
		return e.execListForRun(ctx, policy, in)
	case "list_for_def":
		return e.execListForDef(ctx, policy, in)
	case "aggregate":
		return e.execAggregate(ctx, policy, in)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: submit, get, list_for_run, list_for_def, aggregate)", in.Op)), nil
	}
}

// ---- submit ----

func (e *Evaluation) execSubmit(ctx context.Context, policy tools.EvaluationPolicyValue, in evaluationInput) (tools.Result, error) {
	if in.RunID == "" {
		return errResult("submit: missing required field: run_id"), nil
	}
	if in.Score == nil {
		return errResult("submit: missing required field: score"), nil
	}
	if e.MaxJudgementBytes > 0 && len(in.Judgement) > e.MaxJudgementBytes {
		return errResult(fmt.Sprintf("submit: judgement (%d bytes) exceeds max %d", len(in.Judgement), e.MaxJudgementBytes)), nil
	}
	if e.MaxRationaleBytes > 0 && len(in.Rationale) > e.MaxRationaleBytes {
		return errResult(fmt.Sprintf("submit: rationale (%d bytes) exceeds max %d", len(in.Rationale), e.MaxRationaleBytes)), nil
	}

	// Look up target run to derive emitter_role + capture def_id.
	target, err := e.Store.GetRun(ctx, in.RunID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("submit: run_id %q not found", in.RunID)), nil
		}
		return errResult(fmt.Sprintf("submit: %s", err)), nil
	}
	emitter := tools.RunIdentity(ctx)
	role := deriveEmitterRole(emitter, target)
	if err := checkSubmitScope(policy, role); err != nil {
		return errResult(err.Error()), nil
	}

	row := store.EvaluationRow{
		EvalID:         mintEvalID(),
		RunID:          in.RunID,
		DefID:          target.AgentDefID, // denormalised from target run (v0.8.5 PR 5)
		Score:          *in.Score,
		Dimensions:     in.Dimensions,
		Judgement:      in.Judgement,
		Rationale:      in.Rationale,
		EmitterRole:    role,
		EmitterAgentID: emitter.AgentID,
	}

	saved, err := e.Store.EvaluationSubmit(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("submit: %s", err)), nil
	}
	return okJSON(map[string]any{
		"eval_id":      saved.EvalID,
		"run_id":       saved.RunID,
		"def_id":       saved.DefID,
		"emitter_role": saved.EmitterRole,
		"score":        saved.Score,
		"created_at":   saved.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z"),
	})
}

// ---- get / list_for_* / aggregate ----

func (e *Evaluation) execGet(ctx context.Context, policy tools.EvaluationPolicyValue, in evaluationInput) (tools.Result, error) {
	if !hasScope(policy, "read_any") {
		return errResult("get: requires evaluation_scopes containing \"read_any\""), nil
	}
	if in.EvalID == "" {
		return errResult("get: missing required field: eval_id"), nil
	}
	row, err := e.Store.EvaluationGet(ctx, in.EvalID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("get: eval_id %q not found", in.EvalID)), nil
		}
		return errResult(fmt.Sprintf("get: %s", err)), nil
	}
	return okJSON(evalRowResponse(row))
}

func (e *Evaluation) execListForRun(ctx context.Context, policy tools.EvaluationPolicyValue, in evaluationInput) (tools.Result, error) {
	if !hasScope(policy, "read_any") {
		return errResult("list_for_run: requires evaluation_scopes containing \"read_any\""), nil
	}
	if in.RunID == "" {
		return errResult("list_for_run: missing required field: run_id"), nil
	}
	rows, err := e.Store.EvaluationListForRun(ctx, in.RunID, in.Limit)
	if err != nil {
		return errResult(fmt.Sprintf("list_for_run: %s", err)), nil
	}
	return okJSON(map[string]any{"run_id": in.RunID, "evaluations": evalRowsResponse(rows)})
}

func (e *Evaluation) execListForDef(ctx context.Context, policy tools.EvaluationPolicyValue, in evaluationInput) (tools.Result, error) {
	if !hasScope(policy, "read_any") {
		return errResult("list_for_def: requires evaluation_scopes containing \"read_any\""), nil
	}
	if in.DefID == "" {
		return errResult("list_for_def: missing required field: def_id"), nil
	}
	rows, err := e.Store.EvaluationListForDef(ctx, in.DefID, in.Limit)
	if err != nil {
		return errResult(fmt.Sprintf("list_for_def: %s", err)), nil
	}
	return okJSON(map[string]any{"def_id": in.DefID, "evaluations": evalRowsResponse(rows)})
}

func (e *Evaluation) execAggregate(ctx context.Context, policy tools.EvaluationPolicyValue, in evaluationInput) (tools.Result, error) {
	if !hasScope(policy, "read_any") {
		return errResult("aggregate: requires evaluation_scopes containing \"read_any\""), nil
	}
	if in.DefID == "" {
		return errResult("aggregate: missing required field: def_id"), nil
	}
	agg, err := e.Store.EvaluationAggregate(ctx, in.DefID, store.AggregateOpts{IncludeLineage: in.IncludeLineage})
	if err != nil {
		return errResult(fmt.Sprintf("aggregate: %s", err)), nil
	}
	return okJSON(agg)
}

// ---- helpers ----

// deriveEmitterRole inspects the emitter's RunIdentity vs the target
// run's AgentID + ParentAgentID and assigns one of:
//
//	self      — emitter is the run's own agent (emitter.AgentID == run.AgentID)
//	sibling   — emitter shares parent with the run (same ParentAgentID, non-empty)
//	parent    — emitter spawned the run (emitter.AgentID == run.ParentAgentID)
//	external  — emitter has no AgentID (admin API submission path)
//	unrelated — none of the above (most common when an orchestrator with
//	             submit_any emits against an unrelated run)
func deriveEmitterRole(emitter tools.RunIdentityValue, target store.Run) string {
	if emitter.AgentID == "" {
		return "external"
	}
	if emitter.AgentID == target.AgentID {
		return "self"
	}
	if emitter.AgentID == target.ParentAgentID && target.ParentAgentID != "" {
		return "parent"
	}
	// Sibling iff both have the same non-empty parent_agent_id AND
	// the emitter isn't the target itself (caught by the self
	// branch above already).
	if target.ParentAgentID != "" {
		// We don't have the emitter's ParentAgentID directly on
		// RunIdentityValue; future PR can add it. For now, treat
		// "same parent" as the case where the emitter ran from a
		// run with the same parent — unrelated otherwise.
		// Without the emitter's ParentAgentID we can't reliably
		// detect sibling vs unrelated, so we conservatively return
		// "unrelated" — the agent's submit_any scope is the
		// escape hatch for this case.
	}
	return "unrelated"
}

// checkSubmitScope returns nil iff the agent's evaluation_scopes
// permits a submit with the given derived emitter_role.
//
// Two roles have known limitations that operators must understand:
//
//   - "sibling": deriveEmitterRole cannot derive this today because
//     RunIdentityValue doesn't carry the emitter's ParentAgentID.
//     The sibling case collapses to "unrelated" — `submit_siblings`
//     in yaml is reserved but currently inert. Plumbing emitter
//     ParentAgentID through tools.WithRunIdentity is queued for
//     v0.9.x; until then operators wanting cross-sibling rating
//     must grant `submit_any`.
//
//   - "external": only fires when the emitter's AgentID is empty,
//     which is the admin-API submission path (no agent ctx). The
//     top-level `submit_any` check above is the ONLY scope that
//     permits external submissions — no dedicated `submit_external`
//     exists. We don't add a redundant case branch here because the
//     `submit_any` short-circuit at line 305 already covers it.
func checkSubmitScope(policy tools.EvaluationPolicyValue, role string) error {
	if hasScope(policy, "submit_any") {
		return nil
	}
	switch role {
	case "self":
		if hasScope(policy, "submit_self") {
			return nil
		}
	case "sibling":
		// Reserved; deriveEmitterRole never returns this today.
		// See the type-level doc above for the limitation + plan.
		if hasScope(policy, "submit_siblings") {
			return nil
		}
	case "parent":
		// No dedicated scope for "I am the parent rating my child";
		// fall through to submit_descendants or submit_any.
		if hasScope(policy, "submit_descendants") {
			return nil
		}
		// "external" is handled by the submit_any short-circuit above —
		// no dedicated submit_external scope.
	}
	return fmt.Errorf("Evaluation tool: emitter_role=%q not permitted by this agent's evaluation_scopes %v (default-deny)", role, policy.Scopes)
}

func hasScope(policy tools.EvaluationPolicyValue, want string) bool {
	for _, sc := range policy.Scopes {
		if sc == want {
			return true
		}
	}
	return false
}

func evalRowResponse(row store.EvaluationRow) map[string]any {
	return map[string]any{
		"eval_id":          row.EvalID,
		"run_id":           row.RunID,
		"def_id":           row.DefID,
		"score":            row.Score,
		"dimensions":       row.Dimensions,
		"judgement":        row.Judgement,
		"rationale":        row.Rationale,
		"emitter_role":     row.EmitterRole,
		"emitter_agent_id": row.EmitterAgentID,
		"emitter_run_id":   row.EmitterRunID,
		"created_at":       row.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z"),
	}
}

func evalRowsResponse(rows []store.EvaluationRow) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, evalRowResponse(r))
	}
	return out
}

// mintEvalID is the eval_id minter. Same shape as mintDefID
// (def_<hex>) but with eval_ prefix. 64 bits entropy.
func mintEvalID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "eval_" + hex.EncodeToString(b[:])
}

// totalRawBudget computes the upper-bound raw-bytes cap for the
// pre-unmarshal size guard in Execute. Returns 0 (= "no cap") when
// either field cap is 0 — matches the per-field "0 disables the cap"
// semantics. envelopeHeadroom covers the other input fields (op,
// run_id, eval_id, def_id, dimensions, etc.) which are bounded in
// practice by the model's emitted token budget.
const envelopeHeadroom = 4 * 1024

func totalRawBudget(maxJudgement, maxRationale int) int {
	if maxJudgement <= 0 || maxRationale <= 0 {
		return 0
	}
	return maxJudgement + maxRationale + envelopeHeadroom
}
