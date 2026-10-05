package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// scheduledTeam runs one agent state and ticks into its own `ticks` channel
// every minute.
const scheduledTeam = `{"entry":"review",
  "local":{"channels":{"ticks":{"scope":"tenant"}},
    "schedules":{"minute":{"schedule":"@every 1m","channel":"./ticks"}}},
  "states":[{"state":"review","handler":{"kind":"agent","agent":"reviewer"}},
    {"state":"done","handler":{"kind":"terminal"}}],
  "transitions":[{"from":"review","to":"done","on":"success"}]}`

// lifecycleLog records, in order, what a walk's lifecycle hooks saw.
type lifecycleLog struct {
	mu     sync.Mutex
	events []string
}

func (l *lifecycleLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *lifecycleLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.events, ",")
}

// wire installs a WalkRun and an ArmWalkTriggers that log to l, the arm
// failing with armErr when it is set.
func (l *lifecycleLog) wire(tool *TeamDef, armErr error) {
	tool.WalkRun = func(c context.Context, _ WalkRunSpec) (context.Context, string, func(WalkEnd), error) {
		l.add("open")
		return c, "r_walk1", func(end WalkEnd) {
			if end.Err != nil {
				l.add("finish:failed")
				return
			}
			l.add("finish")
		}, nil
	}
	tool.ArmWalkTriggers = func(c context.Context, def teamgraph.Definition) (func(), error) {
		if armErr != nil {
			l.add("arm:refused")
			return nil, armErr
		}
		l.add("arm:" + strings.Join(def.LocalScheduleNames(), "+"))
		return func() { l.add("disarm") }, nil
	}
}

// A walk's own schedules are armed once the walk is about to start and
// disarmed BEFORE its run is recorded as over, on success and failure alike.
func TestTeamDefRun_DisarmsTheTeamsSchedulesBeforeTheWalkEnds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spawn error
		want  string
	}{
		{"completed", nil, "open,arm:minute,disarm,finish"},
		{"failed", errors.New("member broke"), "open,arm:minute,disarm,finish:failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool, ctx, done := teamDefFixture(t)
			defer done()
			createTeam(t, tool, ctx, "clocked", scheduledTeam)
			tool.Spawn = textSpawn(func(context.Context, string, teamrun.Prompt, string) (string, error) { return "ok", tc.spawn })
			log := &lifecycleLog{}
			log.wire(tool, nil)
			_, _ = tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"clocked","input":"x"}`))
			if got := log.String(); got != tc.want {
				t.Errorf("lifecycle = %s, want %s", got, tc.want)
			}
		})
	}
}

// A walk refused before it could start never arms anything; one whose triggers
// cannot be armed is refused, and its run is closed.
func TestTeamDefRun_ArmsOnlyAWalkThatStarts(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()
	createTeam(t, tool, ctx, "clocked", scheduledTeam)
	tool.Spawn = textSpawn(func(context.Context, string, teamrun.Prompt, string) (string, error) { return "ok", nil })

	log := &lifecycleLog{}
	log.wire(tool, nil)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"clocked","input":"x","breakpoints":["nowhere"]}`))
	if !res.IsError {
		t.Fatalf("a bad breakpoint must refuse the walk: %s", res.Text)
	}
	if got := log.String(); got != "open,finish:failed" {
		t.Errorf("a refused walk's lifecycle = %s, want it never armed", got)
	}

	log = &lifecycleLog{}
	log.wire(tool, errors.New("the clock is broken"))
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"clocked","input":"x"}`))
	if !res.IsError || !strings.Contains(res.Text, "the clock is broken") {
		t.Fatalf("an arm failure must refuse the walk with its reason, got %s", res.Text)
	}
	if got := log.String(); got != "open,arm:refused,finish:failed" {
		t.Errorf("an unarmable walk's lifecycle = %s, want its run closed", got)
	}
}

// A team that declares schedules is refused on a server that cannot run them
// (its walk would wait on a clock that never ticks), and when run by an admin
// in another tenant than the team's (its own channels are not reachable there).
func TestTeamDefRun_RefusesATeamWhoseSchedulesCannotRun(t *testing.T) {
	tool, base, done := teamDefFixture(t)
	defer done()
	owner := tools.WithRunIdentity(base, tools.RunIdentityValue{AgentID: "a_test", TenantID: "acme"})
	created := createTeam(t, tool, owner, "clocked", scheduledTeam)
	tool.Spawn = textSpawn(func(context.Context, string, teamrun.Prompt, string) (string, error) { return "ok", nil })

	res, _ := tool.Execute(owner, json.RawMessage(`{"op":"run","name":"clocked","input":"x"}`))
	wantRefused(t, res, "no arming wired", "declares schedules of its own", "cannot run them")

	log := &lifecycleLog{}
	log.wire(tool, nil)
	admin := auth.WithPrincipal(tools.WithRunIdentity(base, tools.RunIdentityValue{AgentID: "a_root", TenantID: "globex"}),
		auth.Principal{TenantID: "globex", Subject: "root", Scopes: []string{auth.ScopeAdmin}})
	res, _ = tool.Execute(admin, json.RawMessage(`{"op":"run","def_id":"`+created["def_id"].(string)+`","input":"x"}`))
	wantRefused(t, res, "another tenant", "belongs to another tenant", "run it as that tenant")
	if got := log.String(); got != "" {
		t.Errorf("a walk refused for its tenant must cost no run: %s", got)
	}
}

// A team with no schedules of its own needs no arming to run.
func TestTeamDefRun_ATeamWithoutSchedulesNeedsNoArming(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()
	createTeam(t, tool, ctx, "plain", strings.Replace(scheduledTeam,
		`"schedules":{"minute":{"schedule":"@every 1m","channel":"./ticks"}}`, `"schedules":{}`, 1))
	tool.Spawn = textSpawn(func(context.Context, string, teamrun.Prompt, string) (string, error) { return "ok", nil })
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"plain","input":"x"}`))
	if res.IsError {
		t.Fatalf("run: %s", res.Text)
	}
}

