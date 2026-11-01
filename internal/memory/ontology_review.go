package memory

// ontology_review.go — the ontology's Review section: a tenant's review policy
// (RFC FS P2).
//
// A knowledge base fills with proposed statements, and most of them follow
// rules an operator would apply by hand: a citation a rule matched, with its
// quote found in the text, is right. The policy writes those rules down, in the
// ontology document, so they version and confirm with the vocabulary they rule
// on:
//
//	## Review
//	@section review
//	judge_floor: 0.85
//	judge_audit: 0.1
//	- accept kind:cites,repeals method:rule:citation when quotes:resolved self:false target:held|placeholder
//	- reject kind:elaborates method:extractor:relate when confidence<0.6
//	- judge  kind:elaborates,uses_term floor:0.9 mode:recommend
//	- human  *
//
// Every list item is one rule, in order; the first that matches a proposal
// decides it, and a proposal no rule matches waits for a person.
//
// FAIL CLOSED. A rule this build cannot read is reported and the WHOLE policy is
// left unapplied — every proposal waits for a person — because a policy with one
// rule dropped is a different policy: dropping a `human` or a `reject` above an
// `accept` would widen what the accept lets through. Pure parsing: no I/O.

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// SectionReview is the section kind of the review policy.
const SectionReview = "review"

// Review actions.
const (
	ReviewAccept = "accept"
	ReviewReject = "reject"
	ReviewJudge  = "judge"
	ReviewHuman  = "human"
)

// Where a policy comes from.
const (
	// ReviewSourceSection: the ontology's Review section.
	ReviewSourceSection = "review"
	// ReviewSourceAutoAssert: no Review section; the policy @auto_assert implies.
	ReviewSourceAutoAssert = "auto_assert"
)

// DefaultJudgeFloor is the confidence a judge's verdict needs to rule, when
// neither the section nor the rule names one.
const DefaultJudgeFloor = 0.85

// DefaultJudgeAudit is the share of a judge run's acceptances marked for a
// person's audit when the section names none.
const DefaultJudgeAudit = 0.1

// Judge modes: rule above the floor, or only record a recommendation.
const (
	JudgeModeRule      = "rule"
	JudgeModeRecommend = "recommend"
)

// Targets a rule may name: what the statement's to-end is.
const (
	ReviewTargetHeld        = "held"        // a document the scope holds
	ReviewTargetPlaceholder = "placeholder" // a document filed for a cited work not held yet
	ReviewTargetSubject     = "subject"     // a subject node
	ReviewTargetConcept     = "concept"     // a concept of the ontology's schemes
)

var reviewTargets = map[string]bool{
	ReviewTargetHeld: true, ReviewTargetPlaceholder: true, ReviewTargetSubject: true, ReviewTargetConcept: true,
}

// ReviewPolicy is a parsed review policy.
type ReviewPolicy struct {
	Source string       `json:"source"`
	Rules  []ReviewRule `json:"rules"`
	// JudgeFloor is the section's judge_floor, else DefaultJudgeFloor.
	JudgeFloor float64 `json:"judge_floor"`
	// JudgeAudit is the section's judge_audit, else DefaultJudgeAudit: the
	// share of each judge run's acceptances marked for a person's audit (at
	// least one when the run accepted anything). Never 0: the sample is one
	// of the guards that let the judge rule.
	JudgeAudit float64 `json:"judge_audit"`
	// Issues are the section's errors. Any issue leaves the whole policy
	// unapplied: every proposal waits for a person.
	Issues []string `json:"issues,omitempty"`
}

// Applied reports whether the policy rules on anything: it parsed cleanly.
func (p ReviewPolicy) Applied() bool { return len(p.Issues) == 0 }

