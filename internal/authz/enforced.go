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
	// SiteMCPTool: the /v1/_mcp per-tool gate (MCPToolSurfaces).
	SiteMCPTool Site = "mcp-tool"
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
	{"agent", "run"}:   {SiteHTTPRoute, SiteGRPCRPC},
	{"agent", "read"}:  {SiteHTTPRoute},
	{"agent", "spawn"}: {Proof("TestACLRun_DeniedSpawnIsRefusedAtTheAgentTool")},

	{"volume", "read"}:  {SiteHTTPRoute},
	{"volume", "write"}: {Proof("TestNarrowVolumesByACL")},

	{"tool", "invoke"}:  {Proof("TestACLRun_DeniedToolIsWithheldFromTheRun")},
	{"skill", "invoke"}: {Proof("TestNarrowSkillsByACL")},
	{"skill", "read"}:   {SiteHTTPRoute},
	{"mcp", "invoke"}:   {Proof("TestResourceForTool_RuleMatchesTheMappedName")},
	{"a2a", "invoke"}:   {Proof("TestResourceForTool_RuleMatchesTheMappedName")},

	{"mcp-tool", "invoke"}: {SiteMCPTool},

	{"memory", "read"}:  {SiteHTTPRoute, SiteGRPCRPC},
	{"memory", "write"}: {SiteHTTPRoute},
	{"sqlmem", "read"}:  {Proof("TestNarrowScopesByACL")},
	{"sqlmem", "write"}: {Proof("TestReadOnlyScopesByACL_NamesTheReadableScopesItMayNotWrite")},

	{"document", "read"}:    {SiteHTTPRoute, SiteGRPCRPC},
	{"document", "write"}:   {SiteHTTPRoute, SiteGRPCRPC},
	{"document", "delete"}:  {Proof("TestDocumentACL_EveryOpIsMapped")},
	{"document", "link"}:    {Proof("TestDocumentACL_EveryOpIsMapped")},
	{"document", "propose"}: {Proof("TestDocumentACL_EveryOpIsMapped")},
	{"document", "review"}:  {Proof("TestReviewStatement_AMemberNeedsTheReviewGrant")},

	{"history", "read"}: {SiteHTTPRoute, SiteGRPCRPC},

	{"channel", "publish"}:   {SiteHTTPRoute, SiteGRPCRPC},
	{"channel", "subscribe"}: {SiteHTTPRoute, SiteGRPCRPC},
	{"channel", "ack"}:       {SiteHTTPRoute, SiteGRPCRPC},
	{"channel", "await"}:     {SiteHTTPRoute, SiteGRPCRPC},
	{"channel", "broadcast"}: {SiteHTTPRoute, SiteGRPCRPC},
	{"channel", "manage"}:    {SiteHTTPRoute, SiteGRPCRPC},

	{"def", "author"}: {Proof("TestAuthorizeDefOp_AsksTheOpsVerbOfTheNamedDefinition")},
	{"def", "retire"}: {Proof("TestAuthorizeDefOp_AsksTheOpsVerbOfTheNamedDefinition")},
	{"def", "read"}:   {SiteHTTPRoute, SiteGRPCRPC},
	{"def", "manage"}: {SiteHTTPRoute},

	{"team", "run"}:    {Proof("TestAuthorizeTeamOp_AsksTheWalkOpsVerbOfTheNamedTeam")},
	{"team", "read"}:   {Proof("TestAuthorizeTeamOp_AsksTheWalkOpsVerbOfTheNamedTeam")},
	{"team", "cancel"}: {Proof("TestAuthorizeTeamOp_AsksTheWalkOpsVerbOfTheNamedTeam")},

	{"credential", "use"}:    {SiteHTTPRoute, SiteGRPCRPC},
	{"credential", "manage"}: {Proof("TestCredentialDefACL_ManageDenyRefusesRotateOfThatName")},

	{"crypto", "use"}: {SiteHTTPRoute, SiteGRPCRPC},

	{"gateway", "use"}: {SiteHTTPRoute, SiteGRPCRPC},

	{"evaluation", "read"}: {SiteHTTPRoute, SiteGRPCRPC},

	{"run", "read"}:    {SiteHTTPRoute, SiteGRPCRPC},
	{"run", "delete"}:  {SiteHTTPRoute, SiteGRPCRPC},
	{"run", "steer"}:   {SiteHTTPRoute, SiteGRPCRPC},
	{"run", "cancel"}:  {SiteHTTPRoute, SiteGRPCRPC},
	{"run", "compact"}: {SiteHTTPRoute, SiteGRPCRPC},
	{"run", "retune"}:  {SiteHTTPRoute, SiteGRPCRPC},
	{"run", "review"}:  {SiteHTTPRoute, SiteGRPCRPC},

	{"principal", "read"}:   {SiteHTTPRoute, SiteGRPCRPC},
	{"principal", "manage"}: {SiteHTTPRoute},
	{"principal", "mint"}:   {SiteHTTPRoute, SiteGRPCRPC},

	{"runtime", "read"}:   {SiteHTTPRoute, SiteGRPCRPC},
	{"runtime", "manage"}: {SiteHTTPRoute, SiteGRPCRPC},

	{"transport", "use"}: {SiteHTTPRoute},
}

