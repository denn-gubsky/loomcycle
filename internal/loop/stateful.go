package loop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// --- RFC CR L2: structured execution state (the `context.mode: stateful` loop) ---
//
// Instead of appending history, the model is fed only (P = the preamble/system,
// Σ = the structured state object, O = the latest observation) and emits, via one
// `emit_state` tool call, (reasoning, patch, action). The runtime validates the
// patch against the state schema, merges it into Σ with null-deletion, discards
// the reasoning, executes the named action to produce the next observation, and
// loops. Cost is O(T): the fed prompt never grows. The full event stream (each
// EventContextState marker) is persisted — ⚠️ AND NO LONGER ONLY FOR AUDIT:
// statefulSeedFromEvents recovers a RESUMED run's Σ from the last such
// marker (RFC DH P2), so trimming them to save transcript bytes would silently
// make every resumed stateful run forget everything it knew.
//
// This is a self-contained loop, deliberately separate from the append/recap
// Run() body so it cannot regress the shipped path.
//
// Interactive steering and end_turn parking ARE wired here now (RFC DH P1), and
// a resumed run recovers Σ from its transcript (P2). What is still absent is
// RFC BH turn-cancel and a per-turn step budget — P3.

// contextStatefulMode reports whether the resolved policy selects L2 stateful.
func contextStatefulMode(cx *config.Context) bool {
	return cx != nil && cx.Mode != nil && *cx.Mode == config.ContextModeStateful
}

// contextAutoMode reports whether the policy is tier-routed (mode: auto).
func contextAutoMode(cx *config.Context) bool {
	return cx != nil && cx.Mode != nil && *cx.Mode == config.ContextModeAuto
}

// StatefulMode reports whether a run with this context policy runs the
// stateful loop, resolving mode:auto exactly as Run will. A caller that builds
// a run's INPUT — resume, a session continuation — needs the answer before
// Run is called: a stateful run is seeded from its recorded state, not from
// its replayed messages.
func StatefulMode(cx *config.Context, local, interactive bool) bool {
	if contextAutoMode(cx) {
		cx = resolveAutoContextMode(cx, local, interactive)
	}
	return contextStatefulMode(cx)
}

// resolveAutoContextMode turns mode:auto into a concrete mode (RFC CR tier-
// routing): a local backend → recap (schema-free, safe for a weaker model), a
// frontier API → stateful. An interactive run takes recap regardless of tier.
//
// ⚠️ THE ORIGINAL REASON FOR THAT IS GONE: it said the stateful loop had no
// steer or park, and RFC DH P1 gave it both. The clause is kept on purpose
// until an interactive stateful chat has been verified end to end — Σ carried
// across turns in the embedded terminal, on a frontier and on a local model —
// because stateful is the less forgiving mode for a model that fumbles its
// emit shape. An explicit `mode: stateful` has always bypassed it. Returns a
// CLONE carrying the concrete mode so the shared agent def is never mutated.
func resolveAutoContextMode(cx *config.Context, local, interactive bool) *config.Context {
	mode := config.ContextModeStateful
	if local || interactive {
		mode = config.ContextModeRecap
	}
	out := cx.Clone()
	out.Mode = &mode
	return out
}

// statefulOperatorPrefix marks an observation as a PERSON speaking rather than
// a tool result (RFC DH P1).
//
// ⚠️ THE PREFIX IS LOAD-BEARING, not decoration. A stateful run has no
// transcript, so the operator's message has to arrive through the one slot the
// loop has for "something happened outside the model" — the observation. Without
// a marker it is indistinguishable from the output of whatever action ran last,
// and buildStatefulSystem tells the model what it means.
const statefulOperatorPrefix = "operator: "

