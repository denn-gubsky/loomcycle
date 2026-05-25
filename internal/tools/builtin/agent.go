package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// SubAgentRunner runs a named sub-agent with a given user prompt and
// returns the sub-agent's final assistant text. Implementations are
// expected to:
//
//   - Reject unknown agent names (operator-controlled allowlist:
//     cfg.Agents).
//   - Apply the sub-agent's own AllowedTools as the security floor; the
//     parent's tool set does NOT widen the child's. Parent and child are
//     both operator-vetted definitions, so each agent's declared
//     allow-list is authoritative for itself.
//   - When defID is non-empty, resolve the sub-agent against the
//     agent_defs row at that id (v0.8.5 substrate). The row's Name MUST
//     match the requested name — cross-name pinning is refused. The
//     def's mutable fields (system_prompt, allowed_tools, model, tier,
//     etc.) override the static cfg.Agents entry for this sub-run. The
//     pinned defID is also persisted on the sub-run row so evaluations
//     denormalise correctly.
//   - Persist the sub-agent's run as a separate session in the Store so
//     transcripts are replayable independently of the parent.
//   - Carry the parent's context.Context (so cancellation, deadlines,
//     and the loomcycle agent-depth value all propagate).
type SubAgentRunner func(ctx context.Context, name string, prompt string, defID string) (output string, err error)

// MaxConcurrentChildrenLookup resolves the per-agent
// `max_concurrent_children` cap for the named CALLING agent (the
// parent that is invoking Agent.parallel_spawn). Returns 0 when the
// agent has no explicit override; the tool then falls back to
// DefaultMaxConcurrentChildren.
//
// The lookup runs against the same resolver chain that drives sub-run
// dispatch (yaml > dynamic_agents > AgentDef substrate), so an agent
// edited via the substrate UI sees the updated cap on its next
// parallel_spawn without a runtime restart.
//
// nil = "no lookup wired" — every call falls through to
// DefaultMaxConcurrentChildren. Acceptable for tests and zero-config
// dev runs where every agent shares the default.
type MaxConcurrentChildrenLookup func(ctx context.Context, callingAgentName string) int

// MaxAgentDepth caps recursion: a top-level run is depth 0, the agents
// it spawns are depth 1, etc. The cap is a safety rail against runaway
// self-spawning prompt loops; cv-batch-adapter spawning cv-adapter is
// depth 1 today. Hardcoded for v0.4.0; lifted to config in a later
// release if a real workflow demands deeper.
const MaxAgentDepth = 3

// DefaultMaxConcurrentChildren caps how many sub-agents Agent.parallel_spawn
// will spin up concurrently for one call when the calling agent has
// no explicit `max_concurrent_children` yaml override. Four is a
// pragmatic default for fan-out workflows on a single VM: high
// enough to amortize the latency of slow-model children (claude-opus
// at ~10-15s per response) but low enough to avoid blowing past the
// per-tenant fairness cap (v0.10.1 default = 4) on the global
// semaphore.
//
// Sequential Agent.spawn calls are unaffected — the cap only applies
// inside a single parallel_spawn op's `spawns` array. Operators
// override per-agent via `max_concurrent_children: N` in
// loomcycle.yaml or via the AgentDef substrate overlay.
const DefaultMaxConcurrentChildren = 4

// MaxParallelSpawns caps how many entries one parallel_spawn op's
// `spawns` array may carry, regardless of the calling agent's
// max_concurrent_children. The cap is the absolute ceiling on
// per-call fan-out: a single tool call can't enqueue more than
// MaxParallelSpawns children, even if the per-agent cap is higher
// (a poorly-written prompt asking for 100 specialists would
// otherwise saturate the global semaphore from a single tool call).
// The per-agent cap still applies on top — it controls how many of
// those MaxParallelSpawns actually run concurrently vs serialized.
//
// 32 is a deliberately wide ceiling: large enough that no real fan-
// out workflow hits it, low enough that a runaway prompt can't kite
// the substrate. Hardcoded; not yaml-configurable in v1.
const MaxParallelSpawns = 32

