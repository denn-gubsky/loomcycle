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
// THE RULE: INLINE WHAT IS PRECISE, DIRECT THE AGENT TO WHAT IS NOT.
//
//	a section or a chunk  → INLINED, complete
//	a whole document      → an INSTRUCTION to read it with the Document tool
//
// A precise ref names something bounded that the operator chose deliberately,
// so it is resolved content the agent cannot decline to read — the binding
// Decision 5 asks for. A whole document is unbounded, and every way of forcing
// it into a prompt is worse than pointing at it:
//
//   - truncating it hands the model half a spec, which it cannot distinguish
//     from a complete one and will answer from confidently;
//   - outlining it spends prompt on a table of contents the agent must then act
//     on anyway;
//   - inlining it whole is a size gamble that fails on exactly the documents
//     worth referencing.
//
// So a whole-document ref renders an instruction naming the tool and the path.
// It is deliberately ADVISORY where the precise forms are not — an agent may
// decline to read it, which is the honest cost of a reference it can act on
// lazily and against the LIVE document rather than an assembly-time snapshot.
//
// A useful consequence: a whole-document ref does NO store read at prompt
// assembly, so it cannot fail, cannot slow assembly, and needs no scope
// resolution at a point in the run lifecycle where policies are not yet stamped.
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

// IsWholeDocument reports a ref that names a document by path with no section
// selector — the one form that renders an INSTRUCTION rather than content.
func (r DocRef) IsWholeDocument() bool { return !r.IsID() && r.Heading == "" }

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
// The ref is a sequence of PATH CHARACTERS or COMPLETE ${...} tokens, and
// nothing else. Bare `{` and `}` are excluded, which makes the grammar
// unambiguous and safe at the same time:
//
//   - a ref can never contain a nested {{...}}, because `{` is not a unit, so
//     the single-pass guarantee cannot be undermined from inside an argument;
//   - `{{document:/a}} text {{document:/b}}` still parses as TWO refs, because
//     `}` cannot be consumed by the argument and therefore terminates it;
//   - `{{document:/specs/${var.pr}}}` parses as ONE ref whose argument is
//     `/specs/${var.pr}`, because a whole variable token IS a unit. A plain
//     non-greedy charset could not express this — it would stop at
//     `/specs/${var.pr` and leave a stray brace behind.
//
// Variables inside the argument are resolved INSIDE the matched placeholder as
// part of resolving it (see expandDocumentPlaceholder), never by a pass over the
// template. That is why widening this charset is safe NOW when it was not
// before: a resolved value lands in a ref that is used as a PATH, and is never
// re-read as template text.
//
// The repetition is `+` rather than a counted bound because RE2 rejects a
// counted repeat wrapping an inner one (the product blows its automaton
// budget). That costs nothing: RE2 is linear-time with no backtracking, and the
// length bound lives in ParseDocRef where it can produce a real message.
const documentPlaceholderPattern = `(\\?)\{\{\s*document\s*:\s*((?:[A-Za-z0-9_./#: @+-]|` + varTokenPattern + `)+)\s*\}\}`

var documentPlaceholderRe = regexp.MustCompile(`(?i)` + documentPlaceholderPattern)

// docRefCharsetRe pins a RESOLVED ref to the same charset the pattern enforces
// on operator-written text.
//
// The pattern's charset is a guarantee about what a ref can contain — no
// quotes, no angle brackets, no newlines — and the frames below are built on
// it: `<document-ref path="...">` and `<document src="...">` are safe to
// assemble by concatenation ONLY because a ref cannot carry the characters that
// would close them. A variable resolved INTO the argument was never checked
// against that charset, so the guarantee held for the half of the ref an
// operator wrote and not for the half an untrusted source supplied.
//
// That is a live path: variables bind from attacker-influenceable sources, and
// a value like `/a"><injected>…` escaped the frame and landed in the SYSTEM
// prompt as markup shaped like a directive. Re-checking after substitution
// costs one anchored match and restores the property the frames assume.
var docRefCharsetRe = regexp.MustCompile(`^[A-Za-z0-9_./#: @+-]+$`)

// ReadInstruction is what a whole-document ref renders: a directive naming the
// tool and the exact path, so the agent can fetch it when the task needs it.
//
// It names the op and the path verbatim rather than describing them, because
// the failure mode of a vague instruction is a model that guesses an argument
// and gets a refusal it then reasons about instead of the document.
func ReadInstruction(ref DocRef) string {
	return "<document-ref path=\"" + ref.Path + "\">\n" +
		"This document is NOT included here. Read it when the task needs it, with the Document tool:\n" +
		"    Document op=export_md path=" + ref.Path + "\n" +
		"Reference one section instead to have it inlined for you: {{document:" + ref.Path + "#<section>}}\n" +
		"</document-ref>"
}

// MaxRefBytes bounds one ref. A path is short by nature; a long one means a
// variable interpolated something that is not a path, and refusing it is a
// clearer outcome than issuing the read.
const MaxRefBytes = 512

// ParseDocRef canonicalises a raw ref token. It reports ok=false for a ref with
// no path, which boot validation surfaces rather than leaving literal.
func ParseDocRef(raw string) (DocRef, bool) {
	if len(raw) > MaxRefBytes {
		return DocRef{}, false
	}
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
func expandDocumentPlaceholder(match string, bodies map[DocRef]string, remaining *int, values map[string]string, refused *[]string) string {
	sub := documentPlaceholderRe.FindStringSubmatch(match)
	if sub == nil {
		return match
	}
	if sub[1] == `\` {
		return match[1:] // escaped → literal, backslash stripped
	}
	// Resolve the argument's variables HERE, inside the matched placeholder,
	// rather than in a pass over the template. A value therefore lands in a ref
	// that is used as a PATH — it is never re-read as template text, so it
	// cannot introduce a placeholder for anything to expand. Trust rule 5b's
	// first mitigation, at the only place it can actually be applied.
	arg := sub[2]
	if values != nil {
		arg = varPlaceholderRe.ReplaceAllStringFunc(arg, func(m string) string {
			return expandVarPlaceholder(m, values, refused)
		})
		// A resolved ref must still BE a ref. Without this the charset the
		// pattern enforces on operator text would not hold for a value an
		// untrusted source supplied, and the frames below — which are
		// concatenated, not escaped — would be closable from inside.
		if !docRefCharsetRe.MatchString(arg) {
			*refused = append(*refused, "document:"+sub[2])
			return ""
		}
	}
	ref, ok := ParseDocRef(arg)
	if !ok {
		return ""
	}
	// A whole document renders an instruction and reads NOTHING — so this form
	// works identically whether or not the document exists, is readable, or the
	// store is even wired.
	if ref.IsWholeDocument() {
		return ReadInstruction(ref)
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

// frameDocument wraps a body in a DATA frame naming its source. The frame is
// what tells the model this is REFERENCE MATERIAL rather than instruction —
// document bodies are author-written and may contain anything, including text
// shaped like a directive.
func frameDocument(ref DocRef, body string) string {
	return "<document src=\"" + ref.String() + "\">\n" + body + "\n</document>"
}
