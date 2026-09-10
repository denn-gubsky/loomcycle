// docinject.go — the {{document:<ref>}} system-prompt expander.
//
// WHY this exists. An operator writing an agent's prompt often wants the agent
// to WORK FROM a document — a spec, a house style guide, a runbook. Today the
// only way is to tell the agent to go and read it, which makes the instruction
// ADVISORY: the model may ignore it, call the tool wrong, or spend a round-trip
// on something the runtime could have inlined. A binding is resolved content in
// the prompt, so the agent receives the document and cannot decline to read it.
//
// The safety argument is NARROWER than the {{tool:...}} family's, deliberately.
// A document ref is a pure READ of the substrate's own Document store, resolved
// UNDER THE RUN'S OWN AUTHORITY: the same scope fold the agent's Document tool
// would apply. So this family adds REACH (content lands in the prompt without a
// tool call) but no new AUTHORITY — it cannot read a document the run could not
// already read. Relaxing that, so a node can hand a document to an agent that
// holds no Document tool, is a separate change with its own guard.
//
// Like inject.go and toolinject.go this file is pure string work, so the config
// package can import it for boot validation without a cycle. The caller supplies
// already-rendered bodies; this half does recognition, escape handling, framing
// and the per-placeholder cap.
package memory

import (
	"regexp"
	"strings"
)

// DocRef identifies one document a system prompt asks to have inlined.
type DocRef struct {
	// Path is the Path-tree location or document id, e.g. "/specs/launch".
	Path string
	// Heading, when set, selects ONE chunk of the document by its title —
	// `{{document:/specs/launch#Risks}}`. Empty means the whole document.
	Heading string
}

// String renders the ref in placeholder form, for error messages.
func (r DocRef) String() string {
	if r.Heading != "" {
		return r.Path + "#" + r.Heading
	}
	return r.Path
}

// documentPlaceholderPattern matches an OPTIONAL leading backslash (the escape)
// followed by {{document:REF}}.
//
// The ref charset is deliberately RESTRICTIVE — path characters, plus `#` for
// the heading selector — and notably excludes `{`, `}` and `$`. That is not
// tidiness:
//
//   - excluding `{` and `}` means a ref can never contain a nested placeholder,
//     so the single-pass guarantee cannot be undermined from inside an argument;
//   - excluding `$` means a ref cannot carry a ${var.*} token today. When a later
//     phase makes team-node prompts expandable, variables inside a ref must be
//     resolved INSIDE the matched argument rather than by a pre-pass over the
//     template — widening this charset without doing that would reintroduce
//     exactly the injection path the single-pass discipline closes.
const documentPlaceholderPattern = `(\\?)\{\{\s*document\s*:\s*([A-Za-z0-9_./#: @+-]{1,512}?)\s*\}\}`

var documentPlaceholderRe = regexp.MustCompile(`(?i)` + documentPlaceholderPattern)

// MaxDocumentBytes caps ONE {{document:...}} body at 16 KB (~4K tokens). A
// document section or a large chunk fits whole; a full spec truncates.
//
// Per-placeholder, and counted against the shared memory budget as well: the
// cap bounds how much ONE ref can contribute, the budget bounds the total.
const MaxDocumentBytes = 16 * 1024

// documentTruncationMarker is appended when a body is cut. Truncating SILENTLY
// is the failure this avoids: a placeholder that quietly renders half a
// document is a debugging nightmare, because the agent's answer looks like a
// reasoning failure rather than a missing input.
const documentTruncationMarker = "\n\n[… truncated at 16 KB — reference a heading (`{{document:/path#Section}}`) to inline a smaller part]"

// ParseDocRef canonicalises a raw ref token. It reports ok=false for a ref with
// no path, which boot validation surfaces rather than leaving literal.
func ParseDocRef(raw string) (DocRef, bool) {
	path, heading, _ := strings.Cut(strings.TrimSpace(raw), "#")
	path = strings.TrimSpace(path)
	heading = strings.TrimSpace(heading)
	if path == "" {
		return DocRef{}, false
	}
	return DocRef{Path: path, Heading: heading}, true
}

// ReferencesDocRefs returns the distinct refs named by UNESCAPED
// {{document:...}} placeholders, in first-appearance order. The caller renders
// exactly these — a prompt with no document ref does no Document read.
func ReferencesDocRefs(s string) []DocRef {
	var out []DocRef
	seen := map[DocRef]bool{}
	for _, m := range documentPlaceholderRe.FindAllStringSubmatch(s, -1) {
		if m[1] == `\` {
			continue // escaped → literal
		}
		ref, ok := ParseDocRef(m[2])
		if !ok || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out
}

// MalformedDocRefs returns the raw ref tokens that do not parse, for boot
// validation. The pattern matches loosely on purpose — same reason as
// {{tool:...}} — so a typo is REFUSED loudly rather than left silently literal
// in a prompt the operator believes is wired.
func MalformedDocRefs(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range documentPlaceholderRe.FindAllStringSubmatch(s, -1) {
		if m[1] == `\` {
			continue
		}
		if _, ok := ParseDocRef(m[2]); !ok && !seen[m[2]] {
			seen[m[2]] = true
			out = append(out, m[2])
		}
	}
	return out
}

// expandDocumentPlaceholder renders one {{document:...}} match.
//
// A ref with no rendered body (absent, unreadable under the run's scope, empty)
// renders to NOTHING rather than erroring: prompt assembly runs at every
// run-entry, sub-agent spawn and resume, and a run must not fail because a
// document moved. The same posture the {{tool:...}} family takes.
func expandDocumentPlaceholder(match string, bodies map[DocRef]string, remaining *int) string {
	sub := documentPlaceholderRe.FindStringSubmatch(match)
	if sub == nil {
		return match
	}
	if sub[1] == `\` {
		return match[1:] // escaped → literal, backslash stripped
	}
	ref, ok := ParseDocRef(sub[2])
	if !ok {
		return ""
	}
	body := strings.TrimSpace(bodies[ref])
	if body == "" {
		return ""
	}
	body = capDocumentBody(body)
	body = takeBudget(remaining, body)
	if body == "" {
		return ""
	}
	return frameDocument(ref, body)
}

// capDocumentBody applies the per-placeholder cap with a VISIBLE marker. It cuts
// on a rune boundary so a multi-byte character is never split into mojibake.
func capDocumentBody(body string) string {
	if len(body) <= MaxDocumentBytes {
		return body
	}
	cut := body[:MaxDocumentBytes]
	for len(cut) > 0 && !isRuneStart(body[len(cut)]) {
		cut = cut[:len(cut)-1]
	}
	return strings.TrimRight(cut, "\n") + documentTruncationMarker
}

// isRuneStart reports whether b begins a UTF-8 rune (i.e. is not a continuation
// byte). Stdlib's utf8.RuneStart, inlined to keep this file import-light.
func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// frameDocument wraps a body in a DATA frame naming its source. The frame is
// what tells the model this is REFERENCE MATERIAL rather than instruction —
// document bodies are author-written and may contain anything, including text
// shaped like a directive.
func frameDocument(ref DocRef, body string) string {
	return "<document src=\"" + ref.String() + "\">\n" + body + "\n</document>"
}