// AgentTool is the built-in `Agent` tool that spawns one or more
// named sub-agents in fresh sessions. The model invokes it with
// either `{op:"spawn", name, prompt, def_id?}` (the v0.4.0 single-
// spawn shape; `op` is omittable and defaults to "spawn") or
// `{op:"parallel_spawn", spawns:[{name, prompt, def_id?}, ...]}`
// for concurrent fan-out (v0.11.8+).
//
// `op:"spawn"` returns the sub-agent's final assistant text as a
// tool_result. `op:"parallel_spawn"` returns a JSON-encoded
// `{results:[{agent,ok,output|error},...]}` envelope in input order
// — sub-agent errors are captured per-child and surfaced inside the
// envelope, NOT escalated to a parent tool error. The parent's
// model decides whether to retry, fall back, or give up.
//
// Wire shape:
//
//	// Single spawn — v0.4.0 default, op optional.
//	{
//	  "op":     "spawn",                // optional; default
//	  "name":   "cv-adapter",          // a key in cfg.Agents
//	  "prompt": "Generate CV for ...", // user-message body the sub sees
//	  "def_id": "def_abc123"            // optional; pin to a specific row
//	}
//
//	// Parallel spawn — v0.11.8+, op required.
//	{
//	  "op": "parallel_spawn",
//	  "spawns": [
//	    {"name": "researcher", "prompt": "Topic A"},
//	    {"name": "researcher", "prompt": "Topic B"},
//	    {"name": "summarizer", "prompt": "..."}
//	  ]
//	}
//
// The sub-agent's `allowed_tools` (from its YAML AgentDef) is the sole
// authority on what the sub can use — the parent's allow-list does not
// widen or narrow it. This matches the trust model: each agent
// definition is operator-curated and self-describing.
//
// The parent observes only the tool_call (with input) and the
// tool_result (with the sub's final text). The sub's intermediate
// events go to the sub's own persisted transcript, retrievable via
// GET /v1/sessions/{sub-session-id}/transcript.
type AgentTool struct {
	// Run is the closure provided by the runtime that knows how to
	// look up sub-agent definitions, build the tool dispatcher, and
	// drive a fresh loop. Constructed by the HTTP server at boot.
	Run SubAgentRunner

	// CapLookup resolves the calling agent's per-agent
	// `max_concurrent_children` override. nil = always use
	// DefaultMaxConcurrentChildren. Wired by the HTTP server at boot.
	CapLookup MaxConcurrentChildrenLookup
}

