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

	"github.com/denn-gubsky/loomcycle/internal/hooks"
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
	HandlerStarter      = "starter"      // reads ONE channel and dispatches a wave of agent runs
	HandlerChannel      = "channel"      // publishes to a channel; it does NOT read one
)

// Transition kinds — the `on` label prefix. success is bare; pushback and
// conditional carry a `:<suffix>`.
const (
	OnSuccess     = "success"
	OnPushback    = "pushback"    // pushback:<reason>
	OnConditional = "conditional" // conditional:<expr>
)

// Fan-out shapes for a starter handler.
const (
	FanoutPerMessage = "message" // one run per message read — dynamic N
	FanoutPerOnce    = "once"    // one run holding the whole batch
	FanoutPerChunk   = "chunk"   // one run per document section — document sources only
)

// What a starter reads.
const (
	SourceChannel  = "channel"  // the default: one channel, behind a cursor
	SourceDocument = "document" // one document's top-level sections, read once
	SourceInput    = "input"    // the walk's own input, read once; entry state only
)

// SelectChunks is the only document selection: the root's direct children
// (the top-level sections), in document order.
const SelectChunks = "chunks"

// When a starter advances its source cursor.
const (
	AckAfterResults = "after_results" // at-least-once: a crash mid-wave redelivers
	AckAfterRead    = "after_read"    // at-most-once: a crash loses the batch
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
	// Channels is the TEAM's channel ACL — the Starter is the single ACL subject
	// for its source and sink, so the authority lives on the workflow rather
	// than on each agent in a wave.
	//
	// Unlike Colors and Layout this IS content and IS hashed: it is authority,
	// and authority that can change without changing the definition's identity
	// is not auditable. It is validated at create/fork to only NARROW what the
	// authoring principal already holds (trust rule 4 — inherit, never widen).
	Channels *TeamChannels `json:"channels,omitempty"`

	// Hooks are the walk's own: the walk is a run, and ends like one, so its
	// run_end hooks report how it ended. Content, and hashed.
	Hooks hooks.EventHooks `json:"hooks,omitempty"`

	// Vars DECLARES the team's variables: name → default value. A walk starts
	// with these, a caller may supply its own value for a declared name when it
	// starts one, and a `vars` state, a `capture` or a starter's `binds` may
	// overwrite either while it runs. Declaring is what lets a start refuse a
	// name the team never reads instead of carrying it silently.
	//
	// A default is a LITERAL: it is never expanded, so a ${…} written in one
	// reaches a prompt as those characters (see vars.go). Content, and hashed.
	Vars map[string]string `json:"vars,omitempty"`
}

// TeamChannels is the workflow's own channel allowlist, same shape as an
// agent's so an operator reads one grammar in both places.
type TeamChannels struct {
	Publish   []string `json:"publish,omitempty"`
	Subscribe []string `json:"subscribe,omitempty"`
}

// State is one node: an id + the handler that runs when a task is in it.
type State struct {
	ID      string  `json:"state"`
	Handler Handler `json:"handler"`
}

// Handler is the "who acts" for a state.
type Handler struct {
	Kind string `json:"kind"` // agent | parallel | consolidator | terminal | vars | input

	// Hooks / ToolHooks are added to every run this state starts — its agent,
	// each parallel member and the consolidator, each Starter wave member — on
	// top of the hooks that agent's own definition carries, and passed on to
	// the sub-agents those runs start. Added only: a team cannot remove a hook
	// an agent carries. Only states that run agents may carry them.
	Hooks     hooks.EventHooks `json:"hooks,omitempty"`
	ToolHooks hooks.ToolHooks  `json:"tool_hooks,omitempty"`
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
	// A template that names {{thread.output}} gets the threaded input there,
	// filled after expansion and never expanded itself.
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
	// strict-subset JSONPath applied to that output. For a state an agent
	// answers, that is the agent's answer as it wrote it (a consolidator's,
	// where the state has one); for a Starter, its results envelope; for an
	// input state, the walk's input. Output that is not JSON has one path, the
	// root `$`, which binds it whole as text.
	// A path that does not resolve binds nothing; it is not an error, mirroring
	// the webhook projector's posture toward an external document.
	Capture map[string]string `json:"capture,omitempty"`
	// Schema — kind=input, or a starter whose source is the walk's input (the
	// entry, so no input state can precede it): the JSON Schema a client
	// renders as the run form. The runtime interprets it only to check a walk's
	// input at start (CheckInput: top-level type, required, property types);
	// it is carried in the definition so a team is self-describing and a
	// headless caller sees the same contract the canvas does.
	Schema json.RawMessage `json:"schema,omitempty"`
	// Source — kind=starter ONLY: the ONE channel this node reads. A Starter is
	// the dispatcher: it listens, spawns a wave, and routes the results onward.
	//
	// One channel, not a set. That is what makes the cursor correct: a channel
	// cursor is keyed (tenant, channel, scope, scope_id) with NO subscriber
	// dimension, so N readers of one channel share one position and compete for
	// messages. One subscriber per Starter is one cursor, by construction.
	//
	// Or ONE document (source.kind=document), whose top-level sections become
	// the wave's items. A document has no cursor, so none of the above applies
	// to it and validation refuses the cursor-shaped fields.
	Source *StarterSource `json:"source,omitempty"`
	// Fanout — kind=starter ONLY: how many runs the wave is, and of what.
	Fanout *StarterFanout `json:"fanout,omitempty"`
	// Prompt — kind=starter ONLY: the wave's prompt templates. A Starter node
	// carries its own prompt here rather than in SystemPrompt/InputTemplate,
	// because the payload it dispatches lands in the reserved
	// {{starter.message}} / {{starter.messages}} data slots and the node needs
	// somewhere to put the text around them.
	Prompt *StarterPrompt `json:"prompt,omitempty"`
	// Sink — kind=starter ONLY: the channel the RUNTIME publishes each spawned
	// run's result to. Declared, never instructed: an agent told in its prompt
	// to publish may forget, and a forgotten publish leaves a downstream wait
	// hanging on a message that never arrives. Declaring it also means an agent
	// in a wave needs no channel grant in either direction.
	Sink *StarterSink `json:"sink,omitempty"`
	// Channel — kind=channel ONLY: the channel this node publishes to. The
	// publish-only half of the old `channel` kind; reading belongs to a Starter.
	Channel string `json:"channel,omitempty"`
	// Binds — kind=starter ONLY: variable name → a strict-subset JSONPath over
	// the SOURCE MESSAGE, into ${var.*}. Same projector and same grammar as a
	// webhook's payload_mapping, so there is one JSONPath dialect in the
	// runtime rather than two.
	//
	// A value bound here is UNTRUSTED — a channel message may be agent-written
	// or webhook-relayed. It is safe in a prompt or a Memory key and is held to
	// trust rules 5b/5c wherever it reaches a placeholder argument.
	Binds map[string]string `json:"binds,omitempty"`
	// Ack — kind=starter ONLY: when the source cursor advances.
	// "after_results" (the default) acks once the wave's results are in, which
	// is at-least-once: a crash mid-wave redelivers the batch. "after_read"
	// acks on read, which is at-most-once and loses a batch to a crash.
	Ack string `json:"ack,omitempty"`
	// Publish — kind=input ONLY: also publish the walk's input to a channel,
	// as the message value itself rather than the channel kind's envelope.
	// See publish.go. omitempty, like every field added after the first
	// hashes were recorded, so a definition without it hashes as before.
	Publish *InputPublish `json:"publish,omitempty"`
	// Payload — kind=channel ONLY: "envelope" (the default when empty) or
	// "raw". See publish.go.
	Payload string `json:"payload,omitempty"`
}

