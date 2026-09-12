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
	// OperatorAuthored is the TEAM definition's authorship, and it gates the
	// widened prompt-expansion families inside System and Input.
	//
	// It travels on the PROMPT rather than on ctx because ctx already carries
	// the spawned AGENT's authorship, and the two answer different questions:
	// a team node's prompt is the TeamDef's text, so who wrote the agent it
	// dispatches to is the wrong subject. Putting both on one ctx key would
	// make the guard read whichever was stamped last.
	OperatorAuthored bool
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

// SpawnFunc runs one named agent with a prompt and returns its final text
// output. It mirrors builtin.SubAgentRunner, so the orchestrator reuses the
// existing sub-agent machinery (tenant/identity inheritance, the recursion depth
// cap, the cancel registry) rather than re-implementing run dispatch.
//
// The Prompt parameter replaced a bare `input string`. Widening rather than
// adding a sibling was deliberate: it breaks every implementor at compile time,
// and none should be silently missed.
type SpawnFunc func(ctx context.Context, agent string, p Prompt, defID string) (string, error)

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

// WithRunnerLogf wires the non-fatal log sink.
func WithRunnerLogf(f func(format string, args ...any)) RunnerOption {
	return func(r *agentRunner) { r.logf = f }
}

func (r *agentRunner) RunHandler(ctx context.Context, st teamgraph.State, task *Task) (Outcome, error) {
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
		out, err := r.spawn(ctx, st.Handler.Agent, r.nodePrompt(st.Handler, input, env), "")
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
		out, err := r.spawn(ctx, st.Handler.Agent, r.nodePrompt(st.Handler, input, env), "")
		if err != nil {
			return Outcome{}, err
		}
		return r.captured(st, task, parseConsolidatorOutcome(out))

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
func (r *agentRunner) nodePrompt(h teamgraph.Handler, threaded string, env Env) Prompt {
	in := threaded
	if h.InputTemplate != "" {
		in = h.InputTemplate
	}
	return Prompt{
		System: h.SystemPrompt, Input: in, Values: env.Values(),
		OperatorAuthored: r.operatorAuthored,
	}
}

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
			out, spawnErr := r.spawn(runCtx, name, prompt, "")
			if spawnErr != nil {
				results[i] = agentResult{Index: i, Agent: name, Ok: false, Error: spawnErr.Error()}
				return
			}
			results[i] = agentResult{Index: i, Agent: name, Ok: true, Output: out}
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
	out, err := r.spawn(ctx, consolidator, Prompt{
		Input: envelope, OperatorAuthored: r.operatorAuthored,
	}, "")
	if err != nil {
		return Outcome{}, err
	}
	return parseConsolidatorOutcome(out), nil
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
	Index  int    `json:"index"`
	Agent  string `json:"agent"`
	Ok     bool   `json:"ok"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
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
