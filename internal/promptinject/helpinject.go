// helpinject.go — the {{help:<topic>}} system-prompt placeholder (RFC GL P4).
//
// WHY this exists. Agent prompts carry call cards — `Document {"op":"search",…}`
// lines showing the model the exact argument shape of a call. Hand-written,
// they drift: a parameter is renamed, an op gains a required field, and the
// prompt keeps teaching the old call. The help articles already hold
// maintained call examples that a test checks against each tool's live input
// schema. {{help:Document/search}} renders that article's `## Examples` calls
// in place, one `<Tool> <arguments JSON>` line each, so a prompt's samples are
// the checked ones.
//
// THE TRUST ARGUMENT. Prompt assembly runs at every run start, sub-agent spawn
// and resume, and a runtime AgentDef's prompt is model-authorable, so a
// placeholder must have no side effects, no recursion, no per-run cost, and
// must not let a model-authored prompt reach anything new. This family meets
// each by construction:
//
//   - No side effects, no per-run cost. The bodies are help articles EMBEDDED
//     in the binary; rendering reads those bytes and calls nothing — no store,
//     no tool, no network, no directory (an operator's LOOMCYCLE_HELP_ROOT
//     override is deliberately not read).
//   - No new reach. Every article is already readable by any agent through
//     Context op=help; the placeholder moves text it could fetch anyway into
//     its prompt, bounded by HelpMaxBytes.
//   - No recursion. A rendered body is substituted inside the single
//     ReplaceAllStringFunc pass of ExpandWithRefusals, which never rescans its
//     own output, so `{{…}}` or `${…}` text inside an example stays literal.
//   - Closed set. Only a topic the embedded corpus holds, and that has call
//     examples, is accepted; every surface where a prompt is authored (config
//     load, AgentDef create/fork, register_agent, team templates) refuses any
//     other (and a malformed placeholder) with a message naming it, so a typo
//     cannot sit in a prompt that looks wired and renders nothing. A stored
//     definition is never refused over it when read.
//
// The body is rendered UNFRAMED, like the trusted memory variants: it is
// loomcycle's own text, and its point is to be copied — framing it "reference
// data, NOT instructions" would say the opposite of what is meant.
//
// Like the other families this file is pure string work: the caller (api/http)
// renders the bodies through internal/help/builtin and hands them in; config
// validates topics against the same renderer.
package promptinject

import (
	"regexp"
	"strings"
)

// HelpMaxBytes caps the TOTAL rendered help examples one prompt (or one caller
// segment) may carry: 8 KiB, about 2K tokens. The largest single article
// renders to ~2.4 KiB and an operation article to a few hundred bytes, so this
// fits the call cards of a dozen operations — the walker agent's whole card —
// while keeping a prompt that names every topic it can from turning into a
// copy of the manual.
//
// Its own budget, not the memory or tool one: budgets are consumed in prompt
// order, and sharing would let a large memory section above the call card
// truncate the samples the agent needs to make its calls.
//
// The budget is per expanded text: the agent's system prompt, and separately
// each team template, since each is a separate pass. Every authoring surface
// refuses a text whose examples exceed it, so at run time the budget only bites when an article grew between the
// binary that accepted a stored definition and the one rendering it; then
// whole lines are dropped (a cut JSON object would teach a broken call) and a
// marker says so.
const HelpMaxBytes = 8192

// helpTruncMarker ends a help body the budget cut.
const helpTruncMarker = "…[help examples truncated]"

// helpPlaceholderPattern matches an OPTIONAL leading backslash (the escape)
// followed by {{help:TOPIC}}.
//
// The topic is matched loosely — any run of characters other than a brace —
// and validated in Go, for the reason the tool family gives: `{{help:../x}}`
// must MATCH the family so MalformedHelpRefs refuses it, rather than sitting in
// the prompt as literal text the operator believes is wired.
//
// Braces are excluded in BOTH directions. Excluding `}` keeps two placeholders
// on one line two matches. Excluding `{` means a help match can never contain
// the opener of another placeholder: with `[^}]*`, `{{help:{{identity}}`
// matched as one help placeholder and swallowed the {{identity}} inside it. An
// opener with no well-formed close (`{{help:${var.t}}}`) therefore does not
// match here; MalformedHelpRefs finds it through helpOpenerRe instead.
const helpPlaceholderPattern = `(\\?)\{\{\s*help\s*:([^{}]*)\}\}`

var helpPlaceholderRe = regexp.MustCompile(`(?i)` + helpPlaceholderPattern)

// helpOpenerRe finds every `{{help:` opener, so validation can refuse one that
// does not close as a placeholder (it would otherwise stay in the prompt as
// literal text, or have a variable substituted into it).
var helpOpenerRe = regexp.MustCompile(`(?i)(\\?)\{\{\s*help\s*:`)

// helpAtStartRe is helpPlaceholderRe anchored, to ask whether an opener
// begins a whole placeholder.
var helpAtStartRe = regexp.MustCompile(`(?i)^` + helpPlaceholderPattern)

