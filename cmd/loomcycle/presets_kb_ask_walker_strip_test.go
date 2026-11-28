package main

import (
	"regexp"
	"strings"
	"testing"
)

// kb/ask returns a walker's answer (the dated route, and the walk after a
// search) without the walker's protocol lines or any sentence about them, the
// same strip kb/survey applies to its composing round (presets_kb_ask_test.go
// and presets_kb_survey_test.go hold the fakes these tests drive).

const askDatedQuestion = "Which RFCs were opened since 9 November 2026?"

// leakedWalkerAnswers are the walker answers measured leaking protocol talk,
// each with what kb/ask must return of it.
var leakedWalkerAnswers = map[string]struct{ text, want string }{
	"own line": {"Five documents cover it: " + askLink + ".\n\nI've omitted the MOVES and MISSING lines as instructed…\nMOVES: search\nMISSING: none",
		"Five documents cover it: " + askLink + "."},
	"bulleted": {"Five documents cover it: " + askLink + ".\n- I've left out the MOVES and MISSING lines, as asked.\n- Nothing is missing from them.",
		"Five documents cover it: " + askLink + ".\n- Nothing is missing from them."},
	"beside prose": {"Five documents cover it: " + askLink + ". I've omitted the MOVES and MISSING lines. Each one fits.\nCOVERAGE: complete",
		"Five documents cover it: " + askLink + ". Each one fits.\nCOVERAGE: complete"},
	"compose echo": {"COMPOSE. Screening found that the documents below fit\nFive documents cover it: " + askLink + ".",
		"Five documents cover it: " + askLink + "."},
}

const askLink = "[A](loom:doc/0123456789abcdef0123456789abcdef?scope=tenant)"

// answerBody is an answer without kb/ask's route line.
func answerBody(out string) string {
	return strings.SplitN(out, "\n\nROUTE:", 2)[0]
}

// TestKbAsk_WalkerProtocolTalkIsStrippedOnTheDatedRoute: the dated route
// returned the walker's text as it was, so a sentence like "I've omitted the
// MOVES and MISSING lines" reached the caller. It is stripped, line and
// sentence, and links, lower-case prose and the COVERAGE line stay.
func TestKbAsk_WalkerProtocolTalkIsStrippedOnTheDatedRoute(t *testing.T) {
	for name, tc := range leakedWalkerAnswers {
		t.Run(name, func(t *testing.T) {
			f := &askFake{answers: map[string]string{"kb/walker": tc.text}}
			out := runAsk(t, f, askDatedQuestion, nil)
			wantSpawns(t, f, "kb/walker")
			if got := answerBody(out); got != tc.want {
				t.Errorf("answer = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestKbAsk_WalkerProtocolTalkIsStrippedAfterASearch: the walk after a
// partial search is stripped the same way.
func TestKbAsk_WalkerProtocolTalkIsStrippedAfterASearch(t *testing.T) {
	for name, tc := range leakedWalkerAnswers {
		t.Run(name, func(t *testing.T) {
			f := &askFake{score: 0.2, answers: map[string]string{"kb/classify": "NO", "kb/search": askSearchAnswer("partial"), "kb/walker": tc.text}}
			out := runAsk(t, f, "Where is the retry limit set?", noFirstCall(nil))
			wantSpawns(t, f, "kb/classify", "kb/search", "kb/walker")
			if got := answerBody(out); got != tc.want {
				t.Errorf("answer = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestKbAsk_SearchAnswerIsUnchangedByTheWalkerStrip: a search answer that
// stands on its own is returned as before, lower-case prose included; only
// its COVERAGE line is taken out.
func TestKbAsk_SearchAnswerIsUnchangedByTheWalkerStrip(t *testing.T) {
	const body = "It is set in [Guide](loom:doc/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa?scope=tenant). Nothing is missing, and it fits the moves we made.\n- See [step](loom:chunk/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb?scope=tenant)."
	f := &askFake{score: 0.05, answers: map[string]string{"kb/search": body + "\nCOVERAGE: complete"}}
	out := runAsk(t, f, "Where is the retry limit set?", noFirstCall(nil))
	wantSpawns(t, f, "kb/search")
	if got := answerBody(out); got != body {
		t.Errorf("answer = %q, want the search answer unchanged: %q", got, body)
	}
}

// TestKbAsk_ProtocolStripMirrorsKbSurvey: two code agents share no code, so
// kb/ask carries kb/survey's strip functions as an exact copy. The copies
// must stay identical, and on the same walker text both agents return the
// same answer.
func TestKbAsk_ProtocolStripMirrorsKbSurvey(t *testing.T) {
	cfg := knowledgeBaseConfig(t)
	ask, survey := cfg.Agents["kb/ask"].Code, cfg.Agents["kb/survey"].Code
	pieces := map[string]*regexp.Regexp{}
	for _, fn := range []string{"plain", "walkLine", "sentences", "withoutProtocolTalk"} {
		pieces[fn] = regexp.MustCompile(`(?ms)^function ` + fn + `\(.*?^}$`)
	}
	pieces["PROTOCOL"] = regexp.MustCompile(`(?m)^var PROTOCOL = .*$`)
	for name, re := range pieces {
		a, s := re.FindAllString(ask, -1), re.FindAllString(survey, -1)
		if len(a) != 1 || len(s) != 1 {
			t.Errorf("%s: found %d in kb/ask and %d in kb/survey, want one each", name, len(a), len(s))
			continue
		}
		if a[0] != s[0] {
			t.Errorf("%s differs between kb/ask and kb/survey:\nkb/ask:\n%s\nkb/survey:\n%s", name, a[0], s[0])
		}
	}

	a := docID(1)
	for name, tc := range leakedWalkerAnswers {
		t.Run(name, func(t *testing.T) {
			text := strings.ReplaceAll(tc.text, "\nCOVERAGE: complete", "")
			af := &askFake{answers: map[string]string{"kb/walker": text}}
			fromAsk := answerBody(runAsk(t, af, askDatedQuestion, nil))
			sf := &surveyFake{
				rows:   map[string][]map[string]any{"P": hits(a)},
				screen: fitsOf(a),
				write:  func([]string) surveyReply { return surveyReply{text: text} },
			}
			fromSurvey := strings.SplitN(runSurvey(t, sf, surveyQuestion), "\n\nCOVERAGE:", 2)[0]
			if fromAsk != fromSurvey {
				t.Errorf("kb/ask returned %q, kb/survey %q, for the same walker text", fromAsk, fromSurvey)
			}
		})
	}
}
