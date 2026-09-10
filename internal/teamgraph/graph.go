// Package teamgraph is the domain model for a TeamDef's `definition` JSON —
// the workflow graph of RFC AP (Agent Teams & Task Workflows): states (nodes),
// transitions (edges), and the per-state handler that decides who acts.
//
// The graph IS the state machine: nodes are author-defined states, edges are the
// allowed transitions (with loops), and each state binds a handler. This package
// is deliberately loomcycle-dependency-free (stdlib only) so the store's
// content-hash (internal/agents/teamsign), the TeamDef tool, the diagram
// renderer, and the runtime orchestrator can all share one model without an
// import cycle.
package teamgraph

import (
	"encoding/json"
	"fmt"
)

// DefaultMaxIterations is the per-state cycle cap applied when a Definition
// omits max_iterations. Exceeding it fires an Interruption at run time.
const DefaultMaxIterations = 10

// MaxAllowedIterations is the upper bound accepted for a definition's
// max_iterations. The cap is a safety net, not a schedule — an absurdly large
// value would let one accepted TeamDef schedule billions of agent runs, so it is
// rejected at create/fork rather than trusted.
const MaxAllowedIterations = 1000

// Handler kinds — the "who acts" bound to a state.
const (
	HandlerAgent        = "agent"        // a single AgentDef run
	HandlerParallel     = "parallel"     // fan out N agents + a consolidator
	HandlerConsolidator = "consolidator" // a standalone consolidator-agent state
	HandlerTerminal     = "terminal"     // an end state; no agent, no outbound edges required
	HandlerVars         = "vars"         // binds ${var.*} from literals, tokens and captures
	HandlerInput        = "input"        // the start form: a typed schema the client renders
)

// Transition kinds — the `on` label prefix. success is bare; pushback and
// conditional carry a `:<suffix>`.
const (
	OnSuccess     = "success"
	OnPushback    = "pushback"    // pushback:<reason>
	OnConditional = "conditional" // conditional:<expr>
)

// Wait modes for a parallel handler (mirrors Channel.await).
const (
	WaitAll     = "all"
	WaitAny     = "any"
	WaitAtLeast = "at_least" // at_least:<N>
)

// Definition is the full `definition` JSON of a TeamDef.
type Definition struct {
	Entry         string       `json:"entry"`
	MaxIterations int          `json:"max_iterations,omitempty"`
	States        []State      `json:"states"`
	Transitions   []Transition `json:"transitions"`
	// Colors is presentation only (unsaturated state fills, saturated transition
	// edges). It is EXCLUDED from the content hash (see sign.go) so recolouring a
	// workflow doesn't fork its identity.
	Colors *Colors `json:"colors,omitempty"`
	// Layout is presentation only: where a canvas draws each node. EXCLUDED from
	// the content hash for exactly Colors' reason — dragging a node to tidy a
	// diagram must not mint a new version, or the version history stops meaning
	// anything. Exclusion is by omission: teamContent in sign.go is a whitelist,
	// so a field is out of the hash unless it is listed there.
	Layout *Layout `json:"layout,omitempty"`
}

// State is one node: an id + the handler that runs when a task is in it.
type State struct {
	ID      string  `json:"state"`
	Handler Handler `json:"handler"`
}