// helpTopicRe is the shape of a topic name: `<topic>`, `<Tool>` or
// `<Tool>/<op>`. The separator may also be written `.` or a space, the way a
// model writes the call it is about to make; ParseHelpTopic folds it to `/`.
// Case is left as written: which article it names (case-insensitively, or
// through an alias) is the corpus's answer, not this grammar's.
var helpTopicRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_-]*)(?:(?:\s*[/.]\s*|\s+)([A-Za-z][A-Za-z0-9_-]*))?$`)

// ParseHelpTopic trims a raw topic token, folds its separator to `/`, and
// reports whether it has the shape of a topic name. Whether the topic EXISTS
// is the caller's check: this package does not hold the corpus.
func ParseHelpTopic(raw string) (string, bool) {
	m := helpTopicRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return strings.TrimSpace(raw), false
	}
	if m[2] == "" {
		return m[1], true
	}
	return m[1] + "/" + m[2], true
}

// ReferencesHelp reports whether s contains any {{help:…}} placeholder,
// escaped, malformed or not: the cheap gate a caller's no-placeholder fast path
// checks, so an escaped one is unescaped and a malformed one removed rather
// than left in the prompt as written.
func ReferencesHelp(s string) bool { return helpPlaceholderRe.MatchString(s) }

// HelpTopics returns the topic of every UNESCAPED, well-formed {{help:…}}
// placeholder in s, in order and with repeats — each occurrence renders, so
// each counts against HelpMaxBytes. ReferencesHelpTopics is the distinct set.
func HelpTopics(s string) []string {
	var out []string
	for _, m := range helpPlaceholderRe.FindAllStringSubmatch(s, -1) {
		if m[1] == `\` {
			continue // escaped → literal
		}
		if topic, ok := ParseHelpTopic(m[2]); ok {
			out = append(out, topic)
		}
	}
	return out
}

// ReferencesHelpTopics returns the distinct topics named by unescaped,
// well-formed {{help:…}} placeholders in s, in first-appearance order: the
// set the caller renders.
func ReferencesHelpTopics(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range HelpTopics(s) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// MalformedHelpRefs returns the distinct raw tokens of unescaped `{{help:`
// openers that do not form a usable placeholder: a topic not shaped like a
// topic name (empty, a path, a third segment), or an opener that never closes
// as a placeholder (`{{help:${var.t}}}`, `{{help:{{identity}}`). Validation
// refuses them; an escaped opener is a literal and is ignored.
func MalformedHelpRefs(s string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(raw string) {
		if !seen[raw] {
			seen[raw] = true
			out = append(out, raw)
		}
	}
	for _, loc := range helpOpenerRe.FindAllStringSubmatchIndex(s, -1) {
		if loc[3] > loc[2] {
			continue // escaped → literal
		}
		rest := s[loc[0]:]
		if m := helpAtStartRe.FindStringSubmatch(rest); m != nil {
			if _, ok := ParseHelpTopic(m[2]); !ok {
				add(strings.TrimSpace(m[2]))
			}
			continue
		}
		// No well-formed close: name what follows the colon, up to the first
		// `}}` or a short cut, so the message points at the text.
		after := s[loc[1]:]
		if i := strings.Index(after, "}}"); i >= 0 {
			after = after[:i]
		}
		if len(after) > 40 {
			after = after[:40]
		}
		add(strings.TrimSpace(after))
	}
	return out
}

// expandHelpPlaceholder substitutes one {{help:…}} match from the rendered
// bodies, consuming the help budget.
func expandHelpPlaceholder(match string, bodies map[string]string, remaining *int) string {
	sub := helpPlaceholderRe.FindStringSubmatch(match)
	if sub == nil {
		return match // unreachable: the caller matched this family
	}
	if sub[1] == `\` {
		return match[1:] // escaped → literal, backslash stripped
	}
	topic, ok := ParseHelpTopic(sub[2])
	if !ok {
		// Validation refused this definition; a run must not fail on prompt
		// assembly, so a malformed placeholder that got past it renders
		// nothing — the {{tool:…}} family's rule for a ref it cannot serve.
		return ""
	}
	body := strings.TrimSpace(bodies[topic])
	if body == "" {
		// An unknown topic, or one that lost its examples since the definition
		// was accepted: nothing to render, same rule.
		return ""
	}
	return takeHelpBudget(remaining, body)
}

// takeHelpBudget returns body cut to the remaining budget at a LINE boundary,
// decrementing *remaining. Each line is one complete call; a line cut in half
// would be a malformed example, so lines that do not fit are dropped whole and
// the marker says so. Once anything is dropped the budget is spent, so a later
// placeholder renders nothing rather than a partial set in a different place.
func takeHelpBudget(remaining *int, body string) string {
	if *remaining <= 0 {
		return ""
	}
	if len(body) <= *remaining {
		*remaining -= len(body)
		return body
	}
	var kept []string
	used := 0
	for _, ln := range strings.Split(body, "\n") {
		cost := len(ln)
		if len(kept) > 0 {
			cost++ // the joining newline
		}
		if used+cost > *remaining {
			break
		}
		kept = append(kept, ln)
		used += cost
	}
	*remaining = 0
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, "\n") + "\n" + helpTruncMarker
}
