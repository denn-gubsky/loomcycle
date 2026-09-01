package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// strategy.go — the three RFC CR retention regimes, as harness-side context
// assemblers driven per step by the runner. Each is a faithful, loop-agnostic
// stand-in for what the loomcycle loop would do server-side, so we can MEASURE the
// idea (RFC CS) before building it into the loop (RFC CR):
//
//   A0 append   — full growing history (today's default; O(T^2) tokens)
//   A1 recap    — immutable preamble + last-N verbatim + a MODEL-maintained running
//                 recap of evicted steps (RFC CR L1; O(T) tokens)
//   A2 stateful — immutable preamble + an explicit JSON state object the model
//                 patches each step; no history (RFC CR L2 / SKILL.state; O(T) tokens)

// Message is one chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Strategy assembles the messages sent each step and records the model's response.
type Strategy interface {
	Name() string
	// StepMessages builds the context to execute one instruction.
	StepMessages(insn Instruction) []Message
	// Observe records the model's response to the executed step.
	Observe(insn Instruction, response string)
	// PendingRecap returns messages for a model-driven recap call the runner should
	// make (A1 only, when a step falls out of the window); nil otherwise.
	PendingRecap() []Message
	// SetRecap installs the recap the runner obtained from PendingRecap().
	SetRecap(recap string)
	// QueryMessages builds the context to answer a post-stream query.
	QueryMessages(q Query) []Message
}

// taskRules is the immutable preamble P shared by every arm — the "skill spec".
func taskRules() string {
	return "You track a set of named integer counters as instructions arrive one at a time. " +
		"Each instruction may SET a counter to a value, ADD to it, SUBTRACT from it, or RESET it to 0. " +
		"A CORRECTION line overrides a counter's value regardless of prior steps. NOTE lines change nothing. " +
		"Maintain the counters exactly. When later asked a question, answer from the current counter values only."
}

// stepUser renders the per-step user turn.
func stepUser(insn Instruction) string {
	return "Instruction: " + insn.Text + "\nAcknowledge in one short line."
}

// ── A0: full append-only history ────────────────────────────────────────────

type A0 struct {
	history []Message
}

func NewA0() *A0 { return &A0{} }

func (s *A0) Name() string { return "A0-append" }

func (s *A0) StepMessages(insn Instruction) []Message {
	msgs := []Message{{Role: "system", Content: taskRules()}}
	msgs = append(msgs, s.history...)
	msgs = append(msgs, Message{Role: "user", Content: stepUser(insn)})
	return msgs
}

func (s *A0) Observe(insn Instruction, response string) {
	s.history = append(s.history,
		Message{Role: "user", Content: stepUser(insn)},
		Message{Role: "assistant", Content: response})
}

func (s *A0) PendingRecap() []Message { return nil }
func (s *A0) SetRecap(string)         {}

func (s *A0) QueryMessages(q Query) []Message {
	msgs := []Message{{Role: "system", Content: taskRules()}}
	msgs = append(msgs, s.history...)
	msgs = append(msgs, Message{Role: "user", Content: q.QuestionText()})
	return msgs
}

// ── A1: last-N window + model-maintained recap (RFC CR L1) ───────────────────

type A1 struct {
	keepLastN int
	window    []Message // recent (user, assistant) pairs, capped at 2*keepLastN
	recap     string    // running distillation of everything evicted
	pending   []Message // a recap call the runner should make
}

func NewA1(keepLastN int) *A1 { return &A1{keepLastN: keepLastN} }

func (s *A1) Name() string { return "A1-recap" }

func (s *A1) preamble() []Message {
	msgs := []Message{{Role: "system", Content: taskRules()}}
	if s.recap != "" {
		msgs = append(msgs, Message{Role: "system",
			Content: "State recap so far (the net effect of all earlier instructions):\n" + s.recap})
	}
	return msgs
}

func (s *A1) StepMessages(insn Instruction) []Message {
	msgs := s.preamble()
	msgs = append(msgs, s.window...)
	msgs = append(msgs, Message{Role: "user", Content: stepUser(insn)})
	return msgs
}

