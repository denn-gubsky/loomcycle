package memory

import (
	"reflect"
	"strings"
	"testing"
)

func ptrBool(b bool) *bool        { return &b }
func ptrFloat(f float64) *float64 { return &f }

// Every rule form reads into the fields it names.
func TestParseReviewRule_ReadsEveryForm(t *testing.T) {
	for _, tc := range []struct {
		text string
		want ReviewRule
	}{
		{"accept kind:cites,repeals method:rule:citation when quotes:resolved self:false target:held|placeholder",
			ReviewRule{Action: ReviewAccept, Kinds: []string{"cites", "repeals"}, Method: "rule:citation",
				QuotesResolved: true, Self: ptrBool(false), Targets: []string{"held", "placeholder"}}},
		{"accept kind:subject method:classify when rule:matched",
			ReviewRule{Action: ReviewAccept, Kinds: []string{"subject"}, Method: "classify", RuleMatched: true}},
		{"reject kind:elaborates method:extractor:relate when confidence<0.6",
			ReviewRule{Action: ReviewReject, Kinds: []string{"elaborates"}, Method: "extractor:relate", Confidence: &ReviewBound{"<", 0.6}}},
		{"accept method:rule:* when confidence>=0.9 self:true target:subject|concept",
			ReviewRule{Action: ReviewAccept, Method: "rule:*", Confidence: &ReviewBound{">=", 0.9}, Self: ptrBool(true), Targets: []string{"subject", "concept"}}},
		{"reject kind:x when confidence<=0.1", ReviewRule{Action: ReviewReject, Kinds: []string{"x"}, Confidence: &ReviewBound{"<=", 0.1}}},
		{"reject kind:x when confidence>0.5", ReviewRule{Action: ReviewReject, Kinds: []string{"x"}, Confidence: &ReviewBound{">", 0.5}}},
		{"judge  kind:elaborates,uses_term floor:0.9 mode:recommend",
			ReviewRule{Action: ReviewJudge, Kinds: []string{"elaborates", "uses_term"}, Floor: ptrFloat(0.9), Mode: JudgeModeRecommend}},
		{"judge kind:elaborates mode:rule", ReviewRule{Action: ReviewJudge, Kinds: []string{"elaborates"}, Mode: JudgeModeRule}},
		{"human *", ReviewRule{Action: ReviewHuman, Any: true}},
		{"HUMAN *", ReviewRule{Action: ReviewHuman, Any: true}},
	} {
		got, err := ParseReviewRule(tc.text)
		if err != nil {
			t.Errorf("%q: %v", tc.text, err)
			continue
		}
		tc.want.Text = tc.text
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q = %+v, want %+v", tc.text, got, tc.want)
		}
	}
}

// A rule this build cannot read is an error, never a narrower rule.
func TestParseReviewRule_RefusesMalformedRules(t *testing.T) {
	for text, want := range map[string]string{
		"":                                        "empty",
		"allow kind:cites":                        "not an action",
		"accept":                                  "name what it selects",
		"accept when quotes:resolved":             "name what it selects",
		"accept * kind:cites":                     "* selects every proposal",
		"accept kind:cites when":                  "when names no condition",
		"accept kind:cites quotes:resolved":       "write it after when",
		"accept kind:cites confidence<0.5":        "write it after when",
		"accept when quotes:resolved kind:x":      "comes before when",
		"accept kind:x when *":                    "comes before when",
		"accept kind:x when quotes:any":           "quotes:resolved",
		"accept kind:x when self:no":              "self:true or self:false",
		"accept kind:x when target:law":           "is not a target",
		"accept kind:x when rule:model":           "rule:matched",
		"accept kind:x when confidence=0.5":       "want <, <=, > or >=",
		"accept kind:x when confidence<2":         "between 0 and 1",
		"accept kind:x floor:0.9":                 "option of judge",
		"accept kind:x mode:rule":                 "option of judge",
		"judge kind:x floor:1.5":                  "between 0 and 1",
		"judge kind:x mode:maybe":                 "mode:rule or mode:recommend",
		"accept kind:a,,b":                        "empty name",
		"accept kind:a kind:b":                    "twice",
		"accept kind:a when when quotes:resolved": "twice",
		"accept method:ru*le":                     "may only end",
		"accept kind:x color:red":                 "not a selector",
		"accept kind:x stray":                     "not a selector",
	} {
		_, err := ParseReviewRule(text)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want one mentioning %q", text, err, want)
		}
	}
}

