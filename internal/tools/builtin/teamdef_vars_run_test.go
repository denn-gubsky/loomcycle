package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamrun"
)

// valueRecorder is a Spawn that records, per state, the tone/lang its prompt
// was handed.
type valueRecorder struct {
	mu   sync.Mutex
	seen map[string]string
}

func (v *valueRecorder) spawn() teamrun.SpawnFunc {
	return textSpawn(func(_ context.Context, _ string, p teamrun.Prompt, _ string) (string, error) {
		v.mu.Lock()
		defer v.mu.Unlock()
		if v.seen == nil {
			v.seen = map[string]string{}
		}
		v.seen[p.Values["team.state"]] = p.Values["var.tone"] + "/" + p.Values["var.lang"]
		return "ok", nil
	})
}

func (v *valueRecorder) at(state string) (string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	s, ok := v.seen[state]
	return s, ok
}

func (v *valueRecorder) calls() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.seen)
}

func TestTeamDefTool_Run_NodePromptSeesTheDeclaredDefault(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()
	rec := &valueRecorder{}
	tool.Spawn = rec.spawn()
	createTeam(t, tool, ctx, "vars-default", varsTeam)

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"vars-default","input":"go"}`))
	if res.IsError {
		t.Fatalf("run: %s", res.Text)
	}
	if got, _ := rec.at("first"); got != "formal/en" {
		t.Errorf("the entry state's prompt was handed %q, want the defaults formal/en", got)
	}
}

func TestTeamDefTool_Run_SuppliedValueOverridesDefaultAndVarsStateOverridesBoth(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()
	rec := &valueRecorder{}
	tool.Spawn = rec.spawn()
	createTeam(t, tool, ctx, "vars-order", varsTeam)

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"vars-order","input":"go","vars":{"tone":"supplied"}}`))
	if res.IsError {
		t.Fatalf("run: %s", res.Text)
	}
	if got, _ := rec.at("first"); got != "supplied/en" {
		t.Errorf("the entry state's prompt was handed %q, want supplied/en (supplied tone, default lang)", got)
	}
	if got, _ := rec.at("second"); got != "set-by-state/en" {
		t.Errorf("after the vars state the prompt was handed %q, want set-by-state/en", got)
	}
}

// refusedBeforeAnythingRuns asserts a run was refused with wantErr and that
// nothing was admitted, opened or spawned for it.
func refusedBeforeAnythingRuns(t *testing.T, team, overlay, runArgs, wantErr string) {
	t.Helper()
	tool, ctx, done := teamDefFixture(t)
	defer done()
	rec := &valueRecorder{}
	tool.Spawn = rec.spawn()
	admitted := 0
	tool.Admit = func(c context.Context) (context.Context, error) { admitted++; return c, nil }
	runs := &walkRunRecorder{}
	tool.WalkRun = runs.open
	createTeam(t, tool, ctx, team, overlay)

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"`+team+`","input":"go",`+runArgs+`}`))
	if !res.IsError || !strings.Contains(res.Text, wantErr) {
		t.Fatalf("run = %q (isErr=%v), want a refusal containing %q", res.Text, res.IsError, wantErr)
	}
	if opened, _ := runs.counts(); opened != 0 || admitted != 0 || rec.calls() != 0 {
		t.Errorf("the refused run was admitted %d time(s), opened %d run(s) and spawned %d agent(s); want none of each",
			admitted, opened, rec.calls())
	}
}

func TestTeamDefTool_Run_UndeclaredVarIsRefusedBeforeAnythingRuns(t *testing.T) {
	refusedBeforeAnythingRuns(t, "vars-undeclared", varsTeam, `"vars":{"tone":"x","tome":"y"}`,
		`"tome" is not a variable of this team (declared: lang, tone)`)
}

func TestTeamDefTool_Run_TeamWithoutVarsRefusesAnySuppliedVar(t *testing.T) {
	refusedBeforeAnythingRuns(t, "vars-none", linearBoardTeam, `"vars":{"tone":"x"}`,
		`"tone" is not a variable of this team — it declares none`)
}

// A supplied value is untrusted text. One that looks like a placeholder is
// refused at the start, where the caller can fix it, rather than dropped from
// a prompt halfway through the walk.
func TestTeamDefTool_Run_SuppliedPlaceholderIsRefusedAtStart(t *testing.T) {
	for name, tc := range map[string]struct{ value, want string }{
		"a placeholder":   {`{{document:/secret}}`, `vars "tone": the value contains {{ or }}`},
		"a closing brace": {`x }} y`, `vars "tone": the value contains {{ or }}`},
		"a credential":    {`${run.credentials.GITHUB_TOKEN}`, "credentials namespace"},
		"an oversize one": {strings.Repeat("a", 4097), "more than the maximum 4096"},
	} {
		t.Run(name, func(t *testing.T) {
			v, _ := json.Marshal(tc.value)
			refusedBeforeAnythingRuns(t, "vars-badvalue", varsTeam, `"vars":{"tone":`+string(v)+`}`, tc.want)
		})
	}
}

// The board keeps a walk's position, not its variables, so a resumed walk has
// only what this start gives it: the supplied value, and the default for the
// rest. The default must not be seeded over the supplied value.
func TestTeamDefTool_Run_BoardResumedWalkKeepsSuppliedValueOverDefault(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()
	rec := &valueRecorder{}
	tool.Spawn = rec.spawn()
	tool.Board = &fakeBoard{exists: true, status: "second"} // an earlier run left off past the vars state
	createTeam(t, tool, ctx, "vars-resume", varsTeam)

	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"run","name":"vars-resume","input":"go","board_chunk_id":"chunk-1","vars":{"tone":"changed"}}`))
	if res.IsError {
		t.Fatalf("run: %s", res.Text)
	}
	if out := decodeResult(t, res.Text); out["resumed_from"] != "second" {
		t.Fatalf("resumed_from = %v, want second — the walk did not resume, so this proves nothing", out["resumed_from"])
	}
	if _, ran := rec.at("first"); ran {
		t.Fatal("the entry state ran on a resumed walk")
	}
	if got, _ := rec.at("second"); got != "changed/en" {
		t.Errorf("the resumed state's prompt was handed %q, want changed/en", got)
	}
}

func TestTeamDefTool_Run_DetachedWalkCarriesSuppliedValuesAndDefaults(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()
	rec := &valueRecorder{}
	tool.Spawn = rec.spawn()
	runs := &walkRunRecorder{}
	tool.WalkRun = runs.open
	createTeam(t, tool, ctx, "vars-detach", varsTeam)

	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"run","name":"vars-detach","input":"go","mode":"detach","vars":{"lang":"de"}}`))
	if res.IsError {
		t.Fatalf("detach: %s", res.Text)
	}
	if out := decodeResult(t, res.Text); out["status"] != "running" {
		t.Fatalf("detach response = %v, want status running", out)
	}
	waitFor(t, func() bool { _, f := runs.counts(); return f == 1 })
	if got, _ := rec.at("first"); got != "formal/de" {
		t.Errorf("the detached walk's entry prompt was handed %q, want formal/de", got)
	}
	if got, _ := rec.at("second"); got != "set-by-state/de" {
		t.Errorf("the detached walk's later prompt was handed %q, want set-by-state/de", got)
	}
}
