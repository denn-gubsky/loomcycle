package teamrun

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

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
	System string
	// Input is the user segment: the node's InputTemplate when it has one, else
	// the output threaded from the previous state.
	Input string
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
func NewAgentRunner(spawn SpawnFunc) Runner {
	return &agentRunner{spawn: spawn}
}

type agentRunner struct {
	spawn SpawnFunc
}

func (r *agentRunner) RunHandler(ctx context.Context, st teamgraph.State, input string) (Outcome, error) {
	switch st.Handler.Kind {
	case teamgraph.HandlerAgent:
		out, err := r.spawn(ctx, st.Handler.Agent, nodePrompt(st.Handler, input), "")
		if err != nil {
			return Outcome{}, err
		}
		if st.Handler.Consolidator == "" {
			// No consolidator → a single-agent state advances on success.
			return Outcome{Output: out}, nil
		}
		// A consolidator re-evaluates the single agent's output and selects the
		// edge (success to advance, pushback to loop back for rework). It reads
		// the SAME {results:[…]} envelope a parallel fan-out produces, so one
		// consolidator agent works uniformly after one agent or after N.
		env, err := resultsEnvelope([]agentResult{{Index: 0, Agent: st.Handler.Agent, Ok: true, Output: out}})
		if err != nil {
			return Outcome{}, err
		}
		return r.runConsolidator(ctx, st.Handler.Consolidator, env)

	case teamgraph.HandlerParallel:
		results, err := r.runParallel(ctx, st, input)
		if err != nil {
			return Outcome{}, err
		}
		// Validate guarantees a parallel handler has a consolidator; it reads the
		// fan-out results and selects the edge.
		env, err := resultsEnvelope(results)
		if err != nil {
			return Outcome{}, err
		}
		return r.runConsolidator(ctx, st.Handler.Consolidator, env)

	case teamgraph.HandlerConsolidator:
		// A standalone judging state: run the agent on the threaded input; its
		// output selects the edge. Unlike a Consolidator that follows a fan-out,
		// it reads the raw work product (not a results envelope) — it judges the
		// previous state's output directly. This state IS the consolidator, so
		// the node's own system prompt applies to it.
		out, err := r.spawn(ctx, st.Handler.Agent, nodePrompt(st.Handler, input), "")
		if err != nil {
			return Outcome{}, err
		}
		return parseConsolidatorOutcome(out), nil

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
func nodePrompt(h teamgraph.Handler, threaded string) Prompt {
	in := threaded
	if h.InputTemplate != "" {
		in = h.InputTemplate
	}
	return Prompt{System: h.SystemPrompt, Input: in}
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
func (r *agentRunner) runParallel(ctx context.Context, st teamgraph.State, input string) ([]agentResult, error) {
	agents := st.Handler.Agents
	// One node, one role: every member of a fan-out shares this state's system
	// prompt and input. States whose members need DIFFERENT roles are separate
	// `agent` states, which is the shape per-node prompts exist to make cheap.
	prompt := nodePrompt(st.Handler, input)
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
	out, err := r.spawn(ctx, consolidator, Prompt{Input: envelope}, "")
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