// The section's list items are its rules, numbered in order; its @section line,
// prose, examples in a code block and comments are not; judge_floor sets the
// default floor.
func TestParseReviewSection_ReadsListItemsInOrder(t *testing.T) {
	body := "@section review\n" +
		"Proposals no rule matches wait for a person.\n" +
		"judge_floor: 0.8   # the judge's default\n\n" +
		"- accept kind:cites method:rule:citation when quotes:resolved\n" +
		"* `reject kind:elaborates when confidence<0.6`\n" +
		"```\n- accept *\n```\n" +
		"    - accept *\n" +
		"- judge kind:uses_term\n" +
		"- human  *                     # default\n"
	p := ParseReviewSection(body)
	if !p.Applied() {
		t.Fatalf("issues: %v", p.Issues)
	}
	if p.Source != ReviewSourceSection || p.JudgeFloor != 0.8 {
		t.Errorf("source %q floor %v", p.Source, p.JudgeFloor)
	}
	var got []string
	for _, r := range p.Rules {
		got = append(got, strings.Join([]string{string(rune('0' + r.Index)), r.Action, r.Text}, "|"))
	}
	want := []string{
		"1|accept|accept kind:cites method:rule:citation when quotes:resolved",
		"2|reject|reject kind:elaborates when confidence<0.6",
		"3|judge|judge kind:uses_term",
		"4|human|human  *",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rules = %v, want %v", got, want)
	}
	if f := p.EffectiveFloor(p.Rules[2]); f != 0.8 {
		t.Errorf("judge floor = %v, want the section's 0.8", f)
	}
}

// One malformed rule, an empty section or a bad judge_floor leaves the whole
// policy unapplied: Decide matches nothing, so every proposal waits for a
// person, and the issue names the rule.
func TestParseReviewSection_FailsClosed(t *testing.T) {
	facts := ReviewFacts{Kind: "cites", Method: "rule:citation", QuotesResolved: true}
	for name, body := range map[string]string{
		"malformed rule": "- accept kind:cites\n- reject kind:x when colour:red\n",
		"no rules":       "@section review\nOnly prose.\n",
		"bad floor":      "judge_floor: 2\n- accept kind:cites\n",
		"floor twice":    "judge_floor: 0.5\njudge_floor: 0.6\n- accept kind:cites\n",
	} {
		p := ParseReviewSection(body)
		if p.Applied() || len(p.Issues) == 0 {
			t.Errorf("%s: applied with issues %v", name, p.Issues)
		}
		if r, ok := p.Decide(facts); ok {
			t.Errorf("%s: an unapplied policy decided %+v", name, r)
		}
	}
	p := ParseReviewSection("- accept kind:cites\n- reject kind:x when colour:red\n")
	if !strings.Contains(strings.Join(p.Issues, "\n"), `rule 2 ("reject kind:x when colour:red")`) {
		t.Errorf("issues do not name rule 2: %v", p.Issues)
	}
}

