package authz

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// The enforcement table (RFC FY Part E): which (verb, type) pairs and which
// condition fields some surface actually evaluates. A rule that names
// anything else would be stored, shown in explain and simulated, and decide
// nothing — so the write surfaces refuse it (RuleNotEnforceable) and the
// boot reports stored rows that still name it.
//
// It is kept by hand and held to the code by census tests in both
// directions:
//   - every catalog pair and every Condition field is classified here
//     (TestACLCatalog_EveryPairIsEnforcedSomewhere);
//   - a pair the table says a transport asks is one that transport's mapping
//     really yields, and every pair the mapping yields is in the table (the
//     route census in internal/api/http, the RPC census in internal/api/grpc,
//     the meta-tool census in internal/api/mcp);
//   - a pair asked only inside the code (a tool, the run-start narrowing, the
//     resolver) names the test that drives that code and shows a rule on the
//     pair changing the outcome; the census checks the test exists.

// Site is where a question is asked.
type Site string

const (
	// SiteHTTPRoute: the HTTP route gate (HTTPSurface).
	SiteHTTPRoute Site = "http-route"
	// SiteGRPCRPC: the gRPC per-RPC gate (GRPCSurface).
	SiteGRPCRPC Site = "grpc-rpc"
	// SiteMCPTool: the /v1/_mcp per-tool gate: the meta-tool's own question
	// and its HTTP twin route's.
	SiteMCPTool Site = "mcp-tool"
	// SiteConnector: the shared operations behind the gRPC RPCs and the MCP
	// meta-tools (memory and channel calls), asking what the HTTP route asks
	// per call.
	SiteConnector Site = "connector"
)

// proofPrefix marks a Site that is a test name: the pair is asked inside the
// code, and that test drives the code and shows the rule taking effect.
const proofPrefix = "test:"

// Proof is a code site, named by the test that proves it.
func Proof(test string) Site { return Site(proofPrefix + test) }

// ProofTest returns the test a code site names, and false for a transport
// site.
func (s Site) ProofTest() (string, bool) {
	return strings.CutPrefix(string(s), proofPrefix)
}

type pairKey struct{ typ, verb string }

