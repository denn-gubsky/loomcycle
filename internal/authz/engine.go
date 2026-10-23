package authz

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// Subject is the principal a decision is about: its tenant, its subject id,
// its transitively closed group memberships (see Closure) and its token
// definition. Every field is server-derived (RFC EY §10 invariant 3).
type Subject struct {
	TenantID   string
	ID         string
	Groups     []string
	TokenDefID string
	// Class is the token class the set's baseline was chosen for
	// (aclres.ClassKey), set by the resolver that compiled it. It takes no
	// part in a decision; it lets a run that outlives its request — paused,
	// restored, restarted — have its caller's set compiled again.
	Class string
}

// Resource is the instance a decision is about. Name is in the type's
// selector shape (`chat/support`, `tenant:/kb/x`, `agent/chat/x`, `own`).
// TenantID is the tenant the resource belongs to ("" for an operator-global
// or shared resource, which any tenant may be allowed to read); Owner is the
// subject that owns it, for the owner_is_self condition.
type Resource struct {
	Type     string
	Name     string
	TenantID string
	Owner    string
}

// Context is what the surface knows about the request that a condition may
// consult. Now is required for time windows and binding expiry; a zero Now
// makes every time window false.
type Context struct {
	Transport   string
	Now         time.Time
	Interactive bool
	Tier        string
	UserTier    string
	SourceIP    net.IP
	Agent       string
	Depth       int
}

// Request is one question.
type Request struct {
	Subject  Subject
	Action   string
	Resource Resource
	Context  Context
}

// Outcome is the engine's answer before any ceiling.
type Outcome int

const (
	// NoMatch: no rule matched. Enforce mode denies; compat mode falls back
	// to the legacy scope gate.
	NoMatch Outcome = iota
	// Denied: at least one deny matched (or the tenant floor refused).
	Denied
	// Allowed: at least one allow matched and no deny did.
	Allowed
)

func (o Outcome) String() string {
	switch o {
	case Denied:
		return "deny"
	case Allowed:
		return "allow"
	}
	return "no_match"
}

// Reason names one matching rule.
type Reason struct {
	ACL       string `json:"acl"`
	Version   int    `json:"version,omitempty"`
	RuleIndex int    `json:"rule"`
	Effect    Effect `json:"effect"`
}

// Decision is the engine's answer. Reasons lists every matching rule, denies
// first, so Explain and the audit row say which rules decided. TenantFloor
// is set when the request crossed a tenant and no rule was read.
type Decision struct {
	Outcome     Outcome
	Reasons     []Reason
	TenantFloor bool
}

// Allow reports whether the outcome is Allowed.
func (d Decision) Allow() bool { return d.Outcome == Allowed }

// ACLVersion is an ACL with the version the store resolved for its name.
type ACLVersion struct {
	ACL     ACL
	Version int
}

// Input is what Compile needs: the principal, every binding that applies to
// it (the caller may pass more; Compile filters by AppliesTo and expiry),
// the active version of every ACL a binding names, and the clock.
type Input struct {
	Subject  Subject
	Bindings []Binding
	ACLs     map[string]ACLVersion
	Now      time.Time
	// Version is the tenant's acl_version the inputs were read at; the
	// cache keys on it.
	Version int64
}

// compiledRule is one rule of one ACL, ready to match.
type compiledRule struct {
	acl     string
	version int
	index   int
	effect  Effect
	actions map[string]bool
	sels    []selector
	when    *compiledCondition
}

// typeRules is the compiled rules that can touch one resource type, split
// by effect, plus a memo of exact-name literals for the condition-free
// rules so the hot path is a map read.
type typeRules struct {
	deny  []*compiledRule
	allow []*compiledRule
	// literalDeny / literalAllow: action → literal name → rules whose only
	// selector for this type is that literal and that carry no condition.
	literalDeny  map[string]map[string][]*compiledRule
	literalAllow map[string]map[string][]*compiledRule
}

// Effective is a principal's compiled rule set. It is immutable once built
// and safe for concurrent use; the surfaces cache one per (tenant, subject,
// acl_version) and stamp it on a run (RFC EY §3.7).
type Effective struct {
	Subject Subject
	Version int64
	byType  map[string]*typeRules
	// applied is the bindings that contributed, for Explain.
	applied []Binding
	// missing is the ACL names bound but not supplied (retired or unknown),
	// which match nothing and which the doctor should name.
	missing []string
}