// agentInput is the JSON shape the model sends. The discriminator is
// the optional `op` field — empty / "spawn" routes to the single-
// child path; "parallel_spawn" routes to the fan-out path.
type agentInput struct {
	Op string `json:"op,omitempty"`

	// Single-spawn fields (op="" or "spawn").
	Name   string `json:"name,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	DefID  string `json:"def_id,omitempty"`

	// Parallel-spawn fields (op="parallel_spawn").
	Spawns []parallelSpawnEntry `json:"spawns,omitempty"`
}

// parallelSpawnEntry is one row in a parallel_spawn `spawns` array.
type parallelSpawnEntry struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
	DefID  string `json:"def_id,omitempty"`
}

// parallelSpawnResult is one entry in the JSON envelope the
// parallel_spawn op returns to the calling model. `Ok` discriminates
// success vs failure: when true, `Output` carries the child's final
// text; when false, `Error` carries the human-readable error string.
// Index preserves input ordering when the model needs to correlate.
type parallelSpawnResult struct {
	Index  int    `json:"index"`
	Agent  string `json:"agent"`
	Ok     bool   `json:"ok"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// agentInputSchema is the JSON Schema the model sees. Discriminated
// by `op`; both shapes documented in `oneOf` so providers that surface
// schema-driven help (Claude, OpenAI) show the right field set per op.
// v0.8.10's Gemini schema sanitizer merges `oneOf` branches so the
// schema lands cleanly there too.
const agentInputSchema = `{
  "type": "object",
  "oneOf": [
    {
      "title": "spawn — single sub-agent (default)",
      "properties": {
        "op":     {"type": "string", "enum": ["spawn"], "description": "Optional; defaults to spawn when omitted."},
        "name":   {"type": "string", "description": "Sub-agent name. Must match a key in the loomcycle.yaml agents map."},
        "prompt": {"type": "string", "description": "User-message body the sub-agent sees. Treat as the task description; do not include auth tokens (the sub-agent gets its own auth context)."},
        "def_id": {"type": "string", "description": "Optional. Pin this sub-run to a specific agent_defs row id (returned by AgentDef.create or AgentDef.fork). The row's name must match the 'name' field."}
      },
      "required": ["name", "prompt"]
    },
    {
      "title": "parallel_spawn — fan out to N sub-agents concurrently",
      "properties": {
        "op": {"type": "string", "enum": ["parallel_spawn"], "description": "Required for the fan-out shape."},
        "spawns": {
          "type": "array",
          "minItems": 1,
          "description": "Sub-agents to spawn concurrently. Returns when ALL children complete (success or error). Per-child errors are captured inside the result envelope, not escalated.",
          "items": {
            "type": "object",
            "properties": {
              "name":   {"type": "string", "description": "Sub-agent name. Must match a key in the loomcycle.yaml agents map."},
              "prompt": {"type": "string", "description": "User-message body the sub-agent sees."},
              "def_id": {"type": "string", "description": "Optional. Pin this sub-run to a specific agent_defs row id."}
            },
            "required": ["name", "prompt"]
          }
        }
      },
      "required": ["op", "spawns"]
    }
  ]
}`

const agentDescription = `Spawn named sub-agents in fresh sessions and return their final outputs. ` +
	`Two ops: 'spawn' (default; one child, return its text) and 'parallel_spawn' (N children concurrently, return JSON envelope with per-child ok/output/error). ` +
	`Each sub-agent has its own tool allowlist (from loomcycle.yaml); your tool set does not transfer. ` +
	`Use 'spawn' when you need the child's output before deciding the next step; use 'parallel_spawn' when N independent specialists can run at once and you'll consolidate after all return. ` +
	`See Context.help(topic="fan-out-patterns") for guidance on parallel_spawn vs sequential spawn vs Channel.publish.`

// Name implements tools.Tool.
func (a *AgentTool) Name() string { return "Agent" }

// Description implements tools.Tool.
func (a *AgentTool) Description() string { return agentDescription }

// InputSchema implements tools.Tool.
func (a *AgentTool) InputSchema() json.RawMessage { return json.RawMessage(agentInputSchema) }

// Execute implements tools.Tool. Validates the input, enforces the
// recursion depth cap, and delegates to the SubAgentRunner. Errors are
// surfaced as IsError tool_result so the model can self-correct rather
// than the run being torn down.
func (a *AgentTool) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	if a.Run == nil {
		return tools.Result{IsError: true, Text: "Agent tool not wired to a sub-agent runner (operator misconfiguration)"}, nil
	}
	var in agentInput
	if err := json.Unmarshal(input, &in); err != nil {
		return tools.Result{IsError: true, Text: fmt.Sprintf("invalid input JSON: %s", err)}, nil
	}
	op := strings.TrimSpace(in.Op)
	if op == "" {
		op = "spawn"
	}
	switch op {
	case "spawn":
		return a.executeSpawn(ctx, in)
	case "parallel_spawn":
		return a.executeParallelSpawn(ctx, in)
	default:
		return tools.Result{IsError: true, Text: fmt.Sprintf("unknown op %q (expected 'spawn' or 'parallel_spawn')", in.Op)}, nil
	}
}