// enforcedPairs: every pair some site asks, and where.
var enforcedPairs = map[pairKey][]Site{
	{"agent", "run"}:   {SiteHTTPRoute, SiteGRPCRPC, SiteMCPTool},
	{"agent", "read"}:  {SiteHTTPRoute, SiteMCPTool},
	{"agent", "spawn"}: {Proof("TestACLRun_DeniedSpawnIsRefusedAtTheAgentTool")},

	{"volume", "read"}:  {SiteHTTPRoute},
	{"volume", "write"}: {Proof("TestNarrowVolumesByACL")},
	// A member's agent definition grants a volume only when the member may
	// mount it (the member capability ceiling, agentdef_ceiling_member.go).
	{"volume", "mount"}: {Proof("TestMemberCeiling_AMountDenyKeepsTheVolumeFromAMembersAgent")},

	{"tool", "invoke"}:  {Proof("TestACLRun_DeniedToolIsWithheldFromTheRun")},
	{"skill", "invoke"}: {Proof("TestNarrowSkillsByACL")},
	{"skill", "read"}:   {SiteHTTPRoute},
	{"mcp", "invoke"}:   {Proof("TestResourceForTool_RuleMatchesTheMappedName")},
	{"a2a", "invoke"}:   {Proof("TestResourceForTool_RuleMatchesTheMappedName")},

	{"mcp-tool", "invoke"}: {SiteMCPTool},

	{"memory", "read"}:  {SiteHTTPRoute, SiteGRPCRPC, SiteMCPTool},
	{"memory", "write"}: {SiteHTTPRoute, SiteConnector},
	{"sqlmem", "read"}:  {Proof("TestNarrowScopesByACL")},
	{"sqlmem", "write"}: {Proof("TestReadOnlyScopesByACL_NamesTheReadableScopesItMayNotWrite"), SiteConnector},

	{"document", "read"}:    {SiteHTTPRoute, SiteGRPCRPC, SiteMCPTool},
	{"document", "write"}:   {SiteHTTPRoute, SiteGRPCRPC, SiteMCPTool},
	{"document", "delete"}:  {Proof("TestDocumentACL_EveryOpIsMapped")},
	{"document", "link"}:    {Proof("TestDocumentACL_EveryOpIsMapped")},
	{"document", "propose"}: {Proof("TestDocumentACL_EveryOpIsMapped")},
	{"document", "review"}:  {Proof("TestReviewStatement_AMemberNeedsTheReviewGrant")},

	{"history", "read"}: {SiteHTTPRoute, SiteGRPCRPC, SiteMCPTool},

	// A member browses a colleague's scope only through an explicit allow
	// (RFC FY P0, D1(b)).
	{"memory", "browse"}:   {Proof("TestBrowse_AnExplicitAllowLetsAMemberReadAColleaguesScope")},
	{"document", "browse"}: {Proof("TestBrowse_AnExplicitAllowLetsAMemberReadAColleaguesScope")},

	{"channel", "publish"}:   {SiteHTTPRoute, SiteGRPCRPC, SiteMCPTool, SiteConnector},
	{"channel", "subscribe"}: {SiteHTTPRoute, SiteGRPCRPC, SiteMCPTool, SiteConnector},
	{"channel", "ack"}:       {SiteHTTPRoute, SiteGRPCRPC, SiteMCPTool, SiteConnector},
	{"channel", "await"}:     {SiteHTTPRoute, SiteGRPCRPC, SiteConnector},
	{"channel", "broadcast"}: {SiteHTTPRoute, SiteGRPCRPC, SiteConnector},
	{"channel", "manage"}:    {SiteHTTPRoute, SiteGRPCRPC, SiteMCPTool, SiteConnector},

	{"def", "author"}: {Proof("TestAuthorizeDefOp_AsksTheOpsVerbOfTheNamedDefinition")},
	{"def", "retire"}: {Proof("TestAuthorizeDefOp_AsksTheOpsVerbOfTheNamedDefinition")},
	{"def", "read"}:   {SiteHTTPRoute, SiteGRPCRPC, SiteMCPTool},
	{"def", "manage"}: {SiteHTTPRoute},

	{"team", "run"}:    {Proof("TestAuthorizeTeamOp_AsksTheWalkOpsVerbOfTheNamedTeam")},
	{"team", "read"}:   {Proof("TestAuthorizeTeamOp_AsksTheWalkOpsVerbOfTheNamedTeam")},
	{"team", "cancel"}: {Proof("TestAuthorizeTeamOp_AsksTheWalkOpsVerbOfTheNamedTeam")},

	{"credential", "use"}:    {SiteHTTPRoute, SiteGRPCRPC, SiteMCPTool},
	{"credential", "manage"}: {Proof("TestCredentialDefACL_ManageDenyRefusesRotateOfThatName")},

	{"crypto", "use"}: {SiteHTTPRoute, SiteGRPCRPC, SiteMCPTool},

	{"gateway", "use"}: {SiteHTTPRoute, SiteGRPCRPC, SiteMCPTool},

	{"evaluation", "read"}: {SiteHTTPRoute, SiteGRPCRPC, SiteMCPTool},

	{"run", "read"}:    {SiteHTTPRoute, SiteGRPCRPC, SiteMCPTool},
	{"run", "delete"}:  {SiteHTTPRoute, SiteGRPCRPC, SiteMCPTool},
	{"run", "steer"}:   {SiteHTTPRoute, SiteGRPCRPC, SiteMCPTool},
	{"run", "cancel"}:  {SiteHTTPRoute, SiteGRPCRPC, SiteMCPTool},
	{"run", "compact"}: {SiteHTTPRoute, SiteGRPCRPC, SiteMCPTool},
	{"run", "retune"}:  {SiteHTTPRoute, SiteGRPCRPC, SiteMCPTool},
	{"run", "review"}:  {SiteHTTPRoute, SiteGRPCRPC, SiteMCPTool},

	{"principal", "read"}:   {SiteHTTPRoute, SiteGRPCRPC, SiteMCPTool},
	{"principal", "manage"}: {SiteHTTPRoute, Proof("TestOperatorTokenDefACL_AsksTheOpsVerbOfTheTokenKind")},
	{"principal", "mint"}:   {SiteHTTPRoute, Proof("TestOperatorTokenDefACL_AsksTheOpsVerbOfTheTokenKind")},

	{"runtime", "read"}:   {SiteHTTPRoute, SiteGRPCRPC, SiteMCPTool},
	{"runtime", "manage"}: {SiteHTTPRoute, SiteGRPCRPC, SiteMCPTool, SiteConnector},

	{"transport", "use"}: {SiteHTTPRoute},

	{"model", "use"}: {Proof("TestResolveAgentDef_AsksTheCallersUseOfTheTierAndTheModel"), Proof("TestModelUse_EveryInferencePathAsks")},
	{"tier", "use"}:  {Proof("TestResolveAgentDef_AsksTheCallersUseOfTheTierAndTheModel")},
	{"git", "fetch"}: {Proof("TestGit_ACLDenyOnTheRemoteRefusesBeforeAnyRequest")},
	{"git", "push"}:  {Proof("TestGit_ACLDenyOnTheRemoteRefusesBeforeAnyRequest")},
}

