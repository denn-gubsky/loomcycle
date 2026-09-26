package teamrun

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/jsonpath"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// Prompt is what a state hands its agent: the node's own role plus the text to
// work on. It replaces the bare `input string` SpawnFunc used to take, which
// could not carry a per-node system prompt at all.
//
// WHY A STRUCT AND NOT []loop.PromptSegment: this package's contract is that it
// is a pure graph-walker with no runtime dependencies — importing internal/loop
// takes teamrun from 2 internal dependencies to 21, for a type whose only use
// here is "a system string and a user string". The server closure that
// implements SpawnFunc already lives where the loop types are and composes the
// segments there. If a team node ever needs multimodal input, adding a field
// here is additive and breaks nobody.
type Prompt struct {
	// System is the NODE's role. The server APPENDS it to the agent's own system
	// prompt as a second system segment rather than replacing it — see
	// teamgraph.Handler.SystemPrompt for why that is what makes one AgentDef
	// serve N differently-roled states.
	//
	// RAW: it still carries its ${...} tokens and {{...}} placeholders. Both are
	// resolved at prompt assembly, in ONE pass, from Values below.
	System string
	// Input is the user segment: the node's InputTemplate when it has one, else
	// the output threaded from the previous state. RAW, like System.
	Input string
	// DataSlots are literal → replacement pairs the assembler substitutes AFTER
	// placeholder expansion has finished, and whose content it never scans.
	//
	// This is a SECURITY boundary, not a convenience. A Starter's source message
	// is attacker-influenceable — anyone who can reach the channel writes it —
	// and expansion resolves {{...}} under the RUNTIME's authority, ungated by
	// the agent's tools or scopes. A payload spliced in before expansion could
	// therefore synthesise a placeholder and get it executed. Substituted after,
	// it is data that happens to contain braces.
	//
	// Empty for every non-starter state, so an ordinary node's assembly is
	// byte-identical to before this existed.
	DataSlots map[string]string
	// SystemAuthored and InputAuthored say, PER SEGMENT, whether that text is
	// operator-authored TEMPLATE text. They gate the widened prompt-expansion
	// families, which resolve under the RUNTIME's authority.
	//
	// TWO FLAGS, NOT ONE, BECAUSE THE TWO SEGMENTS HAVE DIFFERENT AUTHORS.
	// System is always the team's own `system_prompt`. Input is the node's
	// `input_template` when it declares one — also the team's — but OTHERWISE
	// it is the previous state's THREADED OUTPUT: model-generated text, which
	// may carry whatever a tool result, a fetched page or a channel message
	// put into it.
	//
	// Treating the team's authorship as covering both would hand that output
	// the runtime's own reach: an agent could emit {{tool:WebFetch:…}} and have
	// the next node's assembly fetch it. Trust rule 5d exists to stop a value
	// choosing a target — "the operator authors the template, an attacker picks
	// the target" — and threaded output is the attacker's half.
	//
	// They travel on the PROMPT rather than on ctx because ctx already carries
	// the spawned AGENT's authorship, and that is a third, unrelated subject:
	// who wrote the agent says nothing about who wrote the text it is handed.
	SystemAuthored bool
	InputAuthored  bool
	// Values resolves this state's ${var.*} / ${now.*} / ${team.*}, keyed
	// without the ${}: "var.pr", "now.date", "team.state".
	//
	// WHY THE MAP TRAVELS INSTEAD OF THE SUBSTITUTED TEXT. Expanding here and
	// shipping finished strings is a PRE-PASS: a variable bound from an
	// attacker-influenceable source could plant a `{{` that prompt assembly
	// would then read as an operator-authored placeholder and resolve under the
	// runtime's own authority. Carrying the values instead lets the two families
	// be alternatives of a single pass, where no substitution is ever rescanned.
	// The cost is that this map crosses the spawn boundary; the benefit is that
	// the guarantee stops depending on which expander runs first.
	Values map[string]string
}

