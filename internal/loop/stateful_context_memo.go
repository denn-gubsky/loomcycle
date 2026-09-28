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
// capped at contextMemoBudget characters, the oldest entry dropped first. The
// same call made again replaces its entry rather than adding one.

// contextMemoBudget bounds the kept Context results, in characters (about 4K
// tokens). A help article is 1–10K characters, so this holds the few a task
// needs.
const contextMemoBudget = 16000

type contextMemoEntry struct {
	call string // the call's input, compacted: the entry's identity
	text string
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
	kept := m.entries[:0]
	for _, e := range m.entries {
		if e.call != call {
			kept = append(kept, e)
		}
	}
	m.entries = append(kept, contextMemoEntry{call: call, text: text})
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
