package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/redact"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// A walk's run records which TeamDef version it ran and what it was given.
// Its name alone resolves to whatever is active NOW, so after a fork and a
// promote every earlier walk was drawn on a graph it never ran.

// walkSpecTeam reads spec.team off GET /v1/runs/{walk}, the read a viewer
// holding only the walk's run id makes. nil when the run has no team record.
func walkSpecTeam(t *testing.T, h *walkHarness, walkID string) *teamWalkRecord {
	t.Helper()
	code, got := getRun(t, h.srv.Mux(), alicePrincipal(context.Background()), walkID)
	if code != http.StatusOK {
		t.Fatalf("GET /v1/runs/%s = %d", walkID, code)
	}
	if len(got.Spec) == 0 {
		return nil
	}
	var spec struct {
		Team *teamWalkRecord `json:"team"`
	}
	if err := json.Unmarshal(got.Spec, &spec); err != nil {
		t.Fatalf("decode spec %s: %v", got.Spec, err)
	}
	return spec.Team
}

// forkAndPromote writes a second version of a team and makes it the active one,
// the way a fork with promote does.
func forkAndPromote(t *testing.T, st store.Store, tenant, name, parentDefID, defJSON string) store.TeamDefRow {
	t.Helper()
	def, err := teamgraph.Parse([]byte(defJSON))
	if err != nil {
		t.Fatalf("parse fork: %v", err)
	}
	row, err := st.TeamDefCreate(context.Background(), store.TeamDefRow{
		DefID: parentDefID + "_v2", Name: name, TenantID: tenant, ParentDefID: parentDefID,
		Definition: json.RawMessage(defJSON), ContentSHA256: teamgraph.Sign(name, def),
	})
	if err != nil {
		t.Fatalf("fork %s: %v", name, err)
	}
	if err := st.TeamDefSetActive(context.Background(), tenant, name, row.DefID, "a_test", store.TeamDefPromoter{}); err != nil {
		t.Fatalf("promote %s: %v", name, err)
	}
	return row
}

// agentOnlyTeamV2 is agentOnlyTeam with a second agent state: a different
// graph, so a different content hash.
const agentOnlyTeamV2 = `{"entry":"a","states":[` +
	`{"state":"a","handler":{"kind":"agent","agent":"writer"}},` +
	`{"state":"b","handler":{"kind":"agent","agent":"writer"}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"a","to":"b","on":"success"},{"from":"b","to":"done","on":"success"}]}`

func TestTeamWalkRecord_AWalkByNameKeepsTheVersionItRanAfterAForkIsPromoted(t *testing.T) {
	h := newWalkHarness(t)
	seedTenantTeam(t, h.st, "acme", "solo", agentOnlyTeam)
	v1, err := h.st.TeamDefGetActive(context.Background(), "acme", "solo")
	if err != nil {
		t.Fatal(err)
	}

	first := h.postTeamDef(alicePrincipal, `{"op":"run","name":"solo","input":"go"}`)
	got := walkSpecTeam(t, h, first)
	if got == nil {
		t.Fatal("the walk's run records no team — a viewer can only draw the version active now")
	}
	want := teamWalkRecord{
		Name: "solo", DefID: v1.DefID, Version: v1.Version, ContentSHA256: v1.ContentSHA256,
		DefTenant: "acme", ResolvedBy: "name", Input: "go", InputBytes: 2, Mode: "sync",
	}
	if got.Name != want.Name || got.DefID != want.DefID || got.Version != want.Version ||
		got.ContentSHA256 != want.ContentSHA256 || got.DefTenant != want.DefTenant ||
		got.ResolvedBy != want.ResolvedBy || got.Input != want.Input || got.InputBytes != want.InputBytes ||
		got.InputTruncated || got.Mode != want.Mode {
		t.Fatalf("spec.team = %+v, want %+v", *got, want)
	}

	v2 := forkAndPromote(t, h.st, "acme", "solo", v1.DefID, agentOnlyTeamV2)
	if v2.ContentSHA256 == v1.ContentSHA256 || v2.Version == v1.Version {
		t.Fatal("the fork is indistinguishable from v1, so this asserts nothing")
	}
	if again := walkSpecTeam(t, h, first); again == nil || again.DefID != v1.DefID || again.Version != v1.Version || again.ContentSHA256 != v1.ContentSHA256 {
		t.Errorf("after the promote the first walk reads %+v, want v1 (%s) still", again, v1.DefID)
	}
	second := h.postTeamDef(alicePrincipal, `{"op":"run","name":"solo","input":"go"}`)
	if rec := walkSpecTeam(t, h, second); rec == nil || rec.DefID != v2.DefID || rec.Version != v2.Version {
		t.Errorf("a walk started after the promote records %+v, want v2 (%s)", rec, v2.DefID)
	}

	// The connector read is what MCP get_run answers from.
	crun, err := h.srv.GetRunByRunID(alicePrincipal(context.Background()), first)
	if err != nil || !strings.Contains(string(crun.Spec), `"def_id":"`+v1.DefID+`"`) {
		t.Errorf("connector GetRunByRunID spec = %s (%v), want the team record", crun.Spec, err)
	}
}