// SpawnFunc runs one named agent with a prompt and returns what it produced.
// It mirrors builtin.SubAgentRunner, so the orchestrator reuses the existing
// sub-agent machinery (tenant/identity inheritance, the recursion depth cap,
// the cancel registry) rather than re-implementing run dispatch.
//
// The Prompt parameter replaced a bare `input string`, and SpawnResult replaced
// a bare output string. Widening rather than adding a sibling was deliberate
// both times: it breaks every implementor at compile time, and none should be
// silently missed.
type SpawnFunc func(ctx context.Context, agent string, p Prompt, defID string) (SpawnResult, error)

// SpawnResult is what one spawned member produced.
//
// RunID makes the member ADDRESSABLE (RFC DI): with it a caller reads the
// member's own result, prompt and transcript by id instead of from the string
// a walk threaded onward. It is set on failure too whenever the run row
// exists, because a failed member is exactly the one worth opening. Empty when
// the implementor has no run behind the call.
type SpawnResult struct {
	Output string
	RunID  string
	// Status is the member run's terminal status — completed, failed,
	// cancelled or rejected — decided by the same rule the run's own row is
	// written with, so the walk and the row cannot disagree. A member a
	// reviewer rejected is not a failure and not a success; the Starter needs
	// to tell the three apart. Empty when the implementor has no run behind the
	// call.
	Status string
}

// MemberRejected is the SpawnResult.Status of a member a reviewer turned down.
// It is the run's own status value (store.RunRejected) — teamrun does not
// import the store — and a test in the server pins the two equal.
const MemberRejected = "rejected"

// spawnWork spawns a member whose output the walk goes on to use — every
// state but the Starter, which tells a rejected member apart itself. A
// rejected member ends without an error, but its answer was never accepted,
// so it counts as failed here rather than as work to thread onward.
func (r *agentRunner) spawnWork(ctx context.Context, agent string, p Prompt) (SpawnResult, error) {
	sp, err := r.spawn(ctx, agent, p, "")
	if err == nil && sp.Status == MemberRejected {
		err = fmt.Errorf("the answer of %q was rejected (run %s)", agent, sp.RunID)
	}
	return sp, err
}

type reviewArmingKey struct{}

// WithReviewArming attaches a member's review arming to ctx for the SpawnFunc:
// armed, read live whenever the member run finishes an answer, reports whether
// that answer is held for an operator's verdict. It is a callback rather than
// a flag because arming can change while the member runs (a walk armed
// mid-wave holds the members that have not finished yet).
func WithReviewArming(ctx context.Context, armed func(context.Context) bool) context.Context {
	if armed == nil {
		return ctx
	}
	return context.WithValue(ctx, reviewArmingKey{}, armed)
}

// ReviewArming returns the member's review arming, or nil when the walk never
// armed review for it.
func ReviewArming(ctx context.Context) func(context.Context) bool {
	armed, _ := ctx.Value(reviewArmingKey{}).(func(context.Context) bool)
	return armed
}

type reviewTTLKey struct{}

// WithReviewTTL attaches the walk's review deadline for its members: a hold
// nobody rules on within it ends the member rejected.
func WithReviewTTL(ctx context.Context, ttl time.Duration) context.Context {
	if ttl <= 0 {
		return ctx
	}
	return context.WithValue(ctx, reviewTTLKey{}, ttl)
}

// ReviewTTL returns the member's review deadline, or 0 for none.
func ReviewTTL(ctx context.Context) time.Duration {
	ttl, _ := ctx.Value(reviewTTLKey{}).(time.Duration)
	return ttl
}

// maxParallelConcurrency bounds how many of a parallel state's agents run at
// once. It mirrors builtin.DefaultMaxConcurrentChildren (4): high enough to
// amortize slow-model latency, low enough to stay under the per-tenant fairness
// cap so one team's fan-out can't starve the global semaphore.
const maxParallelConcurrency = 4

