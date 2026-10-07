package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// residentCappedTool is an Agent tool whose resident child "r_capped" ended
// at its iteration limit, as the server hands it back, and whose "r_idle" is
// parked waiting for its next send.
func residentCappedTool() *AgentTool {
	answer := func(id string) (string, string, error) {
		if id == "r_capped" {
			return "capped answer", "completed", &ChildCappedError{Name: "res", Limit: 3, RunID: id, Output: "capped answer"}
		}
		return "idle answer", "awaiting_input", nil
	}
	return &AgentTool{
		Run:          func(context.Context, string, string, string) (string, error) { return "", nil },
		LiveChildren: tools.NewLiveChildren(func() int { return 32 }),
		PollChild:    func(_ context.Context, id string, _ int) (string, string, error) { return answer(id) },
		CancelChild:  func(_ context.Context, id string) (string, string, error) { return answer(id) },
		SendChild:    func(_ context.Context, id, _ string, _ int) (string, string, error) { return answer(id) },
	}
}

// A resident child polled or cancelled by child_run_ids whose run ended at
// its iteration limit reads failed, with status max_iterations, the error and
// its last answer, as a spawned child's row does. A parked one reads idle.
func TestAgentPoll_AResidentChildCappedAtItsLimitIsAFailedRowWithItsAnswer(t *testing.T) {
	for _, op := range []string{"poll", "cancel"} {
		t.Run(op, func(t *testing.T) {
			ctx, bg := pollCtx()
			bg.AddResident("r_capped", "res")
			bg.AddResident("r_idle", "res")
			p := decodePoll(t, execJSON(t, residentCappedTool(), ctx, `{"op":"`+op+`","child_run_ids":["r_capped","r_idle"]}`))
			if len(p.Children) != 2 {
				t.Fatalf("rows = %+v, want two", p.Children)
			}
			c, i := p.Children[0], p.Children[1]
			if c.State != tools.ChildFailed || c.Status != ChildStatusMaxIterations ||
				!strings.Contains(c.Error, `sub-agent "res" stopped at its iteration limit of 3`) || c.Output != "capped answer" {
				t.Errorf("capped resident row = %+v, want failed, status max_iterations, the error and its answer", c)
			}
			if i.State != tools.ChildIdle || i.Status != "" || i.Error != "" || i.Output != "idle answer" {
				t.Errorf("parked resident row = %+v, want idle with its answer", i)
			}
		})
	}
}

// One resident child addressed by child_run_id — send, poll, cancel — whose
// run ended at its iteration limit is an error with its last answer.
func TestAgentResident_ACappedTurnIsAnErrorWithItsAnswer(t *testing.T) {
	for _, in := range []string{
		`{"op":"send","child_run_id":"r_capped","prompt":"next"}`,
		`{"op":"poll","child_run_id":"r_capped"}`,
		`{"op":"cancel","child_run_id":"r_capped"}`,
	} {
		ctx, _ := pollCtx()
		res := execJSON(t, residentCappedTool(), ctx, in)
		if !res.IsError || !strings.Contains(res.Text, `sub-agent "res" stopped at its iteration limit of 3`) ||
			!strings.Contains(res.Text, "Its last answer:\n\ncapped answer") {
			t.Errorf("%s = error:%v %q, want an error naming the limit with its answer", in, res.IsError, res.Text)
		}
	}
}