func (s *A1) Observe(insn Instruction, response string) {
	s.window = append(s.window,
		Message{Role: "user", Content: stepUser(insn)},
		Message{Role: "assistant", Content: response})
	// Evict the oldest pair past the window into a pending recap call.
	if len(s.window) > 2*s.keepLastN {
		evicted := s.window[:2]
		s.window = s.window[2:]
		prior := s.recap
		if prior == "" {
			prior = "(none yet)"
		}
		s.pending = []Message{
			{Role: "system", Content: "You maintain a short running recap of counter state. " +
				"Given the prior recap and the steps now leaving the visible window, produce an updated recap " +
				"that preserves every counter value implied so far. Be terse; output only the recap."},
			{Role: "user", Content: fmt.Sprintf("Prior recap:\n%s\n\nSteps leaving the window:\n%s\n%s\n\nUpdated recap:",
				prior, evicted[0].Content, evicted[1].Content)},
		}
	}
}

func (s *A1) PendingRecap() []Message {
	p := s.pending
	s.pending = nil
	return p
}

func (s *A1) SetRecap(recap string) { s.recap = strings.TrimSpace(recap) }

func (s *A1) QueryMessages(q Query) []Message {
	msgs := s.preamble()
	msgs = append(msgs, s.window...)
	msgs = append(msgs, Message{Role: "user", Content: q.QuestionText()})
	return msgs
}

// ── A2: explicit structured state, patched each step (RFC CR L2) ─────────────

type A2 struct {
	state map[string]int
}

// A2StateSchema is the static per-domain schema (SKILL.state): the counter map.
const A2StateSchema = `{"type":"object","additionalProperties":{"type":"integer"}}`

func NewA2(keys []string) *A2 {
	st := map[string]int{}
	for _, k := range keys {
		st[k] = 0
	}
	return &A2{state: st}
}

func (s *A2) Name() string { return "A2-stateful" }

func (s *A2) stateJSON() string {
	// Deterministic key order so the prompt (and its hash) is stable.
	keys := make([]string, 0, len(s.state))
	for k := range s.state {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%q:%d", k, s.state[k])
	}
	b.WriteByte('}')
	return b.String()
}

func (s *A2) preamble() []Message {
	return []Message{{Role: "system", Content: taskRules() +
		"\n\nYou are given the current counter STATE as a JSON object. After applying the instruction, " +
		"reply with ONLY a JSON object patch of the counters that CHANGED (key -> new integer value); " +
		"use null to delete a key. Reply with {} if nothing changed. No prose."}}
}

func (s *A2) StepMessages(insn Instruction) []Message {
	msgs := s.preamble()
	msgs = append(msgs,
		Message{Role: "user", Content: "STATE: " + s.stateJSON() + "\nInstruction: " + insn.Text + "\nPatch:"})
	return msgs
}

// Observe validates the patch against the schema and merges it (null-deletion). An
// unparseable patch is dropped (the runner's rollback-retry is a future extension);
// the merge is the runtime-owned operation, never the model's.
func (s *A2) Observe(insn Instruction, response string) {
	patch, ok := parsePatch(response)
	if !ok {
		return
	}
	for k, v := range patch {
		if v == nil {
			delete(s.state, k)
			continue
		}
		if n, ok := asInt(v); ok {
			s.state[k] = n
		}
	}
}

func (s *A2) PendingRecap() []Message { return nil }
func (s *A2) SetRecap(string)         {}

func (s *A2) QueryMessages(q Query) []Message {
	msgs := []Message{{Role: "system", Content: taskRules() +
		"\n\nThe current counter STATE is given as JSON. Answer the question from it."}}
	msgs = append(msgs,
		Message{Role: "user", Content: "STATE: " + s.stateJSON() + "\n" + q.QuestionText()})
	return msgs
}

// parsePatch extracts the first JSON object from a model reply and decodes it as a
// counter patch (values are integer or null).
func parsePatch(s string) (map[string]any, bool) {
	i := strings.IndexByte(s, '{')
	j := strings.LastIndexByte(s, '}')
	if i < 0 || j < i {
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s[i:j+1]), &m); err != nil {
		return nil, false
	}
	return m, true
}

// asInt coerces a JSON number (which decodes as float64) to an int.
func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}