// consolidatorSignalMarker is the line prefix a consolidator agent emits to
// select the outgoing transition, e.g. `signal: pushback:redo`. The prefix match
// is case-insensitive; the value after it must equal one of the state's outbound
// transition labels (success | pushback:<reason> | conditional:<expr>). This is
// the ONLY channel by which a consolidator drives routing — see
// parseConsolidatorOutcome.
const consolidatorSignalMarker = "signal:"

// NewAgentRunner returns the production Runner. It executes every handler kind:
//   - agent (no consolidator): run the agent, advance on success;
//   - agent + consolidator: run the agent, then a consolidator reads its output
//     (as a one-entry results envelope) and selects the edge (enables pushback);
//   - parallel: fan the agents out concurrently (honoring `wait`), then the
//     required consolidator reads the results envelope and selects the edge;
//   - consolidator (standalone state): run the agent on the threaded input; its
//     output selects the edge.
//
// The returned Outcome (output + edge) is what the Walk engine routes on, so all
// team-graph shapes execute without any change to the walk.
func NewAgentRunner(spawn SpawnFunc, opts ...RunnerOption) Runner {
	r := &agentRunner{spawn: spawn}
	for _, o := range opts {
		o(r)
	}
	return r
}

type agentRunner struct {
	spawn SpawnFunc
	// now is injectable so a test can pin ${now.*}. nil = time.Now.
	now func() time.Time
	// channels is the Starter's wire. nil means a definition containing a
	// starter state cannot run — refused at the state rather than silently
	// skipped, because a workflow whose source never fires looks identical to
	// one whose source is empty.
	channels ChannelIO
	// wave, when set, returns a ctx carrying the wave a spawn belongs to. The
	// server supplies it (it owns the run-creation seam that stamps it); nil
	// simply means the correlation is not recorded.
	wave func(ctx context.Context, walkID, waveID string, index int) context.Context
	// logf reports what a Starter could not do but must not fail for — a sink
	// publish that errored, an ack that did not land. nil = log.Printf.
	logf func(format string, args ...any)
	// operatorAuthored is the TEAM DEFINITION's authorship, stamped onto every
	// Prompt this runner builds. The walk reads it from the def row it
	// resolved; a runner constructed without it builds prompts that cannot use
	// the widened families, which is the safe direction for a test double or
	// an embed that never sets it.
	operatorAuthored bool
	// maxWave is the DEPLOYMENT's ceiling on one wave's width, distinct from
	// the definition's own `fanout.max`. Dynamic fan-out is a spawn amplifier,
	// and the definition is authored by whoever can write a def — the operator
	// bounds it. 0 disables the check (tests, embeds).
	//
	// It is injected rather than imported because the number lives in
	// internal/connector, and teamrun importing that would undo the dependency
	// contract this package is built on.
	maxWave int
	// breakAt is where the operator's arming is READ FROM — consulted at every
	// pause, never captured at dispatch, so a state can be armed while the walk
	// is already running. onBreak is how the pause is asked.
	//
	// BOTH come from the RUN, never from the definition: a `debug: true` in a
	// def would change its content hash, so turning the debugger on would fork
	// the workflow — and then the thing being debugged is not the thing that
	// runs in production.
	//
	// A nil source means no state ever asks, so a walk without breakpoints takes
	// byte-identical paths to one from before this existed.
	breakAt BreakpointSource
	onBreak BreakpointFunc
	// reviewAt answers whether a starter's members are held for review — the
	// same live armed set the debugger reads, at its review phase. Separate from
	// breakAt because review needs no human-ask machinery: the verdict comes
	// through the member run's own review verb, not a walk pause.
	reviewAt  BreakpointSource
	reviewTTL time.Duration
}

// RunnerOption configures the production runner. Options rather than more
// constructor parameters because a Starter needs three collaborators a plain
// agent walk does not, and every existing NewAgentRunner call site should keep
// compiling unchanged.
type RunnerOption func(*agentRunner)

// WithChannels wires the Starter's channel executor.
func WithChannels(io ChannelIO) RunnerOption {
	return func(r *agentRunner) { r.channels = io }
}