// StarterSource is what a Starter reads: a channel (the default), reusing
// Channel op=subscribe's vocabulary rather than inventing a second one, or a
// document whose top-level sections are the wave's work items.
type StarterSource struct {
	// Channel keeps no omitempty: every definition written before document
	// sources existed hashed `"channel":…`, and dropping it would change them.
	Channel string `json:"channel"`
	// Wait — any | at_least. NOT "all": await's `all` counts CHANNELS, so over
	// a single channel it is identical to `any` and returns after the FIRST
	// message. A Starter reads one channel, so `all` there is always a silent
	// wrong answer and validation refuses it.
	Wait string `json:"wait,omitempty"`
	// N — the threshold for wait=at_least. A floor only: with dynamic fan-out
	// the effective count comes from the upstream wave's stamp, because an
	// authored literal is stale the moment the upstream reads one more message.
	N int `json:"n,omitempty"`
	// WaitMS — how long to wait for the predicate before the walk errors. 0
	// means the operator's long-poll cap.
	WaitMS int `json:"wait_ms,omitempty"`
	// Batch — how many messages to read at once. 0 means the store default.
	Batch int `json:"batch,omitempty"`
	// Kind — "channel" (the default when empty), "document" or "input". Every field
	// below is omitempty so a channel source hashes byte-identically to one
	// written before they existed.
	Kind string `json:"kind,omitempty"`
	// Path — kind=document ONLY: the document's absolute Path-tree path. A
	// fixed literal: no ${…} variables, so what a definition reads is visible
	// in the definition.
	Path string `json:"path,omitempty"`
	// Scope — kind=document ONLY: "user" (the default — the tree of the person
	// the walk runs as) or "tenant".
	Scope string `json:"scope,omitempty"`
	// Select — kind=document ONLY: "chunks" (the default and only value): the
	// root's direct children, in document order.
	Select string `json:"select,omitempty"`
}

// IsDocument reports whether the source reads a document rather than a
// channel. Nil-safe, so a caller holding a handler of any kind can ask.
func (s *StarterSource) IsDocument() bool {
	return s != nil && s.Kind == SourceDocument
}

// IsInput reports whether the source is the walk's own input rather than a
// store. Nil-safe, like IsDocument.
func (s *StarterSource) IsInput() bool {
	return s != nil && s.Kind == SourceInput
}

// StarterFanout is the shape of one wave.
type StarterFanout struct {
	// Agent or Agents — what to run. Exactly one of the two.
	Agent  string   `json:"agent,omitempty"`
	Agents []string `json:"agents,omitempty"`
	// Per — "message" spawns one run per message read (DYNAMIC N); "once"
	// spawns a single run holding all of them. Default "message". A document
	// source uses "chunk" (one run per section) or "once" instead.
	Per string `json:"per,omitempty"`
	// Max is the hard ceiling on one wave's width, REQUIRED for per=message
	// and per=chunk.
	// Dynamic fan-out is a spawn amplifier: without a ceiling, a channel that
	// accumulated a thousand messages becomes a thousand agent runs. The
	// substrate's own spawn cap still applies above this.
	Max int `json:"max,omitempty"`
	// Wait — how the walk waits for the wave. Mirrors the parallel handler's.
	Wait string `json:"wait,omitempty"`
}

// StarterPrompt is a wave's prompt. Both templates are operator-authored and
// expanded like any other node's; the message payload is substituted into the
// reserved data slots AFTER expansion and is never scanned as template text.
type StarterPrompt struct {
	System string `json:"system,omitempty"`
	Input  string `json:"input,omitempty"`
}

// StarterSink is where a wave's results go. One message per spawned run.
type StarterSink struct {
	Channel string `json:"channel"`
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