// ReviewRule is one rule: an action, what it selects and when.
type ReviewRule struct {
	// Index is the rule's place in the list, from 1. A statement the rule
	// rules records it: policy:<index>@<ontology version>.
	Index  int    `json:"index"`
	Text   string `json:"text"`
	Action string `json:"action"`
	// Any is `*`: every proposal.
	Any   bool     `json:"any,omitempty"`
	Kinds []string `json:"kinds,omitempty"`
	// Method is an exact method, or a prefix when it ends in `*` (rule:*).
	Method string `json:"method,omitempty"`
	// QuotesResolved: the statement has evidence and every quote of it is in
	// a chunk and not stale.
	QuotesResolved bool `json:"quotes_resolved,omitempty"`
	// Self: both ends are in one document (true) or in two (false).
	Self *bool `json:"self,omitempty"`
	// Targets: what the to-end is (held, placeholder, subject, concept).
	Targets []string `json:"targets,omitempty"`
	// Confidence bounds the writer's own confidence; a statement with none
	// never meets a bound.
	Confidence *ReviewBound `json:"confidence,omitempty"`
	// RuleMatched: a rule made it, not a model.
	RuleMatched bool `json:"rule_matched,omitempty"`
	// Floor and Mode are a judge rule's: the verdict confidence it needs to
	// rule, and whether it rules or only recommends.
	Floor *float64 `json:"floor,omitempty"`
	Mode  string   `json:"mode,omitempty"`
}

// ReviewBound is a comparison: <, <=, > or >= a value.
type ReviewBound struct {
	Op    string  `json:"op"`
	Value float64 `json:"value"`
}

func (b ReviewBound) holds(v float64) bool {
	switch b.Op {
	case "<":
		return v < b.Value
	case "<=":
		return v <= b.Value
	case ">":
		return v > b.Value
	case ">=":
		return v >= b.Value
	}
	return false
}

// ReviewFacts are what a rule asks of one proposal. An unknown fact (an end
// whose chunk is gone) meets no condition that names it.
type ReviewFacts struct {
	Kind           string
	Method         string
	Confidence     *float64
	QuotesResolved bool
	// Self is nil when either end's document is unknown.
	Self *bool
	// Target is "" when the to-end's chunk is gone.
	Target      string
	RuleMatched bool
}

// Matches reports whether the rule selects the proposal.
func (r ReviewRule) Matches(f ReviewFacts) bool {
	if !r.Any {
		if len(r.Kinds) > 0 && !containsString(r.Kinds, f.Kind) {
			return false
		}
		if r.Method != "" {
			if prefix, ok := strings.CutSuffix(r.Method, "*"); ok {
				if !strings.HasPrefix(f.Method, prefix) {
					return false
				}
			} else if f.Method != r.Method {
				return false
			}
		}
	}
	if r.QuotesResolved && !f.QuotesResolved {
		return false
	}
	if r.Self != nil && (f.Self == nil || *f.Self != *r.Self) {
		return false
	}
	if len(r.Targets) > 0 && (f.Target == "" || !containsString(r.Targets, f.Target)) {
		return false
	}
	if r.Confidence != nil && (f.Confidence == nil || !r.Confidence.holds(*f.Confidence)) {
		return false
	}
	if r.RuleMatched && !f.RuleMatched {
		return false
	}
	return true
}

// Decide returns the first rule that matches the proposal; ok is false when
// none does (the proposal waits for a person) or the policy is not applied.
func (p ReviewPolicy) Decide(f ReviewFacts) (rule ReviewRule, ok bool) {
	if !p.Applied() {
		return ReviewRule{}, false
	}
	for _, r := range p.Rules {
		if r.Matches(f) {
			return r, true
		}
	}
	return ReviewRule{}, false
}

// EffectiveFloor is the verdict confidence a judge rule needs to rule: its own
// floor, else the policy's.
func (p ReviewPolicy) EffectiveFloor(r ReviewRule) float64 {
	if r.Floor != nil {
		return *r.Floor
	}
	return p.JudgeFloor
}

