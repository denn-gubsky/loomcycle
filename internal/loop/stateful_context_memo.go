package loop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A stateful run's model reads only its latest observation, so a help article
// it read two steps ago is gone. Measured in the 1.99.0 tool-usage eval: the
// stateful models re-read the same articles again and again — ornith read the
// Document create_document and create_chunk articles 3 times each in one run,
// deepseek read History/recap 4 times in another — and two ornith runs spent
// their whole 10-minute budget that way. So what the run's Context tool
// returned (help, its scopes, its tools) is kept and shown on every later step.
// Only reference ops are kept (see contextOpIsStatic): a kept clock or Σ would
// be shown as current long after it was not.
//
// Bounded, because the stateful prompt is meant to stay flat: the kept text is
// capped at contextMemoBudget characters, the oldest entry dropped first. A
// result of the same thing — the same help topic, however it was asked for —
// replaces its entry rather than adding one (see memoKey).

// contextMemoBudget bounds the kept Context results, in characters (about 4K
// tokens). A help article is 1–10K characters, so this holds the few a task
// needs.
const contextMemoBudget = 16000

type contextMemoEntry struct {
	key  string // what the result is OF (see memoKey): the entry's identity
	call string // the call's input, compacted, as shown to the model
	text string
}

// memoKey is what a Context result is OF, so the same thing read twice is kept
// once however the call was written. A help article's identity is the topic
// Context resolved, which its response names: "Memory/set", "Memory.set", an
// alias, a different key order or an extra empty argument all read one
// article. Any other op's identity is the op and its arguments with the keys
// sorted.
func memoKey(input json.RawMessage, result string) string {
	var in map[string]any
	if json.Unmarshal(input, &in) != nil {
		return "raw:" + string(input)
	}
	if in["op"] == "help" {
		var out struct {
			Name string `json:"name"`
		}
		if json.Unmarshal([]byte(result), &out) == nil && out.Name != "" {
			return "help:" + out.Name
		}
	}
	for k, v := range in {
		if v == nil || v == "" {
			delete(in, k) // an empty argument changes nothing Context returns
		}
	}
	b, _ := json.Marshal(in) // map keys marshal sorted
	return "call:" + string(b)
}

type contextMemo struct {
	entries []contextMemoEntry
	// The latest result of an action that was NOT the Context tool, and the
	// call that produced it. Measured in the 1.100.0 eval: a cloud model
	// created a document, read a help article, and on the next step saw only
	// the article, so the document id create_document had returned was gone
	// and it created a second document. The kept Context results pushed the
	// work out; this keeps the latest piece of work beside them.
	workTool, workCall, workText string
	// workFailed marks a kept action that returned an error. It is shown as
	// failed, not as done: the usual sequence is fail → read help → retry, and
	// at the retry step the failed call would otherwise read "do not repeat it".
	workFailed bool
}

// addWork keeps the latest result of an action that was not a Context call;
// failed is whether that result is an error.
func (m *contextMemo) addWork(tool string, input json.RawMessage, text string, failed bool) {
	m.workTool, m.workCall, m.workText, m.workFailed = tool, compactCall(input), text, failed
}

// clearWork forgets the kept action result, for a new operator turn.
func (m *contextMemo) clearWork() {
	m.workTool, m.workCall, m.workText, m.workFailed = "", "", "", false
}

func compactCall(input json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, input) == nil {
		return b.String()
	}
	return string(input)
}

// helpToolName is the run's Context tool: the one that serves help. "" when the
// run has none.
func helpToolName(ts []tools.Tool) string {
	for _, t := range ts {
		if _, ok := t.(tools.HelpIndex); ok {
			return t.Name()
		}
	}
	return ""
}

// contextOpIsStatic classifies every Context op: static (reference text that
// stays true for the whole run, so it is kept and the model is told not to
// call it again) or live (a clock, Σ, the window's fill, the channel list — a
// kept copy goes stale and would contradict the step's own "Current state:").
// known is false for an op this list does not name: such a result is not kept,
// and TestContextOpIsStatic_ClassifiesEveryContextOp fails until the new op is
// classified here.
func contextOpIsStatic(op string) (static, known bool) {
	switch op {
	case "help", "guide", "doc", "tools", "permissions", "capabilities":
		return true, true
	case "self", "agents", "lineage", "evaluations", "channels", "time", "compact", "state":
		return false, true
	}
	return false, false
}

// add keeps one successful Context result, when its op is static.
func (m *contextMemo) add(input json.RawMessage, text string) {
	var in struct {
		Op string `json:"op"`
	}
	if json.Unmarshal(input, &in) != nil {
		return
	}
	if static, _ := contextOpIsStatic(in.Op); !static {
		return
	}
	call := compactCall(input)
	key := memoKey(input, text)
	kept := m.entries[:0]
	for _, e := range m.entries {
		if e.key != key {
			kept = append(kept, e)
		}
	}
	m.entries = append(kept, contextMemoEntry{key: key, call: call, text: text})
	for len(m.entries) > 1 && m.size() > contextMemoBudget {
		m.entries = m.entries[1:]
	}
}

func (m *contextMemo) size() int {
	n := 0
	for _, e := range m.entries {
		n += len(e.text)
	}
	return n
}

// render is the kept results for the step's prompt, leaving out the one that
// is already the latest observation. "" when there is nothing else to show.
func (m *contextMemo) render(tool, obs string) string {
	var b strings.Builder
	for _, e := range m.entries {
		if e.text == obs {
			continue
		}
		fmt.Fprintf(&b, "%s %s returned:\n%s\n\n", tool, e.call, e.text)
	}
	var out string
	if b.Len() > 0 {
		out = fmt.Sprintf("What you already read with %s in this run (kept for you; do not call it again for these):\n\n%s", tool, b.String())
	}
	if m.workText != "" && m.workText != obs {
		status := "it is done; do not repeat it"
		if m.workFailed {
			status = "it FAILED and is not done; fix what its error names before retrying"
		}
		out += fmt.Sprintf("Your last action (%s), %s %s, returned:\n%s\n\n", status, m.workTool, m.workCall, m.workText)
	}
	return out
}
