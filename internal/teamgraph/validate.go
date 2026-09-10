package teamgraph

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/jsonpath"
)

// Validate checks a TeamDef definition graph for the invariants RFC AP requires
// at create/fork time. It is imperative (mirroring how AgentDef validates code
// and MCPServerDef validates transport) rather than JSON-Schema-enforced.
//
// Rules:
//   - exactly one non-empty `entry`, resolving to a state;
//   - ≥1 state; state ids unique + non-empty;
//   - each state's handler is a valid kind with its required fields;
//   - every transition's from/to resolves to a state; `on` is well-formed
//     (success | pushback:<reason> | conditional:<expr>);
//   - a state's outbound transition labels are unique (so a consolidator's
//     next_edge is unambiguous);
//   - a terminal state has no outbound transitions;
//   - every state is reachable from `entry`;
//   - max_iterations ≥ 0 (0 = use the default). Cycle termination is guaranteed
//     because the per-state cap applies to every state.
//
// varNameRe is the variable-name charset, the same [a-zA-Z0-9_-]{1,64} the
// credentials validator enforces and the expander matches — so a name that
// validates here is a name the expander can actually resolve.
var varNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func Validate(d Definition) error {
	if strings.TrimSpace(d.Entry) == "" {
		return fmt.Errorf("team definition: `entry` is required")
	}
	if len(d.States) == 0 {
		return fmt.Errorf("team definition: at least one state is required")
	}
	if d.MaxIterations < 0 {
		return fmt.Errorf("team definition: max_iterations must be >= 0 (0 = default %d)", DefaultMaxIterations)
	}
	if d.MaxIterations > MaxAllowedIterations {
		return fmt.Errorf("team definition: max_iterations %d exceeds the maximum %d", d.MaxIterations, MaxAllowedIterations)
	}

	// State ids: unique + non-empty; validate each handler.
	states := make(map[string]State, len(d.States))
	for i, s := range d.States {
		if strings.TrimSpace(s.ID) == "" {
			return fmt.Errorf("team definition: state[%d] has an empty `state` id", i)
		}
		if _, dup := states[s.ID]; dup {
			return fmt.Errorf("team definition: duplicate state id %q", s.ID)
		}
		if err := validateHandler(s.ID, s.Handler); err != nil {
			return err
		}
		states[s.ID] = s
	}

	if _, ok := states[d.Entry]; !ok {
		return fmt.Errorf("team definition: entry %q does not resolve to a state", d.Entry)
	}

	// Transitions: endpoints resolve; `on` well-formed; per-state label uniqueness.
	outbound := make(map[string]map[string]bool) // state -> set of `on` labels
	adj := make(map[string][]string)             // state -> reachable states
	for i, t := range d.Transitions {
		if _, ok := states[t.From]; !ok {
			return fmt.Errorf("team definition: transition[%d] from %q does not resolve to a state", i, t.From)
		}
		if _, ok := states[t.To]; !ok {
			return fmt.Errorf("team definition: transition[%d] to %q does not resolve to a state", i, t.To)
		}
		if err := validateOn(i, t.On); err != nil {
			return err
		}
		if states[t.From].Handler.Kind == HandlerTerminal {
			return fmt.Errorf("team definition: terminal state %q must have no outbound transitions", t.From)
		}
		if outbound[t.From] == nil {
			outbound[t.From] = map[string]bool{}
		}
		if outbound[t.From][t.On] {
			return fmt.Errorf("team definition: state %q has duplicate outbound transition label %q (ambiguous route)", t.From, t.On)
		}
		outbound[t.From][t.On] = true
		adj[t.From] = append(adj[t.From], t.To)
	}

	// Reachability: BFS from entry; every state must be reachable.
	seen := map[string]bool{d.Entry: true}
	queue := []string{d.Entry}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, nxt := range adj[cur] {
			if !seen[nxt] {
				seen[nxt] = true
				queue = append(queue, nxt)
			}
		}
	}
	for _, s := range d.States {
		if !seen[s.ID] {
			return fmt.Errorf("team definition: state %q is unreachable from entry %q", s.ID, d.Entry)
		}
	}

	// A non-terminal state must have at least one outbound transition — else the
	// walk enters it, runs its handler (spending a real agent call), and then
	// dead-ends because no edge leaves it. Only a terminal state may have none.
	for _, s := range d.States {
		if s.Handler.Kind != HandlerTerminal && len(outbound[s.ID]) == 0 {
			return fmt.Errorf("team definition: non-terminal state %q has no outbound transition (dead end)", s.ID)
		}
	}

	return nil
}

