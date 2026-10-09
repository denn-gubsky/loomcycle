package teamgraph

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/jsonpath"
)

// varNameRe is the variable-name charset, the same [a-zA-Z0-9_-]{1,64} the
// credentials validator enforces and the expander matches — so a name that
// validates here is a name the expander can actually resolve.
var varNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

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
//   - a local agent's or skill's name is one segment, every "./<name>" a state
//     uses is declared under local.agents, and every "./<name>" a local
//     agent's skills list grants is declared under local.skills;
//   - max_iterations ≥ 0 (0 = use the default). Cycle termination is guaranteed
//     because the per-state cap applies to every state.
//
// It returns the first violation ValidateAll finds (an *Issue), or nil.
func Validate(d Definition) error { return first(ValidateAll(d)) }

// validateAll collects every violation, in the order the checks have always
// run. Where one finding makes a later check meaningless — it would deref a
// missing block, or only restate the finding — that check is skipped, and the
// skip says why.
func validateAll(out *issues, d Definition) {
	entryEmpty := strings.TrimSpace(d.Entry) == ""
	if entryEmpty {
		out.top("entry", "team definition: `entry` is required")
	}
	if len(d.States) == 0 {
		out.top("states", "team definition: at least one state is required")
	}
	if d.MaxIterations < 0 {
		out.top("max_iterations", "team definition: max_iterations must be >= 0 (0 = default %d)", DefaultMaxIterations)
	}
	if d.MaxIterations > MaxAllowedIterations {
		out.top("max_iterations", "team definition: max_iterations %d exceeds the maximum %d", d.MaxIterations, MaxAllowedIterations)
	}
	validateHooks(out, d)
	validateVars(out, d.Vars)

	// State ids: unique + non-empty; validate each handler. A state whose id
	// is empty or repeats one before it is kept out of the id map (the first
	// holder of an id keeps it) and out of the graph checks below: its
	// identity is the finding, and reachability or a dead end judged by that
	// id would be about another state, or about nothing.
	states := make(map[string]State, len(d.States))
	badID := make(map[int]bool)
	for i, s := range d.States {
		if strings.TrimSpace(s.ID) == "" {
			out.top(statePath(i)+".state", "team definition: state[%d] has an empty `state` id", i)
			badID[i] = true
		} else if _, dup := states[s.ID]; dup {
			out.add(&Issue{Path: statePath(i) + ".state", State: s.ID,
				Msg: fmt.Sprintf("team definition: duplicate state id %q", s.ID)})
			badID[i] = true
		}
		validateHandler(out, stateAt{index: i, id: s.ID}, s.Handler)
		if !badID[i] {
			states[s.ID] = s
		}
	}

	// An empty entry was reported above, and with no states every entry
	// "does not resolve" — both would only restate a finding.
	entryOK := false
	if !entryEmpty && len(d.States) > 0 {
		if _, entryOK = states[d.Entry]; !entryOK {
			out.top("entry", "team definition: entry %q does not resolve to a state", d.Entry)
		}
	}
	validateInputSourcePlacement(out, d, entryOK)
	validateLocal(out, d)

	// Transitions: endpoints resolve; `on` well-formed; per-state label uniqueness.
	outbound := make(map[string]map[string]bool) // state -> set of `on` labels
	adj := make(map[string][]string)             // state -> reachable states
	for i, t := range d.Transitions {
		tp := fmt.Sprintf("transitions[%d]", i)
		from, fromOK := states[t.From]
		_, toOK := states[t.To]
		if !fromOK {
			out.top(tp+".from", "team definition: transition[%d] from %q does not resolve to a state", i, t.From)
		}
		if !toOK {
			out.top(tp+".to", "team definition: transition[%d] to %q does not resolve to a state", i, t.To)
		}
		if err := validateOn(i, t.On); err != nil {
			out.top(tp+".on", "%s", err)
		}
		// Every check below is about the state the edge leaves; an edge out of
		// nowhere has none.
		if !fromOK {
			continue
		}
		if from.Handler.Kind == HandlerTerminal {
			out.top(tp, "team definition: terminal state %q must have no outbound transitions", t.From)
			// Not an edge the walk can take: it neither routes nor reaches.
			continue
		}
		if outbound[t.From] == nil {
			outbound[t.From] = map[string]bool{}
		}
		if outbound[t.From][t.On] {
			out.top(tp+".on", "team definition: state %q has duplicate outbound transition label %q (ambiguous route)", t.From, t.On)
		}
		// The label counts as outbound even when `to` does not resolve, so the
		// state is not also called a dead end for an edge it plainly has; the
		// edge joins the graph only when it lands somewhere.
		outbound[t.From][t.On] = true
		if toOK {
			adj[t.From] = append(adj[t.From], t.To)
		}
	}

	validateDecisionEdges(out, d, states)

	// Reachability: BFS from entry; every state must be reachable. Only from
	// an entry that resolves — from none, every state would read unreachable.
	if entryOK {
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
		for i, s := range d.States {
			if !badID[i] && !seen[s.ID] {
				out.add(&Issue{Path: statePath(i), State: s.ID,
					Msg: fmt.Sprintf("team definition: state %q is unreachable from entry %q", s.ID, d.Entry)})
			}
		}
	}

	// A non-terminal state must have at least one outbound transition — else the
	// walk enters it, runs its handler (spending a real agent call), and then
	// dead-ends because no edge leaves it. Only a terminal state may have none.
	// A state whose kind is missing or unknown was reported for that: whether
	// it may end the walk depends on the kind its author meant.
	for i, s := range d.States {
		if badID[i] || !knownKind(s.Handler.Kind) {
			continue
		}
		if s.Handler.Kind != HandlerTerminal && len(outbound[s.ID]) == 0 {
			out.add(&Issue{Path: statePath(i), State: s.ID,
				Msg: fmt.Sprintf("team definition: non-terminal state %q has no outbound transition (dead end)", s.ID)})
		}
	}
}

