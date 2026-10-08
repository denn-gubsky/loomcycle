package decision

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

func newService(t *testing.T, url string) *Service {
	t.Helper()
	d := newDriver(t, Options{BaseURL: url})
	s, err := NewService("decide", []ModelSpec{
		{Name: "decide-deep", Provider: "ollama-local", Model: "clef", Driver: d},
		{Name: "decide", Provider: "ollama-local", Model: "nimble", Driver: d},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s
}

// TestService_AsksTheModelTheNameResolvedTo — a caller names a model as the
// operator listed it; the provider is asked for the model that name resolved
// to, and no name means the default.
func TestService_AsksTheModelTheNameResolvedTo(t *testing.T) {
	st, url := serve(t, replyJSON(http.StatusOK, `{"model":"x","answers":{"q00":{"type":"noul","noul":0.5}}}`))
	s := newService(t, url)
	for _, c := range []struct{ name, want string }{{"", "nimble"}, {"decide", "nimble"}, {"decide-deep", "clef"}} {
		if _, err := s.Decide(context.Background(), c.name, nil, questionsOf(1)); err != nil {
			t.Fatalf("Decide(%q): %v", c.name, err)
		}
		if got := st.bodies[len(st.bodies)-1]["model"]; got != c.want {
			t.Errorf("Decide(%q) asked the provider for %v, want %s", c.name, got, c.want)
		}
	}
	if s.Default() != "decide" {
		t.Errorf("Default = %q, want decide", s.Default())
	}
	lim := Limits{MaxQuestions: 64, MinOptions: 2, MaxOptions: 26}
	want := []ModelInfo{
		{Name: "decide", Provider: "ollama-local", Model: "nimble", Limits: lim},
		{Name: "decide-deep", Provider: "ollama-local", Model: "clef", Limits: lim},
	}
	got := s.Models()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Models = %+v, want %+v", got, want)
	}
}

// TestService_RefusesAModelOutsideTheList — the list is the operator's: a model
// the provider serves but the operator did not list is not reachable by name.
func TestService_RefusesAModelOutsideTheList(t *testing.T) {
	st, url := serve(t, replyJSON(http.StatusOK, `{"answers":{"q00":{"type":"noul","noul":0.5}}}`))
	_, err := newService(t, url).Decide(context.Background(), "nimble", nil, questionsOf(1))
	if CodeOf(err) != CodeModelNotAllowed || st.calls() != 0 {
		t.Errorf("err = %v after %d calls, want model_not_allowed and no call", err, st.calls())
	}
}

// TestService_BooksUsageToTheRun — input and output tokens, the provider, the
// model asked for and whose key paid reach the usage callback on the caller's
// context, once per call.
func TestService_BooksUsageToTheRun(t *testing.T) {
	_, url := serve(t, replyJSON(http.StatusOK, `{"model":"nimble","answers":{"q00":{"type":"noul","noul":0.5}},"usage":{"input_tokens":909,"output_tokens":4}}`))
	s := newService(t, url)
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "run-1")
	var booked []providers.Usage
	s.SetOnUsage(func(c context.Context, u *providers.Usage) {
		if c.Value(ctxKey{}) != "run-1" {
			t.Errorf("usage booked on another context")
		}
		booked = append(booked, *u)
	})
	if _, err := s.Decide(ctx, "decide-deep", nil, questionsOf(1)); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	want := providers.Usage{InputTokens: 909, OutputTokens: 4, Model: "clef", Provider: "ollama-local", CredentialSource: "operator"}
	if len(booked) != 1 || booked[0] != want {
		t.Errorf("booked = %+v, want one record %+v", booked, want)
	}
}

// TestService_AnUnansweredQuestionFailsTheCall — a caller reads answers by the
// names it asked under; a reply missing one is a failed call, not a nil answer,
// and the tokens it reports are still booked.
func TestService_AnUnansweredQuestionFailsTheCall(t *testing.T) {
	_, url := serve(t, replyJSON(http.StatusOK, `{"answers":{"q00":{"type":"noul","noul":0.5}},"usage":{"input_tokens":7,"output_tokens":1}}`))
	s := newService(t, url)
	booked := 0
	s.SetOnUsage(func(context.Context, *providers.Usage) { booked++ })
	_, err := s.Decide(context.Background(), "", nil, questionsOf(2))
	var e *Error
	if CodeOf(err) != CodeCallFailed || !errors.As(err, &e) || e.Question != "q01" {
		t.Errorf("err = %v, want call_failed naming q01", err)
	}
	if booked != 1 {
		t.Errorf("usage booked %d times, want 1: the call was made and paid for", booked)
	}
}

// TestNewService_RefusesAnInconsistentList — a default outside the list, a name
// listed twice, a model with no driver.
func TestNewService_RefusesAnInconsistentList(t *testing.T) {
	d := newDriver(t, Options{BaseURL: "http://ollama.test:11434"})
	ok := ModelSpec{Name: "decide", Provider: "p", Model: "nimble", Driver: d}
	for name, c := range map[string]struct {
		def    string
		models []ModelSpec
	}{
		"default not listed": {"other", []ModelSpec{ok}},
		"listed twice":       {"decide", []ModelSpec{ok, ok}},
		"no driver":          {"decide", []ModelSpec{{Name: "decide", Model: "nimble"}}},
	} {
		if _, err := NewService(c.def, c.models); err == nil {
			t.Errorf("%s: NewService accepted it", name)
		}
	}
}