func TestTeamDefCreate_RefusesALocalScheduleOnAnUndeclaredChannel(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()
	for body, want := range map[string]string{
		`{"schedule":"@every 1m","channel":"./ghost"}`:               `"./ghost" names a channel the team does not declare`,
		`{"schedule":"@every 1m","channel":"ticks"}`:                 "must name one of the team's own channels",
		`{"schedule":"@every 5s","channel":"./ticks"}`:               "at most every 10s",
		`{"schedule":"@every 1m","channel":"./ticks","max_fires":3}`: `"max_fires"`,
	} {
		overlay := strings.Replace(scheduledTeam, `{"schedule":"@every 1m","channel":"./ticks"}`, body, 1)
		wantRefused(t, teamOp(t, tool, ctx, "create", "clocked", overlay), body, `local.schedules["minute"]`, want)
	}
}

// A fork that sends local.schedules replaces them and leaves the team's other
// kinds alone; one that does not send them keeps the parent's.
func TestTeamDefFork_ReplacesOnlyTheLocalSchedules(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()
	createTeam(t, tool, ctx, "clocked", scheduledTeam)
	schedulesOf := func(text string) map[string]any {
		var out struct {
			Definition struct {
				Local struct {
					Channels  map[string]any `json:"channels"`
					Schedules map[string]any `json:"schedules"`
				} `json:"local"`
			} `json:"definition"`
		}
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatalf("decode %s: %v", text, err)
		}
		if len(out.Definition.Local.Channels) != 1 {
			t.Errorf("the team's channels changed: %v", out.Definition.Local.Channels)
		}
		return out.Definition.Local.Schedules
	}
	res := teamOp(t, tool, ctx, "fork", "clocked", `{"description":"same clock"}`)
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	if got := schedulesOf(res.Text); len(got) != 1 || got["minute"] == nil {
		t.Errorf("a fork without local.schedules must keep the parent's, got %v", got)
	}
	res = teamOp(t, tool, ctx, "fork", "clocked", `{"local":{"schedules":{"hour":{"schedule":"@hourly","channel":"./ticks"}}}}`)
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	if got := schedulesOf(res.Text); len(got) != 1 || got["hour"] == nil {
		t.Errorf("a fork that sends local.schedules must replace them, got %v", got)
	}
}

func TestValidateTeamDefBody_RefusesALocalScheduleThatCouldNotRun(t *testing.T) {
	if err := ValidateTeamDefBody(json.RawMessage(scheduledTeam)); err != nil {
		t.Fatalf("a good body must restore: %v", err)
	}
	for _, body := range []string{
		`{"schedule":"@every 1s","channel":"./ticks"}`,
		`{"schedule":"@every 1m","channel":"./ghost"}`,
		`{"schedule":"not a cadence","channel":"./ticks"}`,
	} {
		err := ValidateTeamDefBody(json.RawMessage(strings.Replace(scheduledTeam, `{"schedule":"@every 1m","channel":"./ticks"}`, body, 1)))
		if err == nil || !strings.Contains(err.Error(), `local.schedules["minute"]`) {
			t.Errorf("%s: a restored body must be refused, got %v", body, err)
		}
	}
}

// The model learns a team's own schedules from the tool's schema: it must parse
// and describe them under local.
func TestTeamDefInputSchema_DescribesLocalSchedules(t *testing.T) {
	var schema struct {
		Properties struct {
			Overlay struct {
				Properties struct {
					Local struct {
						Properties map[string]struct {
							AdditionalProperties struct {
								Properties map[string]any `json:"properties"`
								Required   []string       `json:"required"`
							} `json:"additionalProperties"`
						} `json:"properties"`
					} `json:"local"`
				} `json:"properties"`
			} `json:"overlay"`
		} `json:"properties"`
	}
	if err := json.Unmarshal((&TeamDef{}).InputSchema(), &schema); err != nil {
		t.Fatalf("the TeamDef input schema does not parse: %v", err)
	}
	sched, ok := schema.Properties.Overlay.Properties.Local.Properties["schedules"]
	if !ok {
		t.Fatal("local.schedules is not in the TeamDef input schema")
	}
	for _, field := range []string{"schedule", "channel", "payload"} {
		if _, ok := sched.AdditionalProperties.Properties[field]; !ok {
			t.Errorf("local.schedules does not describe %q", field)
		}
	}
	if !strings.Contains((&TeamDef{}).Description(), "schedules of its own") {
		t.Error("the TeamDef description does not mention a team's own schedules")
	}
}