// knownKind reports whether k is a handler kind this runtime runs.
func knownKind(k string) bool {
	switch k {
	case HandlerAgent, HandlerParallel, HandlerConsolidator, HandlerTerminal,
		HandlerVars, HandlerInput, HandlerStarter, HandlerChannel, HandlerDecision:
		return true
	}
	return false
}

// agentFieldSet names the first of agent/agents/consolidator a handler sets,
// for a refusal that covers all three ("" when none is set).
func agentFieldSet(h Handler) string {
	switch {
	case h.Agent != "":
		return "agent"
	case len(h.Agents) != 0:
		return "agents"
	case h.Consolidator != "":
		return "consolidator"
	}
	return ""
}

func validateHandler(out *issues, at stateAt, h Handler) {
	stateID := at.id
	setsAgents := h.Agent != "" || len(h.Agents) != 0 || h.Consolidator != ""
	switch h.Kind {
	case HandlerAgent, HandlerConsolidator:
		if strings.TrimSpace(h.Agent) == "" {
			out.in(at, "agent", "team definition: state %q handler kind %q requires `agent`", stateID, h.Kind)
		}
		if len(h.Agents) > 0 {
			out.in(at, "agents", "team definition: state %q handler kind %q must not set `agents` (use `agent`)", stateID, h.Kind)
		}
	case HandlerParallel:
		if len(h.Agents) == 0 {
			out.in(at, "agents", "team definition: state %q parallel handler requires a non-empty `agents`", stateID)
		}
		for j, a := range h.Agents {
			if strings.TrimSpace(a) == "" {
				out.in(at, fmt.Sprintf("agents[%d]", j), "team definition: state %q parallel handler has an empty agent name", stateID)
			}
		}
		if strings.TrimSpace(h.Consolidator) == "" {
			out.in(at, "consolidator", "team definition: state %q parallel handler requires a `consolidator` agent", stateID)
		}
		if err := validateWait(stateID, h.Wait); err != nil {
			out.in(at, "wait", "%s", err)
		}
	case HandlerVars:
		if len(h.Set) == 0 {
			out.in(at, "set", "team definition: state %q vars handler requires a non-empty `set`", stateID)
		}
		if setsAgents {
			out.in(at, agentFieldSet(h), "team definition: state %q vars handler must not set agent/agents/consolidator", stateID)
		}
		validateSet(out, at, h.Set)
	case HandlerInput:
		if setsAgents {
			out.in(at, agentFieldSet(h), "team definition: state %q input handler must not set agent/agents/consolidator", stateID)
		}
		if len(h.Schema) > 0 && !json.Valid(h.Schema) {
			out.in(at, "schema", "team definition: state %q input handler `schema` is not valid JSON", stateID)
		}
	case HandlerStarter:
		validateStarter(out, at, h)
	case HandlerChannel:
		if strings.TrimSpace(h.Channel) == "" {
			out.in(at, "channel", "team definition: state %q channel handler requires `channel`", stateID)
		}
		if setsAgents {
			out.in(at, agentFieldSet(h), "team definition: state %q channel handler must not set agent/agents/consolidator — it publishes, it does not run", stateID)
		}
		if h.Source != nil {
			out.in(at, "source", "team definition: state %q channel handler must not set `source` — reading a channel is a `starter`, not a `channel`", stateID)
		}
	case HandlerDecision:
		validateDecision(out, at, h)
	case HandlerTerminal:
		if setsAgents {
			out.in(at, agentFieldSet(h), "team definition: state %q terminal handler must not set agent/agents/consolidator", stateID)
		}
	case "":
		// Every check below is defined relative to the kind; without one they
		// would each restate "this field does not belong" for a kind not chosen.
		out.in(at, "kind", "team definition: state %q handler is missing a `kind`", stateID)
		return
	default:
		out.in(at, "kind", "team definition: state %q has unknown handler kind %q (want agent|parallel|consolidator|terminal|vars|input|starter|channel|decision)", stateID, h.Kind)
		return
	}
	// The Starter's own fields belong to the Starter. Left on another kind they
	// would read as configured and do nothing — the same silent-setting failure
	// `set` and `schema` are guarded against above.
	if h.Kind != HandlerStarter {
		// A channel state's `source` was refused above in its own words.
		if h.Source != nil && h.Kind != HandlerChannel {
			out.in(at, "source", "team definition: state %q sets `source` but is kind %q (starter only)", stateID, h.Kind)
		}
		if h.Fanout != nil {
			out.in(at, "fanout", "team definition: state %q sets `fanout` but is kind %q (starter only)", stateID, h.Kind)
		}
		if h.Sink != nil {
			out.in(at, "sink", "team definition: state %q sets `sink` but is kind %q (starter only)", stateID, h.Kind)
		}
		if len(h.Binds) > 0 {
			out.in(at, "binds", "team definition: state %q sets `binds` but is kind %q (starter only)", stateID, h.Kind)
		}
		if h.Ack != "" {
			out.in(at, "ack", "team definition: state %q sets `ack` but is kind %q (starter only)", stateID, h.Kind)
		}
		if h.Prompt != nil {
			out.in(at, "prompt", "team definition: state %q sets `prompt` but is kind %q — another kind's prompts are `system_prompt` + `input_template`", stateID, h.Kind)
		}
	}
	if h.Kind != HandlerChannel && strings.TrimSpace(h.Channel) != "" {
		out.in(at, "channel", "team definition: state %q sets `channel` but is kind %q — a starter names its channels in `source`/`sink`", stateID, h.Kind)
	}
	if h.Kind != HandlerVars && len(h.Set) > 0 {
		out.in(at, "set", "team definition: state %q sets `set` but is kind %q — assignment belongs on a `vars` state, where it is visible", stateID, h.Kind)
	}
	if h.Kind != HandlerInput && len(h.Schema) > 0 && !h.Source.IsInput() {
		out.in(at, "schema", "team definition: state %q sets `schema` but is kind %q "+
			"(an input state, or a starter whose source is the walk's input)", stateID, h.Kind)
	}
	if f := decisionFieldSet(h); f != "" && h.Kind != HandlerDecision {
		out.in(at, f, "team definition: state %q sets `%s` but is kind %q (decision only)", stateID, f, h.Kind)
	}
	validatePublishing(out, at, h)
	captureIssues(out, at, "capture", h.Capture)
	if h.TimeoutMS < 0 {
		out.in(at, "timeout_ms", "team definition: state %q handler timeout_ms must be >= 0", stateID)
	}
	validatePromptSlots(out, at, h)
}

