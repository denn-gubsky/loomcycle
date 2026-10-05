package teamgraph

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/skillmatch"
)

// local.go — what a team declares for ITSELF.
//
// A local agent is an agent that exists only inside its team: it is stored in
// the team's definition and nowhere else, is versioned and hashed with it, and
// resolves only from inside a walk of that team. A state names one as
// "./<name>"; a bare name keeps meaning a global agent, so no definition
// written before this changes meaning.
//
// A local skill is the same for skills: a skill body stored in the team. Only
// the team's own agents can be granted one, and only by naming it "./<name>"
// in their `skills` list — no pattern there reaches it.

// LocalRefPrefix marks an agent reference as team-local: "./reviewer".
const LocalRefPrefix = "./"

// MaxLocalNameLen bounds a local agent's name. Its full name is
// "<team>/<name>", and both halves share the one-segment grammar.
const MaxLocalNameLen = 64

// MaxLocalAgents bounds how many agents a team may declare for itself — the
// number MaxVars uses, for the same reason: enough for a real team, small
// enough that a definition cannot make every name resolved inside its walks
// pay for a list nobody could read.
const MaxLocalAgents = 64

// MaxLocalSkills bounds how many skills a team may declare for itself, for the
// reason MaxLocalAgents gives: every listing of a team agent's skills pays for
// them.
const MaxLocalSkills = 64

// Local is a definition's `local` block: the team's own agents, skills and
// channels (see localchannels.go for channels).
//
// It is decoded by hand (UnmarshalJSON) so that a kind this runtime does not
// know is REFUSED. The rest of a definition tolerates unknown keys, and a
// typed struct drops them silently — which here would accept a team declaring
// local schedules and then run it without them.
type Local struct {
	// Agents maps a local name to that agent's body: the overlay AgentDef
	// create takes. Each body is held in canonical form — keys sorted, no
	// insignificant whitespace — so the bytes stored, compared and hashed do
	// not depend on how the author laid the JSON out.
	Agents map[string]json.RawMessage `json:"agents,omitempty"`
	// Skills maps a local name to that skill: what SkillDef create takes. A
	// typed struct, so its encoding — and the hash — is fixed by the field
	// order below, not by the author's layout. Added after Agents with
	// omitempty, so a team declaring none hashes as it did before skills
	// existed.
	Skills map[string]LocalSkill `json:"skills,omitempty"`
	// Channels maps a local name to that channel's definition: the fields a
	// runtime ChannelDef takes, held canonically like Agents (the type that
	// defines them lives above this leaf package; every reader decodes a body
	// strictly, unknown fields refused). Added after Skills with omitempty, so
	// a team declaring none hashes as it did before channels existed.
	Channels map[string]json.RawMessage `json:"channels,omitempty"`
}

// LocalSkill is one skill a team declares for itself: the shape of a SkillDef
// (its body, its self-description, the tools it needs). DO NOT reorder the
// fields: their order is the encoding the team's content hash is taken over.
type LocalSkill struct {
	Body        string   `json:"body,omitempty"`
	Description string   `json:"description,omitempty"`
	Tools       []string `json:"tools,omitempty"`
}

// localError is a refusal of the `local` block itself, as opposed to JSON that
// does not parse. Parse reports it without the "invalid JSON" framing.
type localError struct{ msg string }

func (e *localError) Error() string { return e.msg }

// UnmarshalJSON decodes the block, refusing any kind but `agents` and
// `skills` and `channels`, an agent or channel body that is not a JSON object,
// and a skill that is not exactly a skill (an unknown field in one is refused,
// not dropped).
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
		switch kind {
		case "agents", "skills", "channels":
		default:
			return &localError{fmt.Sprintf("local: unknown kind %q — a team may declare only local \"agents\", \"skills\" and \"channels\"", kind)}
		}
		if bytes.Equal(bytes.TrimSpace(kinds[kind]), []byte("null")) {
			continue
		}
		if kind == "skills" {
			skills, err := decodeLocalSkills(kinds[kind])
			if err != nil {
				return err
			}
			out.Skills = skills
			continue
		}
		dst := &out.Agents
		if kind == "channels" {
			dst = &out.Channels
		}
		var bodies map[string]json.RawMessage
		if err := json.Unmarshal(kinds[kind], &bodies); err != nil {
			return &localError{fmt.Sprintf("local.%s: must be an object of name → definition", kind)}
		}
		// Non-nil even when empty: a fork that sends `agents: {}` is stating
		// the whole list, and that list is empty.
		*dst = make(map[string]json.RawMessage, len(bodies))
		for name, body := range bodies {
			canon, err := canonicalObject(body)
			if err != nil {
				return &localError{fmt.Sprintf("local.%s[%q]: %s", kind, name, err)}
			}
			(*dst)[name] = canon
		}
	}
	*l = out
	return nil
}

