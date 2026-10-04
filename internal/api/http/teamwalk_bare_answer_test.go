package http

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// Inside a walk an agent's answer is passed on as the agent wrote it. The
// "[sub-agent agent_id=…]" line is the Agent tool's, for a parent model
// reading its own tool results; a walk carried it into the next state's
// prompt, into every envelope entry and into the walk's own result, where an
// operator read it as the team's answer.

// relayWalkTeam hands the writer's answer to the editor, whose answer is the
// walk's.
const relayWalkTeam = `{"entry":"first","states":[` +
	`{"state":"first","handler":{"kind":"agent","agent":"writer"}},` +
	`{"state":"second","handler":{"kind":"agent","agent":"editor"}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"first","to":"second","on":"success"},` +
	`{"from":"second","to":"done","on":"success"}]}`

// fanWalkTeam fans two writers out and hands their results envelope to a
// judge.
const fanWalkTeam = `{"entry":"fan","states":[` +
	`{"state":"fan","handler":{"kind":"parallel","agents":["writer","editor"],"consolidator":"judge"}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"fan","to":"done","on":"success"}]}`

// newBareAnswerHarness is a server whose every agent answers "answer N" and
// whose provider records what each was asked, with both teams promoted in
// tenant acme and the TeamDef tool wired to the real member runner.
func newBareAnswerHarness(t *testing.T) (*walkHarness, *numberedProvider) {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"writer": {Model: "stub-model", SystemPrompt: "write"},
			"editor": {Model: "stub-model", SystemPrompt: "edit"},
			"judge":  {Model: "stub-model", SystemPrompt: "judge"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "bare.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	prov := &numberedProvider{}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, 100*time.Millisecond), st)
	srv.SetTeamDefTool(&builtin.TeamDef{Store: st})
	seedTenantTeam(t, st, "acme", "relay", relayWalkTeam)
	seedTenantTeam(t, st, "acme", "fan", fanWalkTeam)
	return &walkHarness{t: t, srv: srv, st: st}, prov
}

// walkFinalText reads `final_text` from a finished walk's run result.
func walkFinalText(t *testing.T, h *walkHarness, walkID string) string {
	t.Helper()
	got := readWalkRun(t, h.srv, walkID)
	if got.Status != store.RunCompleted {
		t.Fatalf("walk status = %q (%s), want completed", got.Status, got.Error)
	}
	var rec runResultRecord
	if err := json.Unmarshal(got.Result, &rec); err != nil {
		t.Fatalf("walk result %s: %v", got.Result, err)
	}
	return rec.FinalText
}

func TestTeamWalk_NextStateReceivesThePreviousAnswerWithoutAHeader(t *testing.T) {
	h, prov := newBareAnswerHarness(t)
	walkID := h.postTeamDef(alicePrincipal, `{"op":"run","name":"relay","input":"draft"}`)
	awaitWalkEnd(t, h.st, walkID)

	seen := prov.seen()
	if len(seen) != 2 {
		t.Fatalf("the provider was called %d times (%q), want once per state", len(seen), seen)
	}
	if seen[1] != "answer 1" {
		t.Errorf("the second state was asked %q, want exactly the first state's answer %q", seen[1], "answer 1")
	}
}

func TestTeamWalk_RunResultIsTheLastAnswerWithoutAHeader(t *testing.T) {
	h, _ := newBareAnswerHarness(t)
	walkID := h.postTeamDef(alicePrincipal, `{"op":"run","name":"relay","input":"draft"}`)
	awaitWalkEnd(t, h.st, walkID)

	if got := walkFinalText(t, h, walkID); got != "answer 2" {
		t.Errorf("walk final_text = %q, want the last state's bare answer %q", got, "answer 2")
	}
}

// An envelope entry says whose answer it is with `agent` and `run_id`; its
// `output` is the answer alone.
func TestTeamWalk_EnvelopeEntryOutputIsTheBareAnswer(t *testing.T) {
	h, prov := newBareAnswerHarness(t)
	walkID := h.postTeamDef(alicePrincipal, `{"op":"run","name":"fan","input":"draft"}`)
	awaitWalkEnd(t, h.st, walkID)

	// The judge runs last, on the envelope.
	seen := prov.seen()
	if len(seen) != 3 {
		t.Fatalf("the provider was called %d times (%q), want two members and a judge", len(seen), seen)
	}
	var env struct {
		Results []struct {
			Agent  string `json:"agent"`
			RunID  string `json:"run_id"`
			Output string `json:"output"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(seen[2]), &env); err != nil || len(env.Results) != 2 {
		t.Fatalf("the judge was asked %q (%v), want a two-result envelope", seen[2], err)
	}
	for i, want := range []string{"writer", "editor"} {
		got := env.Results[i]
		if got.Agent != want || got.RunID == "" {
			t.Errorf("results[%d] = %+v, want agent %q with a run id", i, got, want)
		}
		// Which member the provider answered first is a race; either answer
		// is the member's own, and neither carries anything before it.
		if got.Output != "answer 1" && got.Output != "answer 2" {
			t.Errorf("results[%d].output = %q, want the member's bare answer", i, got.Output)
		}
	}
	if strings.Contains(seen[2], "sub-agent") {
		t.Errorf("the envelope %q carries the Agent tool's attribution line", seen[2])
	}
}