// parkForStatefulTurn is parkForOperatorTurn's Σ-shaped twin: it parks a
// completed stateful turn and returns the operator's next message as the next
// OBSERVATION. Returns false when the ctx was cancelled, the queue closed or
// the idle deadline passed (opts.idledOut set) — the run is then over.
//
// ⚠️ THIS IS A NEW CALL SITE OF reResolveForOperatorTurn, and
// parkForOperatorTurn's own comment says why that matters: the retune lives at
// the park "so there is one place it can be forgotten from, and none where it
// can disagree". A fourth park that skipped it would give stateful runs a
// silently different answer to "was this retuned while it waited".
func parkForStatefulTurn(ctx context.Context, opts *RunOptions, sinceTurn int, emit func(providers.Event)) (string, bool) {
	emit(providers.Event{Type: providers.EventAwaitingInput,
		AwaitingInput: &providers.AwaitingInputEventInfo{SinceTurn: sinceTurn}})
	pp := newParkPause(ctx, opts.PauseGate)
	defer pp.done()
	deadline := opts.idleDeadline()
	for {
		m, end := parkForInput(ctx, opts, deadline, pp)
		switch end {
		case parkGone:
			return "", false
		case parkIdle:
			opts.idledOut = true
			return "", false
		}
		if m.Kind == steer.KindCompact {
			// A compaction control has nothing to act on here: a stateful run
			// has no history to replace — Σ IS the compaction, rebuilt from
			// scratch every step. Re-park rather than treat it as the
			// operator's turn, which would feed the model a summary of a
			// transcript it never had.
			continue
		}
		if m.IsVerdict() {
			continue // a stateful run is never held for review
		}
		if opts.OnSteer != nil {
			opts.OnSteer(m)
		}
		reResolveForOperatorTurn(ctx, opts, emit)
		return statefulOperatorPrefix + m.Text, true
	}
}

// takeStatefulSteer is the stateful loop's non-blocking read of its steer
// queue, for a run that is not parked on it. It returns the operator messages
// taken, each already reported through OnSteer.
//
// A compaction control and a review verdict are dropped, exactly as
// parkForStatefulTurn drops them: a stateful run has no history to replace and
// is never held for review. Neither carries text an operator typed.
//
// canAnswer false is a run with no step left: a message is not taken — a turn
// taken and never answered would read as the run's last word — but named in
// the append loop's own error, so it is not acknowledged and silently lost.
func takeStatefulSteer(opts *RunOptions, canAnswer bool, emit func(providers.Event)) []string {
	var said []string
	for {
		select {
		case m, ok := <-opts.SteerQueue:
			if !ok {
				return said
			}
			if m.Kind == steer.KindCompact || m.IsVerdict() {
				continue
			}
			if !canAnswer {
				// Through the append loop's drain so the wording cannot drift.
				// It is handed this one message, never the queue itself: it
				// would apply a compaction to a history this loop does not have.
				one := make(chan steer.Message, 1)
				one <- m
				drainSteerUnanswerable(one, nil, emit)
				continue
			}
			if opts.OnSteer != nil {
				opts.OnSteer(m)
			}
			said = append(said, m.Text)
		default:
			return said
		}
	}
}

// statefulOperatorTurn frames the messages a finishing run took as its next
// observation and task. documented is whether the run's prompt explains the
// `operator: ` prefix (see buildStatefulSystem): a run that was built
// autonomous was never told what it means, so its message is framed as what
// that run does know — a task — which is also how a resume or continuation
// frames a person's message for an autonomous run.
func statefulOperatorTurn(said []string, documented bool) string {
	prefix := "Task: "
	if documented {
		prefix = statefulOperatorPrefix
	}
	return prefix + strings.Join(said, "\n")
}

// statefulTaskAddendum heads an operator message that reached a run between
// two steps. It is added to the task rather than made the observation: the
// observation slot holds the result of the action that just ran, which the
// model has not read yet, and the task is the one thing shown on every step —
// so the message is neither bought with that result nor gone a step later.
const statefulTaskAddendum = "Added by the operator while you were working:\n"

const emitStateToolName = "emit_state"

// emitStateToolSpec is the ONLY tool a stateful step offers: the model must call
// it, which is how the runtime reliably gets a structured {reasoning, patch,
// action} object without a provider-specific forced-output primitive.
func emitStateToolSpec() providers.ToolSpec {
	return providers.ToolSpec{
		Name: emitStateToolName,
		Description: "Advance the task by emitting your state update and next action. Call this EXACTLY ONCE per step. " +
			"`patch` is a JSON merge-patch applied to the state object (a null value deletes a key) — keep the state the single " +
			"source of truth for everything you must remember. `action` names the next tool to run; omit it (or set done=true) to " +
			"finish, putting your answer in `final`. `final`, `done` and `action` are fields of this call, next to `patch` — " +
			"never keys inside it: an answer written into the state is not shown to anyone.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "reasoning": {"type": "string", "description": "your step reasoning; it is discarded after this step, so record durable facts in the patch instead"},
    "patch": {"type": "object", "description": "a JSON merge-patch applied to the state; a null value deletes a key. State only — your answer goes in final, not here"},
    "action": {"type": "object", "properties": {"tool": {"type": "string"}, "input": {"type": "object"}}, "description": "the next tool to run; omit to finish"},
    "done": {"type": "boolean", "description": "true when the task is complete"},
    "final": {"type": "string", "description": "the final answer text, when done"},
    "propose_schema": {"type": "object", "description": "OPTIONAL: propose a JSON-Schema (object with typed properties) that this task's state should conform to, when no schema is set or a better one is apparent. It is recorded for an operator to review and adopt; it does NOT take effect this run."}
  },
  "required": ["patch"]
}`),
	}
}

type emitStateOut struct {
	Reasoning     string         `json:"reasoning"`
	Patch         map[string]any `json:"patch"`
	Action        *stateAction   `json:"action"`
	Done          bool           `json:"done"`
	Final         string         `json:"final"`
	ProposeSchema map[string]any `json:"propose_schema"`
}

type stateAction struct {
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input"`
}

