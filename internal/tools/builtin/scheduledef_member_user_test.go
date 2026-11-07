package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A scheduled run executes as the schedule's user_id: with that user's memory,
// documents and history. A member reads and acts on only its own
// conversations, so it may not author, fork or edit a schedule whose runs
// execute as a colleague — nor as nobody (a tenant-level run it could not
// read). A member's schedule with no user_id runs as the member. A tenant
// operator schedules runs as any user of its tenant.
func TestScheduleDef_AMemberCannotScheduleRunsAsAColleague(t *testing.T) {
	tool, operator := scheduleBearerFixture(t)
	scopes, err := auth.GrantableUserScopes("tenant")
	if err != nil {
		t.Fatal(err)
	}
	member := func(subject string) context.Context {
		ctx := tools.WithRunIdentity(operator, tools.RunIdentityValue{AgentID: "a_test", TenantID: "acme"})
		return auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: subject, Scopes: scopes})
	}
	alice := member("alice")

	if text, isErr := scheduleCreate(t, tool, alice, "as-bob", `{"agent":"digest","schedule":"0 6 * * *","user_id":"bob"}`); !isErr {
		t.Errorf("a member created a schedule that runs as bob: %s", text)
	} else if !strings.Contains(text, "user_id") {
		t.Errorf("refusal does not say why: %s", text)
	}
	if text, isErr := scheduleCreate(t, tool, alice, "own", `{"agent":"digest","schedule":"0 6 * * *","user_id":"alice"}`); isErr {
		t.Fatalf("a member's own schedule: %s", text)
	}
	if text, isErr := scheduleCreate(t, tool, alice, "unset", `{"agent":"digest","schedule":"0 6 * * *"}`); isErr {
		t.Fatalf("a member's schedule without user_id: %s", text)
	}
	if got := activeScheduleDef(t, tool, "unset").UserID; got != "alice" {
		t.Errorf("a member's schedule without user_id runs as %q, want alice", got)
	}

	// The operator's schedules: one as bob, one tenant-level.
	if text, isErr := scheduleCreate(t, tool, operator, "bobs", `{"agent":"digest","schedule":"0 6 * * *","user_id":"bob"}`); isErr {
		t.Fatalf("a tenant operator's schedule as bob: %s", text)
	}
	if text, isErr := scheduleCreate(t, tool, operator, "tenant-level", `{"agent":"digest","schedule":"0 6 * * *"}`); isErr {
		t.Fatalf("a tenant operator's tenant-level schedule: %s", text)
	}
	for _, name := range []string{"bobs", "tenant-level"} {
		row, err := tool.Store.ScheduleDefGetActive(context.Background(), "acme", name)
		if err != nil {
			t.Fatal(err)
		}
		if name == "bobs" {
			// The fork inherits user_id bob from its parent.
			res, err := tool.Execute(alice, json.RawMessage(`{"op":"fork","name":"bobs","parent_def_id":"`+row.DefID+`","overlay":{"schedule":"0 7 * * *"}}`))
			if err != nil {
				t.Fatal(err)
			}
			if !res.IsError {
				t.Errorf("a member forked bob's schedule into a version that still runs as bob: %s", res.Text)
			}
		}
		res, err := tool.Execute(alice, json.RawMessage(`{"op":"fork","name":"`+name+`","parent_def_id":"`+row.DefID+`","overlay":{"user_id":"bob"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			t.Errorf("a member forked %s naming user_id bob: %s", name, res.Text)
		}
		res, err = tool.Execute(alice, json.RawMessage(`{"op":"add_hook","def_id":"`+row.DefID+`","hook":{"kind":"memory.set","scope":"user","key":"k"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			t.Errorf("a member added a hook to %s, whose runs are not its own: %s", name, res.Text)
		}
	}
	if got := activeScheduleDef(t, tool, "bobs").UserID; got != "bob" {
		t.Errorf("the operator's schedule now runs as %q, want bob", got)
	}
}
