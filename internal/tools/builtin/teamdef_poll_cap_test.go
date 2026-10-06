package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Two agent states: each step has an output, and the walk's final output is
// the second's.
const twoStepTeam = `{"entry":"draft","states":[` +
	`{"state":"draft","handler":{"kind":"agent","agent":"drafter"}},` +
	`{"state":"edit","handler":{"kind":"agent","agent":"editor"}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"draft","to":"edit","on":"success"},{"from":"edit","to":"done","on":"success"}]}`

// TeamDef poll shares a quarter of the caller's window between the walks it
// answers, as Agent poll does between its rows. Within a walk the final output
// keeps up to the walk's whole share and the steps' outputs split what is
// left; a cut walk says truncated, and the table keeps the whole answer. A
// walk within its share is untouched, and a waited-for run is never cut.
func TestTeamDefPoll_AnswersAreCutToAShareOfAQuarterWindow(t *testing.T) {
	tool, ctx, bg, _, _, done := pollFixture(t)
	defer done()
	createTeam(t, tool, ctx, "pair", twoStepTeam)
	size := map[string]int{"drafter": 300, "editor": 900}
	tool.Spawn = textSpawn(func(_ context.Context, agent string, _ teamrun.Prompt, _ string) (string, error) {
		return strings.Repeat(agent[:1], size[agent]), nil
	})
	start := func() string {
		res := runTD(t, tool, ctx, `{"op":"run","name":"pair","input":"x","mode":"poll"}`)
		if res.IsError {
			t.Fatalf("poll-mode run: %s", res.Text)
		}
		id := decodeResult(t, res.Text)["run_id"].(string)
		waitUntil(t, func() bool { v, _ := bg.Lookup(id); return v.Ended() })
		return id
	}
	a, b := start(), start()

	// A quarter of 1000 shared by two walks: 500 each. editor's 900 is cut to
	// 500, leaving nothing for the steps.
	small := tools.WithEffectiveContextWindow(ctx, 1000)
	p := decodeWalkPoll(t, runTD(t, tool, small, `{"op":"poll","run_ids":["`+a+`","`+b+`"]}`))
	for _, w := range p.Walks {
		if w["truncated"] != true || len(w["final_output"].(string)) != 500 {
			t.Errorf("walk %v: truncated=%v final_output %d chars, want cut to 500", w["run_id"], w["truncated"], len(w["final_output"].(string)))
		}
		for _, s := range w["steps"].([]any) {
			if out := s.(map[string]any)["output"]; out != "" {
				t.Errorf("step output %d chars, want none left for it", len(out.(string)))
			}
		}
	}

	// One walk, a quarter of 1600: final output whole (900), the two steps
	// split the 700 left — drafter's 300 fits, editor's is cut to 350.
	one := tools.WithEffectiveContextWindow(ctx, 1600)
	w := decodeWalkPoll(t, runTD(t, tool, one, `{"op":"poll","run_ids":["`+a+`"]}`)).Walks[0]
	steps := w["steps"].([]any)
	if w["truncated"] != true || len(w["final_output"].(string)) != 900 ||
		len(steps[0].(map[string]any)["output"].(string)) != 300 || len(steps[1].(map[string]any)["output"].(string)) != 350 {
		t.Errorf("one walk under 1600: truncated=%v final %d, steps %v", w["truncated"], len(w["final_output"].(string)), steps)
	}

	// Within the share: untouched, and the table's copy was never cut.
	big := tools.WithEffectiveContextWindow(ctx, 100000)
	w = decodeWalkPoll(t, runTD(t, tool, big, `{"op":"poll","run_ids":["`+a+`"]}`)).Walks[0]
	steps = w["steps"].([]any)
	if w["truncated"] != nil || len(w["final_output"].(string)) != 900 || len(steps[1].(map[string]any)["output"].(string)) != 900 {
		t.Errorf("walk within its share = truncated %v, final %d", w["truncated"], len(w["final_output"].(string)))
	}

	// A waited-for run answers one walk, whole.
	sync := decodeResult(t, runTD(t, tool, small, `{"op":"run","name":"pair","input":"x"}`).Text)
	if len(sync["final_output"].(string)) != 900 || sync["truncated"] != nil {
		t.Errorf("waited-for run final_output %d chars, truncated %v; want whole", len(sync["final_output"].(string)), sync["truncated"])
	}
}
