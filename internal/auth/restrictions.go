package auth

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Restrictions narrows what one credential may do, below what its scopes
// allow. A scope says which kind of thing a principal may do ("create runs");
// a restriction says which ones ("of this agent only"). Every field only
// narrows, so a tenant operator may set any of them on a token it mints: no
// restriction grants anything.
//
// The zero value restricts nothing, and is what every token minted before
// restrictions existed carries.
type Restrictions struct {
	// Agents, when set, is the complete list of agents this credential may
	// start as a top-level run. It also confines the credential to the run
	// plane (see RunPlaneOnly). An agent the listed one spawns is not gated:
	// that follows the agent's own definition.
	Agents []string `json:"agents,omitempty"`
}

// MaxRestrictedAgents bounds an allowlist. It is a list of the few agents a
// client-held token is for, not a catalogue.
const MaxRestrictedAgents = 64

// restrictedAgentNameRe is what an allowlisted name may look like. It is
// deliberately as wide as any agent name the runtime accepts and no wider than
// a name can be: a listed name that cannot exist simply admits nothing.
var restrictedAgentNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:-]{0,127}$`)

// IsZero reports whether nothing is restricted.
func (r Restrictions) IsZero() bool { return len(r.Agents) == 0 }

// RunPlaneOnly reports whether the credential is confined to the run plane:
// starting, continuing, reading and controlling runs, and nothing else.
//
// A credential limited to certain agents must not be able to reach an agent
// some other way, and there are many other ways: authoring a definition (or
// redefining the listed agent), starting a team walk, firing a schedule, a
// direct model call through the gateway or a decision. Refusing each by name
// would miss the next one added, so the rule is the other way round: such a
// credential may use the run endpoints and is refused everywhere else.
func (r Restrictions) RunPlaneOnly() bool { return len(r.Agents) > 0 }

// AgentAllowed reports whether name may be started as a top-level run.
func (r Restrictions) AgentAllowed(name string) bool {
	if len(r.Agents) == 0 {
		return true
	}
	for _, a := range r.Agents {
		if a == name {
			return true
		}
	}
	return false
}

// Validate checks a restrictions record as a caller supplied it.
//
// An empty agents list is refused rather than read as "no agents" or as "no
// restriction": the two readings are opposite, and a mint must not have to
// guess which the caller meant. Leave the field out for no restriction.
func (r Restrictions) Validate(agentsWasSet bool) error {
	if agentsWasSet && len(r.Agents) == 0 {
		return fmt.Errorf("agents must list at least one agent; omit it to leave the token unrestricted")
	}
	if len(r.Agents) > MaxRestrictedAgents {
		return fmt.Errorf("agents lists %d names; the limit is %d", len(r.Agents), MaxRestrictedAgents)
	}
	seen := make(map[string]bool, len(r.Agents))
	for _, a := range r.Agents {
		if !restrictedAgentNameRe.MatchString(a) {
			return fmt.Errorf("agents: %q is not a valid agent name", a)
		}
		if seen[a] {
			return fmt.Errorf("agents: %q is listed twice", a)
		}
		seen[a] = true
	}
	return nil
}

// Describe renders the record for an error or a log line. Never secret.
func (r Restrictions) Describe() string {
	if r.IsZero() {
		return "none"
	}
	return "agents=[" + strings.Join(r.Agents, ",") + "]"
}

// MayStartAgent reports whether the principal on hand may start name as a
// top-level run. True when there is no principal (open mode, a trigger firing
// under a definition's identity): a restriction belongs to a credential, and
// there is none.
func MayStartAgent(p Principal, ok bool, name string) bool {
	return !ok || p.Restrictions.AgentAllowed(name)
}

// MarshalRestrictions renders a record for storage: "" for the zero value, so
// an unrestricted token's row is the row it always was.
func MarshalRestrictions(r Restrictions) (string, error) {
	if r.IsZero() {
		return "", nil
	}
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ParseRestrictions reads a stored record. "" is the zero value.
//
// A record that cannot be read is an ERROR, never "no restrictions": the
// caller must refuse the token, because treating an unreadable narrowing as
// absent would hand a restricted credential the full power of its scopes.
// Unknown keys are refused for the same reason — a row written by a newer
// version may carry a restriction this one does not know how to enforce.
func ParseRestrictions(s string) (Restrictions, error) {
	if strings.TrimSpace(s) == "" {
		return Restrictions{}, nil
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	var r Restrictions
	if err := dec.Decode(&r); err != nil {
		return Restrictions{}, fmt.Errorf("restrictions: %w", err)
	}
	return r, nil
}
