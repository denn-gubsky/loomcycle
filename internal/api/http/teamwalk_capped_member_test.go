package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// A team member that stops at its iteration limit is a failed member to its
// walk: the error names the limit, and its answer stays on the result for the
// entries that carry a failed member's answer. Its own run stays
// completed/max_iterations. A member that finishes is unchanged.

// relayTeam hands the first agent's answer to the second.
func relayTeam(first, second string) string {
	return `{"entry":"first","states":[` +
		`{"state":"first","handler":{"kind":"agent","agent":"` + first + `"}},` +
		`{"state":"second","handler":{"kind":"agent","agent":"` + second + `"}},` +
		`{"state":"done","handler":{"kind":"terminal"}}],` +
		`"transitions":[{"from":"first","to":"second","on":"success"},` +
		`{"from":"second","to":"done","on":"success"}]}`
}

// newCappedWalkHarness is cappedServer's agents behind a walk harness, with a
// team whose first member is capped and one whose members both finish.
func newCappedWalkHarness(t *testing.T) *walkHarness {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"capped": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "you are capped", MaxIterations: 2},
		"whole":  {Model: "stub-model", Tools: []string{}, SystemPrompt: "you are whole"},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "capped-walk.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: cappingProvider{}}, []tools.Tool{}, concurrency.New(4, 4, 100*time.Millisecond), st)
	srv.SetTeamDefTool(&builtin.TeamDef{Store: st})
	seedTenantTeam(t, st, "acme", "capped-first", relayTeam("capped", "whole"))
	seedTenantTeam(t, st, "acme", "all-whole", relayTeam("whole", "whole"))
	return &walkHarness{t: t, srv: srv, st: st}
}

func TestTeamMember_ACappedMemberIsAnErrorWithItsAnswerKept(t *testing.T) {
	srv := cappedServer(t)
	res, err := srv.runTeamMember(context.Background(), "capped", teamrun.Prompt{Input: "go"}, "")
	var capped *builtin.ChildCappedError
	if !errors.As(err, &capped) || !strings.Contains(err.Error(), `sub-agent "capped" stopped at its iteration limit of 2`) {
		t.Fatalf("capped member error = %v, want one naming the limit", err)
	}
	if !res.Capped || res.Output != "capped last answer" || res.FinalText != "capped last answer" || res.RunID == "" {
		t.Errorf("capped member result = %+v, want Capped with its last answer and run id", res)
	}
	run, gerr := srv.store.GetRun(context.Background(), res.RunID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if run.Status != store.RunCompleted || run.StopReason != "max_iterations" || res.Status != string(run.Status) {
		t.Errorf("capped member's run = %s/%s (reported %q), want completed/max_iterations", run.Status, run.StopReason, res.Status)
	}

	res, err = srv.runTeamMember(context.Background(), "whole", teamrun.Prompt{Input: "go"}, "")
	if err != nil || res.Capped || res.Output != "whole answer" || res.Status != string(store.RunCompleted) {
		t.Errorf("finished member = %+v, %v; want its answer and no error", res, err)
	}
}

// A walk whose member stops at its iteration limit ends failed, as it does
// for a failed member, with the error naming the limit — waited for or
// detached. A walk whose members finish completes as before.
func TestTeamWalk_ACappedMemberFailsTheWalk(t *testing.T) {
	h := newCappedWalkHarness(t)

	// Waited for: op=run answers with the walk's error.
	r := httptest.NewRequest(http.MethodPost, "/v1/_teamdef", strings.NewReader(`{"op":"run","name":"capped-first","input":"go"}`))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.srv.handleSubstrateTeamDef(rr, r.WithContext(alicePrincipal(r.Context())))
	if rr.Code == http.StatusOK || !strings.Contains(rr.Body.String(), `sub-agent \"capped\" stopped at its iteration limit of 2`) {
		t.Errorf("op=run of a walk with a capped member = %d %s, want its error naming the limit", rr.Code, rr.Body)
	}

	// Detached: the walk's run ends failed with that error.
	walkID := h.postTeamDef(alicePrincipal, `{"op":"run","name":"capped-first","input":"go","mode":"detach"}`)
	awaitWalkEnd(t, h.st, walkID)
	got := readWalkRun(t, h.srv, walkID)
	if got.Status != store.RunFailed || !strings.Contains(got.Error, `sub-agent "capped" stopped at its iteration limit of 2`) {
		t.Errorf("walk with a capped member = %s (%q), want failed naming the limit", got.Status, got.Error)
	}
	if strings.Contains(got.Error, "iteration_cap") {
		t.Errorf("walk error %q reads as the walk's own iteration cap", got.Error)
	}

	walkID = h.postTeamDef(alicePrincipal, `{"op":"run","name":"all-whole","input":"go","mode":"detach"}`)
	awaitWalkEnd(t, h.st, walkID)
	if final := walkFinalText(t, h, walkID); final != "whole answer" {
		t.Errorf("walk whose members finish answered %q, want %q", final, "whole answer")
	}
}