func parseEmitState(input json.RawMessage) (*emitStateOut, error) {
	var out emitStateOut
	if err := json.Unmarshal(input, &out); err != nil {
		return nil, fmt.Errorf("emit_state was not valid JSON: %w", err)
	}
	if out.Patch == nil {
		out.Patch = map[string]any{} // a step with no state change is legal
	}
	return &out, nil
}

// protocolFields are emit_state's OWN fields — the reply channel, not state.
var protocolFields = []string{"final", "done", "action", "reasoning"}

// liftNestedProtocolFields moves emit_state's own fields out of the patch when
// a model nested them there, and deletes them from Σ.
//
// ⚠️ OBSERVED LIVE, and the answer was LOST. A local model (ornith-1.5:35b)
// ended its turns with `{"patch": {"done": true, "final": "<the whole
// answer>"}}` — the reply written INTO the state. The runtime reads only the
// top-level `final`, so the operator saw an empty turn while the answer sat in
// Σ. Re-prompting did not help: asked for its answer "in `final`", the model
// believed it had given one, and repeated the same shape until the budget ran
// out. The stale `final` and `done` then stayed in Σ into the next turn, where
// the model read its previous answer as part of what it knew.
//
// Lifted only when the state schema does not declare the key, so a schema that
// genuinely calls a Σ field `final` keeps it. A top-level value always wins
// over a nested one; the nested key is still deleted, because it is the reply
// channel leaking into Σ either way. Deleted as a null in the patch — the
// merge's own deletion — so Σ loses a stale copy from an earlier turn too.
func liftNestedProtocolFields(es *emitStateOut, schema map[string]any) {
	declared, _ := schema["properties"].(map[string]any)
	for _, k := range protocolFields {
		v, ok := es.Patch[k]
		if !ok || v == nil {
			continue
		}
		if _, isState := declared[k]; isState {
			continue
		}
		switch k {
		case "final":
			if s, ok := v.(string); ok && strings.TrimSpace(es.Final) == "" {
				es.Final = s
			}
		case "done":
			if b, ok := v.(bool); ok && b {
				es.Done = true
			}
		case "reasoning":
			if s, ok := v.(string); ok && strings.TrimSpace(es.Reasoning) == "" {
				es.Reasoning = s
			}
		case "action":
			if es.Action == nil {
				if b, err := json.Marshal(v); err == nil {
					var a stateAction
					if json.Unmarshal(b, &a) == nil && strings.TrimSpace(a.Tool) != "" {
						es.Action = &a
					}
				}
			}
		}
		es.Patch[k] = nil
	}
	// The same model, finishing, named emit_state itself as its action. When
	// the step also carries an answer or says it is done, that is a turn end
	// written the wrong way round — not a request to run a tool. An
	// emit_state action with neither is still refused as before.
	if es.Action != nil && es.Action.Tool == emitStateToolName {
		var in struct {
			Final *string `json:"final"`
			Done  *bool   `json:"done"`
		}
		if json.Unmarshal(es.Action.Input, &in) == nil {
			if in.Final != nil && strings.TrimSpace(es.Final) == "" {
				es.Final = *in.Final
			}
			if in.Done != nil && *in.Done {
				es.Done = true
			}
		}
		if es.Done || strings.TrimSpace(es.Final) != "" {
			es.Action = nil
		}
	}
}

// retryToolUseID is the id a correction request replays the model's rejected
// emit_state under: the model's own, when it gave one, so the assistant turn
// sent back is the one it produced (a thinking model's replayed block belongs
// to it); a synthetic one for a driver that issues none (Ollama).
func retryToolUseID(modelID string, iter, attempt int) string {
	if modelID != "" {
		return modelID
	}
	return fmt.Sprintf("es-%d-%d", iter, attempt)
}