// ParseReviewSection reads the Review section's body: every list item is a
// rule, a `judge_floor: <x>` line sets the judge's default floor, a
// `judge_audit: <x>` line the share of its acceptances sampled for audit
// (0 < x <= 1), and the rest
// (the @section line, prose, examples in a code block or fence) is not read.
func ParseReviewSection(body string) ReviewPolicy {
	p := ReviewPolicy{Source: ReviewSourceSection, Rules: []ReviewRule{}, JudgeFloor: DefaultJudgeFloor, JudgeAudit: DefaultJudgeAudit}
	inFence, floorSet, auditSet := false, false, false
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") {
			inFence = !inFence
			continue
		}
		if inFence || strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") || t == "" || strings.HasPrefix(t, "@") {
			continue
		}
		if k, v, ok := strings.Cut(t, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "judge_floor") {
			f, err := parseUnitFloat(stripReviewComment(v))
			switch {
			case floorSet:
				p.Issues = append(p.Issues, "judge_floor is set twice")
			case err != nil:
				p.Issues = append(p.Issues, "judge_floor: "+err.Error())
			default:
				p.JudgeFloor = f
			}
			floorSet = true
			continue
		}
		if k, v, ok := strings.Cut(t, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "judge_audit") {
			f, err := parseUnitFloat(stripReviewComment(v))
			switch {
			case auditSet:
				p.Issues = append(p.Issues, "judge_audit is set twice")
			case err != nil:
				p.Issues = append(p.Issues, "judge_audit: "+err.Error())
			case f == 0:
				p.Issues = append(p.Issues, "judge_audit: 0 turns the audit of the judge's acceptances off; it is required while the judge rules (use mode:recommend on the judge rules instead)")
			default:
				p.JudgeAudit = f
			}
			auditSet = true
			continue
		}
		if !strings.HasPrefix(t, "- ") && !strings.HasPrefix(t, "* ") {
			continue // prose
		}
		item := strings.TrimSpace(stripReviewComment(t[2:]))
		if len(item) >= 2 && item[0] == '`' && item[len(item)-1] == '`' {
			item = strings.TrimSpace(item[1 : len(item)-1])
		}
		r, err := ParseReviewRule(item)
		r.Index = len(p.Rules) + 1
		if err != nil {
			p.Issues = append(p.Issues, fmt.Sprintf("rule %d (%s): %v", r.Index, strconv.Quote(item), err))
		}
		p.Rules = append(p.Rules, r)
	}
	if len(p.Rules) == 0 {
		p.Issues = append(p.Issues, "the Review section has no rules (one per list item)")
	}
	return p
}

// stripReviewComment drops a trailing ` # comment`.
func stripReviewComment(s string) string {
	if i := strings.Index(s, " #"); i >= 0 {
		s = s[:i]
	}
	if strings.HasPrefix(strings.TrimSpace(s), "#") {
		return ""
	}
	return s
}

func parseUnitFloat(s string) (float64, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || f < 0 || f > 1 {
		return 0, fmt.Errorf("%q is not a number between 0 and 1", strings.TrimSpace(s))
	}
	return f, nil
}