// typeLevelOnlyPairs: enforced pairs every site asks only at the type level
// (Surface.Name == AnyInstance) — no handler, tool or resolver asks one about
// a named instance. A rule whose selector names some instances of such a pair
// decides nothing as written: at the type level a deny on some instances is no
// deny, and an allow on some instances admits every instance (engine.go,
// compiledRule.matches). So a write refuses it (RFC GD D2) and the boot reports
// a stored one. The reason says what does confine the pair instead.
//
// A pair belongs here exactly when its sites are all transport sites and every
// mapping of it asks AnyInstance; the transport censuses hold the second half
// (no route, RPC or meta-tool asks one at an instance), and
// TestTypeLevelOnlyPairs_AreAskedOnlyAtTransports the first.
var typeLevelOnlyPairs = map[pairKey]string{
	{"run", "read"}:        runByOwner,
	{"run", "delete"}:      runByOwner,
	{"run", "steer"}:       runByOwner,
	{"run", "cancel"}:      runByOwner,
	{"run", "compact"}:     runByOwner,
	{"run", "retune"}:      runByOwner,
	{"run", "review"}:      runByOwner,
	{"agent", "read"}:      "the agent library is read whole; deny run agent:<name> to keep an agent from a caller",
	{"skill", "read"}:      "the skill library is read whole; deny invoke skill:<name> to keep a skill from a caller",
	{"evaluation", "read"}: "evaluations are read whole; the token's evaluation scopes decide whose",
}

// runByOwner is why the run verbs take no instance selector: which runs a
// caller reaches is its tier's (a member its own runs, auth.OwnedRowVisible),
// never a rule's.
const runByOwner = "which runs a caller reaches is decided by who started them, not by a rule"

// unenforcedPairs: catalog pairs no surface asks yet. A rule naming one is
// refused at write; the reason says what the pair is waiting for. Moving a
// pair to enforcedPairs is the change that wires its site.
var unenforcedPairs = map[pairKey]string{
	{"agent", "steer"}:      "a run is steered as run:, never agent:",
	{"agent", "cancel"}:     "a run is cancelled as run:, never agent:",
	{"agent", "review"}:     "a run's answer is reviewed as run:, never agent:",
	{"volume", "provision"}: "a volume is created as def:volume/",
	{"skill", "author"}:     "a skill is authored as def:skill/",
	{"mcp", "read"}:         "a server's definition is read as def:mcpserver/",
	{"a2a", "expose"}:       "",
	{"a2a", "author"}:       "a peer is authored as def:a2a/",
	{"client", "invoke"}:    "a client tool is asked as tool:",
	{"localapi", "invoke"}:  "a LocalAPI tool is asked as tool:",
	{"memory", "recall"}:    "recall is asked as read",
	{"team", "author"}:      "a team is authored as def:team/",
	{"crypto", "manage"}:    "",
}

// retiredPairs: removed from the catalog. They stay parseable so a list
// stored before the removal still loads; every write refuses them, the
// catalog listing leaves them out, and the boot names the stored rows that
// still carry them.
//
// Removed by owner decision (RFC FY D5): no tool had an asked-for use for
// them.
var retiredPairs = map[pairKey]bool{
	{"search", "use"}:           true,
	{"evaluation", "submit"}:    true,
	{"interruption", "raise"}:   true,
	{"interruption", "resolve"}: true,
}

// enforcedConditions: the Condition fields (by JSON name) some surface fills
// in the Context it decides with, and the test that shows it.
var enforcedConditions = map[string]Site{
	"transport":     Proof("TestEvaluate_TheGateSuppliesTheClockAndTheTransport"),
	"time":          Proof("TestEvaluate_TheGateSuppliesTheClockAndTheTransport"),
	"owner_is_self": Proof("TestDocumentACL_ConditionalDenyFallsBackToEachDocument"),
	"source_cidr":   Proof("TestACLGate_ASourceCIDRDenyAppliesAtRunIntake"),
	"interactive":   Proof("TestACLGate_AnInteractiveDenyAppliesAtRunIntake"),
	"agent":         Proof("TestACLGate_AnAgentConditionReadsTheRunsAgent"),
	"depth":         Proof("TestACLGate_ADepthDenyReadsTheSpawnDepth"),
}