// WithWaveContext wires the seam that carries a wave identity to run creation.
func WithWaveContext(f func(ctx context.Context, walkID, waveID string, index int) context.Context) RunnerOption {
	return func(r *agentRunner) { r.wave = f }
}

// WithOperatorAuthored records whether an OPERATOR wrote the definition this
// runner is walking. It gates the widened prompt-expansion families inside
// every node prompt the walk hands out — see Prompt.OperatorAuthored.
func WithOperatorAuthored(v bool) RunnerOption {
	return func(r *agentRunner) { r.operatorAuthored = v }
}

// WithMaxWave wires the deployment's ceiling on one Starter wave.
func WithMaxWave(n int) RunnerOption {
	return func(r *agentRunner) { r.maxWave = n }
}

// WithBreakpoints wires the debug pauses: where the arming is read from, and
// how a pause is asked.
//
// A RUN argument by construction — the caller supplies both for THIS run, so
// the same promoted definition runs straight through in production and paused
// in a canvas. The source is consulted at every pause rather than read once, so
// a caller holding a live set can arm a state after the walk has started.
func WithBreakpoints(src BreakpointSource, f BreakpointFunc) RunnerOption {
	return func(r *agentRunner) {
		if src == nil || f == nil {
			return
		}
		r.breakAt = src
		r.onBreak = f
	}
}

// WithMemberReview makes the walk hold a starter's member runs for review when
// the state is armed at the review phase in src — read live, so arming mid-wave
// holds the members that have not finished yet. ttl, when positive, ends a hold
// nobody rules on as rejected.
func WithMemberReview(src BreakpointSource, ttl time.Duration) RunnerOption {
	return func(r *agentRunner) {
		if src == nil {
			return
		}
		r.reviewAt, r.reviewTTL = src, ttl
	}
}

// WithRunnerLogf wires the non-fatal log sink.
func WithRunnerLogf(f func(format string, args ...any)) RunnerOption {
	return func(r *agentRunner) { r.logf = f }
}

