package grader

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/bench/internal/cases"
)

// fakeJudge implements Judge with a hard-coded score for tests.
type fakeJudge struct {
	score int
	notes string
	err   error
}

func (f *fakeJudge) Score(_ context.Context, _ string) (int, string, error) {
	return f.score, f.notes, f.err
}

// TestSemantic_PassWhenJudgeMeetsThreshold.
func TestSemantic_PassWhenJudgeMeetsThreshold(t *testing.T) {
	r := Semantic(context.Background(), &fakeJudge{score: 85, notes: "good"}, "output", cases.Semantic{
		Rubric:    "score the output",
		Threshold: 70,
	})
	if !r.Pass {
		t.Fatalf("expected pass; reasons: %v", r.Reasons)
	}
	if r.Score != 0.85 {
		t.Errorf("expected score 0.85, got %v", r.Score)
	}
}

// TestSemantic_FailBelowThreshold.
func TestSemantic_FailBelowThreshold(t *testing.T) {
	r := Semantic(context.Background(), &fakeJudge{score: 60}, "output", cases.Semantic{
		Rubric:    "score the output",
		Threshold: 70,
	})
	if r.Pass {
		t.Fatal("expected fail below threshold")
	}
}

// TestSemantic_NoJudgeIsPassThrough — operators running --no-semantic
// (or without ANTHROPIC_API_KEY) get pass=true on this axis.
func TestSemantic_NoJudgeIsPassThrough(t *testing.T) {
	r := Semantic(context.Background(), nil, "output", cases.Semantic{
		Rubric:    "rubric exists",
		Threshold: 70,
	})
	if !r.Pass {
		t.Fatal("expected pass-through when judge is nil")
	}
}

// TestSemantic_JudgeErrorFailsTheAxis — a judge HTTP error shouldn't
// silently pass; it must surface as a failure with the error in reasons.
func TestSemantic_JudgeErrorFailsTheAxis(t *testing.T) {
	r := Semantic(context.Background(), &fakeJudge{err: errors.New("HTTP 503")}, "output", cases.Semantic{
		Rubric:    "score",
		Threshold: 70,
	})
	if r.Pass {
		t.Fatal("expected fail on judge error")
	}
	if !contains(r.Reasons, "503") {
		t.Errorf("expected error in reasons; got %v", r.Reasons)
	}
}

// TestParseJudgeResponse_HappyPath.
func TestParseJudgeResponse_HappyPath(t *testing.T) {
	score, notes := ParseJudgeResponse(`{"score": 85, "notes": "well done"}`)
	if score != 85 || notes != "well done" {
		t.Errorf("got (%d, %q)", score, notes)
	}
}

// TestParseJudgeResponse_StripsCodeFences — many judges wrap in fences.
func TestParseJudgeResponse_StripsCodeFences(t *testing.T) {
	score, _ := ParseJudgeResponse("```json\n{\"score\": 72}\n```")
	if score != 72 {
		t.Errorf("got %d, want 72", score)
	}
}

// TestParseJudgeResponse_RegexFallback — even total format failure
// still extracts the score field if it's there somewhere.
func TestParseJudgeResponse_RegexFallback(t *testing.T) {
	score, _ := ParseJudgeResponse(`prose around it: the model gets {"score": 91, "notes": "x"} for clarity`)
	if score != 91 {
		t.Errorf("got %d, want 91", score)
	}
}

// TestBuildJudgePrompt_IncludesRubric.
func TestBuildJudgePrompt_IncludesRubric(t *testing.T) {
	prompt := BuildJudgePrompt("the output", "the rubric body")
	if !strings.Contains(prompt, "the rubric body") {
		t.Error("rubric not in prompt")
	}
	if !strings.Contains(prompt, "the output") {
		t.Error("output not in prompt")
	}
}
