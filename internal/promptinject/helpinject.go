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
//     examples, is accepted; config load and AgentDef create/fork refuse any
//     other (and a malformed placeholder) with a message naming it, so a typo
//     cannot sit in a prompt that looks wired and renders nothing.
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
// Config load and AgentDef create/fork refuse a prompt whose examples exceed
// it, so at run time the budget only bites when an article grew between the
// binary that accepted a stored definition and the one rendering it; then
// whole lines are dropped (a cut JSON object would teach a broken call) and a
// marker says so.
const HelpMaxBytes = 8192

// helpTruncMarker ends a help body the budget cut.
const helpTruncMarker = "…[help examples truncated]"

// helpPlaceholderPattern matches an OPTIONAL leading backslash (the escape)
// followed by {{help:TOPIC}}.
//
// The topic is matched LOOSELY — anything up to the first `}` — and validated
// in Go, for the reason the tool family gives: `{{help:Document search}}` or
// `{{help:${var.t}}}` must MATCH the family so MalformedHelpRefs refuses it,
// rather than sitting in the prompt as literal text the operator believes is
// wired. Excluding `}` keeps two placeholders on one line two matches.
const helpPlaceholderPattern = `(\\?)\{\{\s*help\s*:([^}]*)\}\}`

var helpPlaceholderRe = regexp.MustCompile(`(?i)` + helpPlaceholderPattern)

// helpTopicRe is the shape of a topic name: `<topic>`, `<Tool>` or
// `<Tool>/<op>`, as the help index names them. Case-sensitive, as the index
// is: the placeholder names one article exactly.
var helpTopicRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*(?:/[A-Za-z][A-Za-z0-9_-]*)?$`)

// ParseHelpTopic trims a raw topic token and reports whether it has the shape
// of a topic name. Whether the topic EXISTS is the caller's check: this
// package does not hold the corpus.
func ParseHelpTopic(raw string) (string, bool) {
	t := strings.TrimSpace(raw)
	return t, helpTopicRe.MatchString(t)
}

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

// MalformedHelpRefs returns the distinct raw tokens of unescaped {{help:…}}
// placeholders whose topic is not shaped like a topic name (empty, spaces, a
// `.` separator, a variable). Validation refuses them; an escaped placeholder
// is a literal and is ignored.
func MalformedHelpRefs(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range helpPlaceholderRe.FindAllStringSubmatch(s, -1) {
		if m[1] == `\` {
			continue
		}
		raw := strings.TrimSpace(m[2])
		if _, ok := ParseHelpTopic(raw); ok || seen[raw] {
			continue
		}
		seen[raw] = true
		out = append(out, raw)
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