// The reserved data-slot markers a walk fills AFTER a prompt's placeholders
// have been expanded, with content it never scans. Declared here, beside the
// validation that says where each may be written, and used by the walk under
// these same names so the two cannot disagree about a marker's spelling.
const (
	// ThreadedOutputSlot receives what the previous state handed over (the
	// walk's own input, on the entry state).
	ThreadedOutputSlot = "{{thread.output}}"
	// StarterMessageSlot / StarterMessagesSlot receive a Starter's work item
	// (per=message, per=chunk) or its whole batch (per=once).
	StarterMessageSlot  = "{{starter.message}}"
	StarterMessagesSlot = "{{starter.messages}}"
)

// validatePromptSlots refuses a reserved slot marker written where the walk
// would not fill it, or must not.
//
// The hand-off between states is another agent's OUTPUT — it may carry whatever
// a tool result or a fetched page put into it. In the user segment that is what
// it is for. In `system_prompt` it would be untrusted text speaking with the
// team's voice, so the marker is refused there on every kind.
//
// The other refusals are silent-setting guards: a Starter threads nothing to
// its runs (each gets its work item), and only a Starter has a work item, so
// the wrong marker in either place would reach the agent as literal braces.
func validatePromptSlots(out *issues, at stateAt, h Handler) {
	stateID := at.id
	if strings.Contains(h.SystemPrompt, ThreadedOutputSlot) {
		out.in(at, "system_prompt", "team definition: state %q `system_prompt` contains %s — the previous state's output is "+
			"another agent's text and may only go in the user prompt; put it in `input_template`", stateID, ThreadedOutputSlot)
	}
	if h.Kind == HandlerStarter {
		if h.Prompt == nil {
			return
		}
		for _, f := range []struct{ name, text string }{{"prompt.system", h.Prompt.System}, {"prompt.input", h.Prompt.Input}} {
			if strings.Contains(f.text, ThreadedOutputSlot) {
				out.in(at, f.name, "team definition: state %q starter `%s` contains %s — a starter hands each run its "+
					"work item, not the previous state's output; use %s (%s for per=once)",
					stateID, f.name, ThreadedOutputSlot, StarterMessageSlot, StarterMessagesSlot)
			}
		}
		return
	}
	for _, f := range []struct{ name, text string }{{"system_prompt", h.SystemPrompt}, {"input_template", h.InputTemplate}} {
		for _, marker := range []string{StarterMessageSlot, StarterMessagesSlot} {
			if strings.Contains(f.text, marker) {
				out.in(at, f.name, "team definition: state %q `%s` contains %s but is kind %q — only a starter has a "+
					"work item; the previous state's output is %s, in `input_template`",
					stateID, f.name, marker, h.Kind, ThreadedOutputSlot)
			}
		}
	}
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

func validateSet(out *issues, at stateAt, set map[string]string) {
	for _, name := range sortedKeys(set) {
		field := PathKey("set", name)
		if !varNameRe.MatchString(name) {
			out.in(at, field, "team definition: state %q set key %q must match [a-zA-Z0-9_-]{1,64}", at.id, name)
		}
		if readsSecretNamespace(set[name]) {
			out.in(at, field, "team definition: state %q set %q reads the credentials namespace — "+
				"variables are non-secret by construction; a secret copied into one becomes a plaintext "+
				"value in every transcript, snapshot and prompt-cache entry downstream", at.id, name)
		}
	}
}

// readsSecretNamespace reports whether a variable's value names a secret
// namespace. One reader for every place a variable is given a value by the
// author or the caller — a vars state's `set`, a declared default, a value
// supplied at start — so the three cannot disagree about what a secret is.
func readsSecretNamespace(value string) bool {
	for _, ns := range secretNamespaces {
		if strings.Contains(value, ns) {
			return true
		}
	}
	return false
}

// validateCapture is captureIssues' first finding for a state with no
// position — what a check of one map alone needs.
func validateCapture(stateID string, capture map[string]string) error {
	var out issues
	captureIssues(&out, stateAt{id: stateID}, "capture", capture)
	return first(out)
}

// captureIssues checks a name → JSONPath map: a state's `capture`, or a
// starter's `binds` (key is which, for the path; the refusal text is the same
// for both, as it always was).
func captureIssues(out *issues, at stateAt, key string, capture map[string]string) {
	for _, name := range sortedKeys(capture) {
		field := PathKey(key, name)
		if !varNameRe.MatchString(name) {
			out.in(at, field, "team definition: state %q capture key %q must match [a-zA-Z0-9_-]{1,64}", at.id, name)
		}
		if _, err := jsonpath.Parse(capture[name]); err != nil {
			out.in(at, field, "team definition: state %q capture %q: %v", at.id, name, err)
		}
	}
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

// validateStarter checks a starter handler's shape. Every refusal here is a
// definition that would otherwise dispatch the wrong number of runs, dispatch
// them to nowhere, or wait for something that can never arrive — each a silent
// wrong answer rather than an error at run time.
func validateStarter(out *issues, at stateAt, h Handler) {
	stateID := at.id
	if h.Agent != "" || len(h.Agents) != 0 || h.Consolidator != "" {
		out.in(at, agentFieldSet(h), "team definition: state %q starter handler names its agents in `fanout`, not in agent/agents/consolidator", stateID)
	}
	isDoc, isInput := h.Source.IsDocument(), h.Source.IsInput()
	switch {
	case isDoc:
		validateDocumentSource(out, at, h)
	case isInput:
		validateInputSource(out, at, h)
	case h.Source != nil && h.Source.Kind != "" && h.Source.Kind != SourceChannel:
		out.in(at, "source.kind", "team definition: state %q starter source has invalid kind %q (want channel|document|input)", stateID, h.Source.Kind)
	default:
		validateChannelSource(out, at, h)
	}

	// Everything that reads the fan-out stops at a missing one; the sink, the
	// ack and the binds do not depend on it and are still checked.
	if h.Fanout == nil {
		out.in(at, "fanout", "team definition: state %q starter handler requires `fanout`", stateID)
	} else {
		validateFanout(out, at, h, isDoc, isInput)
	}
	if h.Sink != nil && strings.TrimSpace(h.Sink.Channel) == "" {
		out.in(at, "sink.channel", "team definition: state %q starter `sink` is present but names no channel", stateID)
	}
	// A document or input source refuses any `ack` in its own words (neither
	// has a cursor); its value would be beside the point.
	if !isDoc && !isInput {
		switch h.Ack {
		case "", AckAfterResults, AckAfterRead:
		default:
			out.in(at, "ack", "team definition: state %q starter has invalid ack %q (want after_results|after_read)", stateID, h.Ack)
		}
	}
	captureIssues(out, at, "binds", h.Binds)
}

// validateFanout checks a starter's (non-nil) fan-out against its source.
func validateFanout(out *issues, at stateAt, h Handler, isDoc, isInput bool) {
	stateID := at.id
	f := h.Fanout
	hasOne, hasMany := strings.TrimSpace(f.Agent) != "", len(f.Agents) > 0
	if hasOne == hasMany {
		out.in(at, "fanout", "team definition: state %q starter fanout needs exactly one of `agent` or `agents`", stateID)
	}
	for j, a := range f.Agents {
		if strings.TrimSpace(a) == "" {
			out.in(at, fmt.Sprintf("fanout.agents[%d]", j), "team definition: state %q starter fanout has an empty agent name", stateID)
		}
	}
	// per=message and per=chunk each belong to one source kind. The empty
	// default means message, so a document source must say which it wants
	// rather than inherit a shape that has no meaning for it.
	//
	// An input source refused per=chunk in its own words already. Once `per`
	// does not fit the source, what `max` must be for that `per` is moot.
	perMismatch := isInput && f.Per == FanoutPerChunk
	switch per := f.Per; {
	case isDoc && (per == "" || per == FanoutPerMessage):
		out.in(at, "fanout.per", "team definition: state %q starter reads a document, which has sections, not messages — "+
			"set fanout.per to chunk (one run per section) or once (one run holding every section)", stateID)
		perMismatch = true
	case !isDoc && !isInput && per == FanoutPerChunk:
		out.in(at, "fanout.per", "team definition: state %q starter fanout per=chunk needs a document source (source.kind: document); "+
			"a channel source fans out per=message or per=once", stateID)
		perMismatch = true
	}
	if !perMismatch {
		switch f.Per {
		case "", FanoutPerMessage:
			// Dynamic fan-out is a spawn amplifier: one run per message, and the
			// message count is whatever accumulated on the channel. An authored
			// ceiling is the only thing the author controls, so it is required.
			if f.Max < 1 {
				out.in(at, "fanout.max", "team definition: state %q starter fanout per=message requires `max` >= 1 — "+
					"the wave is as wide as the channel is deep, so the ceiling is not optional", stateID)
			}
		case FanoutPerChunk:
			// The same amplifier: the wave is as wide as the document is long, and
			// whoever can edit the document decides that, not the definition.
			if f.Max < 1 {
				out.in(at, "fanout.max", "team definition: state %q starter fanout per=chunk requires `max` >= 1 — "+
					"the wave is as wide as the document has sections, so the ceiling is not optional", stateID)
			}
		case FanoutPerOnce:
			if f.Max != 0 {
				out.in(at, "fanout.max", "team definition: state %q starter fanout per=once spawns one run, so `max` means nothing", stateID)
			}
		default:
			out.in(at, "fanout.per", "team definition: state %q starter fanout has invalid per %q (want message|once|chunk)", stateID, f.Per)
		}
	}
	if err := validateWait(stateID, f.Wait); err != nil {
		out.in(at, "fanout.wait", "%s", err)
	}

	// `binds` project THE source message into ${var.*}. per=once hands the agent
	// a batch, so there is no "the" message — binding from an arbitrary one of N
	// would be a silent choice the author never made.
	if f.Per == FanoutPerOnce && len(h.Binds) > 0 {
		out.in(at, "binds", "team definition: state %q starter has `binds` with per=once — "+
			"binds project ONE source message, and per=once hands the agent the whole batch", stateID)
	}
}

// validateChannelSource checks a channel source: the name, and the wait
// predicate over it.
func validateChannelSource(out *issues, at stateAt, h Handler) {
	stateID := at.id
	const needsChannel = "team definition: state %q starter handler requires `source.channel` — a starter reads exactly one channel " +
		"(or, with source.kind: document, one document)"
	if h.Source == nil {
		// Nothing else here exists to check.
		out.in(at, "source", needsChannel, stateID)
		return
	}
	if strings.TrimSpace(h.Source.Channel) == "" {
		out.in(at, "source.channel", needsChannel, stateID)
	}
	// The document fields on a channel source would read as configured and
	// do nothing.
	if f := firstOf(fieldIf{"source.path", h.Source.Path != ""}, fieldIf{"source.scope", h.Source.Scope != ""},
		fieldIf{"source.select", h.Source.Select != ""}); f != "" {
		out.in(at, f, "team definition: state %q starter source sets path/scope/select, which only a document source reads — "+
			"set source.kind: document, or remove them", stateID)
	}
	// `all` counts CHANNELS, not messages (Channel op=await's predicate). Over
	// the single channel a starter reads it is therefore identical to `any` and
	// returns after the FIRST message, so "wait for all of them" written as
	// `all` silently yields one of N. It is never what the author meant.
	nReported := false
	switch h.Source.Wait {
	case "", WaitAny:
	case WaitAtLeast:
		if h.Source.N < 1 {
			out.in(at, "source.n", "team definition: state %q starter source wait=at_least requires `n` >= 1", stateID)
			nReported = true
		}
	case WaitAll:
		out.in(at, "source.wait", "team definition: state %q starter source wait=%q counts CHANNELS, and a starter reads ONE — "+
			"over one channel it returns after the first message. Use at_least with `n`, or any", stateID, WaitAll)
	default:
		out.in(at, "source.wait", "team definition: state %q starter source has invalid wait %q (want any|at_least)", stateID, h.Source.Wait)
	}
	// A negative `n` under at_least was just refused for being below 1.
	if f := firstOf(fieldIf{"source.wait_ms", h.Source.WaitMS < 0}, fieldIf{"source.batch", h.Source.Batch < 0},
		fieldIf{"source.n", h.Source.N < 0 && !nReported}); f != "" {
		out.in(at, f, "team definition: state %q starter source wait_ms/batch/n must be >= 0", stateID)
	}
}

// fieldIf is a handler-relative field and whether a check holds for it.
type fieldIf struct {
	field string
	set   bool
}

// firstOf returns the first field, in order, for which the check holds — the
// field a refusal covering several of them points at — or "".
func firstOf(fields ...fieldIf) string {
	for _, f := range fields {
		if f.set {
			return f.field
		}
	}
	return ""
}

// validateDocumentSource checks a document source. A document has no cursor
// and nothing to wait for, so every field that shapes a channel read — the
// wait predicate, the batch, the ack — is refused rather than ignored: each
// would read as configured and do nothing.
func validateDocumentSource(out *issues, at stateAt, h Handler) {
	stateID := at.id
	src := h.Source
	if strings.TrimSpace(src.Channel) != "" {
		out.in(at, "source.channel", "team definition: state %q starter reads a document, so `source.channel` means nothing — remove it", stateID)
	}
	p := src.Path
	switch {
	case strings.TrimSpace(p) == "":
		out.in(at, "source.path", "team definition: state %q starter document source requires `source.path`", stateID)
	case !strings.HasPrefix(p, "/") || p == "/":
		out.in(at, "source.path", "team definition: state %q starter document source path %q must be an absolute document path, e.g. /specs/acme", stateID, src.Path)
	case strings.Contains(p, "${") || strings.Contains(p, "{{"):
		// A fixed literal, so what a definition reads is what it says. A
		// variable here would make the read depend on the walk's input.
		out.in(at, "source.path", "team definition: state %q starter document source path %q must be a fixed path — variables are not supported", stateID, src.Path)
	}
	switch src.Scope {
	case "", "user", "tenant":
	default:
		// No `agent`: a walk runs as a person, not as an agent, so there is
		// no agent tree for it to read.
		out.in(at, "source.scope", "team definition: state %q starter document source has invalid scope %q (want user|tenant)", stateID, src.Scope)
	}
	switch src.Select {
	case "", SelectChunks:
	default:
		out.in(at, "source.select", "team definition: state %q starter document source has invalid select %q (want chunks)", stateID, src.Select)
	}
	if f := firstOf(fieldIf{"source.wait", src.Wait != ""}, fieldIf{"source.n", src.N != 0},
		fieldIf{"source.wait_ms", src.WaitMS != 0}); f != "" {
		out.in(at, f, "team definition: state %q starter reads a document, which is read once when the wave dispatches — "+
			"there is nothing to wait for, so remove source.wait/n/wait_ms", stateID)
	}
	if src.Batch != 0 {
		out.in(at, "source.batch", "team definition: state %q starter reads a document, which is read whole — remove source.batch "+
			"(fanout.max bounds the wave)", stateID)
	}
	if h.Ack != "" {
		out.in(at, "ack", "team definition: state %q starter reads a document, which has no cursor to acknowledge — remove `ack`", stateID)
	}
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

// ValidateHooks is the hooks part of Validate alone: the checks a snapshot
// restore re-runs over a stored team definition, whose graph it does not
// re-validate (a team dials nothing; its hooks' webhooks do).
func ValidateHooks(d Definition) error {
	var out issues
	validateHooks(&out, d)
	return first(out)
}

// validateHooks checks a team's hooks. The walk's own run makes no model or
// tool calls and runs no agent, so only run_end ever fires for it; a state's
// hooks are added to the runs it starts, so a state that starts none (vars,
// input, channel, terminal) carrying them would name gates that never fire.
//
// Each entry is checked on its own (a one-entry EventHooks / ToolHooks) so
// every bad one is reported at its own path; the text is the one the whole
// map's Validate gives for that entry, since it checks in the same order.
func validateHooks(out *issues, d Definition) {
	for _, event := range sortedPhases(d.Hooks) {
		if event != hooks.PhaseRunEnd {
			out.top(PathKey("hooks", string(event)), "team definition: hooks: the walk only ends, so only run_end fires for it (got %s)", event)
		}
	}
	// Only run_end's entries: any other event was refused just above, and the
	// hooks package would refuse it again as unknown or misplaced.
	runEnd := PathKey("hooks", string(hooks.PhaseRunEnd))
	for j, entry := range d.Hooks[hooks.PhaseRunEnd] {
		if err := (hooks.EventHooks{hooks.PhaseRunEnd: {entry}}).Validate(""); err != nil {
			out.top(fmt.Sprintf("%s[%d]", runEnd, j), "team definition: hooks: %v", err)
		}
	}
	for i, st := range d.States {
		h := st.Handler
		if len(h.Hooks) == 0 && len(h.ToolHooks) == 0 {
			continue
		}
		at := stateAt{index: i, id: st.ID}
		switch h.Kind {
		case HandlerAgent, HandlerParallel, HandlerConsolidator, HandlerStarter:
		default:
			field := "hooks"
			if len(h.Hooks) == 0 {
				field = "tool_hooks"
			}
			out.in(at, field, "team definition: state %q (%s) starts no run, so it cannot carry hooks", st.ID, h.Kind)
			// What the hooks say is moot on a state that may carry none.
			continue
		}
		for _, phase := range sortedPhases(h.Hooks) {
			field := PathKey("hooks", string(phase))
			// The event itself first: a bad one would fail every entry alike.
			if err := (hooks.EventHooks{phase: nil}).Validate(""); err != nil {
				out.in(at, field, "team definition: state %q: %v", st.ID, err)
				continue
			}
			for j, entry := range h.Hooks[phase] {
				if err := (hooks.EventHooks{phase: {entry}}).Validate(""); err != nil {
					out.in(at, fmt.Sprintf("%s[%d]", field, j), "team definition: state %q: %v", st.ID, err)
				}
			}
		}
		tools := make([]string, 0, len(h.ToolHooks))
		for name := range h.ToolHooks {
			tools = append(tools, name)
		}
		sort.Strings(tools)
		for _, name := range tools {
			if err := (hooks.ToolHooks{name: h.ToolHooks[name]}).Validate(); err != nil {
				out.in(at, PathKey("tool_hooks", name), "team definition: state %q: %v", st.ID, err)
			}
		}
	}
}

// sortedPhases orders a hooks map's events, for the reason sortedKeys gives.
func sortedPhases(e hooks.EventHooks) []hooks.Phase {
	out := make([]hooks.Phase, 0, len(e))
	for p := range e {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