func (r *agentRunner) RunHandler(ctx context.Context, st teamgraph.State, task *Task) (Outcome, error) {
	// The state's hooks are added to every run it starts, on top of that
	// agent's own, and ride down to their sub-agents: they go on the ctx every
	// spawn below is made from.
	if add := (hooks.Additions{Hooks: st.Handler.Hooks, ToolHooks: st.Handler.ToolHooks}); !add.Empty() {
		ctx = hooks.WithAdditions(ctx, hooks.AdditionsFrom(ctx).Merge(add))
	}
	input := task.Input
	env := r.envFor(st, task)

	switch st.Handler.Kind {
	case teamgraph.HandlerVars:
		// A vars state assigns and threads its input through unchanged: it is a
		// step in the process, not a transform of the work product.
		for name, tmpl := range st.Handler.Set {
			v, refused := Expand(tmpl, env)
			r.noteRefused(st.ID, name, refused)
			task.SetVar(name, v)
		}
		return Outcome{Output: input}, nil

	case teamgraph.HandlerStarter:
		return r.runStarter(ctx, st, task)

	case teamgraph.HandlerChannel:
		// A publish-only node: the workflow states something on a channel and
		// threads its input onward unchanged. It runs nothing, so there is no
		// output of its own to produce.
		if r.channels == nil {
			return Outcome{}, fmt.Errorf("state %q publishes to a channel but no channel executor is wired", st.ID)
		}
		payload, err := json.Marshal(map[string]any{"state": st.ID, "output": input})
		if err != nil {
			return Outcome{}, fmt.Errorf("state %q channel payload: %w", st.ID, err)
		}
		if err := r.channels.Publish(ctx, st.Handler.Channel, payload); err != nil {
			return Outcome{}, fmt.Errorf("state %q publish %q: %w", st.ID, st.Handler.Channel, err)
		}
		return Outcome{Output: input}, nil

	case teamgraph.HandlerInput:
		// The start form. Its schema is a contract for the CLIENT (and for a
		// headless caller reading the definition); the runtime does not
		// interpret it, so the state threads the caller's input through.
		return r.captured(st, task, Outcome{Output: input})

	case teamgraph.HandlerAgent:
		sp, err := r.spawnWork(ctx, st.Handler.Agent, r.nodePrompt(st.Handler, input, env))
		out := sp.Output
		if err != nil {
			return Outcome{}, err
		}
		if st.Handler.Consolidator == "" {
			// No consolidator → a single-agent state advances on success.
			return r.captured(st, task, Outcome{Output: out})
		}
		// A consolidator re-evaluates the single agent's output and selects the
		// edge (success to advance, pushback to loop back for rework). It reads
		// the SAME {results:[…]} envelope a parallel fan-out produces, so one
		// consolidator agent works uniformly after one agent or after N.
		envelope, err := resultsEnvelope([]agentResult{{Index: 0, Agent: st.Handler.Agent, Ok: true, Output: out}})
		if err != nil {
			return Outcome{}, err
		}
		oc, err := r.runConsolidator(ctx, st.Handler.Consolidator, envelope)
		if err != nil {
			return Outcome{}, err
		}
		return r.captured(st, task, oc)

	case teamgraph.HandlerParallel:
		results, err := r.runParallel(ctx, st, input, env)
		if err != nil {
			return Outcome{}, err
		}
		// Validate guarantees a parallel handler has a consolidator; it reads the
		// fan-out results and selects the edge.
		envelope, err := resultsEnvelope(results)
		if err != nil {
			return Outcome{}, err
		}
		oc, err := r.runConsolidator(ctx, st.Handler.Consolidator, envelope)
		if err != nil {
			return Outcome{}, err
		}
		return r.captured(st, task, oc)

	case teamgraph.HandlerConsolidator:
		// A standalone judging state: run the agent on the threaded input; its
		// output selects the edge. Unlike a Consolidator that follows a fan-out,
		// it reads the raw work product (not a results envelope) — it judges the
		// previous state's output directly. This state IS the consolidator, so
		// the node's own system prompt applies to it.
		sp, err := r.spawnWork(ctx, st.Handler.Agent, r.nodePrompt(st.Handler, input, env))
		if err != nil {
			return Outcome{}, err
		}
		return r.captured(st, task, parseConsolidatorOutcome(sp.Output))

	default:
		// terminal is handled by the walk; anything else is a validation gap.
		return Outcome{}, fmt.Errorf("unexpected handler kind %q", st.Handler.Kind)
	}
}

// nodePrompt composes what a state hands its agent: the node's system prompt,
// and its InputTemplate when set — otherwise the input threaded from the
// previous state.
//
// A template REPLACES the threaded input rather than being prepended to it. That
// is the RFC AP field's declared meaning, and it keeps the rule simple: a state
// either works on what it was handed, or it states its own task. Referring to
// the threaded input from inside a template needs the variable expander, which
// is the next phase; until then a template is used verbatim.
// nodePrompt composes what a state hands its agent. It does NOT substitute:
// the templates travel raw and the values travel beside them, so prompt
// assembly can resolve variables and placeholders in one pass. Substituting
// here would be the pre-pass this design exists to remove (see Prompt.Values).
//
// A METHOD rather than a free function so it can stamp the definition's
// authorship: every prompt a walk hands out carries the flag its own templates
// are expanded under, and no call site can construct one that forgot to.
//
// THREADED OUTPUT TRAVELS IN A DATA SLOT, NOT IN Input. A node that declares no
// input_template works on what the previous state handed it — that agent's
// OUTPUT, which may carry whatever a tool result, a fetched page or a channel
// message put into it. Passing it as Input hands it to the placeholder
// expander, so a `{{document:/…}}` an agent emitted would be resolved under the
// RUNTIME's authority and inlined into the NEXT agent's prompt: one agent
// choosing what another one reads.
//
// A data slot is substituted AFTER expansion and its content is never scanned,
// which is the same reason a Starter's source message travels in one. The
// InputAuthored flag stays false here too, but it is now the BACKSTOP rather
// than the protection — it gates only the widened families, while the slot
// keeps every family off this text.
func (r *agentRunner) nodePrompt(h teamgraph.Handler, threaded string, env Env) Prompt {
	if h.InputTemplate != "" {
		// The team's own words: expanded, and authored by whoever wrote the team.
		return Prompt{
			System: h.SystemPrompt, Input: h.InputTemplate, Values: env.Values(),
			SystemAuthored: r.operatorAuthored,
			InputAuthored:  r.operatorAuthored,
		}
	}
	return Prompt{
		System: h.SystemPrompt, Input: ThreadedOutputSlot, Values: env.Values(),
		DataSlots:      map[string]string{ThreadedOutputSlot: threaded},
		SystemAuthored: r.operatorAuthored,
	}
}