// newActionIDTag is a short random tag that makes one run's action ids unique
// among the session's. crypto/rand never fails on a supported platform; the
// fixed fallback only costs uniqueness, never correctness within the run.
func newActionIDTag() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0"
	}
	return hex.EncodeToString(b[:])
}

// endsTurn reports whether a step ends the turn: done, or no action named.
func endsTurn(es *emitStateOut) bool {
	return es.Done || es.Action == nil || strings.TrimSpace(es.Action.Tool) == ""
}

// emptyTurnNote is what the operator is shown when a turn ended with no answer
// even after the model was asked for one — never an empty message, which reads
// as the chat having broken.
func emptyTurnNote(patch map[string]any) string {
	keys := make([]string, 0, len(patch))
	for k := range patch {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return "(the model ended its turn without an answer)"
	}
	return "(the model ended its turn without an answer; it updated its state: " + strings.Join(keys, ", ") + ")"
}

func actionName(es *emitStateOut) string {
	if es.Action == nil {
		return ""
	}
	return es.Action.Tool
}

// buildStatefulSystem augments the resolved preamble P with the state protocol
// instructions, the available action-tool catalog, and the state schema.
func buildStatefulSystem(base []providers.ContentBlock, toolSpecs []providers.ToolSpec, schema map[string]any, interactive bool) []providers.ContentBlock {
	var b strings.Builder
	b.WriteString("\n\n## Structured execution mode\n")
	b.WriteString("You run in structured-state mode. You do NOT call the task tools directly. Each step you are shown the current state (a JSON object) and the latest observation; respond by calling `emit_state` exactly once:\n")
	b.WriteString("- `reasoning`: your thinking for this step. It is DISCARDED afterwards, so put anything you must remember into the patch.\n")
	b.WriteString("- `patch`: a JSON merge-patch applied to the state. Set keys to record progress; a null value deletes a key.\n")
	b.WriteString("- `action`: the next tool to run, as {\"tool\": <name>, \"input\": {…}}. The runtime executes it and hands you its output as the next observation.\n")
	b.WriteString("- Finish by omitting `action` (or setting `done: true`) and putting your answer in `final`. `final`, `done` and `action` sit NEXT TO `patch` in the call, never inside it — an answer written into the state is never shown.\n")
	if interactive {
		// ⚠️ ADDED ONLY FOR AN INTERACTIVE RUN, so an autonomous one's prompt
		// stays byte-identical — this paragraph describes a thing that cannot
		// happen to it, and a prompt that documents impossible inputs teaches
		// the model nothing and costs every call.
		//
		// It exists because the runtime deliberately does NOT write Σ: a
		// reserved key filled by the runtime would make Σ half model-authored
		// and half not, with state_schema validating only one of the halves. So
		// the model is told the observation is different in kind, and left to
		// decide what is durable — which is its job in this mode.
		b.WriteString("- An observation beginning `" + statefulOperatorPrefix + "` is a PERSON speaking to you, " +
			"not a tool result. You are in a live conversation: after you finish, they may reply. " +
			"Nothing is remembered between turns except the state, so record what they asked " +
			"(and anything you promised) in the patch if you will need it later.\n")
	}
	if len(toolSpecs) > 0 {
		b.WriteString("\n### Action tools you may name\n")
		for _, t := range toolSpecs {
			// The per-run help instruction is appended to the description, so
			// the 240-byte cut below always took it off: a stateful run never
			// learned where a tool's call format was. Shorten the description
			// alone and keep the instruction whole.
			desc := strings.TrimSuffix(strings.TrimRight(t.Description, " \n"), t.Help)
			line := oneLineDesc(desc)
			if t.Help != "" {
				line += " " + t.Help
			}
			fmt.Fprintf(&b, "- `%s`: %s\n", t.Name, line)
		}
	}
	b.WriteString(statefulReplyShapes(toolSpecs))
	if len(schema) > 0 {
		if sj, err := json.Marshal(schema); err == nil {
			fmt.Fprintf(&b, "\n### State schema (the state, and every patch, must conform)\n%s\n", string(sj))
		}
	}
	out := append([]providers.ContentBlock(nil), base...)
	return append(out, providers.ContentBlock{Type: "text", Text: b.String()})
}