// Compile builds a principal's Effective set from its bindings and the ACLs
// they name. A binding to an ACL that is absent from in.ACLs matches
// nothing and is reported in Missing: a retired list silently granting is
// the failure this refuses.
func Compile(in Input) (*Effective, error) {
	e := &Effective{Subject: in.Subject, Version: in.Version, byType: map[string]*typeRules{}}
	seenACL := map[string]bool{}
	for _, b := range in.Bindings {
		if !b.AppliesTo(in.Subject) || !b.Active(in.Now) {
			continue
		}
		e.applied = append(e.applied, b)
		if seenACL[b.ACL] {
			continue // the same list bound twice contributes once
		}
		seenACL[b.ACL] = true
		av, ok := in.ACLs[b.ACL]
		if !ok {
			e.missing = append(e.missing, b.ACL)
			continue
		}
		if err := av.ACL.Validate(); err != nil {
			// A stored list that no longer validates (a dropped catalog
			// type) is refused rather than partially applied.
			return nil, fmt.Errorf("acl %s (version %d): %w", b.ACL, av.Version, err)
		}
		for i, r := range av.ACL.Rules {
			cr := &compiledRule{acl: av.ACL.Name, version: av.Version, index: i, effect: r.Effect,
				actions: make(map[string]bool, len(r.Actions)), when: compileCondition(r.When)}
			for _, a := range r.Actions {
				cr.actions[a] = true
			}
			perType := map[string][]selector{}
			for _, sel := range r.Resources {
				s, err := parseSelector(sel)
				if err != nil {
					return nil, fmt.Errorf("acl %s rule %d: %w", b.ACL, i, err)
				}
				perType[s.typ] = append(perType[s.typ], s)
			}
			for typ, sels := range perType {
				tr := e.byType[typ]
				if tr == nil {
					tr = &typeRules{literalDeny: map[string]map[string][]*compiledRule{}, literalAllow: map[string]map[string][]*compiledRule{}}
					e.byType[typ] = tr
				}
				// One compiled rule per type it touches, carrying only that
				// type's selectors, so a rule over several types matches
				// each independently.
				trc := &compiledRule{acl: cr.acl, version: cr.version, index: cr.index, effect: cr.effect, actions: cr.actions, sels: sels, when: cr.when}
				if cr.effect == Deny {
					tr.deny = append(tr.deny, trc)
				} else {
					tr.allow = append(tr.allow, trc)
				}
				if trc.when == nil && len(sels) == 1 && sels[0].literal != "" {
					memo := tr.literalAllow
					if cr.effect == Deny {
						memo = tr.literalDeny
					}
					for a := range cr.actions {
						if memo[a] == nil {
							memo[a] = map[string][]*compiledRule{}
						}
						memo[a][sels[0].literal] = append(memo[a][sels[0].literal], trc)
					}
				}
			}
		}
	}
	sort.Strings(e.missing)
	return e, nil
}

// Missing returns the ACL names bound to the subject but not resolvable.
func (e *Effective) Missing() []string { return append([]string(nil), e.missing...) }

// Applied returns the bindings that contributed to the set.
func (e *Effective) Applied() []Binding { return append([]Binding(nil), e.applied...) }

// Decide answers one request against the compiled set.
//
// Order of checks (RFC EY §3.4): the tenant floor, then every deny, then
// every allow. Reasons carry every matching rule, denies first. Decide never
// reads the request's subject beyond the owner_is_self condition: the set
// was compiled for one subject and the caller must not reuse it for another
// (Effective.Subject says whose it is).
func (e *Effective) Decide(req Request) Decision {
	if req.Resource.TenantID != "" && req.Resource.TenantID != e.Subject.TenantID {
		return Decision{Outcome: Denied, TenantFloor: true}
	}
	tr := e.byType[req.Resource.Type]
	if tr == nil {
		return Decision{Outcome: NoMatch}
	}
	spec, ok := catalogByName[req.Resource.Type]
	if !ok {
		return Decision{Outcome: NoMatch}
	}
	var reasons []Reason
	denied := false
	for _, r := range tr.deny {
		if r.matches(spec, &req) {
			denied = true
			reasons = append(reasons, Reason{ACL: r.acl, Version: r.version, RuleIndex: r.index, Effect: Deny})
		}
	}
	allowed := false
	for _, r := range tr.allow {
		if r.matches(spec, &req) {
			allowed = true
			reasons = append(reasons, Reason{ACL: r.acl, Version: r.version, RuleIndex: r.index, Effect: Allow})
		}
	}
	switch {
	case denied:
		return Decision{Outcome: Denied, Reasons: reasons}
	case allowed:
		return Decision{Outcome: Allowed, Reasons: reasons}
	}
	return Decision{Outcome: NoMatch}
}