// A walk an agent starts from its own run by def_id runs the version it named,
// not the active one, and says it pinned it.
func TestTeamWalkRecord_AnInBandWalkByDefIDRecordsThePinnedVersion(t *testing.T) {
	h := newWalkHarness(t)
	seedTenantTeam(t, h.st, "acme", "solo", agentOnlyTeam)
	v1, err := h.st.TeamDefGetActive(context.Background(), "acme", "solo")
	if err != nil {
		t.Fatal(err)
	}
	forkAndPromote(t, h.st, "acme", "solo", v1.DefID, agentOnlyTeamV2)

	caller := tools.WithRunID(tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{
		AgentID: "a_caller", UserID: "alice", TenantID: "acme",
	}), "r_caller")
	res, err := h.srv.teamDefTool.Execute(caller, json.RawMessage(`{"op":"run","def_id":"`+v1.DefID+`","input":"go"}`))
	if err != nil || res.IsError {
		t.Fatalf("in-band run: err=%v %s", err, res.Text)
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil || out.RunID == "" {
		t.Fatalf("no run_id in %s", res.Text)
	}
	walk, err := h.st.GetRun(context.Background(), out.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if walk.ParentRunID != "r_caller" {
		t.Fatalf("walk's parent run = %q; this is not the in-band path", walk.ParentRunID)
	}
	rec, ok := decodeRunConfig(walk.RunConfig)
	if !ok || rec.Team == nil {
		t.Fatalf("the in-band walk records no team: %s", walk.RunConfig)
	}
	if rec.Team.DefID != v1.DefID || rec.Team.Version != v1.Version || rec.Team.ResolvedBy != "def_id" {
		t.Errorf("spec.team = %+v, want the pinned v1 (%s) resolved by def_id", *rec.Team, v1.DefID)
	}
}

// A detached walk's record is written by the CreateRun that makes its row, so
// it is readable while the walk is still running — here, paused on a human.
// Its run arguments travel with it, and a retune, which a walk refuses, leaves
// the record as it was.
func TestTeamWalkRecord_ADetachedWalkIsReadableWhileItRunsAndRetuneLeavesItIntact(t *testing.T) {
	h := newWalkHarness(t)
	seedTenantTeam(t, h.st, "acme", "loop", loopTeam)
	walkID := h.postTeamDef(alicePrincipal,
		`{"op":"run","name":"loop","input":"go","interrupt_on_cap":true,"mode":"detach","review_ttl_seconds":30}`)
	pending := waitPendingInterrupt(t, h.st, walkID)
	defer func() {
		_, _ = h.srv.ResolveInterrupt(alicePrincipal(context.Background()), walkID, pending.InterruptID, "", "abort", "", "")
		waitRunEnded(t, h.st, walkID)
	}()

	if run, err := h.st.GetRun(context.Background(), walkID); err != nil || run.Status != store.RunRunning {
		t.Fatalf("walk status = %v (%v); the read under test is of a RUNNING walk", run.Status, err)
	}
	got := walkSpecTeam(t, h, walkID)
	if got == nil {
		t.Fatal("a running detached walk has no team record")
	}
	if got.Mode != "detach" || !got.InterruptOnCap || got.ReviewTTL != 30 ||
		got.Name != "loop" {
		t.Errorf("spec.team = %+v, want mode detach, interrupt_on_cap, review_ttl_seconds 30", *got)
	}

	if err := h.srv.RetuneRun(alicePrincipal(context.Background()), walkID, connector.RunOverrides{Model: "stub-model"}); err == nil {
		t.Error("a retune of a walk run was accepted; a walk runs no loop to adopt it")
	}
	if after := walkSpecTeam(t, h, walkID); after == nil || after.DefID != got.DefID || after.Mode != "detach" {
		t.Errorf("after the refused retune spec.team = %+v, want %+v", after, got)
	}
}

// The walk's input is masked before it is stored, then cut, and the cut is
// flagged with the input's real length: a secret in a walk's input must not be
// put at rest unmasked on a second surface, and a reader must be able to tell
// a cut input from a short one.
func TestTeamWalkRecord_TheInputIsMaskedAndBounded(t *testing.T) {
	h := newWalkHarness(t)
	const secret = "ghs_walkinputsecret_0123456789abcdef"
	h.srv.redactor = redact.New(map[string]string{"LOOMCYCLE_GITEA_TOKEN": secret}, true)
	seedTenantTeam(t, h.st, "acme", "solo", agentOnlyTeam)

	// Two-byte runes after a prefix whose MASKED length puts the cap inside
	// one, so a plain byte cut would split it.
	prefix := "token is " + secret + " "
	if (teamWalkInputCap-len(h.srv.redactor.String(prefix)))%2 == 0 {
		prefix += "x"
	}
	input := prefix + strings.Repeat("é", teamWalkInputCap)
	body, _ := json.Marshal(map[string]string{"op": "run", "name": "solo", "input": input})
	walkID := h.postTeamDef(alicePrincipal, string(body))

	got := walkSpecTeam(t, h, walkID)
	if got == nil {
		t.Fatal("no team record")
	}
	if strings.Contains(got.Input, secret) {
		t.Error("the walk's input is stored unmasked")
	}
	if !strings.HasPrefix(got.Input, "token is ") {
		t.Errorf("stored input starts %q, want the masked input", got.Input[:min(len(got.Input), 40)])
	}
	if !got.InputTruncated || got.InputBytes != len(input) {
		t.Errorf("input_truncated=%v input_bytes=%d, want true and %d", got.InputTruncated, got.InputBytes, len(input))
	}
	if len(got.Input) > teamWalkInputCap || len(got.Input) < teamWalkInputCap-1 {
		t.Errorf("stored input is %d bytes, want the cap (%d) less at most a partial rune", len(got.Input), teamWalkInputCap)
	}
	// JSON encodes a split rune as U+FFFD, so the decoded text is valid UTF-8
	// either way; the replacement character is what a split leaves behind.
	if !utf8.ValidString(got.Input) || strings.ContainsRune(got.Input, utf8.RuneError) {
		t.Error("the cut split a rune")
	}
}

// Every start argument a viewer needs to explain the walk reaches the record.
func TestTeamWalkRecordOf_CarriesEveryStartArgument(t *testing.T) {
	got := teamWalkRecordOf(nil, builtin.WalkRunSpec{
		Name: "t", DefID: "tdf_1", Version: 3, ContentSHA256: "abc", DefTenant: "acme", ResolvedBy: "def_id",
		Input: "go", Detach: true,
		Board:       &builtin.WalkBoard{Scope: "agent", ChunkID: "c1", ResumedFrom: "b"},
		Breakpoints: []string{"wave"}, Review: []string{"b"}, ReviewTTLSeconds: 60, InterruptOnCap: true,
	})
	b, _ := json.Marshal(got)
	const want = `{"name":"t","def_id":"tdf_1","version":3,"content_sha256":"abc","def_tenant":"acme",` +
		`"resolved_by":"def_id","input":"go","input_bytes":2,"mode":"detach",` +
		`"board":{"scope":"agent","chunk_id":"c1","resumed_from":"b"},"breakpoints":["wave"],"review":["b"],` +
		`"review_ttl_seconds":60,"interrupt_on_cap":true}`
	if string(b) != want {
		t.Errorf("record = %s\nwant      %s", b, want)
	}
}

// A walk recorded before the record existed has no spec, and reads as such
// rather than failing: its version was not recorded, and nothing guesses it.
func TestTeamWalkRecord_AWalkRecordedBeforeItReadsWithNoTeam(t *testing.T) {
	h := newWalkHarness(t)
	sess, err := h.st.CreateSession(context.Background(), "acme", "team:solo", "alice")
	if err != nil {
		t.Fatal(err)
	}
	old, err := h.st.CreateRun(context.Background(), sess.ID, store.RunIdentity{AgentID: "team:solo", UserID: "alice", TenantID: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	code, got := getRun(t, h.srv.Mux(), alicePrincipal(context.Background()), old.ID)
	if code != http.StatusOK {
		t.Fatalf("GET /v1/runs/%s = %d", old.ID, code)
	}
	if len(got.Spec) != 0 {
		t.Errorf("a pre-record walk reads spec %s, want none", got.Spec)
	}
}

// Only the runtime writes the record: a run request shaped like it — at the
// top or under spec — is not a way to claim a team version.
func TestTeamWalkRecord_ARunRequestCannotWriteATeamRecord(t *testing.T) {
	_, ts, _, st := toolChoiceServer(t)
	forged := `{"name":"solo","def_id":"tdf_forged","version":9,"resolved_by":"def_id","mode":"sync"}`
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"agent","user_id":"u1","sampling":{"temperature":0.2},"team":`+forged+`,"spec":{"team":`+forged+`},`+
			`"segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	run := onlyRun(t, st, extractSessionID(string(body)))
	rec, ok := decodeRunConfig(run.RunConfig)
	if !ok || rec.Sampling == nil {
		t.Fatalf("the request's own sampling was not recorded, so this asserts nothing: %s", run.RunConfig)
	}
	if rec.Team != nil || strings.Contains(string(run.RunConfig), "tdf_forged") {
		t.Errorf("a run request wrote a team record: %s", run.RunConfig)
	}
}