// statefulReplyShapes shows the model the only two replies the loop accepts,
// and the wrong ones by name.
//
// ⚠️ THE PROSE ABOVE WAS NOT ENOUGH, and the failures were all SHAPE errors a
// field list does not prevent. A local model (ornith-1.5:35b) that had read it
// wrote its answer INSIDE the patch, named emit_state as its action, and — in
// 4 of 9 replays of one step — sent a patch and a plan ("I will search the
// web next") with no action at all. The runtime now repairs or re-prompts
// each of these, but every repair costs a call; an example of the right shape
// is the cheapest fix, and the one a small model follows most reliably.
//
// The example tool is the first one this agent was actually offered, so the
// example never names a tool the model cannot call.
func statefulReplyShapes(toolSpecs []providers.ToolSpec) string {
	var b strings.Builder
	b.WriteString("\n### Your emit_state call is always one of these shapes\n")
	if len(toolSpecs) > 0 {
		fmt.Fprintf(&b, "1. Keep working — run a tool, and its output comes back as your next observation:\n"+
			"   {\"reasoning\": \"…\", \"patch\": {\"progress\": \"…\"}, \"action\": {\"tool\": \"%s\", \"input\": {…}}}\n"+
			"2. Answer — end your turn with the full answer, which is the ONLY text shown:\n"+
			"   {\"patch\": {\"progress\": \"answered\"}, \"done\": true, \"final\": \"<the complete answer>\"}\n", toolSpecs[0].Name)
	} else {
		b.WriteString("This agent has no tools, so every reply is an answer — the ONLY text shown:\n" +
			"   {\"patch\": {\"progress\": \"answered\"}, \"done\": true, \"final\": \"<the complete answer>\"}\n")
	}
	b.WriteString("Not accepted — nothing reaches anyone, and you will be asked again:\n" +
		"- the answer inside the patch: {\"patch\": {\"final\": \"…\"}} — `final`, `done` and `action` go next to `patch`.\n" +
		"- a plan with no action: {\"patch\": {…}, \"reasoning\": \"I will search next\"} — if you mean to use a tool, name it in `action` in THIS call.\n" +
		"- `emit_state` as the action — it is how you reply, not a tool.\n")
	return b.String()
}

func oneLineDesc(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return strings.TrimSpace(s)
}

// statefulUserMessage renders the fed context for one step: the task, Σ and O
// (no history). This is the whole point — the prompt stays flat over the
// horizon.
//
// The task is shown on every step, not only as the first observation. It used
// to arrive only as O_0, and the next tool result replaced it: a model that
// had not copied the request into Σ itself — measured live, most of the time —
// had no task from step 2 on. It re-derived one from the state, asked the user
// what to do, or, once, invented a value and reported it saved. The runtime
// still does not write Σ; the task sits beside it, and "" (the first step,
// whose observation IS the task) renders nothing extra.
//
// reference is what the run's Context tool returned on earlier steps (see
// contextMemo), shown between the task and the state; "" renders nothing.
func statefulUserMessage(sigma map[string]any, task, reference, obs string) providers.Message {
	sj, _ := json.Marshal(sigma)
	var b strings.Builder
	if task != "" && task != obs {
		fmt.Fprintf(&b, "Your task (keep working on it until it is done):\n%s\n\n", task)
	}
	if reference != "" {
		b.WriteString(reference)
	}
	fmt.Fprintf(&b, "Current state:\n%s\n\nLatest observation:\n%s", string(sj), obs)
	return providers.Message{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: b.String()}}}
}

// statefulTask is the request the run is working on: the first user message's
// text. On a resume the seed is the replayed transcript, and only its first
// user turn is the request; the rest is history the state already carries.
func statefulTask(msgs []providers.Message) string {
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		var b strings.Builder
		for _, c := range m.Content {
			if c.Type == "text" && c.Text != "" {
				b.WriteString(c.Text)
				b.WriteString("\n")
			}
		}
		if s := strings.TrimSpace(b.String()); s != "" {
			return s
		}
	}
	return ""
}

// initialObservation renders the run's task (the seed segments) as the first
// observation O_0.
func initialObservation(msgs []providers.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		for _, c := range m.Content {
			if c.Type == "text" && c.Text != "" {
				b.WriteString(c.Text)
				b.WriteString("\n")
			}
		}
	}
	s := strings.TrimSpace(b.String())
	if s == "" {
		return "(no task provided)"
	}
	return "Task: " + s
}

