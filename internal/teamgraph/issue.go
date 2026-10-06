package teamgraph

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// issue.go — a validation finding, addressed.
//
// Validation used to stop at the first violation. An editor that shows a
// definition as JSON text wants all of them at once, each pinned to the value
// that caused it, so the validators collect Issues and the error-returning
// entry points (Validate, CheckLocalRefs, …) return the first one. The first
// is the same refusal, word for word, those entry points always returned:
// existing callers and the texts they surface do not change.

// Kinds of Issue that a caller may meet from another source too.
const (
	// IssueLocalAgentMissing / IssueLocalChannelMissing are the kinds TeamDef
	// verify's reference sweep reports for the same finding, so a caller
	// merging the two can dedupe on (Kind, State, Field).
	IssueLocalAgentMissing   = "local_agent_missing"
	IssueLocalChannelMissing = "local_channel_missing"
)

// Issue is one violation found in a team definition.
type Issue struct {
	// Kind is "" for an ordinary graph violation. An undeclared "./<name>"
	// reference sets "local_agent_missing" or "local_channel_missing" (the same
	// kinds TeamDef verify's reference sweep uses, so a caller can dedupe).
	Kind string
	// Path is the JSON path of the offending value in the definition:
	// "entry", "max_iterations", "states[2].handler.fanout.max",
	// "states[0].state", "transitions[3].on", "local.agents.reviewer",
	// "vars.tone", "hooks.run_end", "channels.publish[1]" ...
	Path string
	// State is the state id when the violation is inside a state, and Field the
	// handler-relative path ("fanout.max", "source.channel", "agents[1]").
	State string
	Field string
	// Msg is the refusal text — byte-identical to what Validate returned before.
	Msg string
}

func (i *Issue) Error() string { return i.Msg }

// ValidateAll returns every violation, in the order Validate has always
// checked them. Nil when the definition is valid.
func ValidateAll(d Definition) []*Issue {
	var out issues
	validateAll(&out, d)
	return out
}

// pathKeyRe is the charset a map key may be written in after a dot. Anything
// else — a dot, a space, a quote — is written as a bracketed JSON string, so
// the path stays unambiguous whatever the key.
var pathKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// PathKey appends a map key to a JSON path: base + "." + key when key matches
// [A-Za-z0-9_-]+, else base + "[" + JSON string + "]".
func PathKey(base, key string) string {
	if pathKeyRe.MatchString(key) {
		if base == "" {
			return key
		}
		return base + "." + key
	}
	// A JSON string literal rather than strconv.Quote: the path addresses JSON
	// text, and Go's \x escapes are not JSON. HTML escaping would turn "<" into
	// < for no reader's benefit.
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(key) // encoding a string cannot fail
	return base + "[" + strings.TrimSuffix(b.String(), "\n") + "]"
}

// issues is the collector every validator appends to.
type issues []*Issue

// add appends a fully formed issue.
func (is *issues) add(i *Issue) { *is = append(*is, i) }

// top records a violation outside any state.
func (is *issues) top(path, format string, args ...any) {
	*is = append(*is, &Issue{Path: path, Msg: fmt.Sprintf(format, args...)})
}

// in records a violation inside the state `at`, at the handler-relative field
// (the handler itself when field is "").
func (is *issues) in(at stateAt, field, format string, args ...any) {
	path := handlerPath(at.index)
	if field != "" {
		path += "." + field
	}
	*is = append(*is, &Issue{Path: path, State: at.id, Field: field, Msg: fmt.Sprintf(format, args...)})
}

// stateAt identifies the state a handler check is about: its position (for
// the path) and its id (for the message and Issue.State).
type stateAt struct {
	index int
	id    string
}

// first is the error-returning form of a collecting check: its first issue,
// or nil. A nil slice must come back as an untyped nil, not a nil *Issue
// inside a non-nil error.
func first(is []*Issue) error {
	if len(is) == 0 {
		return nil
	}
	return is[0]
}

// handlerRel is a state-level path relative to its handler:
// "states[1].handler.agents[2]" → "agents[2]".
func handlerRel(path string) string {
	_, rel, _ := strings.Cut(path, ".handler.")
	return rel
}