// ThreadedOutputSlot is where a node's threaded input is carried when the node
// declares no input_template: the Input the agent receives is this marker, and
// the previous state's output is substituted into it after expansion has
// finished.
//
// Reserved, like the Starter's slots. It never co-occurs with those — a Starter
// node reads from a channel and threads nothing — so no slot's content can
// contain another slot's marker and be substituted a second time.
const ThreadedOutputSlot = "{{thread.output}}"

// envFor snapshots what the expander may read for one state's turn. Now is read
// ONCE per state so every ${now.*} in a state's prompts and assignments agrees
// — two tokens in one template resolving a millisecond apart would be a
// genuinely confusing bug to chase.
func (r *agentRunner) envFor(st teamgraph.State, task *Task) Env {
	now := r.now
	if now == nil {
		now = time.Now
	}
	return Env{
		Vars:      task.Vars,
		Now:       now(),
		State:     st.ID,
		Iteration: task.IterationCounts[st.ID],
		WalkID:    task.WalkID,
	}
}

// captured applies a handler's `capture` paths to its output and records the
// results on the task.
//
// A path that does not resolve binds NOTHING and is not an error — the same
// posture the webhook projector takes toward an external document, and for the
// same reason: an agent's output is not a schema, so a workflow that hard-failed
// whenever a model phrased its JSON differently would be unusable. The variable
// simply stays unset, and an unset variable expands to empty.
func (r *agentRunner) captured(st teamgraph.State, task *Task, oc Outcome) (Outcome, error) {
	if len(st.Handler.Capture) == 0 {
		return oc, nil
	}
	var doc interface{}
	if err := json.Unmarshal([]byte(oc.Output), &doc); err != nil {
		// Not JSON — nothing to project. Deliberately not an error.
		return oc, nil
	}
	for name, path := range st.Handler.Capture {
		segs, err := jsonpath.Parse(path)
		if err != nil {
			// Validate already rejected malformed paths at create/fork; a def
			// that got here with one is data the store accepted, so honour it
			// by skipping rather than aborting a walk mid-flight.
			continue
		}
		v, ok := jsonpath.Eval(doc, segs)
		if !ok {
			continue
		}
		task.SetVar(name, jsonpath.Stringify(v))
	}
	return oc, nil
}

// noteRefused surfaces a variable dropped for carrying placeholder delimiters.
// It is a security event, not a formatting quirk — something bound a value that
// tried to synthesise a {{…}} placeholder — so it is logged rather than
// swallowed, WITHOUT the value, which is the untrusted part.
func (r *agentRunner) noteRefused(stateID, field string, refused []string) {
	if len(refused) == 0 {
		return
	}
	log.Printf("teamrun: state %q %s: refused variable(s) %v — value contained {{ or }}; "+
		"a variable may not introduce a prompt placeholder", stateID, field, refused)
}

