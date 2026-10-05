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
//   - a local agent's name is one segment, and every "./<name>" a state uses
//     is declared under local.agents;
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
	if err := validateHooks(d); err != nil {
		return err
	}
	if err := validateVars(d.Vars); err != nil {
		return err
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
	if err := validateInputSourcePlacement(d); err != nil {
		return err
	}
	if err := validateLocal(d); err != nil {
		return err
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
	case HandlerStarter:
		if err := validateStarter(stateID, h); err != nil {
			return err
		}
	case HandlerChannel:
		if strings.TrimSpace(h.Channel) == "" {
			return fmt.Errorf("team definition: state %q channel handler requires `channel`", stateID)
		}
		if h.Agent != "" || len(h.Agents) != 0 || h.Consolidator != "" {
			return fmt.Errorf("team definition: state %q channel handler must not set agent/agents/consolidator — it publishes, it does not run", stateID)
		}
		if h.Source != nil {
			return fmt.Errorf("team definition: state %q channel handler must not set `source` — reading a channel is a `starter`, not a `channel`", stateID)
		}
	case HandlerTerminal:
		if h.Agent != "" || len(h.Agents) != 0 || h.Consolidator != "" {
			return fmt.Errorf("team definition: state %q terminal handler must not set agent/agents/consolidator", stateID)
		}
	case "":
		return fmt.Errorf("team definition: state %q handler is missing a `kind`", stateID)
	default:
		return fmt.Errorf("team definition: state %q has unknown handler kind %q (want agent|parallel|consolidator|terminal|vars|input|starter|channel)", stateID, h.Kind)
	}
	// The Starter's own fields belong to the Starter. Left on another kind they
	// would read as configured and do nothing — the same silent-setting failure
	// `set` and `schema` are guarded against above.
	if h.Kind != HandlerStarter {
		switch {
		case h.Source != nil:
			return fmt.Errorf("team definition: state %q sets `source` but is kind %q (starter only)", stateID, h.Kind)
		case h.Fanout != nil:
			return fmt.Errorf("team definition: state %q sets `fanout` but is kind %q (starter only)", stateID, h.Kind)
		case h.Sink != nil:
			return fmt.Errorf("team definition: state %q sets `sink` but is kind %q (starter only)", stateID, h.Kind)
		case len(h.Binds) > 0:
			return fmt.Errorf("team definition: state %q sets `binds` but is kind %q (starter only)", stateID, h.Kind)
		case h.Ack != "":
			return fmt.Errorf("team definition: state %q sets `ack` but is kind %q (starter only)", stateID, h.Kind)
		case h.Prompt != nil:
			return fmt.Errorf("team definition: state %q sets `prompt` but is kind %q — another kind's prompts are `system_prompt` + `input_template`", stateID, h.Kind)
		}
	}
	if h.Kind != HandlerChannel && strings.TrimSpace(h.Channel) != "" {
		return fmt.Errorf("team definition: state %q sets `channel` but is kind %q — a starter names its channels in `source`/`sink`", stateID, h.Kind)
	}
	if h.Kind != HandlerVars && len(h.Set) > 0 {
		return fmt.Errorf("team definition: state %q sets `set` but is kind %q — assignment belongs on a `vars` state, where it is visible", stateID, h.Kind)
	}
	if h.Kind != HandlerInput && len(h.Schema) > 0 && !h.Source.IsInput() {
		return fmt.Errorf("team definition: state %q sets `schema` but is kind %q "+
			"(an input state, or a starter whose source is the walk's input)", stateID, h.Kind)
	}
	if err := validatePublishing(stateID, h); err != nil {
		return err
	}
	if err := validateCapture(stateID, h.Capture); err != nil {
		return err
	}
	if h.TimeoutMS < 0 {
		return fmt.Errorf("team definition: state %q handler timeout_ms must be >= 0", stateID)
	}
	return validatePromptSlots(stateID, h)
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
func validatePromptSlots(stateID string, h Handler) error {
	if strings.Contains(h.SystemPrompt, ThreadedOutputSlot) {
		return fmt.Errorf("team definition: state %q `system_prompt` contains %s — the previous state's output is "+
			"another agent's text and may only go in the user prompt; put it in `input_template`", stateID, ThreadedOutputSlot)
	}
	if h.Kind == HandlerStarter {
		if h.Prompt == nil {
			return nil
		}
		for _, f := range []struct{ name, text string }{{"prompt.system", h.Prompt.System}, {"prompt.input", h.Prompt.Input}} {
			if strings.Contains(f.text, ThreadedOutputSlot) {
				return fmt.Errorf("team definition: state %q starter `%s` contains %s — a starter hands each run its "+
					"work item, not the previous state's output; use %s (%s for per=once)",
					stateID, f.name, ThreadedOutputSlot, StarterMessageSlot, StarterMessagesSlot)
			}
		}
		return nil
	}
	for _, f := range []struct{ name, text string }{{"system_prompt", h.SystemPrompt}, {"input_template", h.InputTemplate}} {
		for _, marker := range []string{StarterMessageSlot, StarterMessagesSlot} {
			if strings.Contains(f.text, marker) {
				return fmt.Errorf("team definition: state %q `%s` contains %s but is kind %q — only a starter has a "+
					"work item; the previous state's output is %s, in `input_template`",
					stateID, f.name, marker, h.Kind, ThreadedOutputSlot)
			}
		}
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
		if readsSecretNamespace(set[name]) {
			return fmt.Errorf("team definition: state %q set %q reads the credentials namespace — "+
				"variables are non-secret by construction; a secret copied into one becomes a plaintext "+
				"value in every transcript, snapshot and prompt-cache entry downstream", stateID, name)
		}
	}
	return nil
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

// validateStarter checks a starter handler's shape. Every refusal here is a
// definition that would otherwise dispatch the wrong number of runs, dispatch
// them to nowhere, or wait for something that can never arrive — each a silent
// wrong answer rather than an error at run time.
func validateStarter(stateID string, h Handler) error {
	if h.Agent != "" || len(h.Agents) != 0 || h.Consolidator != "" {
		return fmt.Errorf("team definition: state %q starter handler names its agents in `fanout`, not in agent/agents/consolidator", stateID)
	}
	isDoc := h.Source.IsDocument()
	switch {
	case isDoc:
		if err := validateDocumentSource(stateID, h); err != nil {
			return err
		}
	case h.Source.IsInput():
		if err := validateInputSource(stateID, h); err != nil {
			return err
		}
	case h.Source != nil && h.Source.Kind != "" && h.Source.Kind != SourceChannel:
		return fmt.Errorf("team definition: state %q starter source has invalid kind %q (want channel|document|input)", stateID, h.Source.Kind)
	default:
		if err := validateChannelSource(stateID, h); err != nil {
			return err
		}
	}

	if h.Fanout == nil {
		return fmt.Errorf("team definition: state %q starter handler requires `fanout`", stateID)
	}
	hasOne, hasMany := strings.TrimSpace(h.Fanout.Agent) != "", len(h.Fanout.Agents) > 0
	if hasOne == hasMany {
		return fmt.Errorf("team definition: state %q starter fanout needs exactly one of `agent` or `agents`", stateID)
	}
	for _, a := range h.Fanout.Agents {
		if strings.TrimSpace(a) == "" {
			return fmt.Errorf("team definition: state %q starter fanout has an empty agent name", stateID)
		}
	}
	// per=message and per=chunk each belong to one source kind. The empty
	// default means message, so a document source must say which it wants
	// rather than inherit a shape that has no meaning for it.
	switch per := h.Fanout.Per; {
	case isDoc && (per == "" || per == FanoutPerMessage):
		return fmt.Errorf("team definition: state %q starter reads a document, which has sections, not messages — "+
			"set fanout.per to chunk (one run per section) or once (one run holding every section)", stateID)
	case !isDoc && per == FanoutPerChunk:
		return fmt.Errorf("team definition: state %q starter fanout per=chunk needs a document source (source.kind: document); "+
			"a channel source fans out per=message or per=once", stateID)
	}
	switch h.Fanout.Per {
	case "", FanoutPerMessage:
		// Dynamic fan-out is a spawn amplifier: one run per message, and the
		// message count is whatever accumulated on the channel. An authored
		// ceiling is the only thing the author controls, so it is required.
		if h.Fanout.Max < 1 {
			return fmt.Errorf("team definition: state %q starter fanout per=message requires `max` >= 1 — "+
				"the wave is as wide as the channel is deep, so the ceiling is not optional", stateID)
		}
	case FanoutPerChunk:
		// The same amplifier: the wave is as wide as the document is long, and
		// whoever can edit the document decides that, not the definition.
		if h.Fanout.Max < 1 {
			return fmt.Errorf("team definition: state %q starter fanout per=chunk requires `max` >= 1 — "+
				"the wave is as wide as the document has sections, so the ceiling is not optional", stateID)
		}
	case FanoutPerOnce:
		if h.Fanout.Max != 0 {
			return fmt.Errorf("team definition: state %q starter fanout per=once spawns one run, so `max` means nothing", stateID)
		}
	default:
		return fmt.Errorf("team definition: state %q starter fanout has invalid per %q (want message|once|chunk)", stateID, h.Fanout.Per)
	}
	if err := validateWait(stateID, h.Fanout.Wait); err != nil {
		return err
	}

	// `binds` project THE source message into ${var.*}. per=once hands the agent
	// a batch, so there is no "the" message — binding from an arbitrary one of N
	// would be a silent choice the author never made.
	if h.Fanout.Per == FanoutPerOnce && len(h.Binds) > 0 {
		return fmt.Errorf("team definition: state %q starter has `binds` with per=once — "+
			"binds project ONE source message, and per=once hands the agent the whole batch", stateID)
	}
	if h.Sink != nil && strings.TrimSpace(h.Sink.Channel) == "" {
		return fmt.Errorf("team definition: state %q starter `sink` is present but names no channel", stateID)
	}
	switch h.Ack {
	case "", AckAfterResults, AckAfterRead:
	default:
		return fmt.Errorf("team definition: state %q starter has invalid ack %q (want after_results|after_read)", stateID, h.Ack)
	}
	return validateCapture(stateID, h.Binds)
}

// validateChannelSource checks a channel source: the name, and the wait
// predicate over it.
func validateChannelSource(stateID string, h Handler) error {
	if h.Source == nil || strings.TrimSpace(h.Source.Channel) == "" {
		return fmt.Errorf("team definition: state %q starter handler requires `source.channel` — a starter reads exactly one channel "+
			"(or, with source.kind: document, one document)", stateID)
	}
	// The document fields on a channel source would read as configured and
	// do nothing.
	if h.Source.Path != "" || h.Source.Scope != "" || h.Source.Select != "" {
		return fmt.Errorf("team definition: state %q starter source sets path/scope/select, which only a document source reads — "+
			"set source.kind: document, or remove them", stateID)
	}
	// `all` counts CHANNELS, not messages (Channel op=await's predicate). Over
	// the single channel a starter reads it is therefore identical to `any` and
	// returns after the FIRST message, so "wait for all of them" written as
	// `all` silently yields one of N. It is never what the author meant.
	switch h.Source.Wait {
	case "", WaitAny:
	case WaitAtLeast:
		if h.Source.N < 1 {
			return fmt.Errorf("team definition: state %q starter source wait=at_least requires `n` >= 1", stateID)
		}
	case WaitAll:
		return fmt.Errorf("team definition: state %q starter source wait=%q counts CHANNELS, and a starter reads ONE — "+
			"over one channel it returns after the first message. Use at_least with `n`, or any", stateID, WaitAll)
	default:
		return fmt.Errorf("team definition: state %q starter source has invalid wait %q (want any|at_least)", stateID, h.Source.Wait)
	}
	if h.Source.WaitMS < 0 || h.Source.Batch < 0 || h.Source.N < 0 {
		return fmt.Errorf("team definition: state %q starter source wait_ms/batch/n must be >= 0", stateID)
	}
	return nil
}

// validateDocumentSource checks a document source. A document has no cursor
// and nothing to wait for, so every field that shapes a channel read — the
// wait predicate, the batch, the ack — is refused rather than ignored: each
// would read as configured and do nothing.
func validateDocumentSource(stateID string, h Handler) error {
	src := h.Source
	if strings.TrimSpace(src.Channel) != "" {
		return fmt.Errorf("team definition: state %q starter reads a document, so `source.channel` means nothing — remove it", stateID)
	}
	p := src.Path
	switch {
	case strings.TrimSpace(p) == "":
		return fmt.Errorf("team definition: state %q starter document source requires `source.path`", stateID)
	case !strings.HasPrefix(p, "/") || p == "/":
		return fmt.Errorf("team definition: state %q starter document source path %q must be an absolute document path, e.g. /specs/acme", stateID, src.Path)
	case strings.Contains(p, "${") || strings.Contains(p, "{{"):
		// A fixed literal, so what a definition reads is what it says. A
		// variable here would make the read depend on the walk's input.
		return fmt.Errorf("team definition: state %q starter document source path %q must be a fixed path — variables are not supported", stateID, src.Path)
	}
	switch src.Scope {
	case "", "user", "tenant":
	default:
		// No `agent`: a walk runs as a person, not as an agent, so there is
		// no agent tree for it to read.
		return fmt.Errorf("team definition: state %q starter document source has invalid scope %q (want user|tenant)", stateID, src.Scope)
	}
	switch src.Select {
	case "", SelectChunks:
	default:
		return fmt.Errorf("team definition: state %q starter document source has invalid select %q (want chunks)", stateID, src.Select)
	}
	switch {
	case src.Wait != "" || src.N != 0 || src.WaitMS != 0:
		return fmt.Errorf("team definition: state %q starter reads a document, which is read once when the wave dispatches — "+
			"there is nothing to wait for, so remove source.wait/n/wait_ms", stateID)
	case src.Batch != 0:
		return fmt.Errorf("team definition: state %q starter reads a document, which is read whole — remove source.batch "+
			"(fanout.max bounds the wave)", stateID)
	case h.Ack != "":
		return fmt.Errorf("team definition: state %q starter reads a document, which has no cursor to acknowledge — remove `ack`", stateID)
	}
	return nil
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
func ValidateHooks(d Definition) error { return validateHooks(d) }

// validateHooks checks a team's hooks. The walk's own run makes no model or
// tool calls and runs no agent, so only run_end ever fires for it; a state's
// hooks are added to the runs it starts, so a state that starts none (vars,
// input, channel, terminal) carrying them would name gates that never fire.
func validateHooks(d Definition) error {
	for event := range d.Hooks {
		if event != hooks.PhaseRunEnd {
			return fmt.Errorf("team definition: hooks: the walk only ends, so only run_end fires for it (got %s)", event)
		}
	}
	if err := d.Hooks.Validate(""); err != nil {
		return fmt.Errorf("team definition: hooks: %w", err)
	}
	for _, st := range d.States {
		h := st.Handler
		if len(h.Hooks) == 0 && len(h.ToolHooks) == 0 {
			continue
		}
		switch h.Kind {
		case HandlerAgent, HandlerParallel, HandlerConsolidator, HandlerStarter:
		default:
			return fmt.Errorf("team definition: state %q (%s) starts no run, so it cannot carry hooks", st.ID, h.Kind)
		}
		if err := h.Hooks.Validate(""); err != nil {
			return fmt.Errorf("team definition: state %q: %w", st.ID, err)
		}
		if err := h.ToolHooks.Validate(); err != nil {
			return fmt.Errorf("team definition: state %q: %w", st.ID, err)
		}
	}
	return nil
}