// executeSpawn handles the v0.4.0 single-child path. Behavior is
// unchanged from the pre-v0.11.8 AgentTool — same depth guard, same
// error surface, same "no final text" hint.
func (a *AgentTool) executeSpawn(ctx context.Context, in agentInput) (tools.Result, error) {
	if len(in.Spawns) > 0 {
		return tools.Result{IsError: true, Text: "op=spawn must not carry a 'spawns' array; use op=parallel_spawn for fan-out"}, nil
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return tools.Result{IsError: true, Text: "missing required field: name"}, nil
	}
	if in.Prompt == "" {
		return tools.Result{IsError: true, Text: "missing required field: prompt"}, nil
	}
	if AgentDepth(ctx) >= MaxAgentDepth {
		return tools.Result{
			IsError: true,
			Text: fmt.Sprintf(
				"max sub-agent recursion depth (%d) reached at agent %q; refusing to spawn deeper",
				MaxAgentDepth, in.Name,
			),
		}, nil
	}
	subCtx := IncrementAgentDepth(ctx)
	output, err := a.Run(subCtx, in.Name, in.Prompt, in.DefID)
	if err != nil {
		return tools.Result{IsError: true, Text: err.Error()}, nil
	}
	if output == "" {
		return tools.Result{Text: fmt.Sprintf("(sub-agent %q completed with no final text)", in.Name)}, nil
	}
	return tools.Result{Text: output}, nil
}

