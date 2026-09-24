package teamrun

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// rejectingSpawn is a member run that ends rejected: no error, but its answer
// was never accepted.
func rejectingSpawn(context.Context, string, Prompt, string) (SpawnResult, error) {
	return SpawnResult{Output: "the refused answer", RunID: "r_rej", Status: MemberRejected}, nil
}

// Outside the Starter a rejected member is a failed one: its answer must not
// be threaded onward as the state's work, in any of the states that use it.
func TestRunHandler_ARejectedMemberFailsTheState(t *testing.T) {
	for name, st := range map[string]teamgraph.State{
		"agent": agentState("writer"),
		"consolidator": {ID: "judge", Handler: teamgraph.Handler{
			Kind: teamgraph.HandlerConsolidator, Agent: "judge"}},
		"agent + consolidator": {ID: "s", Handler: teamgraph.Handler{
			Kind: teamgraph.HandlerAgent, Agent: "writer", Consolidator: "judge"}},
		"parallel": {ID: "fan", Handler: teamgraph.Handler{
			Kind: teamgraph.HandlerParallel, Agents: []string{"a", "b"}, Consolidator: "judge"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := varsRunner(rejectingSpawn)
			oc, err := r.RunHandler(context.Background(), st, &Task{Input: "go"})
			if err == nil || !strings.Contains(err.Error(), "rejected") {
				t.Fatalf("outcome = %+v, err = %v; want the state to fail on the rejected member", oc, err)
			}
			if strings.Contains(oc.Output, "the refused answer") {
				t.Errorf("the refused answer was threaded onward: %q", oc.Output)
			}
		})
	}
}