// decodeLocalSkills decodes local.skills strictly: a skill is a fixed shape,
// and a misspelt field ("bdy") dropped silently would store a skill without
// the part its author wrote.
func decodeLocalSkills(raw json.RawMessage) (map[string]LocalSkill, error) {
	var bodies map[string]json.RawMessage
	if err := json.Unmarshal(raw, &bodies); err != nil {
		return nil, &localError{"local.skills: must be an object of name → skill definition"}
	}
	// Non-nil even when empty, as for agents: a fork that sends `skills: {}`
	// is stating the whole list.
	out := make(map[string]LocalSkill, len(bodies))
	for name, body := range bodies {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		var sk LocalSkill
		if err := dec.Decode(&sk); err != nil || bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
			return nil, &localError{fmt.Sprintf("local.skills[%q]: must be an object of body, description and tools", name)}
		}
		out[name] = sk
	}
	return out, nil
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
		return nil, errors.New("must be an object (its definition)")
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
func ValidateLocalName(name string) error { return validateLocalName("agent", name) }

// validateLocalName is ValidateLocalName for a local agent or skill (kind).
func validateLocalName(kind, name string) error {
	if name == "" {
		return fmt.Errorf("a local %s needs a name", kind)
	}
	if len(name) > MaxLocalNameLen {
		return fmt.Errorf("local %s name is %d bytes; the limit is %d", kind, len(name), MaxLocalNameLen)
	}
	for _, r := range name {
		ok := r == '_' || r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !ok {
			return fmt.Errorf("local %s name %q has invalid character %q (allowed: A-Z a-z 0-9 _ - in one segment of at most %d characters)", kind, name, r, MaxLocalNameLen)
		}
	}
	return nil
}