// DenyPatterns returns the names of every UNCONDITIONAL deny selector on
// typ for action, as written (`support/**`, `Bash`), for a consumer that
// enforces with its own pattern list — the `skills:` allowlist takes `-glob`
// entries in the same `/`-segmented syntax. A conditional deny cannot be
// expressed as a pattern and is left to the per-call check.
func (e *Effective) DenyPatterns(typ, action string) []string {
	tr := e.byType[typ]
	if tr == nil {
		return nil
	}
	var out []string
	for _, r := range tr.deny {
		if r.when != nil || !r.actions[action] {
			continue
		}
		for _, s := range r.sels {
			out = append(out, s.raw)
		}
	}
	return out
}

// Quick is the hot-path form of Decide for a condition-free question about
// an exact name (a tool call, a volume binding): a map read when every rule
// that could match is a literal, else a full Decide. It returns the same
// outcome Decide would.
func (e *Effective) Quick(typ, action, name string) Outcome {
	tr := e.byType[typ]
	if tr == nil {
		return NoMatch
	}
	// Any glob or conditional rule on this type means the memo is not
	// authoritative; fall back.
	for _, r := range tr.deny {
		if r.when != nil || len(r.sels) != 1 || r.sels[0].literal == "" {
			return e.Decide(Request{Subject: e.Subject, Action: action, Resource: Resource{Type: typ, Name: name}}).Outcome
		}
	}
	for _, r := range tr.allow {
		if r.when != nil || len(r.sels) != 1 || r.sels[0].literal == "" {
			return e.Decide(Request{Subject: e.Subject, Action: action, Resource: Resource{Type: typ, Name: name}}).Outcome
		}
	}
	if len(tr.literalDeny[action][name]) > 0 {
		return Denied
	}
	if len(tr.literalAllow[action][name]) > 0 {
		return Allowed
	}
	return NoMatch
}

// matches reports whether the rule applies to the request. At the type
// level (Name == AnyInstance) the two effects read differently, on purpose:
// an ALLOW matches when it names any instance of the type ("the principal
// may do this on something, so the route is reachable; the instance check
// decides which"), a DENY only when it covers every instance ("nothing of
// this type is permitted"). A deny on some instances is therefore neither at
// the type level, and the handler's instance question settles it.
func (r *compiledRule) matches(spec TypeSpec, req *Request) bool {
	if !r.actions[req.Action] {
		return false
	}
	hit := false
	if req.Resource.Name == AnyInstance && r.effect == Allow {
		hit = len(r.sels) > 0
	} else {
		for _, s := range r.sels {
			if s.matches(spec, req.Resource.Name) {
				hit = true
				break
			}
		}
	}
	if !hit {
		return false
	}
	return r.when.holds(req)
}

// Semantic is the batch evaluation mode, AuthZEN 1.0's
// `options.evaluations_semantic`.
type Semantic string

const (
	ExecuteAll         Semantic = "execute_all"
	DenyOnFirstDeny    Semantic = "deny_on_first_deny"
	PermitOnFirstAllow Semantic = "permit_on_first_permit"
)

// DecideAll answers several requests. Under DenyOnFirstDeny it stops at the
// first non-allow (NoMatch counts as not allowed); under PermitOnFirstAllow
// at the first allow. Unevaluated entries are returned as NoMatch with no
// reasons, in request order.
func (e *Effective) DecideAll(reqs []Request, sem Semantic) []Decision {
	out := make([]Decision, len(reqs))
	for i, req := range reqs {
		out[i] = e.Decide(req)
		switch sem {
		case DenyOnFirstDeny:
			if out[i].Outcome != Allowed {
				return out
			}
		case PermitOnFirstAllow:
			if out[i].Outcome == Allowed {
				return out
			}
		}
	}
	return out
}