// unenforcedConditions: Condition fields no surface can fill with a fact the
// caller cannot choose; a rule using one is refused at write, and a stored
// one reads the field as unknown (a deny holds, an allow does not).
//
// A stored rule carrying one is inert at evaluation (compiledCondition.inert):
// skipped whole, as it was before writes refused it.
var unenforcedConditions = map[string]string{
	"tier":      "a run request names its own tier",
	"user_tier": "a run request names its own user tier, and nothing binds one to the principal",
}

// stampedTransports are the transport values a surface stamps (authz
// WithTransport callers): a `transport` condition naming any other value —
// `bridge`, `bridge:<name>` — can never match, so it is refused at write.
var stampedTransports = map[string]bool{
	"http": true, "webui": true, "mcp": true, "grpc": true, "a2a": true, "webhook": true, "schedule": true,
}

// IsTransportStamped reports whether some surface stamps transport value t.
func IsTransportStamped(t string) bool { return stampedTransports[t] }

// IsEnforced reports whether some surface asks verb of typ.
func IsEnforced(typ, verb string) bool {
	_, ok := enforcedPairs[pairKey{typ, verb}]
	return ok
}

// IsTypeLevelOnly reports whether every site asks verb of typ only at the type
// level, so a selector naming some instances of it decides nothing.
func IsTypeLevelOnly(typ, verb string) bool {
	_, ok := typeLevelOnlyPairs[pairKey{typ, verb}]
	return ok
}

// TypeLevelOnlyVerbs returns typ's type-level-only verbs in catalog order, for
// the catalog listing a rule picker is built from.
func TypeLevelOnlyVerbs(typ string) []string {
	var out []string
	for _, v := range EnforcedVerbs(typ) {
		if IsTypeLevelOnly(typ, v) {
			out = append(out, v)
		}
	}
	return out
}

// TypeLevelOnlyPairs returns every type-level-only pair as "verb type",
// sorted, for the transport censuses.
func TypeLevelOnlyPairs() []string {
	out := make([]string, 0, len(typeLevelOnlyPairs))
	for k := range typeLevelOnlyPairs {
		out = append(out, k.verb+" "+k.typ)
	}
	sort.Strings(out)
	return out
}

// coversEveryInstance reports whether sel (`type:name`, validated) names every
// instance of its type — the only selector the type-level question matches.
func coversEveryInstance(sel string) bool {
	s, err := parseSelector(sel)
	if err != nil {
		return false
	}
	return s.matches(catalogByName[s.typ], AnyInstance)
}

// SelectorDecidesNothing reports whether a rule's selector sel names some
// instances of a type-level-only pair (verb of sel's type), where it decides
// nothing as written.
func SelectorDecidesNothing(verb, sel string) bool {
	typ, _, err := SplitSelector(sel)
	return err == nil && IsTypeLevelOnly(typ, verb) && !coversEveryInstance(sel)
}

// QuestionNotEnforceable is the refusal of a question no surface asks: a pair
// no surface evaluates, or a named instance of a type-level-only pair. An
// evaluation, an explanation or a `Context op=can` asked so is refused as a
// rule naming it is. name is the resource name ("" or AnyInstance for the
// type-level question).
func QuestionNotEnforceable(typ, verb, name string) error {
	if !IsEnforced(typ, verb) {
		reason := unenforcedPairs[pairKey{typ, verb}]
		if IsRetired(typ, verb) {
			reason = "removed from the catalog"
		}
		return &NotEnforceableError{Verb: verb, Type: typ, Reason: reason}
	}
	if name != "" && name != AnyInstance && IsTypeLevelOnly(typ, verb) {
		return &NotEnforceableError{Verb: verb, Type: typ, Selector: typ + ":" + name, Reason: typeLevelOnlyPairs[pairKey{typ, verb}]}
	}
	return nil
}

// IsRetired reports whether verb on typ was removed from the catalog.
func IsRetired(typ, verb string) bool { return retiredPairs[pairKey{typ, verb}] }

