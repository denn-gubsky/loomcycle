package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// authoringCtx is a teamdef ctx whose principal holds the given channel grants,
// so the team-ACL narrowing check has something to narrow FROM.
func authoringCtx(publish, subscribe []string) context.Context {
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_test"})
	return tools.WithChannelPolicy(ctx, tools.ChannelPolicyValue{
		Publish:   publish,
		Subscribe: subscribe,
	})
}

const starterGraph = `{"entry":"wave","states":[` +
	`{"state":"wave","handler":{"kind":"starter","source":{"channel":"pr-events"},` +
	`"fanout":{"agent":"reviewer","per":"message","max":4},"sink":{"channel":"verdicts"}}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"wave","to":"done","on":"success"}]}`

// A fork that supplies ONLY channels must keep them. Every Definition field
// needs its own applyTeamOverlay case; Layout was added without one and a fork
// returned 200, minted a version and silently dropped every node position.
// Dropping the ACL instead would hand a forked workflow no channel authority.
func TestTeamDef_ForkKeepsTheChannelACL(t *testing.T) {
	tool, _, cleanup := teamDefFixture(t)
	defer cleanup()
	ctx := authoringCtx([]string{"verdicts"}, []string{"pr-events"})

	created := createTeam(t, tool, ctx, "triage",
		strings.TrimSuffix(starterGraph, "}")+`,"channels":{"publish":["verdicts"],"subscribe":["pr-events"]}}`)
	_ = created

	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"fork","name":"triage","overlay":{"max_iterations":7}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	forked := decodeResult(t, res.Text)
	defID, _ := forked["def_id"].(string)

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"get","def_id":"`+defID+`"}`))
	if res.IsError {
		t.Fatalf("get: %s", res.Text)
	}
	def, ok := decodeResult(t, res.Text)["definition"].(map[string]any)
	if !ok {
		t.Fatalf("no definition in the forked def")
	}
	chans, ok := def["channels"].(map[string]any)
	if !ok {
		t.Fatalf("the fork DROPPED the channel ACL: %v", def)
	}
	sub, _ := chans["subscribe"].([]any)
	if len(sub) != 1 || sub[0] != "pr-events" {
		t.Errorf("subscribe = %v, want [pr-events]", chans["subscribe"])
	}
}

// Trust rule 4: a team ACL may only NARROW what its author holds. The field is
// only data; what makes it authority is that the Starter reads and publishes
// under it rather than under each agent's grants, so an author who could name a
// channel they cannot reach would escalate by authoring a definition.
func TestTeamDef_ChannelACLCannotWidenTheAuthorsOwn(t *testing.T) {
	for _, c := range []struct {
		name    string
		pub     []string
		sub     []string
		acl     string
		wantErr string
	}{
		{"narrower is fine", []string{"verdicts", "other"}, []string{"pr-events", "more"},
			`{"publish":["verdicts"],"subscribe":["pr-events"]}`, ""},
		{"equal is fine", []string{"verdicts"}, []string{"pr-events"},
			`{"publish":["verdicts"],"subscribe":["pr-events"]}`, ""},
		{"wider subscribe is refused", []string{"verdicts"}, []string{"pr-events"},
			`{"subscribe":["pr-events","secrets"]}`, "may only narrow"},
		{"wider publish is refused", []string{"verdicts"}, []string{"pr-events"},
			`{"publish":["verdicts","payouts"]}`, "may only narrow"},
		// No policy is default-deny, not "no limit".
		{"no policy at all cannot declare one", nil, nil,
			`{"subscribe":["pr-events"]}`, "may only narrow"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tool, _, cleanup := teamDefFixture(t)
			defer cleanup()
			ctx := authoringCtx(c.pub, c.sub)
			res, _ := tool.Execute(ctx, json.RawMessage(
				`{"op":"create","name":"t","overlay":`+strings.TrimSuffix(starterGraph, "}")+`,"channels":`+c.acl+`}}`))
			if c.wantErr == "" {
				if res.IsError {
					t.Fatalf("create refused a narrowing ACL: %s", res.Text)
				}
				return
			}
			if !res.IsError {
				t.Fatalf("create accepted a widening ACL")
			}
			if !strings.Contains(res.Text, c.wantErr) {
				t.Errorf("refusal = %q, want it to mention %q", res.Text, c.wantErr)
			}
		})
	}
}

// The same check runs on FORK, because a fork is an authoring act by whoever
// forks — not by whoever wrote the parent.
func TestTeamDef_ForkCannotWidenTheChannelACL(t *testing.T) {
	tool, _, cleanup := teamDefFixture(t)
	defer cleanup()
	broad := authoringCtx([]string{"verdicts"}, []string{"pr-events"})
	createTeam(t, tool, broad, "triage",
		strings.TrimSuffix(starterGraph, "}")+`,"channels":{"subscribe":["pr-events"]}}`)

	res, _ := tool.Execute(broad, json.RawMessage(
		`{"op":"fork","name":"triage","overlay":{"channels":{"subscribe":["pr-events","secrets"]}}}`))
	if !res.IsError {
		t.Fatalf("a fork widened the team ACL")
	}
	if !strings.Contains(res.Text, "may only narrow") {
		t.Errorf("refusal = %q, want it to mention narrowing", res.Text)
	}
}
