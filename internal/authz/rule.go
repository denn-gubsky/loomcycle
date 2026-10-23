package authz

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Effect is what a matching rule does.
type Effect string

const (
	Allow Effect = "allow"
	Deny  Effect = "deny"
)

// Bounds on a rule list. They bound evaluation cost and the error surface of
// a typo; none is a policy limit anyone should reach.
const (
	MaxRulesPerACL       = 256
	MaxResourcesPerRule  = 64
	MaxActionsPerRule    = 32
	MaxACLNameBytes      = 64
	MaxDescriptionBytes  = 1024
	MaxSubjectIDBytes    = 128
	MaxGroupMembers      = 10000
	MaxGroupNestingDepth = 8
)

// Rule is one entry of an ACL: an effect on a set of actions over a set of
// resource selectors, optionally only under a condition.
type Rule struct {
	Effect      Effect     `json:"effect"`
	Actions     []string   `json:"actions"`
	Resources   []string   `json:"resources"`
	When        *Condition `json:"when,omitempty"`
	Description string     `json:"description,omitempty"`
}

// ACL is a named, ordered rule list (RFC EY §3.1). Order is for reading and
// for Explain; it does not change the outcome.
type ACL struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Rules       []Rule `json:"rules"`
}

// nameRe is what an ACL or group name may look like: the agent-name shape,
// with `/` so lists can be grouped (`support/tier-1`).
var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:/-]{0,63}$`)

// ValidName reports whether name may name an ACL or a group.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// ParseACL reads an ACL from JSON, refusing unknown fields: a rule list a
// newer build wrote may carry something this one cannot enforce, and it must
// not read as a looser list (the RFC EF ParseRestrictions posture).
func ParseACL(b []byte) (ACL, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var a ACL
	if err := dec.Decode(&a); err != nil {
		return ACL{}, fmt.Errorf("acl: %w", err)
	}
	if dec.More() {
		return ACL{}, fmt.Errorf("acl: trailing data after the document")
	}
	if err := a.Validate(); err != nil {
		return ACL{}, err
	}
	return a, nil
}

// ParseRules reads a rule list alone (the stored `rules` column), with the
// same strictness as ParseACL.
func ParseRules(b []byte) ([]Rule, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var rules []Rule
	if err := dec.Decode(&rules); err != nil {
		return nil, fmt.Errorf("acl rules: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("acl rules: trailing data after the list")
	}
	for i := range rules {
		if err := rules[i].Validate(); err != nil {
			return nil, fmt.Errorf("rule %d: %w", i, err)
		}
	}
	return rules, nil
}

// Validate checks the ACL against the catalog and the bounds. It is called
// on every write surface and again when a stored list is read.
func (a ACL) Validate() error {
	if !ValidName(a.Name) {
		return fmt.Errorf("acl: %q is not a valid name", a.Name)
	}
	if len(a.Description) > MaxDescriptionBytes {
		return fmt.Errorf("acl %s: description is longer than %d bytes", a.Name, MaxDescriptionBytes)
	}
	if len(a.Rules) == 0 {
		return fmt.Errorf("acl %s: no rules; an empty list grants and denies nothing and is a mistake", a.Name)
	}
	if len(a.Rules) > MaxRulesPerACL {
		return fmt.Errorf("acl %s: %d rules; the limit is %d", a.Name, len(a.Rules), MaxRulesPerACL)
	}
	for i, r := range a.Rules {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("acl %s rule %d: %w", a.Name, i, err)
		}
	}
	return nil
}

// Validate checks one rule: a known effect, at least one action and one
// resource, every selector parseable, and every action a verb of every
// listed type (a rule naming `read` on `tool:*` is refused: tools have no
// `read`).
func (r Rule) Validate() error {
	if r.Effect != Allow && r.Effect != Deny {
		return fmt.Errorf("effect %q is not allow or deny", r.Effect)
	}
	if len(r.Actions) == 0 {
		return fmt.Errorf("no actions")
	}
	if len(r.Actions) > MaxActionsPerRule {
		return fmt.Errorf("%d actions; the limit is %d", len(r.Actions), MaxActionsPerRule)
	}
	if len(r.Resources) == 0 {
		return fmt.Errorf("no resources")
	}
	if len(r.Resources) > MaxResourcesPerRule {
		return fmt.Errorf("%d resources; the limit is %d", len(r.Resources), MaxResourcesPerRule)
	}
	if len(r.Description) > MaxDescriptionBytes {
		return fmt.Errorf("description is longer than %d bytes", MaxDescriptionBytes)
	}
	seenAction := map[string]bool{}
	for _, a := range r.Actions {
		if seenAction[a] {
			return fmt.Errorf("action %q is listed twice", a)
		}
		seenAction[a] = true
	}
	seenRes := map[string]bool{}
	for _, sel := range r.Resources {
		if seenRes[sel] {
			return fmt.Errorf("resource %q is listed twice", sel)
		}
		seenRes[sel] = true
		s, err := parseSelector(sel)
		if err != nil {
			return err
		}
		for _, a := range r.Actions {
			if !ValidVerb(s.typ, a) {
				spec := catalogByName[s.typ]
				return fmt.Errorf("action %q is not a verb of %s (one of: %s)", a, s.typ, strings.Join(spec.Verbs, ", "))
			}
		}
	}
	if r.When != nil {
		if err := r.When.Validate(); err != nil {
			return fmt.Errorf("when: %w", err)
		}
	}
	return nil
}

// SubjectKind is what a binding attaches an ACL to.
type SubjectKind string

const (
	SubjectUser   SubjectKind = "user"
	SubjectGroup  SubjectKind = "group"
	SubjectToken  SubjectKind = "token"
	SubjectTenant SubjectKind = "tenant"
)

// Binding attaches an ACL (by name; the active version resolves at
// evaluation) to one subject.
type Binding struct {
	ID          string      `json:"id,omitempty"`
	ACL         string      `json:"acl"`
	SubjectKind SubjectKind `json:"subject_kind"`
	// SubjectID is the subject, group name or token def id; empty for a
	// tenant-wide binding.
	SubjectID string    `json:"subject_id,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// MarshalJSON leaves an unset ExpiresAt out (`omitempty` does nothing for a