// executeParallelSpawn fans out to N children concurrently. Returns
// when ALL children complete (success or error); per-child errors are
// captured inside the JSON envelope rather than escalated.
//
// Concurrency is bounded by min(per-agent max_concurrent_children,
// len(spawns)) — a semaphore-shaped channel rate-limits goroutine
// admission. Depth is guarded once for the entire call: the parent's
// current depth must be < MaxAgentDepth (so children dispatch at
// depth+1, identical to the single-spawn path).
//
// Result text is a deterministic-ordering JSON envelope:
//
//	{"results": [{"index":0,"agent":"researcher","ok":true,"output":"..."},
//	             {"index":1,"agent":"researcher","ok":false,"error":"..."}]}
//
// The envelope is a tool_result Text payload (not IsError) regardless
// of per-child success — the call as a whole succeeded; the per-child
// disposition is the model's to read.
func (a *AgentTool) executeParallelSpawn(ctx context.Context, in agentInput) (tools.Result, error) {
	if in.Name != "" || in.Prompt != "" || in.DefID != "" {
		return tools.Result{IsError: true, Text: "op=parallel_spawn must not carry top-level name/prompt/def_id fields; put each child in the 'spawns' array"}, nil
	}
	if len(in.Spawns) == 0 {
		return tools.Result{IsError: true, Text: "op=parallel_spawn requires a non-empty 'spawns' array"}, nil
	}
	if len(in.Spawns) > MaxParallelSpawns {
		return tools.Result{
			IsError: true,
			Text: fmt.Sprintf(
				"parallel_spawn 'spawns' array has %d entries; the per-call ceiling is %d (split the work across multiple calls if you genuinely need more)",
				len(in.Spawns), MaxParallelSpawns,
			),
		}, nil
	}
	// Per-entry input validation BEFORE we kick anything off — a
	// malformed entry should fail the whole call up-front, not
	// arrive as a per-child error inside an otherwise-successful
	// envelope.
	for i, sp := range in.Spawns {
		name := strings.TrimSpace(sp.Name)
		if name == "" {
			return tools.Result{IsError: true, Text: fmt.Sprintf("spawns[%d]: missing required field: name", i)}, nil
		}
		if sp.Prompt == "" {
			return tools.Result{IsError: true, Text: fmt.Sprintf("spawns[%d] (%s): missing required field: prompt", i, name)}, nil
		}
		in.Spawns[i].Name = name
	}
	// Depth guard fires once for the whole call. Each child
	// dispatches at depth+1 (same as single-spawn).
	if AgentDepth(ctx) >= MaxAgentDepth {
		return tools.Result{
			IsError: true,
			Text: fmt.Sprintf(
				"max sub-agent recursion depth (%d) reached; refusing to parallel_spawn at depth %d",
				MaxAgentDepth, AgentDepth(ctx),
			),
		}, nil
	}
	subCtx := IncrementAgentDepth(ctx)

	// Resolve the per-call concurrency cap: per-agent override (if
	// the calling agent set one + the lookup is wired) else the
	// runtime default. Cap is then min'd with the spawn count so a
	// single-entry call doesn't allocate a buffered chan of size 4
	// for no reason.
	callingAgent := tools.AgentName(ctx)
	// Avoid shadowing the built-in `cap` identifier — `concurrencyCap`
	// reads better at the call site and future-proofs against an edit
	// that wants `cap(slice)` here.
	concurrencyCap := DefaultMaxConcurrentChildren
	if a.CapLookup != nil && callingAgent != "" {
		if override := a.CapLookup(ctx, callingAgent); override > 0 {
			concurrencyCap = override
		}
	}
	if concurrencyCap > len(in.Spawns) {
		concurrencyCap = len(in.Spawns)
	}

	results := make([]parallelSpawnResult, len(in.Spawns))
	sem := make(chan struct{}, concurrencyCap)
	var wg sync.WaitGroup
	for i, sp := range in.Spawns {
		i, sp := i, sp
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Acquire a slot or bail on ctx cancellation. ctx
			// cancellation propagates from the parent run, so a
			// cancelled parent reliably terminates outstanding
			// children rather than waiting on the semaphore.
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-subCtx.Done():
				results[i] = parallelSpawnResult{Index: i, Agent: sp.Name, Ok: false, Error: subCtx.Err().Error()}
				return
			}
			out, err := a.Run(subCtx, sp.Name, sp.Prompt, sp.DefID)
			if err != nil {
				results[i] = parallelSpawnResult{Index: i, Agent: sp.Name, Ok: false, Error: err.Error()}
				return
			}
			if out == "" {
				out = fmt.Sprintf("(sub-agent %q completed with no final text)", sp.Name)
			}
			results[i] = parallelSpawnResult{Index: i, Agent: sp.Name, Ok: true, Output: out}
		}()
	}
	wg.Wait()

	envelope := struct {
		Results []parallelSpawnResult `json:"results"`
	}{Results: results}
	body, err := json.Marshal(envelope)
	if err != nil {
		// json.Marshal on a slice of plain structs effectively never
		// fails; keep the defensive path so future field additions
		// surface loudly rather than silently.
		return tools.Result{IsError: true, Text: fmt.Sprintf("internal: marshal parallel_spawn envelope: %s", err)}, nil
	}
	return tools.Result{Text: string(body)}, nil
}

// errAgentBadInput is reserved for future structured errors. Currently
// we surface every input problem as an IsError tool_result, but a
// caller that wants to break out of the loop entirely (rather than let
// the model self-correct) can return this from a SubAgentRunner.
var errAgentBadInput = errors.New("agent: bad input")

type ctxKeyAgentDepth struct{}

// IncrementAgentDepth bumps the depth counter on ctx. The Agent tool
// applies this before invoking the SubAgentRunner so a recursive child
// inherits a higher depth than the parent.
func IncrementAgentDepth(ctx context.Context) context.Context {
	d, _ := ctx.Value(ctxKeyAgentDepth{}).(int)
	return context.WithValue(ctx, ctxKeyAgentDepth{}, d+1)
}

// AgentDepth reports the current recursion depth (0 at the top level).
// Exposed so tests and the loop can assert depth invariants without
// reaching into the unexported context key.
func AgentDepth(ctx context.Context) int {
	d, _ := ctx.Value(ctxKeyAgentDepth{}).(int)
	return d
}

var _ tools.Tool = (*AgentTool)(nil)