// The first rule that matches decides; a later, broader rule never overrides
// it, and a proposal no rule matches is not decided.
func TestReviewPolicy_FirstMatchWins(t *testing.T) {
	p := ParseReviewSection("- human kind:cites method:rule:citation when self:true\n" +
		"- accept kind:cites method:rule:* when quotes:resolved\n" +
		"- reject kind:cites\n" +
		"- judge kind:elaborates\n")
	if !p.Applied() {
		t.Fatal(p.Issues)
	}
	for _, tc := range []struct {
		name  string
		facts ReviewFacts
		want  int // rule index, 0 = none
	}{
		{"self-citation", ReviewFacts{Kind: "cites", Method: "rule:citation", QuotesResolved: true, Self: ptrBool(true)}, 1},
		{"citation", ReviewFacts{Kind: "cites", Method: "rule:citation", QuotesResolved: true, Self: ptrBool(false)}, 2},
		{"unresolved citation", ReviewFacts{Kind: "cites", Method: "rule:citation", Self: ptrBool(false)}, 3},
		{"unknown self", ReviewFacts{Kind: "cites", Method: "rule:citation", QuotesResolved: true}, 2},
		{"model citation", ReviewFacts{Kind: "cites", Method: "extractor:relate", QuotesResolved: true}, 3},
		{"elaborates", ReviewFacts{Kind: "elaborates", Method: "extractor:relate"}, 4},
		{"other kind", ReviewFacts{Kind: "defines", Method: "rule:terms", QuotesResolved: true}, 0},
	} {
		r, ok := p.Decide(tc.facts)
		if got := map[bool]int{true: r.Index}[ok]; got != tc.want {
			t.Errorf("%s: rule %d, want %d", tc.name, got, tc.want)
		}
	}
}

// Each condition fails closed on a fact it cannot know: no confidence meets no
// bound, a gone target meets no target, an unknown self meets neither value.
func TestReviewRule_UnknownFactsMeetNoCondition(t *testing.T) {
	for text, f := range map[string]ReviewFacts{
		"reject kind:x when confidence<0.6":  {Kind: "x"},
		"accept kind:x when target:held":     {Kind: "x"},
		"accept kind:x when self:false":      {Kind: "x"},
		"accept kind:x when self:true":       {Kind: "x"},
		"accept kind:x when rule:matched":    {Kind: "x", Method: "extractor:m"},
		"accept kind:x when quotes:resolved": {Kind: "x", Method: "rule:m"},
	} {
		r, err := ParseReviewRule(text)
		if err != nil {
			t.Fatal(err)
		}
		if r.Matches(f) {
			t.Errorf("%q matched %+v", text, f)
		}
	}
}

// The policy @auto_assert implies accepts exactly what the per-kind check did:
// the kind, a rule: method, every quote resolved.
func TestImpliedReviewPolicy_IsTheAutoAssertCheck(t *testing.T) {
	p := ImpliedReviewPolicy([]string{"supervises", "cites"})
	if p.Source != ReviewSourceAutoAssert || !p.Applied() || len(p.Rules) != 2 {
		t.Fatalf("policy = %+v", p)
	}
	if p.Rules[0].Text != "accept kind:cites method:rule:* when quotes:resolved" || p.Rules[0].Index != 1 {
		t.Errorf("rule 1 = %+v", p.Rules[0])
	}
	// The text reads back as the rule it is.
	for _, r := range p.Rules {
		parsed, err := ParseReviewRule(r.Text)
		parsed.Index = r.Index
		if err != nil || !reflect.DeepEqual(parsed, r) {
			t.Errorf("rule %q reads back as %+v (%v)", r.Text, parsed, err)
		}
	}
	for _, kind := range []string{"cites", "supervises", "other"} {
		for _, method := range []string{"rule:l2", "extractor:m", "manual", "classify"} {
			for _, resolved := range []bool{true, false} {
				legacy := (kind == "cites" || kind == "supervises") && strings.HasPrefix(method, "rule:") && resolved
				r, ok := p.Decide(ReviewFacts{Kind: kind, Method: method, QuotesResolved: resolved})
				if got := ok && r.Action == ReviewAccept; got != legacy {
					t.Errorf("%s/%s/%v: accept %v, the @auto_assert check %v", kind, method, resolved, got, legacy)
				}
			}
		}
	}
}
