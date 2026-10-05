package teamgraph

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// local.go — what a team declares for ITSELF.
//
// A local agent is an agent that exists only inside its team: it is stored in
// the team's definition and nowhere else, is versioned and hashed with it, and
// resolves only from inside a walk of that team. A state names one as
// "./<name>"; a bare name keeps meaning a global agent, so no definition
// written before this changes meaning.

// LocalRefPrefix marks an agent reference as team-local: "./reviewer".
const LocalRefPrefix = "./"

// MaxLocalNameLen bounds a local agent's name. Its full name is
// "<team>/<name>", and both halves share the one-segment grammar.
const MaxLocalNameLen = 64

// Local is a definition's `local` block. Only agents exist today.
//
// It is decoded by hand (UnmarshalJSON) so that a kind this runtime does not
// know is REFUSED. The rest of a definition tolerates unknown keys, and a
// typed struct drops them silently — which here would accept a team declaring
// local skills or channels and then run it without them.
type Local struct {
	// Agents maps a local name to that agent's body: the overlay AgentDef
	// create takes. Each body is held in canonical form — keys sorted, no
	// insignificant whitespace — so the bytes stored, compared and hashed do
	// not depend on how the author laid the JSON out.
	Agents map[string]json.RawMessage `json:"agents,omitempty"`
}

// localError is a refusal of the `local` block itself, as opposed to JSON that
// does not parse. Parse reports it without the "invalid JSON" framing.
type localError struct{ msg string }

func (e *localError) Error() string { return e.msg }

// UnmarshalJSON decodes the block, refusing any kind but `agents` and any
// agent body that is not a JSON object.
func (l *Local) UnmarshalJSON(b []byte) error {
	var kinds map[string]json.RawMessage
	if err := json.Unmarshal(b, &kinds); err != nil {
		return &localError{"local: must be an object of kinds, e.g. {\"agents\": {...}}"}
	}
	out := Local{}
	names := make([]string, 0, len(kinds))
	for kind := range kinds {
		names = append(names, kind)
	}
	sort.Strings(names)
	for _, kind := range names {
		if kind != "agents" {
			return &localError{fmt.Sprintf("local: unknown kind %q — a team may declare only local \"agents\"", kind)}
		}
		if bytes.Equal(bytes.TrimSpace(kinds[kind]), []byte("null")) {
			continue
		}
		var bodies map[string]json.RawMessage
		if err := json.Unmarshal(kinds[kind], &bodies); err != nil {
			return &localError{"local.agents: must be an object of name → agent definition"}
		}
		// Non-nil even when empty: a fork that sends `agents: {}` is stating
		// the whole list, and that list is empty.
		out.Agents = make(map[string]json.RawMessage, len(bodies))
		for name, body := range bodies {
			canon, err := canonicalObject(body)
			if err != nil {
				return &localError{fmt.Sprintf("local.agents[%q]: %s", name, err)}
			}
			out.Agents[name] = canon
		}
	}
	*l = out
	return nil
}

// canonicalObject re-encodes a JSON object with its keys sorted at every depth
// and no insignificant whitespace. Numbers keep their literal text.
func canonicalObject(raw json.RawMessage) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, errors.New("is not valid JSON")
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, errors.New("must be an object (the agent's definition)")
	}
	// encoding/json writes a map's keys sorted.
	out, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// LocalRef reports whether an agent reference is team-local, and the local
// name it carries: "./reviewer" → ("reviewer", true).
func LocalRef(ref string) (string, bool) {
	return strings.CutPrefix(ref, LocalRefPrefix)
}

// QualifiedLocalName is the name a local agent runs under and is shown by:
// "<team>/<name>". Runs, events and agent-scoped state key on it.
func QualifiedLocalName(team, name string) string {
	return team + "/" + name
}

// LocalAgent returns the body of the local agent `name`, if the definition
// declares it.
func (d Definition) LocalAgent(name string) (json.RawMessage, bool) {
	if d.Local == nil {
		return nil, false
	}
	body, ok := d.Local.Agents[name]
	return body, ok
}