// Handler is the "who acts" for a state.
type Handler struct {
	Kind string `json:"kind"` // agent | parallel | consolidator | terminal
	// Agent — for kind=agent and kind=consolidator: the AgentDef name to run.
	Agent string `json:"agent,omitempty"`
	// Agents — for kind=parallel: the AgentDef names fanned out concurrently.
	Agents []string `json:"agents,omitempty"`
	// Wait — for kind=parallel: all | any | at_least:<N> (default all).
	Wait string `json:"wait,omitempty"`
	// Consolidator — the AgentDef name that reads the handler's output(s) and
	// picks the outgoing transition. REQUIRED for kind=parallel; OPTIONAL for
	// kind=agent ("re-evaluate after one agent").
	Consolidator string `json:"consolidator,omitempty"`
	// SystemPrompt is this NODE's role, APPENDED to the agent's own system
	// prompt as a second system segment rather than replacing it: the AgentDef
	// says what the agent IS, the node says what it is doing here.
	//
	// It is why a fan-out of N reviewers is ONE AgentDef and N states. Cloning an
	// agent per node would fork a def per role, and — because the agent's base
	// prompt is sent with cache_control — would also lose prompt caching across
	// nodes that share an agent. The composed segments keep the base cacheable
	// and add this one uncached, so N nodes still hit one cached prefix.
	//
	// Content-identifying: Handler rides States, which teamContent hashes, so
	// editing a node's role forks the definition. Existing defs omit the field
	// and hash byte-identically (omitempty).
	SystemPrompt string `json:"system_prompt,omitempty"`
	// InputTemplate is this node's user prompt. When set it REPLACES the input
	// threaded from the previous state; when empty the threaded input is used.
	// Declared by RFC AP and read for the first time here.
	InputTemplate string `json:"input_template,omitempty"`
	TimeoutMS     int    `json:"timeout_ms,omitempty"`
	// Set — kind=vars ONLY: variable name → a value that may itself contain
	// ${…} tokens, resolved when the state runs. This is the one place a
	// workflow assigns a variable, and it is its OWN node kind rather than a
	// block on an agent handler because a canvas exists to make the process
	// legible: an invisible assignment riding something that looks like an agent
	// is exactly what it should prevent.
	//
	// Values are refused at validation if they reference the credentials
	// namespace — variables are non-secret BY CONSTRUCTION. See validateSet.
	Set map[string]string `json:"set,omitempty"`
	// Capture — any handler that produces output: variable name → a
	// strict-subset JSONPath applied to that output when it is JSON. The
	// consolidator envelope is already JSON, so this works on day one.
	// A path that does not resolve binds nothing; it is not an error, mirroring
	// the webhook projector's posture toward an external document.
	Capture map[string]string `json:"capture,omitempty"`
	// Schema — kind=input ONLY: the JSON Schema a client renders as the run
	// form. The runtime does not interpret it; it is carried in the definition
	// so a team is self-describing and a headless caller sees the same contract
	// the canvas does.
	Schema json.RawMessage `json:"schema,omitempty"`
}

// Layout is the optional canvas geometry: where each node sits. Presentation
// only, and excluded from the content hash (see Definition.Layout).
type Layout struct {
	// Nodes maps a state id to its position. A state with no entry is
	// auto-placed by the client.
	Nodes map[string]NodePos `json:"nodes,omitempty"`
}

// NodePos is one node's canvas position (and optional size).
type NodePos struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w,omitempty"`
	H int `json:"h,omitempty"`
}

// Transition is one edge: from-state → to-state, gated by an `on` label.
type Transition struct {
	From string `json:"from"`
	To   string `json:"to"`
	On   string `json:"on"` // success | pushback:<reason> | conditional:<expr>
}

// Colors is the optional presentation scheme.
type Colors struct {
	// Transitions maps a transition kind/label (success, pushback,
	// pushback:<reason>, conditional) to a saturated edge colour.
	Transitions map[string]string `json:"transitions,omitempty"`
	// States maps a state id to an unsaturated fill colour (hex or a named key).
	States map[string]string `json:"states,omitempty"`
}

// Parse unmarshals a TeamDef definition JSON. It does NOT validate the graph —
// call Validate for that. Unknown keys are tolerated (forward-compat + operator
// extension), matching the AgentDef overlay's additionalProperties:true stance.
func Parse(defJSON []byte) (Definition, error) {
	var d Definition
	if err := json.Unmarshal(defJSON, &d); err != nil {
		return Definition{}, fmt.Errorf("team definition: invalid JSON: %w", err)
	}
	return d, nil
}