// ParseReviewRule reads one rule:
//
//	<accept|reject|judge|human> <kind:a,b | method:m | method:p* | *>… [when <condition>…] [floor:x] [mode:recommend|rule]
//
// Conditions are quotes:resolved, self:true|false, target:a|b, rule:matched
// and confidence<x / <=x / >x / >=x. floor and mode are a judge rule's.
func ParseReviewRule(text string) (ReviewRule, error) {
	r := ReviewRule{Text: text}
	toks := strings.Fields(text)
	if len(toks) == 0 {
		return r, fmt.Errorf("empty rule")
	}
	r.Action = strings.ToLower(toks[0])
	switch r.Action {
	case ReviewAccept, ReviewReject, ReviewJudge, ReviewHuman:
	default:
		return r, fmt.Errorf("%q is not an action (want accept, reject, judge or human)", toks[0])
	}
	seen := map[string]bool{}
	once := func(key string) error {
		if seen[key] {
			return fmt.Errorf("%s is given twice", key)
		}
		seen[key] = true
		return nil
	}
	inWhen, conditions := false, 0
	for _, tok := range toks[1:] {
		if tok == "when" {
			if inWhen {
				return r, fmt.Errorf("when is given twice")
			}
			inWhen = true
			continue
		}
		if tok == "*" {
			if inWhen {
				return r, fmt.Errorf("* selects; it comes before when")
			}
			if err := once("*"); err != nil {
				return r, err
			}
			r.Any = true
			continue
		}
		if rest, ok := strings.CutPrefix(tok, "confidence"); ok && rest != "" && rest[0] != ':' {
			if !inWhen {
				return r, fmt.Errorf("%s is a condition; write it after when", tok)
			}
			if err := once("confidence"); err != nil {
				return r, err
			}
			b, err := parseReviewBound(rest)
			if err != nil {
				return r, fmt.Errorf("%s: %v", tok, err)
			}
			r.Confidence = &b
			conditions++
			continue
		}
		key, val, ok := strings.Cut(tok, ":")
		if !ok || val == "" {
			return r, fmt.Errorf("%q is not a selector, condition or option", tok)
		}
		key = strings.ToLower(key)
		if err := once(key); err != nil {
			return r, err
		}
		switch key {
		case "kind", "method":
			if inWhen {
				return r, fmt.Errorf("%s: selects; it comes before when", key)
			}
		case "quotes", "self", "target", "rule":
			if !inWhen {
				return r, fmt.Errorf("%s is a condition; write it after when", tok)
			}
			conditions++
		case "floor", "mode":
			if r.Action != ReviewJudge {
				return r, fmt.Errorf("%s: is an option of judge, not of %s", key, r.Action)
			}
		}
		switch key {
		case "kind":
			for _, k := range strings.Split(val, ",") {
				if k = strings.TrimSpace(k); k == "" {
					return r, fmt.Errorf("kind: has an empty name")
				}
				r.Kinds = append(r.Kinds, k)
			}
		case "method":
			if i := strings.Index(val, "*"); i >= 0 && i != len(val)-1 {
				return r, fmt.Errorf("method: * may only end the method, as a prefix (rule:*)")
			}
			r.Method = val
		case "quotes":
			if val != "resolved" {
				return r, fmt.Errorf("quotes:%s: the condition is quotes:resolved", val)
			}
			r.QuotesResolved = true
		case "self":
			b, err := strconv.ParseBool(val)
			if err != nil || (val != "true" && val != "false") {
				return r, fmt.Errorf("self:%s: want self:true or self:false", val)
			}
			r.Self = &b
		case "target":
			for _, t := range strings.Split(val, "|") {
				if !reviewTargets[t] {
					return r, fmt.Errorf("target:%s: %q is not a target (want %s)", val, t, joinComma(sortedKeys(reviewTargets)))
				}
				r.Targets = append(r.Targets, t)
			}
		case "rule":
			if val != "matched" {
				return r, fmt.Errorf("rule:%s: the condition is rule:matched", val)
			}
			r.RuleMatched = true
		case "floor":
			f, err := parseUnitFloat(val)
			if err != nil {
				return r, fmt.Errorf("floor: %v", err)
			}
			r.Floor = &f
		case "mode":
			if val != JudgeModeRule && val != JudgeModeRecommend {
				return r, fmt.Errorf("mode:%s: want mode:rule or mode:recommend", val)
			}
			r.Mode = val
		default:
			return r, fmt.Errorf("%q is not a selector, condition or option", tok)
		}
	}
	switch {
	case !r.Any && len(r.Kinds) == 0 && r.Method == "":
		return r, fmt.Errorf("name what it selects: kind:, method: or *")
	case r.Any && (len(r.Kinds) > 0 || r.Method != ""):
		return r, fmt.Errorf("* selects every proposal; drop it or the kind:/method: beside it")
	case inWhen && conditions == 0:
		return r, fmt.Errorf("when names no condition")
	}
	return r, nil
}

func parseReviewBound(s string) (ReviewBound, error) {
	for _, op := range []string{"<=", ">=", "<", ">"} {
		if v, ok := strings.CutPrefix(s, op); ok {
			f, err := parseUnitFloat(v)
			if err != nil {
				return ReviewBound{}, err
			}
			return ReviewBound{Op: op, Value: f}, nil
		}
	}
	return ReviewBound{}, fmt.Errorf("want <, <=, > or >= and a number between 0 and 1")
}

// ImpliedReviewPolicy is the policy an ontology without a Review section has:
// each @auto_assert kind accepted when a rule made it with every quote
// resolved, everything else left for a person.
func ImpliedReviewPolicy(autoAssertKinds []string) ReviewPolicy {
	kinds := append([]string(nil), autoAssertKinds...)
	sort.Strings(kinds)
	p := ReviewPolicy{Source: ReviewSourceAutoAssert, Rules: []ReviewRule{}, JudgeFloor: DefaultJudgeFloor, JudgeAudit: DefaultJudgeAudit}
	for i, k := range kinds {
		p.Rules = append(p.Rules, ReviewRule{
			Index: i + 1, Text: "accept kind:" + k + " method:rule:* when quotes:resolved",
			Action: ReviewAccept, Kinds: []string{k}, Method: "rule:*", QuotesResolved: true,
		})
	}
	return p
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