// runParallel fans a parallel state's agents out concurrently with bounded
// concurrency, honoring the handler's `wait` mode, and returns one result per
// agent (index-aligned, mirroring Agent.parallel_spawn's envelope). It never
// leaks a goroutine: every spawned goroutine writes its slot and returns, and
// wg.Wait blocks until all have.
//
// `wait` semantics (need = required successes):
//   - all (default): need = len(agents); every agent is awaited, and a single
//     failure means need is unmet → a clear error aborts the walk;
//   - any: need = 1; the first success cancels the still-running siblings;
//   - at_least:<N>: need = N (clamped to len(agents)); the Nth success cancels
//     the rest.
//
// Only the "enough successes" threshold cancels siblings — a failure never does,
// so wait:all awaits every agent as documented.
func (r *agentRunner) runParallel(ctx context.Context, st teamgraph.State, input string, env Env) ([]agentResult, error) {
	agents := st.Handler.Agents
	// One node, one role: every member of a fan-out shares this state's system
	// prompt and input. States whose members need DIFFERENT roles are separate
	// `agent` states, which is the shape per-node prompts exist to make cheap.
	prompt := r.nodePrompt(st.Handler, input, env)
	n := len(agents)
	need, err := requiredSuccesses(st.Handler.Wait, n)
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([]agentResult, n)
	sem := make(chan struct{}, parallelConcurrency(n))
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0

	for i, name := range agents {
		i, name := i, name
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Acquire a concurrency slot or bail on cancellation (a sibling hit
			// the success threshold, or the parent run was cancelled).
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-runCtx.Done():
				results[i] = agentResult{Index: i, Agent: name, Ok: false, Error: runCtx.Err().Error()}
				return
			}
			sp, spawnErr := r.spawnWork(runCtx, name, prompt)
			if spawnErr != nil {
				results[i] = agentResult{Index: i, Agent: name, RunID: sp.RunID, Ok: false, Error: spawnErr.Error()}
				return
			}
			results[i] = agentResult{Index: i, Agent: name, RunID: sp.RunID, Ok: true, Output: sp.Output}
			mu.Lock()
			successes++
			if successes >= need {
				cancel() // enough succeeded → stop the rest (a no-op for wait:all)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if successes < need {
		wait := st.Handler.Wait
		if wait == "" {
			wait = teamgraph.WaitAll
		}
		var fails []string
		for _, res := range results {
			if !res.Ok {
				fails = append(fails, fmt.Sprintf("%s: %s", res.Agent, res.Error))
			}
		}
		return nil, fmt.Errorf("parallel handler: %d of %d agents succeeded, need %d (wait=%q): %s",
			successes, n, need, wait, strings.Join(fails, "; "))
	}
	return results, nil
}

// runConsolidator runs the consolidator agent on the results envelope and maps
// its output to an Outcome via the signal convention.
// The consolidator is a DIFFERENT agent from the state's own, so the node's
// system prompt (which describes that agent's role) is deliberately not applied
// to it; it receives only the envelope.
func (r *agentRunner) runConsolidator(ctx context.Context, consolidator, envelope string) (Outcome, error) {
	// The envelope is built from the agents' OWN OUTPUTS — the most obviously
	// model-written text in a walk, and the one a consolidator is definitionally
	// handed. It rides a data slot for the same reason threaded output does.
	sp, err := r.spawnWork(ctx, consolidator, Prompt{
		Input:          ThreadedOutputSlot,
		DataSlots:      map[string]string{ThreadedOutputSlot: envelope},
		SystemAuthored: r.operatorAuthored,
	})
	if err != nil {
		return Outcome{}, err
	}
	return parseConsolidatorOutcome(sp.Output), nil
}

// parseConsolidatorOutcome extracts the selected edge from a consolidator's
// output. The consolidator names its edge on a line `signal: <edge-label>`
// (case-insensitive prefix); the last non-empty signal wins so the agent can
// reason first and commit last. Signal lines are stripped from the output that
// threads to the next state (the pushback reason/feedback in the surrounding
// prose is kept). An absent signal leaves Edge empty, which the Walk engine
// defaults to success — a consolidator that says nothing means "advance".
func parseConsolidatorOutcome(out string) Outcome {
	var kept []string
	edge := ""
	for _, line := range strings.Split(out, "\n") {
		if v, ok := cutSignalPrefix(strings.TrimSpace(line)); ok {
			if v != "" {
				edge = v // last non-empty signal wins
			}
			continue // drop the marker line from the threaded output
		}
		kept = append(kept, line)
	}
	return Outcome{
		Output: strings.TrimRight(strings.Join(kept, "\n"), "\n"),
		Edge:   edge,
	}
}

// cutSignalPrefix reports whether line is a signal marker and returns the trimmed
// edge label after it. The prefix match is case-insensitive; the value is passed
// through verbatim (transition labels are matched exactly by the walk).
func cutSignalPrefix(line string) (string, bool) {
	m := consolidatorSignalMarker
	if len(line) >= len(m) && strings.EqualFold(line[:len(m)], m) {
		return strings.TrimSpace(line[len(m):]), true
	}
	return "", false
}

// requiredSuccesses maps a handler's wait mode to the number of agent successes
// needed. It clamps at_least:<N> to the agent count because Validate accepts
// at_least:<N> without bounding N against len(agents) — clamping honors a graph
// the store already accepted rather than failing it at run time.
func requiredSuccesses(wait string, n int) (int, error) {
	switch wait {
	case "", teamgraph.WaitAll:
		return n, nil
	case teamgraph.WaitAny:
		return 1, nil
	}
	if s, ok := strings.CutPrefix(wait, teamgraph.WaitAtLeast+":"); ok {
		k, err := strconv.Atoi(s)
		if err != nil || k < 1 {
			return 0, fmt.Errorf("parallel handler: invalid wait %q", wait)
		}
		if k > n {
			k = n
		}
		return k, nil
	}
	return 0, fmt.Errorf("parallel handler: unknown wait mode %q", wait)
}

// parallelConcurrency caps a fan-out at maxParallelConcurrency (never more than
// the agent count, so a small fan-out doesn't over-allocate the semaphore).
func parallelConcurrency(n int) int {
	if n < maxParallelConcurrency {
		return n
	}
	return maxParallelConcurrency
}

// agentResult is one entry in the consolidator's input envelope. Its JSON shape
// mirrors builtin.ParallelSpawnResult exactly (index/agent/ok/output/error) so a
// consolidator reads the same {results:[…]} envelope Agent.parallel_spawn emits.
// It is duplicated here rather than imported: internal/tools/builtin imports
// internal/teamrun, so importing it back would be a cycle.
type agentResult struct {
	Index int    `json:"index"`
	Agent string `json:"agent"`
	// RunID names the member's own run (RFC DI), so a consolidator's envelope —
	// and a reader of it — can point at the run that produced each result.
	RunID  string `json:"run_id,omitempty"`
	Ok     bool   `json:"ok"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
	// Status is set only for a member a reviewer REJECTED ("rejected"): not ok,
	// but not a failure either — the run did its work and a person turned the
	// answer down. Absent otherwise, so an envelope nobody reviewed is
	// byte-identical to before review existed.
	Status string `json:"status,omitempty"`
}

// resultsEnvelope serializes results as {"results":[…]} — the input a
// consolidator agent reads.
func resultsEnvelope(results []agentResult) (string, error) {
	body, err := json.Marshal(struct {
		Results []agentResult `json:"results"`
	}{Results: results})
	if err != nil {
		return "", fmt.Errorf("teamrun: marshal consolidator envelope: %w", err)
	}
	return string(body), nil
}

type walkHooksKey struct{}

// WalkHooks is what a walk's own run carries: the definition's hooks, and
// whether an operator wrote that definition.
type WalkHooks struct {
	Hooks            hooks.EventHooks
	OperatorAuthored bool
}

// WithWalkHooks hands the definition's walk hooks to whatever opens the walk's
// run (the WalkRun seam), which fires them for that run.
func WithWalkHooks(ctx context.Context, w WalkHooks) context.Context {
	return context.WithValue(ctx, walkHooksKey{}, w)
}

// WalkHooksFrom returns the walk hooks on ctx.
func WalkHooksFrom(ctx context.Context) WalkHooks {
	w, _ := ctx.Value(walkHooksKey{}).(WalkHooks)
	return w
}