// EnforcedVerbs returns typ's verbs some surface asks, in catalog order.
func EnforcedVerbs(typ string) []string {
	spec, ok := catalogByName[typ]
	if !ok {
		return nil
	}
	var out []string
	for _, v := range spec.Verbs {
		if IsEnforced(typ, v) {
			out = append(out, v)
		}
	}
	return out
}

// conditionFieldsSet returns the JSON names of c's set fields.
func conditionFieldsSet(c *Condition) []string {
	if c == nil {
		return nil
	}
	var out []string
	v := reflect.ValueOf(*c)
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		if !v.Field(i).IsZero() {
			out = append(out, conditionJSONName(t.Field(i)))
		}
	}
	return out
}

func conditionJSONName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	return name
}

// NotEnforceableError is a rule naming a pair or a condition field no
// surface evaluates.
type NotEnforceableError struct {
	// Verb and Type name the pair; Condition is set instead for a field.
	Verb, Type, Condition string
	// Selector is set when the pair is enforced but only at the type level,
	// and the rule or question names some instances of it.
	Selector string
	Reason   string
}

func (e *NotEnforceableError) Error() string {
	if e.Selector != "" {
		msg := fmt.Sprintf("%s %s: no surface asks %s of one %s, only of every one; name %s:* or leave the rule out", e.Verb, e.Selector, e.Verb, e.Type, e.Type)
		if e.Reason != "" {
			msg += " (" + e.Reason + ")"
		}
		return msg
	}
	if e.Condition != "" {
		msg := fmt.Sprintf("condition %q is not evaluated by any surface", e.Condition)
		r := e.Reason
		if r == "" {
			r = unenforcedConditions[e.Condition]
		}
		if r != "" {
			msg += " (" + r + ")"
		}
		return msg
	}
	msg := fmt.Sprintf("%s %s is not evaluated by any surface", e.Verb, e.Type)
	if e.Reason != "" {
		msg += " (" + e.Reason + ")"
	}
	return msg
}

// RuleNotEnforceable returns the first pair or condition field of r that no
// surface evaluates, as a *NotEnforceableError, or nil. r must have passed
// Validate.
func RuleNotEnforceable(r Rule) error {
	for _, sel := range r.Resources {
		typ, _, err := SplitSelector(sel)
		if err != nil {
			continue // Validate reports it
		}
		for _, verb := range r.Actions {
			if !IsEnforced(typ, verb) {
				return QuestionNotEnforceable(typ, verb, AnyInstance)
			}
			if SelectorDecidesNothing(verb, sel) {
				return &NotEnforceableError{Verb: verb, Type: typ, Selector: sel, Reason: typeLevelOnlyPairs[pairKey{typ, verb}]}
			}
		}
	}
	for _, f := range conditionFieldsSet(r.When) {
		if _, ok := enforcedConditions[f]; !ok {
			return &NotEnforceableError{Condition: f}
		}
	}
	if r.When != nil {
		for _, t := range r.When.Transport {
			if !IsTransportStamped(t) {
				return &NotEnforceableError{Condition: "transport", Reason: fmt.Sprintf("no surface stamps the transport %q", t)}
			}
		}
	}
	return nil
}

// RulesNotEnforceable is RuleNotEnforceable over a list, the error naming
// the rule's index.
func RulesNotEnforceable(rules []Rule) error {
	for i, r := range rules {
		if err := RuleNotEnforceable(r); err != nil {
			return fmt.Errorf("rule %d: %w", i, err)
		}
	}
	return nil
}

// EnforcementPairs returns every enforced pair as "verb type" with its
// sites, sorted, for the census tests.
func EnforcementPairs() map[string][]Site {
	out := make(map[string][]Site, len(enforcedPairs))
	for k, sites := range enforcedPairs {
		out[k.verb+" "+k.typ] = append([]Site(nil), sites...)
	}
	return out
}

// PairsAskedAt returns the "verb type" pairs the table says site asks,
// sorted.
func PairsAskedAt(site Site) []string {
	var out []string
	for k, sites := range enforcedPairs {
		for _, s := range sites {
			if s == site {
				out = append(out, k.verb+" "+k.typ)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// ConditionProofs returns each enforced condition field's site.
func ConditionProofs() map[string]Site {
	out := make(map[string]Site, len(enforcedConditions))
	for k, v := range enforcedConditions {
		out[k] = v
	}
	return out
}

// IsConditionEnforced reports whether some surface fills the Condition field
// named by its JSON name.
func IsConditionEnforced(field string) bool {
	_, ok := enforcedConditions[field]
	return ok
}