// time.Time, so a binding with no expiry was written as year 1) and writes a
// set one in UTC, as the ACL store rows do. Decoding is unchanged.
func (b Binding) MarshalJSON() ([]byte, error) {
	type plain Binding
	var exp *time.Time
	if !b.ExpiresAt.IsZero() {
		u := b.ExpiresAt.UTC()
		exp = &u
	}
	return json.Marshal(struct {
		plain
		ExpiresAt *time.Time `json:"expires_at,omitempty"`
	}{plain(b), exp})
}

// Validate checks a binding as a caller supplied it.
func (b Binding) Validate() error {
	if !ValidName(b.ACL) {
		return fmt.Errorf("binding: acl %q is not a valid name", b.ACL)
	}
	switch b.SubjectKind {
	case SubjectUser, SubjectToken:
		if b.SubjectID == "" {
			return fmt.Errorf("binding: a %s binding needs a subject_id", b.SubjectKind)
		}
	case SubjectGroup:
		if !ValidName(b.SubjectID) {
			return fmt.Errorf("binding: group %q is not a valid name", b.SubjectID)
		}
	case SubjectTenant:
		if b.SubjectID != "" {
			return fmt.Errorf("binding: a tenant binding takes no subject_id")
		}
	default:
		return fmt.Errorf("binding: subject_kind %q is not one of user, group, token, tenant", b.SubjectKind)
	}
	if len(b.SubjectID) > MaxSubjectIDBytes {
		return fmt.Errorf("binding: subject_id is longer than %d bytes", MaxSubjectIDBytes)
	}
	return nil
}

// Active reports whether the binding applies at now.
func (b Binding) Active(now time.Time) bool {
	return b.ExpiresAt.IsZero() || now.Before(b.ExpiresAt)
}

// AppliesTo reports whether the binding's subject is one of the principal's:
// its own subject, one of its (transitively closed) groups, its token
// definition, or the whole tenant.
func (b Binding) AppliesTo(s Subject) bool {
	switch b.SubjectKind {
	case SubjectTenant:
		return true
	case SubjectUser:
		return s.ID != "" && b.SubjectID == s.ID
	case SubjectToken:
		return s.TokenDefID != "" && b.SubjectID == s.TokenDefID
	case SubjectGroup:
		for _, g := range s.Groups {
			if g == b.SubjectID {
				return true
			}
		}
	}
	return false
}