// errNoEmitState is the model answering the wrong SHAPE, as opposed to the
// provider or the transport failing.
//
// ⚠️ THE DISTINCTION IS THE WHOLE POINT. A 500, a dropped socket or a refused
// request is nothing the model can be asked to fix, so it terminates the run.
// "You replied in prose instead of calling the tool" is a correctable mistake
// of exactly the kind the rejected-patch path already re-prompts for, and
// treating the two identically is what turned one conversational reply into a
// dead run on step zero.
var errNoEmitState = errors.New("model did not call emit_state")

// emitStateCall is one step's raw result: the tool input when the model complied,
// and — when it did not — what it produced instead.
type emitStateCall struct {
	input json.RawMessage
	// text is the assistant's prose. Kept because it is BOTH halves of the fix:
	// the retry shows the model its own reply, and the terminal error can say
	// what the run actually got rather than only what it wanted.
	text string
	// thinking is a reasoning trace, kept SEPARATE and never replayed. A model
	// whose entire output was thinking looks identical from outside to one that
	// emitted nothing, and those call for different answers — raise the budget
	// versus change the model. (The same confusion produced the silent recap
	// failure: a thinking model's output never reached the text accumulator.)
	thinking string
	usage    *providers.Usage
	// reasoning / reasoningSig are the driver's own record of the thinking
	// block, from EventDone — what a thinking model requires to be replayed on
	// an assistant turn that is sent back to it. callID is the tool_use id the
	// model gave its emit_state call, for the same reason.
	reasoning    string
	reasoningSig string
	callID       string
}

// callForEmitState makes one provider call and returns the emit_state tool input
// plus whatever else the model produced. NO OnEvent hook is set — the returned
// channel is the event source (mirrors summarizeWith); the tool call is what
// matters, but the text is no longer thrown away.
//
// now is the clock the call's timing is taken with (nil = time.Now); the timing
// rides the returned usage.
func callForEmitState(ctx context.Context, provider providers.Provider, req providers.Request, now func() time.Time,
	price func(string, string, *providers.Usage) (float64, string)) (emitStateCall, error) {
	var out emitStateCall
	timer := providers.StartCallTimer(now)
	mc := startModelCall(ctx, ctx, modelPurposeEmitState, provider, req, timer, price, "")
	defer mc.End("the response stream ended before the model finished its step")
	ch, err := provider.Call(mc.Ctx(), req)
	if err != nil {
		mc.End(err.Error())
		return out, err
	}
	var text, thinking strings.Builder
	var streamErr string
	sawDone := false
	for ev := range ch {
		timer.Observe(ev)
		mc.Observe(ev)
		switch ev.Type {
		case providers.EventToolCall:
			if ev.ToolUse != nil && ev.ToolUse.Name == emitStateToolName && out.input == nil {
				out.input = ev.ToolUse.Input
				out.callID = ev.ToolUse.ID
			}
		case providers.EventText:
			text.WriteString(ev.Text)
		case providers.EventThinking:
			thinking.WriteString(ev.Text)
		case providers.EventDone:
			// A copy, for the same reason as the append loop's: the timing
			// is stamped onto it, and the provider's value may be shared.
			out.usage = nil
			if ev.Usage != nil {
				u := *ev.Usage
				out.usage = &u
			}
			out.reasoning, out.reasoningSig = ev.Reasoning, ev.ReasoningSignature
			sawDone = true
		case providers.EventError:
			streamErr = ev.Error
		}
	}
	timer.Stamp(out.usage)
	out.text = strings.TrimSpace(text.String())
	out.thinking = strings.TrimSpace(thinking.String())
	// The run's ctx ended mid-step, so the stream closed unfinished. Read as a
	// reply with no emit_state, its partial prose could become the run's final
	// answer once the patch retries ran out; it is the cancel, as in readTurn.
	if !sawDone && ctx.Err() != nil {
		return out, ctx.Err()
	}
	if streamErr != "" {
		return out, errors.New(streamErr)
	}
	if len(out.input) == 0 {
		return out, errNoEmitState
	}
	return out, nil
}

// producedInstead describes, for an operator, what came back when the tool call
// did not — the difference between "answered the wrong way", "only thought" and
// "returned nothing at all", which are three different problems.
func producedInstead(c emitStateCall) string {
	switch {
	case c.text != "":
		return fmt.Sprintf("it replied with %d characters of prose instead: %q", len(c.text), snippet(c.text))
	case c.thinking != "":
		return fmt.Sprintf("it produced only a reasoning trace (%d characters) and no reply: %q",
			len(c.thinking), snippet(c.thinking))
	default:
		return "it returned no content at all"
	}
}

func snippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

