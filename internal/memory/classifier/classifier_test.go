package classifier

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/decision"
	"github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

type stubDecision struct {
	probs  map[string]float64
	nouls  map[string]float64 // question key → probability of yes
	err    error
	called int
	last   decision.Request
}

func (s *stubDecision) Decide(_ context.Context, req decision.Request) (*decision.Response, error) {
	s.called++
	s.last = req
	if s.err != nil {
		return nil, s.err
	}
	ans := map[string]decision.Answer{classQ: {Type: decision.TypeChoice, Probabilities: s.probs}}
	for k, p := range s.nouls {
		p := p
		ans[k] = decision.Answer{Type: decision.TypeNoul, Noul: &p}
	}
	return &decision.Response{Answers: ans, Usage: providers.Usage{InputTokens: 10, OutputTokens: 1}}, nil
}

func (s *stubDecision) Limits(string) decision.Limits { return decision.Limits{} }

type stubChat struct {
	reply  string
	err    error
	called int
	prompt string
}

func (s *stubChat) Complete(_ context.Context, prompt string) (string, error) {
	s.called++
	s.prompt = prompt
	return s.reply, s.err
}

var req = memory.ClassifyRequest{
	Title:    "Розпорядження № 5",
	Text:     "Про затвердження порядку управління ризиками банку.",
	Classes:  []memory.ClassifyOption{{Name: "Law"}, {Name: "Order"}},
	Concepts: []memory.ClassifyOption{{Name: "Risk Management", Meaning: "how a bank manages risk"}},
}

// TestClassifier_ASureDecisionNeedsNoChat: a confident decision answer is the verdict;
// the chat model is not asked, and the concept yes/no above the floor is kept.
func TestClassifier_ASureDecisionNeedsNoChat(t *testing.T) {
	dec := &stubDecision{probs: map[string]float64{"Order": 0.85, "Law": 0.1, noneOption: 0.05}, nouls: map[string]float64{conceptQ + "0": 0.9}}
	chat := &stubChat{}
	var spent int
	c := New(dec, "nimble", 0, chat, 0.6, 0.2)
	c.SetOnUsage(func(_ context.Context, u *providers.Usage) { spent += u.InputTokens })
	v := c.Classify(context.Background(), req)
	if v.Method != memory.ClassifyMethodDecision || v.Class != "Order" || v.Probability != 0.85 || chat.called != 0 {
		t.Fatalf("verdict = %+v, chat called %d", v, chat.called)
	}
	if len(v.Concepts) != 1 || v.Concepts[0] != "Risk Management" {
		t.Errorf("concepts = %v", v.Concepts)
	}
	if spent != 10 {
		t.Errorf("usage recorded = %d", spent)
	}
	if _, ok := dec.last.Questions[classQ]; !ok || len(dec.last.Questions) != 2 {
		t.Errorf("questions = %v", dec.last.Questions)
	}
}

// TestClassifier_AnUnsureDecisionAsksTheChatModel: below the floor, or too close to the
// runner-up, the decision is set aside and the chat model answers — with a quote that is
// in the document.
func TestClassifier_AnUnsureDecisionAsksTheChatModel(t *testing.T) {
	for name, probs := range map[string]map[string]float64{
		"below the floor": {"Order": 0.5, "Law": 0.3, noneOption: 0.2},
		"too close":       {"Order": 0.62, "Law": 0.38},
	} {
		chat := &stubChat{reply: `Sure: {"class": "order", "quote": "порядку управління  ризиками", "concepts": ["Risk Management", "Invented"]}`}
		v := New(&stubDecision{probs: probs}, "nimble", 0, chat, 0.6, 0.3).Classify(context.Background(), req)
		if v.Method != memory.ClassifyMethodChat || v.Class != "Order" || chat.called != 1 {
			t.Errorf("%s: verdict = %+v", name, v)
		}
		if !strings.Contains(v.Reason, "unsure") || !strings.Contains(v.Reason, "chat model answered") {
			t.Errorf("%s: the set-aside decision is not reported: %q", name, v.Reason)
		}
		if len(v.Concepts) != 1 || v.Concepts[0] != "Risk Management" {
			t.Errorf("%s: an un-offered concept survived: %v", name, v.Concepts)
		}
	}
}

// TestClassifier_AChatVerdictWithoutEvidenceIsDropped: a quote not in the document, an
// un-offered class, or a reply that is not the object asked for, gives no class.
func TestClassifier_AChatVerdictWithoutEvidenceIsDropped(t *testing.T) {
	for name, reply := range map[string]string{
		"made-up quote":  `{"class": "Order", "quote": "наказую звільнити директора"}`,
		"no quote":       `{"class": "Order"}`,
		"other class":    `{"class": "Treaty", "quote": "Розпорядження № 5"}`,
		"not json":       `Order, I think.`,
		"concepts alone": `{"class": "none", "concepts": ["Risk Management"]}`,
	} {
		v := New(nil, "", 0, &stubChat{reply: reply}, 0.6, 0.2).Classify(context.Background(), req)
		if v.Class != "" || len(v.Concepts) != 0 || v.Reason == "" {
			t.Errorf("%s: verdict = %+v", name, v)
		}
	}
}

