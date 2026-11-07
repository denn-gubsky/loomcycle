package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
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

// A run carries no principal when a schedule fires it, a webhook delivers to
// it, it resumes, or an A2A peer starts it. Such a run writing a schedule is
// held to its own user, as a member is, unless the run's recorded owner is a
// tenant operator: otherwise a member schedules an agent whose run writes the
// schedule the member may not write itself.
func TestScheduleDef_ARunWithNoPrincipalSchedulesOnlyAsItsOwnUser(t *testing.T) {
	tool, operator := scheduleBearerFixture(t)
	// inRunOwnedBy is a run executing as user, fired by a trigger owner
	// recorded with ownerClass (none when ownerClass is "").
	inRunOwnedBy := func(user, owner, ownerClass string) context.Context {
		ctx := tools.WithAgentName(context.Background(), "scheduler-orchestrator")
		ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_run", TenantID: "acme", UserID: user})
		ctx = tools.WithScheduleDefPolicy(ctx, tools.ScheduleDefPolicyValue{Scopes: []string{"any"}, SelfName: "scheduler-orchestrator"})
		ctx = tools.WithRunID(ctx, "r_"+user)
		if ownerClass != "" {
			ctx = tools.UpdateRunPolicy(ctx, func(p *tools.RunPolicy) {
				p.ACL = &authz.Effective{Subject: authz.Subject{TenantID: "acme", ID: owner, Class: ownerClass}}
			})
		}
		return ctx
	}
	inRun := func(user, ownerClass string) context.Context { return inRunOwnedBy(user, user, ownerClass) }

	for _, class := range []string{"", "member", "isolated"} {
		alicesRun := inRun("alice", class)
		if text, isErr := scheduleCreate(t, tool, alicesRun, "as-bob-"+class, `{"agent":"digest","schedule":"0 6 * * *","user_id":"bob"}`); !isErr {
			t.Errorf("a run of alice's (owner class %q) created a schedule that runs as bob: %s", class, text)
		}
		if text, isErr := scheduleCreate(t, tool, alicesRun, "own-"+class, `{"agent":"digest","schedule":"0 6 * * *"}`); isErr {
			t.Fatalf("a run of alice's (owner class %q) scheduling as alice: %s", class, text)
		}
		if got := activeScheduleDef(t, tool, "own-"+class).UserID; got != "alice" {
			t.Errorf("a schedule a run of alice's wrote without user_id runs as %q, want alice", got)
		}
	}

	// An ownerless tenant-level run (no user) keeps writing tenant-level
	// schedules; a member's run does not, as a member does not off-run.
	if text, isErr := scheduleCreate(t, tool, inRun("", ""), "tenant-level", `{"agent":"digest","schedule":"0 6 * * *"}`); isErr {
		t.Errorf("an ownerless tenant-level run's tenant-level schedule: %s", text)
	}
	if text, isErr := scheduleCreate(t, tool, inRunOwnedBy("", "alice", "member"), "members-tenant-level", `{"agent":"digest","schedule":"0 6 * * *"}`); isErr {
		t.Fatalf("a tenant-level run alice owns, scheduling with no user_id: %s", text)
	}
	if got := activeScheduleDef(t, tool, "members-tenant-level").UserID; got != "alice" {
		t.Errorf("a schedule a run alice owns wrote without user_id runs as %q, want alice", got)
	}

	// A run alice owns that executes as bob (a schedule she stored before
	// this rule) is held to alice, not to the user it runs as.
	if text, isErr := scheduleCreate(t, tool, inRunOwnedBy("bob", "alice", "member"), "still-as-bob", `{"agent":"digest","schedule":"0 6 * * *","user_id":"bob"}`); !isErr {
		t.Errorf("a run alice owns, executing as bob, created a schedule that runs as bob: %s", text)
	}

	// A run a tenant operator owns may schedule as any user of its tenant,
	// as the operator may off-run.
	if text, isErr := scheduleCreate(t, tool, inRun("alice", "tenant"), "op-run-as-bob", `{"agent":"digest","schedule":"0 6 * * *","user_id":"bob"}`); isErr {
		t.Errorf("a run a tenant operator owns scheduling as bob: %s", text)
	}

	// A hook edit from a member-owned run on the operator's schedule as bob.
	if text, isErr := scheduleCreate(t, tool, operator, "bobs", `{"agent":"digest","schedule":"0 6 * * *","user_id":"bob"}`); isErr {
		t.Fatalf("a tenant operator's schedule as bob: %s", text)
	}
	row, err := tool.Store.ScheduleDefGetActive(context.Background(), "acme", "bobs")
	if err != nil {
		t.Fatal(err)
	}
	res, err := tool.Execute(inRun("alice", "member"), json.RawMessage(`{"op":"add_hook","def_id":"`+row.DefID+`","hook":{"kind":"memory.set","scope":"user","key":"k"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("a run of alice's added a hook to bob's schedule: %s", res.Text)
	}
}