// unenforcedPairs: catalog pairs no surface asks yet. A rule naming one is
// refused at write; the reason says what the pair is waiting for. Moving a
// pair to enforcedPairs is the change that wires its site.
var unenforcedPairs = map[pairKey]string{
	{"agent", "steer"}:          "a run is steered as run:, never agent:",
	{"agent", "cancel"}:         "a run is cancelled as run:, never agent:",
	{"agent", "review"}:         "a run's answer is reviewed as run:, never agent:",
	{"volume", "provision"}:     "a volume is created as def:volume/",
	{"volume", "mount"}:         "a volume binding is asked read and write",
	{"skill", "author"}:         "a skill is authored as def:skill/",
	{"mcp", "read"}:             "a server's definition is read as def:mcpserver/",
	{"a2a", "expose"}:           "",
	{"a2a", "author"}:           "a peer is authored as def:a2a/",
	{"client", "invoke"}:        "a client tool is asked as tool:",
	{"localapi", "invoke"}:      "a LocalAPI tool is asked as tool:",
	{"memory", "recall"}:        "recall is asked as read",
	{"memory", "browse"}:        "",
	{"document", "browse"}:      "",
	{"team", "author"}:          "a team is authored as def:team/",
	{"crypto", "manage"}:        "",
	{"model", "use"}:            "",
	{"tier", "use"}:             "",
	{"git", "fetch"}:            "",
	{"git", "push"}:             "",
	{"search", "use"}:           "",
	{"evaluation", "submit"}:    "",
	{"interruption", "raise"}:   "",
	{"interruption", "resolve"}: "",
}

// retiredPairs: removed from the catalog. They stay parseable so a list
// stored before the removal still loads; every write refuses them, the
// catalog listing leaves them out, and the boot names the stored rows that
// still carry them.
var retiredPairs = map[pairKey]bool{}

// enforcedConditions: the Condition fields (by JSON name) some surface fills
// in the Context it decides with, and the test that shows it.
var enforcedConditions = map[string]Site{
	"transport":     Proof("TestEvaluate_TheGateSuppliesTheClockAndTheTransport"),
	"time":          Proof("TestEvaluate_TheGateSuppliesTheClockAndTheTransport"),
	"owner_is_self": Proof("TestDocumentACL_ConditionalDenyFallsBackToEachDocument"),
}

// unenforcedConditions: Condition fields no surface fills yet; a rule using
// one is refused at write.
var unenforcedConditions = map[string]string{
	"interactive": "no surface fills it",
	"tier":        "no surface fills it",
	"user_tier":   "no surface fills it",
	"source_cidr": "no surface fills it",
	"agent":       "no surface fills it",
	"depth":       "no surface fills it",
}

// IsEnforced reports whether some surface asks verb of typ.
func IsEnforced(typ, verb string) bool {
	_, ok := enforcedPairs[pairKey{typ, verb}]
	return ok
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
	Reason                string
}

func (e *NotEnforceableError) Error() string {
	if e.Condition != "" {
		return fmt.Sprintf("condition %q is not evaluated by any surface", e.Condition)
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
			if IsEnforced(typ, verb) {
				continue
			}
			reason := unenforcedPairs[pairKey{typ, verb}]
			if IsRetired(typ, verb) {
				reason = "removed from the catalog"
			}
			return &NotEnforceableError{Verb: verb, Type: typ, Reason: reason}
		}
	}
	for _, f := range conditionFieldsSet(r.When) {
		if _, ok := enforcedConditions[f]; !ok {
			return &NotEnforceableError{Condition: f}
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