// LocalAgentNames returns the declared local agent names, sorted.
func (d Definition) LocalAgentNames() []string {
	if d.Local == nil {
		return nil
	}
	out := make([]string, 0, len(d.Local.Agents))
	for name := range d.Local.Agents {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ValidateLocalName checks a local agent's name: one segment of
// [A-Za-z0-9_-], 1..64 characters — the team-name grammar, for the same
// reason: it is one half of "<team>/<name>".
func ValidateLocalName(name string) error {
	if name == "" {
		return fmt.Errorf("a local agent needs a name")
	}
	if len(name) > MaxLocalNameLen {
		return fmt.Errorf("local agent name is %d bytes; the limit is %d", len(name), MaxLocalNameLen)
	}
	for _, r := range name {
		ok := r == '_' || r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !ok {
			return fmt.Errorf("local agent name %q has invalid character %q (allowed: A-Z a-z 0-9 _ - in one segment of at most %d characters)", name, r, MaxLocalNameLen)
		}
	}
	return nil
}

// validateLocal checks what a definition can say about its own local agents
// without a store: each declared name is well formed, and every "./<name>" a
// state uses is declared. Whether a body is an acceptable agent is the
// authoring caller's to judge (it depends on who is writing), not this
// package's.
func validateLocal(d Definition) error {
	for _, name := range d.LocalAgentNames() {
		if err := ValidateLocalName(name); err != nil {
			return fmt.Errorf("team definition: local.agents: %w", err)
		}
	}
	return CheckLocalRefs(d)
}

// CheckLocalRefs reports the first "./<name>" reference that names a local
// agent the definition does not declare. A walk checks it again before it
// starts: a stored body may predate this rule or have been restored unchecked,
// and a reference that silently fell through to a global agent of the same
// full name would run something the author never named.
func CheckLocalRefs(d Definition) error {
	for _, ref := range AgentRefs(d) {
		name, isLocal := LocalRef(ref.Agent)
		if !isLocal {
			continue
		}
		if _, ok := d.LocalAgent(name); ok {
			continue
		}
		declared := "it declares none"
		if names := d.LocalAgentNames(); len(names) > 0 {
			declared = "declared: " + strings.Join(names, ", ")
		}
		return fmt.Errorf("team definition: state %q %s: %q names a local agent the team does not declare under local.agents (%s)",
			ref.State, ref.Field, ref.Agent, declared)
	}
	return nil
}

// UnreferencedLocalAgents returns the declared local agents no state names,
// sorted. Not an error: an agent of the team may still start one itself.
func UnreferencedLocalAgents(d Definition) []string {
	used := map[string]bool{}
	for _, ref := range AgentRefs(d) {
		if name, ok := LocalRef(ref.Agent); ok {
			used[name] = true
		}
	}
	var out []string
	for _, name := range d.LocalAgentNames() {
		if !used[name] {
			out = append(out, name)
		}
	}
	return out
}

// QualifyLocalRefs returns the definition with every "./<name>" reference
// rewritten to the agent's full name, "<team>/<name>". A walk runs on the
// result, so every member it starts, and every place it names one, carries the
// full name. The stored definition is never rewritten: it keeps what its
// author wrote.
func QualifyLocalRefs(d Definition, team string) Definition {
	states := make([]State, len(d.States))
	for i, s := range d.States {
		h := s.Handler
		// Copied before the rewrite: the slices and the fan-out are shared
		// with the definition the caller still holds.
		h.Agents = append([]string(nil), h.Agents...)
		if h.Fanout != nil {
			f := *h.Fanout
			f.Agents = append([]string(nil), f.Agents...)
			h.Fanout = &f
		}
		visitAgentRefs(&h, func(ref *string, _ string) {
			if name, ok := LocalRef(*ref); ok {
				*ref = QualifiedLocalName(team, name)
			}
		})
		s.Handler = h
		states[i] = s
	}
	d.States = states
	return d
}

// localContent is the `local` block as the content hash sees it: nil when the
// team declares nothing, so `local: {}` and no block at all hash alike.
func localContent(l *Local) *Local {
	if l == nil || len(l.Agents) == 0 {
		return nil
	}
	return l
}
