// docinject.go — the {{document:<ref>}} system-prompt expander.
//
// WHY this exists. An operator writing an agent's prompt often wants the agent
// to WORK FROM a document — a spec, a house style guide, a runbook. Today the
// only way is to tell the agent to go and read it, which makes the instruction
// ADVISORY: the model may ignore it, call the tool wrong, or spend a round-trip
// on something the runtime could have inlined. A binding is resolved content in
// the prompt, so the agent receives the document and cannot decline to read it.
//
// WHAT A REF ADDRESSES. Three forms, in increasing precision:
//
//	{{document:/specs/launch}}          a document
//	{{document:/specs/launch#Risks}}    ONE section of it
//	{{document:<chunk-id>}}             ONE chunk, by id
//
// The precise forms exist because inlining a whole document is usually the wrong
// answer. A Document is CHUNKED by design, and a chunk is the unit an author
// wrote and a reader wants — so addressing one gives the agent something
// complete and small, rather than a wall of text it must search.
//
// AND NOTHING IS EVER TRUNCATED. An earlier version cut an oversized body at
// 16 KB with a marker. That was wrong: a visible marker helps a human reading
// the resolved prompt, but the MODEL still answers confidently from half a
// spec, and a half-spec is indistinguishable from a complete one to it. When a
// document does not fit, this family inlines a REFERENCE instead — the
// document's title, path, and its section outline — so the agent gets an
// accurate map and the operator sees exactly which selector to narrow to. A
// correct map beats an arbitrary first-16-KB slice.
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

// DocRef identifies what a system prompt asks to have inlined.
type DocRef struct {
	// Path is a Path-tree location ("/specs/launch") when it starts with "/",
	// and otherwise a chunk or document id.
	Path string
	// Heading, when set, selects ONE section by its title —
	// `{{document:/specs/launch#Risks}}`. Empty means the whole document.
	Heading string
}

// IsID reports whether the ref addresses a chunk or document by id rather than
// by Path-tree location. Ids carry no leading slash.
func (r DocRef) IsID() bool { return !strings.HasPrefix(r.Path, "/") }

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

// MaxDocumentBytes is the size ONE {{document:...}} body may inline: 16 KB
// (~4K tokens). A chunk or a section fits comfortably; a full spec generally
// does not.
//
// It is a FIT THRESHOLD, not a truncation point. A body over it is not cut —
// the caller renders an outline instead (see OutlineFor). Per-placeholder, and
// still counted against the shared memory budget: this bounds what ONE ref may
// contribute, the budget bounds the total.
const MaxDocumentBytes = 16 * 1024

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
	body = takeBudget(remaining, body)
	if body == "" {
		return ""
	}
	return frameDocument(ref, body)
}

// Fits reports whether a body may be inlined whole. The caller renders an
// outline for anything larger rather than cutting it.
func Fits(body string) bool { return len(body) <= MaxDocumentBytes }

// OutlineFor renders the REFERENCE a document gets when its full text does not
// fit: what it is, where it is, and what sections it contains.
//
// This is what the agent receives INSTEAD of a truncated body, and it is
// strictly more useful. A cut document is a confident half-answer; an outline is
// an accurate map, and it names the exact selector that would inline any one
// part — so an operator reading the resolved prompt can see what to narrow the
// ref to, and an agent that holds the Document tool can fetch precisely.
func OutlineFor(ref DocRef, title string, sections []string) string {
	var b strings.Builder
	b.WriteString("This document is too large to inline. Its outline follows; the full text was NOT included.\n\n")
	if title != "" {
		b.WriteString("title: " + title + "\n")
	}
	b.WriteString("ref: " + ref.String() + "\n")
	if len(sections) == 0 {
		b.WriteString("\n(no sections)\n")
		return b.String()
	}
	b.WriteString("\nsections:\n")
	for _, s := range sections {
		b.WriteString("  - " + s + "\n")
	}
	b.WriteString("\nTo inline one section instead of the whole document, reference it as " +
		"{{document:" + ref.Path + "#<section>}}.\n")
	return b.String()
}

// frameDocument wraps a body in a DATA frame naming its source. The frame is
// what tells the model this is REFERENCE MATERIAL rather than instruction —
// document bodies are author-written and may contain anything, including text
// shaped like a directive.
func frameDocument(ref DocRef, body string) string {
	return "<document src=\"" + ref.String() + "\">\n" + body + "\n</document>"
}