// validateLocal checks what a definition can say about its own local agents
// and skills without a store: each declared name is well formed, every
// "./<name>" a state uses is a declared agent, and every "./<name>" a local
// agent's skills list grants is a declared skill. Whether a body is an
// acceptable agent or skill is the authoring caller's to judge (it depends on
// who is writing), not this package's.
func validateLocal(d Definition) error {
	if d.Local != nil && len(d.Local.Agents) > MaxLocalAgents {
		return fmt.Errorf("team definition: local.agents declares %d agents, more than the maximum %d", len(d.Local.Agents), MaxLocalAgents)
	}
	if d.Local != nil && len(d.Local.Skills) > MaxLocalSkills {
		return fmt.Errorf("team definition: local.skills declares %d skills, more than the maximum %d", len(d.Local.Skills), MaxLocalSkills)
	}
	for _, name := range d.LocalAgentNames() {
		if err := ValidateLocalName(name); err != nil {
			return fmt.Errorf("team definition: local.agents: %w", err)
		}
	}
	for _, name := range d.LocalSkillNames() {
		if err := validateLocalName("skill", name); err != nil {
			return fmt.Errorf("team definition: local.skills: %w", err)
		}
	}
	if err := validateLocalChannels(d); err != nil {
		return err
	}
	if err := checkLocalSkillGrants(d); err != nil {
		return err
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
	return CheckLocalChannelRefs(d)
}

// CheckLocalRunNames refuses a definition in which a state names, as a GLOBAL
// agent, the very name one of the team's own agents runs under: with team
// "sdlc" declaring "reviewer", a state `agent: "sdlc/reviewer"`. A bare
// reference is never the team's own agent, so that state asks for a global
// agent which cannot coexist with the local one (the two would share
// agent-scoped state) — and inside a walk the two could not be told apart by
// name. The author means "./reviewer", and is told so.
//
// It needs the team's name, which Validate does not have; whoever stores or
// walks a definition calls it.
func CheckLocalRunNames(d Definition, team string) error {
	if d.Local == nil || len(d.Local.Agents) == 0 {
		return nil
	}
	for _, ref := range AgentRefs(d) {
		if _, isLocal := LocalRef(ref.Agent); isLocal {
			continue
		}
		name, qualified := strings.CutPrefix(ref.Agent, team+"/")
		if !qualified {
			continue
		}
		if _, declared := d.LocalAgent(name); declared {
			return fmt.Errorf("team definition: state %q %s: %q is the name the team's own agent %q runs under, "+
				"and a bare name is a global agent — write %q to run the team's own",
				ref.State, ref.Field, ref.Agent, name, LocalRefPrefix+name)
		}
	}
	return nil
}

// LocalRunNames maps the name each declared local agent runs under
// ("<team>/<name>") back to its reference ("./<name>"). A walk runs on the
// definition QualifyLocalRefs returns, whose members are named by run name;
// this is how it tells one of the team's own from a global agent again when
// it starts them — unambiguously, given CheckLocalRunNames.
func LocalRunNames(d Definition, team string) map[string]string {
	out := map[string]string{}
	for _, name := range d.LocalAgentNames() {
		out[QualifiedLocalName(team, name)] = LocalRefPrefix + name
	}
	return out
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
	if l == nil || (len(l.Agents) == 0 && len(l.Skills) == 0 && len(l.Channels) == 0) {
		return nil
	}
	return l
}

// LocalSkill returns the local skill `name`, if the definition declares it.
func (d Definition) LocalSkill(name string) (LocalSkill, bool) {
	if d.Local == nil {
		return LocalSkill{}, false
	}
	sk, ok := d.Local.Skills[name]
	return sk, ok
}

// LocalSkillNames returns the declared local skill names, sorted.
func (d Definition) LocalSkillNames() []string {
	if d.Local == nil {
		return nil
	}
	out := make([]string, 0, len(d.Local.Skills))
	for name := range d.Local.Skills {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// LocalSkillGrants returns the local skill names an agent's `skills` list
// names by "./<name>" (or "+./<name>"), in list order, malformed ones
// included — checkLocalSkillGrants refuses those at authoring. Negative
// entries are not grants and are skipped.
//
// Only an exact "./<name>" grants a team's own skill. A pattern — `*`,
// `doc/*`, even `./*` — never does: those govern global skills, and a team
// author who wants an agent to have a skill of the team's says which one.
func LocalSkillGrants(skills []string) []string {
	var out []string
	for _, entry := range skills {
		entry = strings.TrimPrefix(strings.TrimSpace(entry), "+")
		if name, ok := LocalRef(entry); ok {
			out = append(out, name)
		}
	}
	return out
}

// LocalSkillGranted reports whether an agent's `skills` list grants the
// team's own skill `name`: an exact "./<name>" entry names it, and no negative
// entry denies it (`-*` denies every skill, a team's own included — a negative
// is a hard floor, as everywhere else in the list).
func LocalSkillGranted(skills []string, name string) bool {
	granted := false
	for _, g := range LocalSkillGrants(skills) {
		granted = granted || g == name
	}
	if !granted {
		return false
	}
	var negatives []string
	for _, entry := range skills {
		if e := strings.TrimSpace(entry); strings.HasPrefix(e, "-") {
			negatives = append(negatives, e)
		}
	}
	// With negatives only, the list is a blacklist: allowed unless one matches.
	return skillmatch.Allowed(negatives, LocalRefPrefix+name)
}

// checkLocalSkillGrants refuses a local agent whose `skills` list grants, by
// "./<name>", a skill the team does not declare — or writes a pattern there,
// which grants nothing and would read as if it did. A body whose `skills` is
// not a list of strings is left to the agent gates, which refuse it.
func checkLocalSkillGrants(d Definition) error {
	for _, agent := range d.LocalAgentNames() {
		var body struct {
			Skills []string `json:"skills"`
		}
		if json.Unmarshal(d.Local.Agents[agent], &body) != nil {
			continue
		}
		for _, name := range LocalSkillGrants(body.Skills) {
			if strings.ContainsAny(name, "*?") {
				return fmt.Errorf("team definition: local.agents[%q].skills: %q is a pattern, and a team's own skill is granted only "+
					"by its exact name — list each one as \"./<name>\"", agent, LocalRefPrefix+name)
			}
			if err := validateLocalName("skill", name); err != nil {
				return fmt.Errorf("team definition: local.agents[%q].skills: %w", agent, err)
			}
			if _, ok := d.LocalSkill(name); ok {
				continue
			}
			declared := "it declares none"
			if names := d.LocalSkillNames(); len(names) > 0 {
				declared = "declared: " + strings.Join(names, ", ")
			}
			return fmt.Errorf("team definition: local.agents[%q].skills: %q names a skill the team does not declare under local.skills (%s)",
				agent, LocalRefPrefix+name, declared)
		}
	}
	return nil
}
