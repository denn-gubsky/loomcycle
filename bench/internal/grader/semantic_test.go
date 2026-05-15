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
	r := Semantic(context.Background(), &fakeJudge{score: 85, notes: "good"}, "output", nil, cases.Semantic{
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
	r := Semantic(context.Background(), &fakeJudge{score: 60}, "output", nil, cases.Semantic{
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
	r := Semantic(context.Background(), nil, "output", nil, cases.Semantic{
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
	r := Semantic(context.Background(), &fakeJudge{err: errors.New("HTTP 503")}, "output", nil, cases.Semantic{
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
	prompt := BuildJudgePrompt("the output", nil, "the rubric body")
	if !strings.Contains(prompt, "the rubric body") {
		t.Error("rubric not in prompt")
	}
	if !strings.Contains(prompt, "the output") {
		t.Error("output not in prompt")
	}
}

// TestBuildJudgePrompt_IncludesToolCallTrace — the operator's Issue 3
// from the Sweep #6 analysis. Trace-dependent rubrics fail without
// this evidence.
func TestBuildJudgePrompt_IncludesToolCallTrace(t *testing.T) {
	tools := []ToolCallSummary{
		{Name: "mcp__jobs__getAgentContext", Args: "{}"},
		{Name: "mcp__jobs__patchApplication", Args: `{"id":"x","body":{"status":"ready"}}`},
	}
	prompt := BuildJudgePrompt("final text", tools, "rubric")
	if !strings.Contains(prompt, "mcp__jobs__getAgentContext({})") {
		t.Errorf("expected first tool call in prompt; got %q", prompt)
	}
	if !strings.Contains(prompt, "mcp__jobs__patchApplication") {
		t.Errorf("expected second tool call in prompt; got %q", prompt)
	}
}

// TestBuildJudgePrompt_NoToolsExplicitlyNoted — when the model made
// no tool calls, the prompt should say so explicitly rather than
// being silent (silence misleads the judge into assuming traces are
// hidden).
func TestBuildJudgePrompt_NoToolsExplicitlyNoted(t *testing.T) {
	prompt := BuildJudgePrompt("final text", nil, "rubric")
	if !strings.Contains(prompt, "(none — model produced no tool calls)") {
		t.Errorf("expected explicit no-tools note; got %q", prompt)
	}
}