func validateHandler(stateID string, h Handler) error {
	switch h.Kind {
	case HandlerAgent, HandlerConsolidator:
		if strings.TrimSpace(h.Agent) == "" {
			return fmt.Errorf("team definition: state %q handler kind %q requires `agent`", stateID, h.Kind)
		}
		if len(h.Agents) > 0 {
			return fmt.Errorf("team definition: state %q handler kind %q must not set `agents` (use `agent`)", stateID, h.Kind)
		}
	case HandlerParallel:
		if len(h.Agents) == 0 {
			return fmt.Errorf("team definition: state %q parallel handler requires a non-empty `agents`", stateID)
		}
		for _, a := range h.Agents {
			if strings.TrimSpace(a) == "" {
				return fmt.Errorf("team definition: state %q parallel handler has an empty agent name", stateID)
			}
		}
		if strings.TrimSpace(h.Consolidator) == "" {
			return fmt.Errorf("team definition: state %q parallel handler requires a `consolidator` agent", stateID)
		}
		if err := validateWait(stateID, h.Wait); err != nil {
			return err
		}
	case HandlerVars:
		if len(h.Set) == 0 {
			return fmt.Errorf("team definition: state %q vars handler requires a non-empty `set`", stateID)
		}
		if h.Agent != "" || len(h.Agents) != 0 || h.Consolidator != "" {
			return fmt.Errorf("team definition: state %q vars handler must not set agent/agents/consolidator", stateID)
		}
		if err := validateSet(stateID, h.Set); err != nil {
			return err
		}
	case HandlerInput:
		if h.Agent != "" || len(h.Agents) != 0 || h.Consolidator != "" {
			return fmt.Errorf("team definition: state %q input handler must not set agent/agents/consolidator", stateID)
		}
		if len(h.Schema) > 0 && !json.Valid(h.Schema) {
			return fmt.Errorf("team definition: state %q input handler `schema` is not valid JSON", stateID)
		}
	case HandlerTerminal:
		if h.Agent != "" || len(h.Agents) != 0 || h.Consolidator != "" {
			return fmt.Errorf("team definition: state %q terminal handler must not set agent/agents/consolidator", stateID)
		}
	case "":
		return fmt.Errorf("team definition: state %q handler is missing a `kind`", stateID)
	default:
		return fmt.Errorf("team definition: state %q has unknown handler kind %q (want agent|parallel|consolidator|terminal|vars|input)", stateID, h.Kind)
	}
	if h.Kind != HandlerVars && len(h.Set) > 0 {
		return fmt.Errorf("team definition: state %q sets `set` but is kind %q — assignment belongs on a `vars` state, where it is visible", stateID, h.Kind)
	}
	if h.Kind != HandlerInput && len(h.Schema) > 0 {
		return fmt.Errorf("team definition: state %q sets `schema` but is kind %q (input only)", stateID, h.Kind)
	}
	if err := validateCapture(stateID, h.Capture); err != nil {
		return err
	}
	if h.TimeoutMS < 0 {
		return fmt.Errorf("team definition: state %q handler timeout_ms must be >= 0", stateID)
	}
	return nil
}

// secretNamespaces are the ${…} namespaces a `vars` state may never bind from.
//
// ${run.credentials.*} and ${run.user_bearer} are FAIL-CLOSED by design:
// unresolved, they drop the whole header rather than emit a placeholder. A vars
// state that could copy one into ${var.x} would convert a fail-closed secret
// into a fail-open plaintext string — and that string is then a legitimate
// Memory key, a prompt fragment, and a value in every transcript, snapshot and
// prompt-cache entry downstream.
//
// Secrets keep their own namespace and their own posture. Variables are
// non-secret BY CONSTRUCTION, which is what lets the expander substitute an
// unresolved one to empty instead of dropping its container.
var secretNamespaces = []string{"${run.credentials.", "${run.user_bearer"}

func validateSet(stateID string, set map[string]string) error {
	for _, name := range sortedKeys(set) {
		if !varNameRe.MatchString(name) {
			return fmt.Errorf("team definition: state %q set key %q must match [a-zA-Z0-9_-]{1,64}", stateID, name)
		}
		for _, ns := range secretNamespaces {
			if strings.Contains(set[name], ns) {
				return fmt.Errorf("team definition: state %q set %q reads the credentials namespace — "+
					"variables are non-secret by construction; a secret copied into one becomes a plaintext "+
					"value in every transcript, snapshot and prompt-cache entry downstream", stateID, name)
			}
		}
	}
	return nil
}

func validateCapture(stateID string, capture map[string]string) error {
	for _, name := range sortedKeys(capture) {
		if !varNameRe.MatchString(name) {
			return fmt.Errorf("team definition: state %q capture key %q must match [a-zA-Z0-9_-]{1,64}", stateID, name)
		}
		if _, err := jsonpath.Parse(capture[name]); err != nil {
			return fmt.Errorf("team definition: state %q capture %q: %w", stateID, name, err)
		}
	}
	return nil
}

// sortedKeys makes a map-driven validation report DETERMINISTIC: without it the
// error an operator sees for a definition with two bad entries depends on Go's
// map iteration order and changes between runs.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func validateWait(stateID, wait string) error {
	if wait == "" || wait == WaitAll || wait == WaitAny {
		return nil
	}
	if n, ok := strings.CutPrefix(wait, WaitAtLeast+":"); ok {
		k, err := strconv.Atoi(n)
		if err != nil || k < 1 {
			return fmt.Errorf("team definition: state %q wait %q: at_least:<N> needs a positive integer", stateID, wait)
		}
		return nil
	}
	return fmt.Errorf("team definition: state %q has invalid wait %q (want all|any|at_least:<N>)", stateID, wait)
}

func validateOn(i int, on string) error {
	if on == OnSuccess {
		return nil
	}
	if reason, ok := strings.CutPrefix(on, OnPushback+":"); ok {
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("team definition: transition[%d] pushback: needs a non-empty reason", i)
		}
		return nil
	}
	if expr, ok := strings.CutPrefix(on, OnConditional+":"); ok {
		if strings.TrimSpace(expr) == "" {
			return fmt.Errorf("team definition: transition[%d] conditional: needs a non-empty expression", i)
		}
		return nil
	}
	return fmt.Errorf("team definition: transition[%d] has invalid `on` %q (want success | pushback:<reason> | conditional:<expr>)", i, on)
}
