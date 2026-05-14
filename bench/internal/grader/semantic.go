package grader

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/denn-gubsky/loomcycle/bench/internal/cases"
)

// Judge is the interface the semantic grader needs: hand it a
// rubric + a candidate output, get back a 0..100 score (and free-form
// notes). Production implementation is an Anthropic call; tests pass
// a fake.
type Judge interface {
	Score(ctx context.Context, prompt string) (int, string, error)
}

// Semantic grades the candidate text by handing it to the judge with
// the case's rubric. Returns Pass=(score>=threshold) and score
// normalised to 0..1.
func Semantic(ctx context.Context, judge Judge, finalText string, exp cases.Semantic) AxisResult {
	if exp.Rubric == "" {
		// No rubric = trivial pass. Used by cases that don't care
		// about content quality (e.g., a pure tool-routing test).
		return AxisResult{Pass: true, Score: 1.0}
	}
	if judge == nil {
		// Bench was invoked with semantic grading disabled. Emit
		// a passing result so other axes still count.
		return AxisResult{
			Pass:    true,
			Score:   1.0,
			Reasons: []string{"semantic grading disabled (no judge wired)"},
		}
	}
	prompt := BuildJudgePrompt(finalText, exp.Rubric)
	score, notes, err := judge.Score(ctx, prompt)
	if err != nil {
		return AxisResult{
			Pass:    false,
			Score:   0,
			Reasons: []string{"judge call failed: " + err.Error()},
		}
	}
	r := AxisResult{Score: float64(score) / 100.0}
	r.Pass = score >= exp.Threshold
	if notes != "" {
		r.Reasons = append(r.Reasons, fmt.Sprintf("judge score %d/100: %s", score, notes))
	} else {
		r.Reasons = append(r.Reasons, fmt.Sprintf("judge score %d/100", score))
	}
	return r
}

// BuildJudgePrompt assembles the rubric prompt sent to the judge.
// Exposed for tests + so the cmd layer can dump it on --verbose.
func BuildJudgePrompt(finalText, rubric string) string {
	var b strings.Builder
	b.WriteString("You are a strict but fair benchmark judge.\n\n")
	b.WriteString("Below is a candidate model's output for a capability test. ")
	b.WriteString("Score it 0..100 against the rubric. Use the full range; ")
	b.WriteString("65 is a marginal borderline pass and 100 is reserved for ")
	b.WriteString("clearly correct output with no nits.\n\n")
	b.WriteString("Output ONE line of JSON: {\"score\": <int 0-100>, \"notes\": \"<one sentence>\"}.\n")
	b.WriteString("No prose around the JSON. No code fences. The first character must be `{`.\n\n")
	b.WriteString("RUBRIC:\n")
	b.WriteString(strings.TrimSpace(rubric))
	b.WriteString("\n\nCANDIDATE OUTPUT (between <BEGIN> and <END>):\n<BEGIN>\n")
	// Bound the candidate text to keep judge input from exploding.
	b.WriteString(truncateForJudge(finalText, 8000))
	b.WriteString("\n<END>\n")
	return b.String()
}

// ParseJudgeResponse extracts {score, notes} from the judge's reply.
// Tolerant of leading/trailing prose, fences, missing notes — judges
// drift even at temperature 0. Returns (-1, "") when no score could
// be parsed.
func ParseJudgeResponse(reply string) (int, string) {
	body := stripCodeFences(reply)
	// Try strict JSON first.
	var strict struct {
		Score int    `json:"score"`
		Notes string `json:"notes"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &strict); err == nil && strict.Score > 0 {
		return clampScore(strict.Score), strict.Notes
	}
	// Fallback: find the first JSON object substring and try again.
	if idx := strings.IndexByte(body, '{'); idx >= 0 {
		if end := strings.LastIndexByte(body, '}'); end > idx {
			if err := json.Unmarshal([]byte(body[idx:end+1]), &strict); err == nil && strict.Score > 0 {
				return clampScore(strict.Score), strict.Notes
			}
		}
	}
	// Last resort: regex for `"score":\s*<int>`.
	re := regexp.MustCompile(`"score"\s*:\s*(\d{1,3})`)
	m := re.FindStringSubmatch(body)
	if len(m) == 2 {
		n, err := strconv.Atoi(m[1])
		if err == nil {
			return clampScore(n), ""
		}
	}
	return -1, ""
}

func clampScore(s int) int {
	if s < 0 {
		return 0
	}
	if s > 100 {
		return 100
	}
	return s
}

func truncateForJudge(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n\n...[truncated for judge]..."
}
