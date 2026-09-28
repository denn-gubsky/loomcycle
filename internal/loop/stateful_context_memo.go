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

// add keeps one successful Context result.
func (m *contextMemo) add(input json.RawMessage, text string) {
	var b bytes.Buffer
	call := string(input)
	if json.Compact(&b, input) == nil {
		call = b.String()
	}
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
	if b.Len() == 0 {
		return ""
	}
	return fmt.Sprintf("What you already read with %s in this run (kept for you; do not call it again for these):\n\n%s", tool, b.String())
}