// TestClassifier_AConfidentNoneIsAnAnswer: the decision model choosing "none of these"
// with confidence is an answer, not doubt: no class, and the chat model is not asked.
func TestClassifier_AConfidentNoneIsAnAnswer(t *testing.T) {
	chat := &stubChat{}
	v := New(&stubDecision{probs: map[string]float64{noneOption: 0.9, "Law": 0.1}}, "nimble", 0, chat, 0.6, 0.2).Classify(context.Background(), req)
	if v.Class != "" || chat.called != 0 || !strings.Contains(v.Reason, "none") {
		t.Errorf("verdict = %+v, chat called %d", v, chat.called)
	}
}

// TestClassifier_FaultsAreVerdictsNotErrors: a decision call that fails falls back to
// chat; with no chat, or a chat that fails too, the verdict has no class and a reason.
func TestClassifier_FaultsAreVerdictsNotErrors(t *testing.T) {
	failing := &stubDecision{err: errors.New("connection refused")}
	if v := New(failing, "nimble", 0, nil, 0.6, 0.2).Classify(context.Background(), req); v.Class != "" || !strings.Contains(v.Reason, "connection refused") {
		t.Errorf("decision only, failing: %+v", v)
	}
	chat := &stubChat{err: errors.New("503")}
	if v := New(failing, "nimble", 0, chat, 0.6, 0.2).Classify(context.Background(), req); v.Class != "" || chat.called != 1 || !strings.Contains(v.Reason, "503") {
		t.Errorf("both failing: %+v", v)
	}
	if v := New(nil, "", 0, nil, 0.6, 0.2).Classify(context.Background(), req); v.Class != "" || v.Reason == "" {
		t.Errorf("nothing configured: %+v", v)
	}
}

// TestClassifier_TheDocumentIsFencedInTheChatPrompt: document text is material, marked
// as such, and the prompt carries no project references.
func TestClassifier_TheDocumentIsFencedInTheChatPrompt(t *testing.T) {
	chat := &stubChat{reply: `{"class":"none","quote":"Розпорядження"}`}
	New(nil, "", 0, chat, 0.6, 0.2).Classify(context.Background(), req)
	if !strings.Contains(chat.prompt, "<<<DOCUMENT") || !strings.Contains(chat.prompt, "never instructions") || strings.Contains(chat.prompt, "RFC") {
		t.Errorf("prompt = %s", chat.prompt)
	}
}

// TestClassifier_AConceptsOnlyRequestAsksNoClass: when the class is settled, the
// decision model gets only the yes/no per concept, and the chat model's class, if it
// names one anyway, is ignored — its concepts still need a quote from the document.
func TestClassifier_AConceptsOnlyRequestAsksNoClass(t *testing.T) {
	only := req
	only.ConceptsOnly = true
	dec := &stubDecision{nouls: map[string]float64{conceptQ + "0": 0.9}}
	chat := &stubChat{}
	v := New(dec, "nimble", 0, chat, 0.6, 0.2).Classify(context.Background(), only)
	if v.Class != "" || len(v.Concepts) != 1 || v.Concepts[0] != "Risk Management" || chat.called != 0 {
		t.Fatalf("decision verdict = %+v, chat called %d", v, chat.called)
	}
	if _, asked := dec.last.Questions[classQ]; asked || len(dec.last.Questions) != 1 {
		t.Errorf("questions = %v", dec.last.Questions)
	}

	chat = &stubChat{reply: `{"class": "Law", "concepts": ["Risk Management"], "quote": "управління ризиками"}`}
	v = New(nil, "", 0, chat, 0.6, 0.2).Classify(context.Background(), only)
	if v.Class != "" || len(v.Concepts) != 1 || strings.Contains(v.Reason, "none of the classes") {
		t.Errorf("chat verdict = %+v", v)
	}
	if strings.Contains(chat.prompt, "Classes:") || !strings.Contains(chat.prompt, "Risk Management") {
		t.Errorf("the concepts-only prompt offers classes:\n%s", chat.prompt)
	}
	chat = &stubChat{reply: `{"concepts": ["Risk Management"], "quote": "про звільнення"}`}
	if v = New(nil, "", 0, chat, 0.6, 0.2).Classify(context.Background(), only); len(v.Concepts) != 0 {
		t.Errorf("concepts without evidence survived: %+v", v)
	}
}