func addUsage(dst *providers.Usage, u *providers.Usage) {
	if u == nil {
		return
	}
	dst.InputTokens += u.InputTokens
	dst.OutputTokens += u.OutputTokens
	dst.CacheCreationTokens += u.CacheCreationTokens
	dst.CacheReadTokens += u.CacheReadTokens
	if u.Model != "" {
		dst.Model = u.Model
	}
	// Which key paid, for the run-level summary (runs.credential_source) — the
	// append loop carries the last call's; the per-call split is the ledger's.
	if u.CredentialSource != "" {
		dst.CredentialSource = u.CredentialSource
		dst.CredentialScopeID = u.CredentialScopeID
	}
}

// statefulRecovery is the provider-fault recovery state of one stateful run:
// the append loop's same-provider retry budget and fallback counter.
type statefulRecovery struct {
	sameProviderRetries int
	fallbackAttempts    int
	firstStepSucceeded  bool
}

type statefulCallOutcome int

const (
	statefulCallFatal statefulCallOutcome = iota
	statefulCallRetry
	statefulCallCancelled
)

// recoverStatefulCall applies the append loop's answer to a provider fault, in
// the same order: a same-provider retry with backoff for a retryable error
// while the budget lasts; then resolver feedback; then a cross-provider
// fallback under the run's policy. Retry means "send this step again" — on the
// same provider, or on the one opts now names.
func recoverStatefulCall(ctx context.Context, opts *RunOptions, rec *statefulRecovery, err error,
	emit func(providers.Event), msgs []providers.Message) statefulCallOutcome {
	if ctx.Err() != nil {
		return statefulCallCancelled
	}
	if rec.sameProviderRetries < opts.MaxSameProviderRetries &&
		providers.ClassifyError(err) == providers.ErrorClassRetryable {
		rec.sameProviderRetries++
		backoff := sameProviderRetryBackoff(rec.sameProviderRetries)
		emit(providers.Event{Type: providers.EventRetry, Retry: &providers.RetryInfo{
			Provider: opts.Provider.ID(), Attempt: rec.sameProviderRetries,
			WaitMs: backoff.Milliseconds(), Reason: providers.RetryReasonSchedule,
		}})
		select {
		case <-ctx.Done():
			return statefulCallCancelled
		case <-time.After(backoff):
		}
		return statefulCallRetry
	}
	// Resolver feedback, decided exactly as Run's is (reportCallFault). A
	// restricted run's refusal to use the operator key (ErrOperatorKeyForbidden)
	// is a per-run policy decision, not an outage, and must not touch the matrix.
	if !errors.Is(err, providers.ErrOperatorKeyForbidden) {
		reportCallFault(opts, err)
	}
	if tryProviderFallback(ctx, opts, &rec.fallbackAttempts, err, emit, msgs, rec.firstStepSucceeded) == fallbackOutcomeSwitched {
		rec.sameProviderRetries = 0
		return statefulCallRetry
	}
	return statefulCallFatal
}

func applyStatefulSampling(req *providers.Request, s *config.Sampling) {
	if s == nil {
		return
	}
	req.Temperature = s.Temperature
	req.TopP = s.TopP
	req.TopK = s.TopK
	req.FrequencyPenalty = s.FrequencyPenalty
	req.PresencePenalty = s.PresencePenalty
	req.Seed = s.Seed
	req.Stop = s.Stop
}

// schemasDiffer reports whether two schema objects are not JSON-equal (a nil/empty
// active schema differs from any non-empty proposal). Used to suppress a proposal
// that merely restates the already-adopted schema.
func schemasDiffer(a, b map[string]any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return true
	}
	return string(ab) != string(bb)
}

// unforcedNote says whether the run could even ASK for the tool on the wire.
//
// ⚠️ THE TWO FAILURES READ IDENTICALLY WITHOUT IT, and they call for opposite
// next moves. A model that ignored a protocol-level constraint needs replacing;
// one that was never given the constraint may be fine on a provider that has
// one. Ollama has no tool_choice on /api/chat, so a forced request there
// degrades rather than being refused (RFC DG) — which is the right call, and
// exactly the kind of silent degradation this codebase keeps having to make
// visible after the fact.
//
// Asks the per-model answer, not the provider's coarse bit: a provider that has
// tool_choice on the wire still drops a forced one for a thinking model
// (Anthropic under budgeted thinking, DeepSeek in thinking mode), and that run
// was just as unenforced as one on Ollama.
func unforcedNote(opts RunOptions) string {
	forced := providers.ToolChoice{Mode: providers.ToolChoiceTool, Name: emitStateToolName}
	if opts.Provider == nil || providers.EnforcesToolChoice(opts.Provider, opts.Model, opts.Effort, forced) {
		return ""
	}
	if !opts.Provider.Capabilities().SupportsToolChoice {
		return fmt.Sprintf(" (note: provider %q has no tool_choice on the wire, so the call was NOT "+
			"enforced — the tool was requested in the prompt only)", opts.Provider.ID())
	}
	return fmt.Sprintf(" (note: provider %q does not enforce tool_choice for model %q in this mode, so the call "+
		"was NOT enforced — the tool was requested in the prompt only)", opts.Provider.ID(), opts.Model)
}