// Filter is the resource-search form (AuthZEN search/resource): the subset
// of candidates the subject is allowed the action on, in input order. A
// NoMatch candidate is kept only when fallback says so (compat mode passes
// the legacy answer; enforce mode passes false).
func (e *Effective) Filter(action string, candidates []Resource, ctx Context, fallback func(Resource) bool) []Resource {
	var out []Resource
	for _, r := range candidates {
		d := e.Decide(Request{Subject: e.Subject, Action: action, Resource: r, Context: ctx})
		switch d.Outcome {
		case Allowed:
			out = append(out, r)
		case NoMatch:
			if fallback != nil && fallback(r) {
				out = append(out, r)
			}
		}
	}
	return out
}

// Trace is one rule's part in an Explain: whether it matched and, if not,
// why not.
type Trace struct {
	ACL       string `json:"acl"`
	Version   int    `json:"version,omitempty"`
	RuleIndex int    `json:"rule"`
	Effect    Effect `json:"effect"`
	Matched   bool   `json:"matched"`
	Why       string `json:"why"`
}

// Explanation is Decide plus the full trace over every rule that names the
// resource's type, including the ones that did not match and why.
type Explanation struct {
	Decision Decision  `json:"decision"`
	Traces   []Trace   `json:"traces"`
	Missing  []string  `json:"missing_acls,omitempty"`
	Applied  []Binding `json:"applied_bindings"`
}

// Explain answers a request and says why, rule by rule. It is for the
// operator console and the `acl explain` command; it is never on a hot path.
func (e *Effective) Explain(req Request) Explanation {
	ex := Explanation{Decision: e.Decide(req), Missing: e.Missing(), Applied: e.Applied()}
	if ex.Decision.TenantFloor {
		return ex
	}
	tr := e.byType[req.Resource.Type]
	spec := catalogByName[req.Resource.Type]
	if tr == nil {
		return ex
	}
	trace := func(r *compiledRule) {
		t := Trace{ACL: r.acl, Version: r.version, RuleIndex: r.index, Effect: r.effect}
		switch {
		case !r.actions[req.Action]:
			t.Why = "action not listed"
		case !(req.Resource.Name == AnyInstance && r.effect == Allow) && !selectorsHit(r.sels, spec, req.Resource.Name):
			t.Why = "no selector matches the resource"
		case !r.when.holds(&req):
			t.Why = "condition does not hold"
		default:
			t.Matched = true
			t.Why = "matched"
		}
		ex.Traces = append(ex.Traces, t)
	}
	for _, r := range tr.deny {
		trace(r)
	}
	for _, r := range tr.allow {
		trace(r)
	}
	return ex
}

func selectorsHit(sels []selector, spec TypeSpec, name string) bool {
	for _, s := range sels {
		if s.matches(spec, name) {
			return true
		}
	}
	return false
}

// Mode is the deployment-wide (or per-tenant) rollout mode (RFC EY §12).
type Mode string

const (
	// ModeCompat: the legacy scope gate decides; an explicit deny is
	// honoured on top of it. The default (owner decision D2).
	ModeCompat Mode = "compat"
	// ModeShadow: the legacy gate decides; the engine's answer is computed
	// and every divergence is audited.
	ModeShadow Mode = "shadow"
	// ModeEnforce: the engine decides; NoMatch is deny.
	ModeEnforce Mode = "enforce"
)

// ParseMode reads a mode from configuration. Empty is compat.
func ParseMode(s string) (Mode, error) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "", "compat":
		return ModeCompat, nil
	case "shadow":
		return ModeShadow, nil
	case "enforce":
		return ModeEnforce, nil
	}
	return "", fmt.Errorf("LOOMCYCLE_ACL: %q is not one of compat, shadow, enforce", s)
}

// Resolve folds the engine's outcome and the legacy answer into the final
// yes/no under a mode. legacy is what the scope gate says today.
//
//	compat:  deny if the engine denied; else legacy.
//	shadow:  legacy (the caller audits a divergence when engine != legacy).
//	enforce: the engine; NoMatch is deny.
func Resolve(mode Mode, d Decision, legacy bool) bool {
	switch mode {
	case ModeEnforce:
		return d.Outcome == Allowed
	case ModeShadow:
		return legacy
	default:
		if d.Outcome == Denied {
			return false
		}
		return legacy
	}
}

// Diverges reports whether the engine's answer differs from the legacy one
// in a way shadow mode should record: an engine deny where legacy allowed,
// or an engine allow where legacy denied. NoMatch is never a divergence (the
// engine has nothing to say).
func Diverges(d Decision, legacy bool) bool {
	switch d.Outcome {
	case Denied:
		return legacy
	case Allowed:
		return !legacy
	}
	return false
}
