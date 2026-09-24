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
	"github.com/denn-gubsky/loomcycle/internal/contextplugin"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/statepatch"
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
// OBSERVATION. Returns false when the ctx was cancelled or the queue closed —
// the run is then over.
//
// ⚠️ THIS IS A NEW CALL SITE OF reResolveForOperatorTurn, and
// parkForOperatorTurn's own comment says why that matters: the retune lives at
// the park "so there is one place it can be forgotten from, and none where it
// can disagree". A fourth park that skipped it would give stateful runs a
// silently different answer to "was this retuned while it waited".
func parkForStatefulTurn(ctx context.Context, opts *RunOptions, sinceTurn int, emit func(providers.Event)) (string, bool) {
	emit(providers.Event{Type: providers.EventAwaitingInput,
		AwaitingInput: &providers.AwaitingInputEventInfo{SinceTurn: sinceTurn}})
	for {
		m, resumed := parkForInput(ctx, opts.SteerQueue, opts.OnHeartbeat)
		if !resumed {
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
			fmt.Fprintf(&b, "- `%s`: %s\n", t.Name, oneLineDesc(t.Description))
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

// statefulUserMessage renders the fed context for one step: only Σ + O (no
// history). This is the whole point — the prompt stays flat over the horizon.
func statefulUserMessage(sigma map[string]any, obs string) providers.Message {
	sj, _ := json.Marshal(sigma)
	var b strings.Builder
	fmt.Fprintf(&b, "Current state:\n%s\n\nLatest observation:\n%s", string(sj), obs)
	return providers.Message{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: b.String()}}}
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
func callForEmitState(ctx context.Context, provider providers.Provider, req providers.Request) (emitStateCall, error) {
	var out emitStateCall
	ch, err := provider.Call(ctx, req)
	if err != nil {
		return out, err
	}
	var text, thinking strings.Builder
	var streamErr string
	for ev := range ch {
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
			out.usage = ev.Usage
			out.reasoning, out.reasoningSig = ev.Reasoning, ev.ReasoningSignature
		case providers.EventError:
			streamErr = ev.Error
		}
	}
	out.text = strings.TrimSpace(text.String())
	out.thinking = strings.TrimSpace(thinking.String())
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
	// Resolver feedback, split by class exactly as Run's is: a 429 cools the
	// pair briefly, anything else marks it stalled. An operator-key refusal is a
	// per-run policy decision, not an outage, and must not poison the matrix.
	if !errors.Is(err, providers.ErrOperatorKeyForbidden) {
		if providers.IsRateLimit(err) {
			if opts.MarkRateLimited != nil {
				opts.MarkRateLimited(opts.Provider.ID(), opts.Model, 0)
			}
		} else if opts.MarkStalled != nil {
			opts.MarkStalled(opts.Provider.ID(), opts.Model, err.Error())
		}
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

// runStateful executes the L2 loop. `system` is the resolved preamble P (already
// split from opts.Segments); `initial` is the seed conversation (the task);
// `toolSpecs` is the action-tool catalog; `emit` forwards + persists events.
func runStateful(ctx context.Context, opts RunOptions, system []providers.ContentBlock, initial []providers.Message, toolSpecs []providers.ToolSpec, iterCap int, emit func(providers.Event)) (RunResult, error) {
	cx := opts.Context
	var schema map[string]any
	onInvalid := config.ContextDefaultOnInvalidPatch
	maxRetries := config.ContextDefaultMaxPatchRetries
	if cx != nil {
		schema = cx.StateSchema
		if cx.OnInvalidPatch != nil {
			onInvalid = *cx.OnInvalidPatch
		}
		if cx.MaxPatchRetries != nil {
			maxRetries = *cx.MaxPatchRetries
		}
	}
	// ⚠️ THE CAP IS COMPUTED ONCE, IN Run, and handed down. This loop used to
	// derive its own from opts.MaxIterations and so missed every lift Run
	// applies — including the one that matters here: an interactive run gets
	// max_iterations raised to the hard ceiling because each PARK consumes an
	// iteration. A stateful turn spends several steps, so a parked chat on the
	// default 16 died after a handful of exchanges reporting max_iterations,
	// which is an answer about the wrong thing.
	maxIter := iterCap
	if maxIter <= 0 {
		maxIter = 16
	}

	interactive := opts.Interactive && opts.SteerQueue != nil
	statefulSystem := buildStatefulSystem(system, toolSpecs, schema, interactive)
	emitTool := []providers.ToolSpec{emitStateToolSpec()}

	// Seeded from a resumed run's last recorded Σ (RFC DH P2); empty for a
	// fresh one. Copied rather than aliased so the caller's map is never
	// mutated by a merge — opts is passed by value but the map inside it is not.
	sigma := map[string]any{}
	for k, v := range opts.InitialState {
		sigma[k] = v
	}
	// lastWritten records which step last touched each Σ key, so eviction can
	// break ties by recency WITHIN a retention class. The class is the
	// operator's statement of what matters; recency only orders equals.
	lastWritten := map[string]int{}
	// ⚠️ STATEFUL HAD NO FOOTPRINT AT ALL. The transcript is rebuilt from
	// (Σ, observation) each step so it cannot accumulate — which is why the
	// gate was never wired here — but Σ ITSELF accumulates, and
	// statefulUserMessage serialises the whole of it into the prompt every
	// step. Nothing measured it, nothing bounded it, and the run grew until
	// the provider refused.
	preambleTokens := estimatePreambleTokens(system, toolSpecs)
	lastIn := preambleTokens + sigmaTokens(sigma)
	lastWindow := effectiveWindow(0, opts)
	// Same rule as the append/recap gate: EVICT on the estimate if you must,
	// but only REPORT a number a provider returned. A driver that reports no
	// usage would otherwise leave lastIn at the preamble estimate forever and
	// tell the operator its state cannot be reduced — about a Σ that is empty.
	footprintMeasured := false
	seenExhaustedState := map[int]bool{}
	holder := &tools.ExecStateHolder{Sigma: sigma}
	dispatchCtx := tools.WithExecutionState(ctx, holder) // the action sees the live Σ (Context op=state)

	var total providers.Usage
	var rec statefulRecovery
	// ⚠️ ACTION IDS ARE PERSISTED, and the step counter restarts at 0 in every
	// run — while a session holds many (each continuation is a new run). A
	// session that later replays as messages (mode auto sends an interactive
	// run to recap) would then carry duplicate tool_use ids, which a provider
	// may reject, and the Web UI pairs calls with results by id. A per-run tag
	// keeps them unique; the in-request retry ids never persist and need none.
	actionIDTag := newActionIDTag()
	// finish emits the run's ONE terminal done. ⚠️ DONE MEANS THE RUN IS OVER, to
	// every consumer that reads it — the Web UI marks the run completed on it, and
	// the MCP / connector spawn paths take their final stop reason from it. This
	// loop used to emit one at every turn boundary before parking, so the embedded
	// terminal showed a parked chat as finished after its first answer and sent
	// the operator's next message as a brand-new continuation run. The append loop
	// has never emitted a mid-run done: the turn boundary of an interactive run is
	// awaiting_input, and per-call usage rides EventUsage.
	finish := func(stop string) {
		emit(providers.Event{Type: providers.EventDone, StopReason: stop, Usage: &total})
	}

	obs := initialObservation(initial)
	if opts.InitialObservation != "" {
		obs = opts.InitialObservation
	}
	// StartParked: a re-attached interactive run waits for the operator before
	// spending a model call. Mirrors Run's handling — an abandoned park ends the
	// run on the turn it had already reached rather than calling the provider.
	if opts.StartParked && interactive {
		next, resumed := parkForStatefulTurn(ctx, &opts, 0, emit)
		if !resumed {
			finish("end_turn")
			return RunResult{StopReason: "end_turn", State: sigma}, nil
		}
		obs = next
	}
	var lastProposed map[string]any // the last schema the model proposed that differs from the active one

	promptSnapshotted := false // RFC DI: the first request is recorded once
	for iter := 0; iter < maxIter; iter++ {
		if err := ctx.Err(); err != nil {
			return RunResult{StopReason: "cancelled", Iterations: iter, Usage: total, State: sigma}, err
		}
		// Same per-iteration pulse the append loop sends; Run's lifetime ticker
		// covers a step that blocks longer than one interval.
		if opts.OnHeartbeat != nil {
			opts.OnHeartbeat()
		}
		// Cooperative pause (RFC X / F41), at the same boundary the append loop
		// parks at: before a model call, never between an action and its
		// result. ⚠️ THIS LOOP NEVER CHECKED IT, so a runtime pause did not
		// quiesce a stateful run — it kept calling the provider while the
		// runtime reported itself paused, Pause() waited out its whole timeout,
		// and the row never reached pause_state='paused', which is the only
		// state resume re-dispatches. Here Σ is persisted (the last
		// context_state) and so is the pending observation (a tool_result or
		// the operator's user_input), which is what a resume rebuilds from.
		if opts.PauseGate != nil && opts.PauseGate.PauseRequested() {
			if err := opts.PauseGate.Park(ctx); err != nil {
				return RunResult{StopReason: "cancelled", Iterations: iter, Usage: total, State: sigma}, ctx.Err()
			}
		}
		msgs := []providers.Message{statefulUserMessage(sigma, obs)}
		var es *emitStateOut
		for attempt := 0; ; attempt++ {
			// RFC DG: say on the WIRE what the system prompt has only ever
			// asked for. Narrowing `tools` to emit_state was never enough —
			// the run that produced this RFC had exactly one tool offered and
			// replied in prose anyway.
			//
			// A provider without the parameter DROPS it and the step proceeds
			// unforced, which is why this is an optimisation and not a
			// precondition: the contract still lives in the prompt, and a
			// model that ignores it is still re-prompted rather than fatal.
			//
			// ⚠️ THE CONTEXT-TRANSFORM CHAIN RUNS HERE TOO, on a copy, exactly as
			// Run applies it to the append loop's request. This request used to
			// go out untransformed, so the `redact` plugin never saw a stateful
			// run — and Σ and the observation are where tool output, and any
			// secret in it, lands.
			reqSystem, reqMsgs := statefulSystem, msgs
			if len(opts.ContextPlugins) > 0 && opts.Provider.ID() != codeJSProviderID {
				cs, cm, perr := contextplugin.Apply(ctx, opts.ContextPlugins, statefulSystem, msgs)
				if perr != nil {
					emit(providers.Event{Type: providers.EventError, Error: "context transform: " + perr.Error()})
					return RunResult{StopReason: "error", Iterations: iter, Usage: total, State: sigma}, perr
				}
				reqSystem, reqMsgs = cs, cm
			}
			req := providers.Request{Model: opts.Model, System: reqSystem, Messages: reqMsgs, Tools: emitTool,
				MaxTokens:        opts.MaxTokens,
				MaxContextTokens: opts.MaxContextTokens, // RFC CJ — without it Ollama ignored a per-agent context size
				Effort:           opts.Effort,
				ToolChoice:       providers.ToolChoice{Mode: providers.ToolChoiceTool, Name: emitStateToolName},
				OnEvent:          emit, // a driver's retry-while-rate-limited event reaches the stream
			}
			applyStatefulSampling(&req, opts.Sampling)
			if !promptSnapshotted {
				// RFC DI: the stateful loop's first request, as sent — its system
				// carries the state instructions the main loop never adds.
				promptSnapshotted = true
				emit(providers.Event{Type: providers.EventPromptSnapshot, PromptSnapshot: providers.NewPromptSnapshot(req.System, req.Messages)})
			}
			call, err := callForEmitState(ctx, opts.Provider, req)
			input, usage := call.input, call.usage
			addUsage(&total, usage)
			if usage != nil {
				total.Provider = opts.Provider.ID() // the SERVING provider, across a fallback
				// The provider's own count of the whole request, which is what
				// the eviction threshold must measure against — the same
				// numerator the append/recap gate uses.
				if in := usage.InputTokens + usage.CacheReadTokens + usage.CacheCreationTokens; in > 0 {
					lastIn = in
					footprintMeasured = true
				}
				lastWindow = effectiveWindow(usage.MaxContextTokens, opts)

				// ⚠️ THIS LOOP NEVER REPORTED A SINGLE TOKEN IT SPENT. It
				// accumulated `total` and attached it to the final EventDone,
				// and emitted no per-call EventUsage at all — which is the
				// event three separate subsystems key off:
				//
				//   the UI gauge        → a stateful run read "0 in / 0 out"
				//                         for its whole life
				//   the RFC AV ledger   → recordCallUsage never fired, so
				//                         token_usage had no rows, /v1/_usage
				//                         was blind to stateful runs, and
				//                         runs.cost summed an empty set to NULL
				//   the RFC AW budgets  → limits.Add never fired, so a
				//                         mode:stateful agent spent tokens that
				//                         counted against NO per-scope budget
				//
				// The last one is the reason this is not a display bug. An
				// operator who set a hard token limit was not protected from
				// this mode, and nothing said so.
				//
				// Stamped like the append loop's: the effective window so the
				// gauge has a denominator, and the SERVING provider so the
				// ledger records which key paid across a mid-run fallback.
				iterUsage := *usage
				iterUsage.MaxContextTokens = lastWindow
				iterUsage.Provider = opts.Provider.ID()
				emit(providers.Event{Type: providers.EventUsage, Usage: &iterUsage})
			}
			if err != nil && !errors.Is(err, errNoEmitState) {
				// Transport or provider fault — nothing the MODEL can correct, but
				// the append loop's recovery applies unchanged, and this loop used
				// to have none: one 429 or "overloaded" killed a stateful run that
				// an append run on the same agent would ride out. Neither retry
				// consumes a max_patch_retries attempt — that budget is for the
				// model's mistakes, and this was not one.
				switch recoverStatefulCall(ctx, &opts, &rec, err, emit, msgs) {
				case statefulCallRetry:
					attempt--
					continue
				case statefulCallCancelled:
					return RunResult{StopReason: "cancelled", Iterations: iter, Usage: total, State: sigma}, ctx.Err()
				}
				emit(providers.Event{Type: providers.EventError, Error: "stateful step failed: " + err.Error()})
				return RunResult{StopReason: "error", Iterations: iter, Usage: total, State: sigma}, err
			}
			// The call reached the provider and came back: the pair is healthy
			// enough to answer, whatever the model then did with the answer.
			rec.sameProviderRetries = 0
			if opts.ClearStall != nil {
				opts.ClearStall(opts.Provider.ID(), opts.Model)
			}
			if err != nil {
				// ⚠️ THE SAME BUDGET AS A REJECTED PATCH, and it used to get
				// none. `on_invalid_patch` / `max_patch_retries` covered a patch
				// that arrived and failed validation; a model that answered in
				// prose — the ordinary behaviour of a local model handed a
				// conversational question — got one shot and killed the run on
				// step zero, with its reply discarded unread.
				//
				// Both are the same condition from the runtime's side: the
				// output was not usable, and the model is the one who can fix
				// it. So it is re-prompted with what it actually said.
				if onInvalid == "fail" || attempt >= maxRetries {
					msg := fmt.Sprintf("stateful step failed: model did not call emit_state after %d attempt(s) — %s. "+
						"context.mode: stateful requires a model that reliably emits tool calls; "+
						"raise context.max_patch_retries, or move this agent to a model that does%s",
						attempt+1, producedInstead(call), unforcedNote(opts))
					emit(providers.Event{Type: providers.EventError, Error: msg})
					return RunResult{StopReason: "error", Iterations: iter, Usage: total, State: sigma}, errors.New(msg)
				}
				// Show it its own reply, then say what was required. An EMPTY
				// assistant turn is never appended — the providers reject one,
				// so a model that returned nothing gets the instruction alone.
				// A reasoning trace is not replayed either: it is not an
				// assistant turn the API will accept back.
				if call.text != "" {
					msgs = append(msgs, providers.Message{Role: "assistant",
						Content:   []providers.ContentBlock{{Type: "text", Text: call.text}},
						Reasoning: call.reasoning, ReasoningSignature: call.reasoningSig})
				}
				msgs = append(msgs, providers.Message{Role: "user",
					Content: []providers.ContentBlock{{Type: "text", Text: "That reply was not usable: in structured " +
						"execution mode the ONLY accepted response is a single `emit_state` tool call, and prose is " +
						"discarded. Send the same content again as `emit_state` — put your answer in `final` with " +
						"`done: true` if you are finished, otherwise name the next `action`."}}})
				continue
			}
			parsed, perr := parseEmitState(input)
			var verr error
			if perr == nil {
				liftNestedProtocolFields(parsed, schema)
				verr = statepatch.ValidatePatch(schema, parsed.Patch)
			}
			if perr != nil || verr != nil {
				cause := perr
				if cause == nil {
					cause = verr
				}
				if onInvalid == "fail" || attempt >= maxRetries {
					msg := fmt.Sprintf("stateful patch rejected after %d attempt(s): %v", attempt+1, cause)
					emit(providers.Event{Type: providers.EventError, Error: msg})
					return RunResult{StopReason: "invalid_patch", Iterations: iter, Usage: total, State: sigma}, errors.New(msg)
				}
				// Rollback-retry: show the model its rejected emit_state + the reason,
				// as a proper tool_use/tool_result pair, and ask for a correction.
				tid := retryToolUseID(call.callID, iter, attempt)
				msgs = append(msgs,
					providers.Message{Role: "assistant", Content: []providers.ContentBlock{{Type: "tool_use", ToolUseID: tid, ToolName: emitStateToolName, ToolInput: input}},
						Reasoning: call.reasoning, ReasoningSignature: call.reasoningSig},
					providers.Message{Role: "user", Content: []providers.ContentBlock{{Type: "tool_result", ToolUseID: tid, Text: "emit_state rejected: " + cause.Error() + ". Emit a corrected emit_state."}}})
				continue
			}
			// ⚠️ A TURN THAT ENDS WITH NOTHING TO SAY. A turn ends only when the
			// model says so — `done`, or an answer in `final` — and a step that
			// ends one with no `final` is sent back, on the same budget as a
			// rejected patch. Not treated as not-done: a model that cannot
			// produce `final` would then loop.
			//
			// Two shapes reach here, and they need different words:
			//
			//   - UNFINISHED: no action, no `done`, no `final` — only a patch and
			//     a reasoning that says what it will do next. Observed live on
			//     ornith-1.5:35b in 4 of 9 replays of one step: "I will run a web
			//     search for the exact figures", and no `action`. This used to END
			//     THE TURN and show that plan to the operator as the answer, so
			//     the chat stopped mid-task twice in a row.
			//   - FINISHED WITHOUT AN ANSWER: `done` with `final` empty. The
			//     reasoning fallback used to show the model's inner monologue
			//     ("Operator said continue. The state shows…") as the reply.
			//
			// Once the budget is spent the turn still ends and the reasoning
			// fallback applies, so a model that cannot comply is never stuck.
			if endsTurn(parsed) && strings.TrimSpace(parsed.Final) == "" &&
				onInvalid != "fail" && attempt < maxRetries {
				tid := retryToolUseID(call.callID, iter, attempt)
				why := "it ends your turn with no `final`, so nothing would be shown. Send it again with your answer in the top-level " +
					"`final` field of emit_state — next to `patch`, not inside it."
				if !parsed.Done && parsed.Action == nil {
					why = "it names no `action` and gives no `final`, so your turn would end with nothing shown. If you meant to run " +
						"a tool next — as your reasoning describes — name it in `action` as {\"tool\": <name>, \"input\": {…}}. If you are " +
						"finished, put your answer in the top-level `final` field with `done: true`."
				}
				msgs = append(msgs,
					providers.Message{Role: "assistant", Content: []providers.ContentBlock{{Type: "tool_use", ToolUseID: tid, ToolName: emitStateToolName, ToolInput: input}},
						Reasoning: call.reasoning, ReasoningSignature: call.reasoningSig},
					providers.Message{Role: "user", Content: []providers.ContentBlock{{Type: "tool_result", ToolUseID: tid, Text: "emit_state not accepted: " + why}}})
				continue
			}
			es = parsed
			rec.firstStepSucceeded = true
			break
		}

		sigmaBefore := sigmaTokens(sigma)
		sigma = statepatch.Merge(sigma, es.Patch)
		for k := range es.Patch {
			lastWritten[k] = iter
		}
		holder.Sigma = sigma

		// ⚠️ STRUCTURAL COMPACTION. Summarising is the wrong operation on a
		// state object: Σ is validated against state_schema, and prose is not a
		// Σ — the next patch would have nothing well-formed to merge into. The
		// structural equivalent is EVICTION of the least significant entries.
		var evicted []string
		// ⚠️ lastIn MEASURED THE REQUEST THIS STEP ANSWERED, which carried the
		// previous Σ. A large patch merged just now was never weighed before
		// the next request went out, so one big write could take the prompt
		// past the window with no eviction first. The gate counts the growth;
		// what is REPORTED stays the provider's own number.
		gateIn := lastIn
		if grown := lastIn - sigmaBefore + sigmaTokens(sigma); grown > gateIn {
			gateIn = grown
		}
		if aboveBackstop(opts.Compaction, gateIn, lastWindow) && backstopAvailable(opts.Compaction) {
			// The budget is Σ's share of the window. The preamble is fixed for
			// the run and the observation is one step, so Σ is the only part
			// eviction can move.
			budget := lastWindow*compactionKeptTailBudgetPct/100 - preambleTokens
			if plan := sigmaEvictionPlan(sigma, schema, lastWritten, budget); len(plan) > 0 {
				// Bank BEFORE dropping. Σ is the run's working memory, and
				// silently losing it is worse than a large prompt — a later
				// recall must be able to fetch back what was evicted.
				bankEvictedSigma(ctx, opts, emit, sigma, plan)
				for _, k := range plan {
					delete(sigma, k)
					delete(lastWritten, k)
				}
				holder.Sigma = sigma
				evicted = plan
				lastIn = preambleTokens + sigmaTokens(sigma)
			} else if footprintMeasured && aboveBackstop(opts.Compaction, lastIn, lastWindow) {
				// Reported only on the provider's own number — the grown
				// estimate may open the gate, but it is not a finding.
				// Nothing evictable and still over: every key is core, or the
				// preamble alone is the problem. Either way the run is heading
				// for the provider's limit and must say so.
				reportExhausted(emit, seenExhaustedState, opts.Compaction, lastIn, lastWindow,
					[]providers.ContextTierVerdict{{
						Mode:   "stateful",
						Reason: providers.DistillDeclineSplitDeclined,
						Message: "stateful state cannot be reduced: every key is retention " +
							"core (the default) — declare x-retention: scratch or derived on " +
							"the state_schema properties that are safe to drop",
					}})
			}
		}

		// Model-proposed schema (RFC CR): recorded for the operator to review +
		// adopt (by forking the agent def's context.state_schema). INERT — it does
		// not change validation this run. Only surfaced when it differs from the
		// active schema, so an agent restating the adopted schema is not noise.
		var proposed map[string]any
		if len(es.ProposeSchema) > 0 && schemasDiffer(es.ProposeSchema, schema) {
			proposed = es.ProposeSchema
			lastProposed = es.ProposeSchema
		}
		emit(providers.Event{Type: providers.EventContextState,
			ContextState: &providers.ContextStateEventInfo{State: sigma, Patch: es.Patch, Iter: iter, Action: actionName(es), Reasoning: es.Reasoning, ProposedSchema: proposed, Evicted: evicted}})

		// Recall harvest (RFC CT): stateful is the most lossy mode — only Σ and the
		// latest observation are fed, so each step's reasoning + observation are
		// otherwise discarded. Embed them into the run-scoped index so a later
		// Recall(query) can fetch the original detail back. nil-safe when recall off.
		stepSpan := []providers.Message{
			{Role: "assistant", Reasoning: es.Reasoning,
				Content: []providers.ContentBlock{{Type: "text", Text: obs}}},
		}
		opts.RecallIndex.Harvest(ctx, stepSpan)
		// Persistent-memory harvest (RFC CT P2): bank the same evicted step for the
		// consolidator when the agent opted in. No-op unless context.harvest_to_memory.
		harvestToMemory(ctx, opts, emit, stepSpan)

		// Turn boundary: done flag, or no action named.
		if endsTurn(es) {
			final := es.Final
			if final == "" {
				// A fallback, and a bend in the contract that reasoning is
				// discarded — kept because an answer found only there is still
				// better shown than lost.
				final = es.Reasoning
			}
			if strings.TrimSpace(final) == "" {
				final = emptyTurnNote(es.Patch)
			}
			emit(providers.Event{Type: providers.EventText, Text: final})

			// ⚠️ RFC DH P1: AN INTERACTIVE RUN PARKS HERE INSTEAD OF ENDING, and
			// this one line is the whole of the reported bug. The loop reached
			// the model's `done` and returned, so a terminal chat stopped after
			// every answer no matter what `interactive` said — the flag reached
			// a loop with nowhere to put it.
			//
			// The park belongs at `done` and not at every step: the step loop
			// is internal machinery the operator never sees, and the thing they
			// wait for is the answer. That is the one place the two loops
			// already agreed, which is why this is a substitution rather than a
			// new concept.
			//
			// interactiveAtBoundary (not the static flag) so a run promoted or
			// demoted mid-flight is honoured, exactly as the append loop does.
			if opts.interactiveAtBoundary(ctx) && opts.SteerQueue != nil {
				next, resumed := parkForStatefulTurn(ctx, &opts, iter, emit)
				if resumed {
					obs = next
					continue
				}
				// Cancelled while parked, or the queue closed: the run ends on
				// the turn it had already completed.
			}
			finish("end_turn")
			return RunResult{StopReason: "end_turn", FinalText: final, Iterations: iter + 1, Usage: total, State: sigma, ProposedSchema: lastProposed}, nil
		}

		// Execute the named action → next observation.
		tid := fmt.Sprintf("es-%s-act-%d", actionIDTag, iter)
		switch {
		case opts.Dispatcher == nil:
			obs = "ERROR: no tools are available to run action " + es.Action.Tool
		// ⚠️ THE ACTION NAME WENT STRAIGHT TO THE DISPATCHER, unchecked. A model
		// that named something that is not a tool got back the dispatcher's
		// "tool not found: X" as its observation — an answer that says the name
		// was wrong and not one word about which names are right.
		//
		// Reported from a live chat: the model named `emit_state` as its
		// action. That is the worst case of the class, because emit_state is
		// not a tool at all — it is the channel the model is ALREADY speaking
		// through, offered as the only entry in `tools` on every step. Asking
		// to "run" it is a category error the runtime was in the best position
		// to name and instead forwarded to a lookup that could only miss.
		case es.Action.Tool == emitStateToolName:
			obs = "ERROR: `" + emitStateToolName + "` is not an action — it is how you reply. " +
				"Every step you make exactly one " + emitStateToolName + " call; its `action` field names " +
				"a DIFFERENT tool for the runtime to run for you. " + statefulActionHint(toolSpecs)
			emit(providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: tid, Name: es.Action.Tool, Input: es.Action.Input}})
			emit(providers.Event{Type: providers.EventToolResult, ToolUse: &providers.ToolUse{ID: tid, Name: es.Action.Tool, Input: es.Action.Input}, Text: obs, IsError: true})
		case !offersTool(toolSpecs, es.Action.Tool):
			// Refused HERE rather than dispatched, so the observation can name
			// the alternatives. The dispatcher knows every tool in the process;
			// only this loop knows which ones THIS agent was offered.
			obs = "ERROR: no tool named `" + es.Action.Tool + "` is available to this agent. " +
				statefulActionHint(toolSpecs)
			emit(providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: tid, Name: es.Action.Tool, Input: es.Action.Input}})
			emit(providers.Event{Type: providers.EventToolResult, ToolUse: &providers.ToolUse{ID: tid, Name: es.Action.Tool, Input: es.Action.Input}, Text: obs, IsError: true})
		case missingRequiredInput(toolSpecs, es.Action.Tool, es.Action.Input) != "":
			// Refused before dispatch, generally rather than per tool: the
			// schema already says what the input needs, and naming it here
			// costs the model one step instead of a guess. Observed live as
			// `{"tool":"Interruption","input":{}}` — a fumble of the same class
			// as naming emit_state, with a real tool.
			obs = "ERROR: " + missingRequiredInput(toolSpecs, es.Action.Tool, es.Action.Input)
			emit(providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: tid, Name: es.Action.Tool, Input: es.Action.Input}})
			emit(providers.Event{Type: providers.EventToolResult, ToolUse: &providers.ToolUse{ID: tid, Name: es.Action.Tool, Input: es.Action.Input}, Text: obs, IsError: true})
		default:
			tu := providers.ToolUse{ID: tid, Name: es.Action.Tool, Input: es.Action.Input}
			emit(providers.Event{Type: providers.EventToolCall, ToolUse: &tu})
			// ⚠️ THROUGH THE APPEND LOOP'S DISPATCH, not the dispatcher directly.
			// This used to call Dispatcher.Execute, which skipped everything
			// executePendingTools adds around a call: the operator's Pre-hooks
			// (a deny did not apply to a stateful agent), Post-hooks, the
			// tool-use id the parallel_spawn ledger keys on, and the RFC DA
			// classification of a failure. It also emits the tool_result.
			ident := tools.RunIdentity(ctx)
			hookIdent := hooks.Identity{Agent: opts.AgentName, UserID: ident.UserID, AgentID: ident.AgentID, Tenant: ident.TenantID}
			// What the append loop stamps per iteration, so Context op=self in a
			// stateful action reports the provider/model it is actually running
			// on (after any fallback), its sampling, and how full the window is.
			// Stamped here rather than at the top of the step because a fallback
			// during this step's call can change the first two.
			actCtx := tools.WithResolvedProvider(dispatchCtx, opts.Provider.ID())
			actCtx = tools.WithResolvedModel(actCtx, opts.Model)
			actCtx = tools.WithResolvedSampling(actCtx, opts.Sampling)
			actCtx = tools.WithMaxContextTokens(actCtx, opts.MaxContextTokens)
			actCtx = tools.WithContextUsage(actCtx, lastIn, lastWindow)
			blocks := executePendingTools(actCtx, opts.Dispatcher, []providers.ToolUse{tu}, 1, opts.Hooks, hookIdent, emit)
			obs = blocks[0].Text
			if blocks[0].IsError {
				obs = "ERROR: " + obs
			}
		}
	}

	finish("max_iterations")
	return RunResult{StopReason: "max_iterations", Iterations: maxIter, Usage: total, State: sigma, ProposedSchema: lastProposed}, nil
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
func unforcedNote(opts RunOptions) string {
	if opts.Provider == nil || opts.Provider.Capabilities().SupportsToolChoice {
		return ""
	}
	return fmt.Sprintf(" (note: provider %q has no tool_choice on the wire, so the call was NOT "+
		"enforced — the tool was requested in the prompt only)", opts.Provider.ID())
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