// missingRequiredInput names what an action's input lacks against its tool's
// declared schema — the top-level `required` fields only. "" means nothing is
// missing, or the schema declares nothing to check. Deliberately not a full
// JSON-Schema validation: every tool validates its own input, and a second
// validator here would drift from the first.
func missingRequiredInput(specs []providers.ToolSpec, name string, input json.RawMessage) string {
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	for _, t := range specs {
		if t.Name == name {
			_ = json.Unmarshal(t.InputSchema, &schema)
			break
		}
	}
	if len(schema.Required) == 0 {
		return ""
	}
	var got map[string]json.RawMessage
	_ = json.Unmarshal(input, &got) // absent, null or a non-object all count as no fields
	var missing []string
	for _, f := range schema.Required {
		if _, ok := got[f]; !ok {
			missing = append(missing, "`"+f+"`")
		}
	}
	if len(missing) == 0 {
		return ""
	}
	props := make([]string, 0, len(schema.Properties))
	for p := range schema.Properties {
		props = append(props, "`"+p+"`")
	}
	sort.Strings(props)
	msg := "`" + name + "` needs " + strings.Join(missing, ", ") + " in its input, which was not given."
	if len(props) > 0 {
		msg += " Its input fields are: " + strings.Join(props, ", ") + "."
	}
	return msg
}

// statefulRefusal is a refusal the stateful loop makes itself, before any
// dispatch, as a failed tool result — so a model reads one kind of error, and a
// stream consumer sees one kind of event, whatever refused the action.
func statefulRefusal(cat tools.ErrorCategory, msg, fix string) tools.Result {
	return tools.Result{
		Text:    msg,
		IsError: true,
		Error:   &tools.ErrorInfo{Category: cat, Retryable: false, Description: fix},
	}
}

// emitActionRefusal emits a refused action as a tool call and its failed
// result — carrying the structure as error_info, as a dispatched failure's
// event does — and returns the observation the model reads.
func emitActionRefusal(emit func(providers.Event), tid string, a *stateAction, res tools.Result) string {
	text := renderToolResultText(res)
	tu := providers.ToolUse{ID: tid, Name: a.Tool, Input: a.Input}
	emit(providers.Event{Type: providers.EventToolCall, ToolUse: &tu})
	emit(providers.Event{Type: providers.EventToolResult, ToolUse: &tu, Text: text, IsError: true, ErrorInfo: res.Error})
	return text
}

// offersTool reports whether this agent was offered a tool by that name. The
// set is toolSpecs — the same list buildStatefulSystem prints under "Action
// tools you may name", so what the loop accepts and what the prompt advertised
// cannot drift apart.
func offersTool(specs []providers.ToolSpec, name string) bool {
	for _, t := range specs {
		if t.Name == name {
			return true
		}
	}
	return false
}

// statefulActionHint lists what the model MAY name, because an error that only
// says what was wrong leaves it guessing — and a model that guesses twice burns
// two steps of a bounded loop.
func statefulActionHint(specs []providers.ToolSpec) string {
	if len(specs) == 0 {
		return "This agent has no action tools, so omit `action` and put your answer in `final`."
	}
	names := make([]string, 0, len(specs))
	for _, t := range specs {
		names = append(names, "`"+t.Name+"`")
	}
	return "Available tools: " + strings.Join(names, ", ") +
		". Omit `action` (or set `done: true`) when you are ready to answer."
}

// overBudget asks the run's BudgetGate, when it has one, and reports a
// refusal as the run's error event. Asked before EVERY model call this loop
// makes, a re-prompt included.
func overBudget(opts RunOptions, emit func(providers.Event)) error {
	if opts.BudgetGate == nil {
		return nil
	}
	err := opts.BudgetGate()
	if err != nil {
		emit(providers.Event{Type: providers.EventError, Error: err.Error()})
	}
	return err
}
