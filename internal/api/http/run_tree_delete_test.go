package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// DELETE /v1/runs/{run_id}/tree and DELETE /v1/sessions/{id}?cascade=children
// delete one run tree. These tests go through the real mux AND the real auth
// middleware with stored tokens: the scope gate is part of what they assert.

const treeCanary = "CANARY-7f3a91-do-not-keep"

// Tokens the tree-delete server knows, by what they are.
const (
	tokLegacy      = "legacy-shared-secret"
	tokOperator    = "lct_acme_operator"    // substrate:tenant in acme
	tokOtherTenant = "lct_other_operator"   // substrate:tenant in another tenant
	tokMember      = "lct_acme_member"      // a tenant-mode member's delegated scopes
	tokMemberDel   = "lct_acme_member_del"  // the same, plus runs:delete granted at mint
	tokIsolated    = "lct_acme_isolated"    // substrate:user, subject bob
	tokIsolatedDel = "lct_acme_isolated_dl" // substrate:user + runs:delete, subject dana
	tokReadOnly    = "lct_acme_readonly"    // runs:read only
)

func treeDeleteServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:   map[string]config.AgentDef{"writer": {Model: "stub-model", SystemPrompt: "write"}},
	}
	cfg.Env.AuthToken = tokLegacy
	cfg.Env.OperatorTokenPepper = testTokenPepper
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "treedelete.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	member, err := auth.GrantableUserScopes("tenant")
	if err != nil {
		t.Fatal(err)
	}
	seedToken(t, st, tokOperator, "acme", "op", []string{auth.ScopeTenant}, time.Time{})
	seedToken(t, st, tokOtherTenant, "other", "op2", []string{auth.ScopeTenant}, time.Time{})
	seedToken(t, st, tokMember, "acme", "alice", member, time.Time{})
	seedToken(t, st, tokMemberDel, "acme", "carol", append(append([]string{}, member...), auth.ScopeRunsDelete), time.Time{})
	seedToken(t, st, tokIsolated, "acme", "bob", []string{auth.ScopeUser}, time.Time{})
	seedToken(t, st, tokIsolatedDel, "acme", "dana", []string{auth.ScopeUser, auth.ScopeRunsDelete}, time.Time{})
	seedToken(t, st, tokReadOnly, "acme", "reader", []string{auth.ScopeRunsRead}, time.Time{})
	srv := New(cfg, &stubResolver{p: &numberedProvider{}}, []tools.Tool{}, concurrency.New(4, 4, 100*time.Millisecond), st)
	return srv, srv.Mux()
}

type treeDeleteBody struct {
	connector.RunTreeDeletion
	Code  string `json:"code"`
	Error string `json:"error"`
	// LiveRunID is the run a run_tree_live refusal names; on a success the
	// same key is the root that was asked for.
}

func doDelete(t *testing.T, mux http.Handler, token, path string) (int, treeDeleteBody, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var body treeDeleteBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body, rec.Body.String()
}

// seedTreeRun writes one run of (tenant, user) under parent, in a session of
// its own, with two events carrying the canary and one usage row; done
// finishes it.
func seedTreeRun(t *testing.T, st store.Store, tenant, user, agent, parent string, done bool) store.Run {
	t.Helper()
	ctx := context.Background()
	r := addRun(t, st, tenant, user, agent, parent)
	for _, typ := range []string{"user_input", "text"} {
		if err := st.AppendEvent(ctx, r.ID, typ, []byte(`{"text":"`+treeCanary+` in `+agent+`"}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RecordCallUsage(ctx, store.TokenUsageRow{
		RunID: r.ID, SessionID: r.SessionID, TenantID: tenant, UserID: user, Provider: "p", Model: "m",
		CredentialSource: "operator", InputTokens: 7,
	}); err != nil {
		t.Fatal(err)
	}
	if done {
		finishTreeRun(t, st, r.ID)
	}
	return r
}

func finishTreeRun(t *testing.T, st store.Store, runID string) {
	t.Helper()
	if err := st.FinishRun(context.Background(), runID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
}

// eventsHolding lists the events table and returns how many payloads hold s.
func eventsHolding(t *testing.T, st store.Store, s string) (holding, total int) {
	t.Helper()
	events, _, err := st.ListEvents(context.Background(), store.EventFilter{}, 10000, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if strings.Contains(string(ev.Payload), s) {
			holding++
		}
	}
	return holding, len(events)
}

func runExists(t *testing.T, st store.Store, runID string) bool {
	t.Helper()
	_, err := st.GetRun(context.Background(), runID)
	var nf *store.ErrNotFound
	if err != nil && !errors.As(err, &nf) {
		t.Fatal(err)
	}
	return err == nil
}

func sessionExists(t *testing.T, st store.Store, id string) bool {
	t.Helper()
	_, err := st.GetSession(context.Background(), id)
	var nf *store.ErrNotFound
	if err != nil && !errors.As(err, &nf) {
		t.Fatal(err)
	}
	return err == nil
}

func setTrace(t *testing.T, st store.Store, tenant, user, key string, v traceTurnValue) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MemorySet(context.Background(), tenant, store.MemoryScopeUser, user, key, raw, time.Hour); err != nil {
		t.Fatal(err)
	}
}

func treeTraceKeys(t *testing.T, st store.Store, tenant, user string) map[string]bool {
	t.Helper()
	rows, _, err := st.MemoryList(context.Background(), tenant, store.MemoryScopeUser, user, store.TraceTurnKeyPrefix, 1000)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, r := range rows {
		out[r.Key] = true
	}
	return out
}

func TestDeleteRunTree_RemovesTheRunItsDescendantsAndTheirSessions(t *testing.T) {
	srv, mux := treeDeleteServer(t)
	st := srv.store
	ctx := context.Background()
	parent := seedTreeRun(t, st, "acme", "alice", "lead", "", true)
	child := seedTreeRun(t, st, "acme", "alice", "worker", parent.ID, true)
	grandchild := seedTreeRun(t, st, "acme", "alice", "helper", child.ID, true)
	unrelated := seedTreeRun(t, st, "acme", "alice", "elsewhere", "", true)
	if _, err := st.InterruptCreate(ctx, store.InterruptRow{
		InterruptID: store.MintInterruptID(time.Now()), RunID: child.ID, UserID: "alice",
		Question: treeCanary, Priority: store.InterruptPriorityNormal, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EvaluationSubmit(ctx, store.EvaluationRow{
		EvalID: "ev_tree_1", RunID: parent.ID, Score: 1, EmitterRole: "external", Rationale: treeCanary,
	}); err != nil {
		t.Fatal(err)
	}
	// The trace index: one turn of the child, one of the unrelated run.
	childTrace := store.TraceTurnKeyPrefix + child.SessionID + ":1-1"
	keptTrace := store.TraceTurnKeyPrefix + unrelated.SessionID + ":1-1"
	setTrace(t, st, "acme", "alice", childTrace, traceTurnValue{Text: treeCanary, Speaker: "user", SessionID: child.SessionID, RunID: child.ID})
	setTrace(t, st, "acme", "alice", keptTrace, traceTurnValue{Text: "kept", Speaker: "user", SessionID: unrelated.SessionID, RunID: unrelated.ID})

	if holding, _ := eventsHolding(t, st, treeCanary); holding != 8 {
		t.Fatalf("%d events hold the canary before the delete, want 8 (the check below could not fail)", holding)
	}

	code, body, raw := doDelete(t, mux, tokOperator, "/v1/runs/"+parent.ID+"/tree")
	if code != http.StatusOK {
		t.Fatalf("DELETE tree = %d: %s", code, raw)
	}
	want := map[string]int{
		"runs": 3, "events": 6, "sessions": 3, "session_embeddings": 0,
		"interrupts": 1, "evaluations": 1, "memory_pending": 0, "trace_index": 1,
	}
	if body.DeletedRuns != 3 || fmt.Sprint(body.Counts) != fmt.Sprint(want) {
		t.Errorf("deleted_runs %d counts %v, want 3 and %v", body.DeletedRuns, body.Counts, want)
	}
	if body.RunID != parent.ID || body.Kept["usage_ledger"] == "" {
		t.Errorf("answer names run %q and keeps %v; want the root and the usage ledger", body.RunID, body.Kept)
	}
	reached := map[string]bool{}
	for _, nr := range body.NotReached {
		reached[nr.Name] = true
		if nr.Reason == "" {
			t.Errorf("not_reached %q gives no reason", nr.Name)
		}
	}
	for _, name := range []string{"snapshots", "retention_exports", "derived_facts"} {
		if !reached[name] {
			t.Errorf("not_reached does not name %q: %s", name, raw)
		}
	}

	// Listed, not taken from the counts.
	for _, r := range []store.Run{parent, child, grandchild} {
		if runExists(t, st, r.ID) {
			t.Errorf("run %s (%s) is still stored", r.ID, r.AgentID)
		}
		if sessionExists(t, st, r.SessionID) {
			t.Errorf("the session of run %s (%s) is still stored", r.ID, r.AgentID)
		}
		if usage, err := st.TokenUsageForRun(ctx, r.ID); err != nil || len(usage) != 1 {
			t.Errorf("usage rows of deleted run %s = %d (err %v), want the 1 kept", r.ID, len(usage), err)
		}
	}
	holding, total := eventsHolding(t, st, treeCanary)
	if holding != 2 || total != 2 {
		t.Errorf("events table holds %d rows, %d with the canary; want only the unrelated run's 2", total, holding)
	}
	if !runExists(t, st, unrelated.ID) || !sessionExists(t, st, unrelated.SessionID) {
		t.Error("the unrelated run or its session was deleted")
	}
	if ints, _ := st.InterruptListByRun(ctx, child.ID, ""); len(ints) != 0 {
		t.Errorf("%d interrupts of the deleted child are still stored", len(ints))
	}
	if evs, _ := st.EvaluationListForRun(ctx, parent.ID, 10); len(evs) != 0 {
		t.Errorf("%d evaluations of the deleted parent are still stored", len(evs))
	}
	keys := treeTraceKeys(t, st, "acme", "alice")
	if keys[childTrace] || !keys[keptTrace] {
		t.Errorf("trace index holds %v; want the unrelated run's turn only", keys)
	}

	// A second call is not an error and deletes nothing.
	code, again, raw := doDelete(t, mux, tokOperator, "/v1/runs/"+parent.ID+"/tree")
	if code != http.StatusOK || again.DeletedRuns != 0 {
		t.Fatalf("second DELETE = %d deleted_runs %d, want 200 and 0: %s", code, again.DeletedRuns, raw)
	}
	for table, n := range again.Counts {
		if n != 0 {
			t.Errorf("second DELETE counts %s = %d, want 0", table, n)
		}
	}
	if len(again.Counts) != len(want) || len(again.NotReached) != 0 {
		t.Errorf("second DELETE: counts %v not_reached %v; want every table at zero and nothing unreached", again.Counts, again.NotReached)
	}
	if !runExists(t, st, unrelated.ID) {
		t.Error("the second call deleted the unrelated run")
	}
}

// A tree with a live run in it is refused whole: the caller cancels first.
func TestDeleteRunTree_RefusesATreeWithALiveRunAndDeletesNothing(t *testing.T) {
	srv, mux := treeDeleteServer(t)
	st := srv.store
	parent := seedTreeRun(t, st, "acme", "alice", "lead", "", true)
	child := seedTreeRun(t, st, "acme", "alice", "worker", parent.ID, false) // still running

	code, body, raw := doDelete(t, mux, tokOperator, "/v1/runs/"+parent.ID+"/tree")
	var named struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal([]byte(raw), &named)
	if code != http.StatusConflict || body.Code != "run_tree_live" || named.RunID != child.ID {
		t.Fatalf("DELETE = %d code %q naming %q; want 409 run_tree_live naming the running child %s: %s",
			code, body.Code, named.RunID, child.ID, raw)
	}
	if !runExists(t, st, parent.ID) || !runExists(t, st, child.ID) {
		t.Error("a run was deleted by a refused call")
	}
	if holding, _ := eventsHolding(t, st, treeCanary); holding != 4 {
		t.Errorf("%d events hold the canary after a refused call, want all 4", holding)
	}
	// A paused run is live too, whatever its status.
	finishTreeRun(t, st, child.ID)
	if err := st.SetRunPauseState(context.Background(), child.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	if code, body, raw := doDelete(t, mux, tokOperator, "/v1/runs/"+parent.ID+"/tree"); code != http.StatusConflict || body.Code != "run_tree_live" {
		t.Errorf("DELETE with a paused child = %d %q, want 409 run_tree_live: %s", code, body.Code, raw)
	}
	if err := st.SetRunPauseState(context.Background(), child.ID, store.PauseStateRunning); err != nil {
		t.Fatal(err)
	}
	if code, body, raw := doDelete(t, mux, tokOperator, "/v1/runs/"+parent.ID+"/tree"); code != http.StatusOK || body.DeletedRuns != 2 {
		t.Errorf("DELETE once nothing is live = %d deleted_runs %d, want 200 and 2: %s", code, body.DeletedRuns, raw)
	}
}

// A run the caller cannot read is answered exactly as an unknown run is, and
// nothing is deleted. Unknown is 200 with zero counts (a repeated delete must
// not be an error), so unreadable is too.
func TestDeleteRunTree_ARunTheCallerCannotReadIsAnsweredAsAnUnknownRunIs(t *testing.T) {
	srv, mux := treeDeleteServer(t)
	st := srv.store
	alices := seedTreeRun(t, st, "acme", "alice", "lead", "", true)
	seedTreeRun(t, st, "acme", "alice", "worker", alices.ID, true)

	_, _, unknown := doDelete(t, mux, tokOtherTenant, "/v1/runs/r_does_not_exist/tree")
	for name, token := range map[string]string{
		"another tenant's operator": tokOtherTenant,
		// Holds runs:delete, but is isolated and the run is another user's.
		"an isolated user of the same tenant, with the scope": tokIsolatedDel,
	} {
		code, body, raw := doDelete(t, mux, token, "/v1/runs/"+alices.ID+"/tree")
		if code != http.StatusOK || body.DeletedRuns != 0 {
			t.Errorf("%s: %d deleted_runs %d, want 200 and 0: %s", name, code, body.DeletedRuns, raw)
		}
		if got := strings.ReplaceAll(raw, alices.ID, "r_does_not_exist"); got != unknown {
			t.Errorf("%s: answer differs from an unknown run's:\n  %s\n  %s", name, got, unknown)
		}
		if !runExists(t, st, alices.ID) {
			t.Fatalf("%s: the run was deleted", name)
		}
	}
	if holding, _ := eventsHolding(t, st, treeCanary); holding != 4 {
		t.Errorf("%d events hold the canary, want all 4 untouched", holding)
	}
	// The same holds for a session.
	_, _, unknownSess := doDelete(t, mux, tokOtherTenant, "/v1/sessions/s_does_not_exist?cascade=children")
	code, _, raw := doDelete(t, mux, tokOtherTenant, "/v1/sessions/"+alices.SessionID+"?cascade=children")
	if got := strings.ReplaceAll(raw, alices.SessionID, "s_does_not_exist"); code != http.StatusOK || got != unknownSess {
		t.Errorf("another tenant's session delete = %d %s; want the unknown session's answer %s", code, got, unknownSess)
	}
	if !sessionExists(t, st, alices.SessionID) {
		t.Error("another tenant deleted the session")
	}
}

// runs:delete is not implied by substrate:user and is not among a member's
// delegated scopes: without an explicit grant both are refused at the gate,
// on both routes, and with one each may delete what it can read.
func TestDeleteRunTree_NeedsTheRunsDeleteScope(t *testing.T) {
	srv, mux := treeDeleteServer(t)
	st := srv.store
	alices := seedTreeRun(t, st, "acme", "alice", "lead", "", true)
	bobs := seedTreeRun(t, st, "acme", "bob", "lead", "", true)

	for name, token := range map[string]string{
		"a member (tenant mode), delegated scopes": tokMember,
		"an isolated user":                         tokIsolated,
		"a read-only token":                        tokReadOnly,
	} {
		for _, path := range []string{
			"/v1/runs/" + alices.ID + "/tree",
			"/v1/runs/" + bobs.ID + "/tree", // the isolated user's own run
			"/v1/sessions/" + alices.SessionID + "?cascade=children",
			"/v1/sessions/" + bobs.SessionID + "?cascade=children",
		} {
			req := httptest.NewRequest(http.MethodDelete, path, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Header().Get("WWW-Authenticate"), auth.ScopeRunsDelete) {
				t.Errorf("%s on %s: %d (WWW-Authenticate %q), want 403 naming %s",
					name, path, rec.Code, rec.Header().Get("WWW-Authenticate"), auth.ScopeRunsDelete)
			}
		}
	}
	if !runExists(t, st, alices.ID) || !runExists(t, st, bobs.ID) {
		t.Fatal("a refused caller deleted a run")
	}

	// With the scope granted at mint: a member is not isolated, so it reads —
	// and may delete — its tenant's runs.
	if code, body, raw := doDelete(t, mux, tokMemberDel, "/v1/runs/"+alices.ID+"/tree"); code != http.StatusOK || body.DeletedRuns != 1 {
		t.Errorf("member granted runs:delete = %d deleted_runs %d, want 200 and 1: %s", code, body.DeletedRuns, raw)
	}
	// An isolated user granted it deletes its own run only.
	danas := seedTreeRun(t, st, "acme", "dana", "lead", "", true)
	if code, body, raw := doDelete(t, mux, tokIsolatedDel, "/v1/runs/"+bobs.ID+"/tree"); code != http.StatusOK || body.DeletedRuns != 0 || !runExists(t, st, bobs.ID) {
		t.Errorf("isolated user on another user's run = %d deleted_runs %d, want 200, 0 and the run kept: %s", code, body.DeletedRuns, raw)
	}
	if code, body, raw := doDelete(t, mux, tokIsolatedDel, "/v1/runs/"+danas.ID+"/tree"); code != http.StatusOK || body.DeletedRuns != 1 {
		t.Errorf("isolated user granted runs:delete on its own run = %d deleted_runs %d, want 200 and 1: %s", code, body.DeletedRuns, raw)
	}
	// The legacy shared secret is an admin, as on every route.
	if code, body, raw := doDelete(t, mux, tokLegacy, "/v1/runs/"+bobs.ID+"/tree"); code != http.StatusOK || body.DeletedRuns != 1 {
		t.Errorf("legacy token = %d deleted_runs %d, want 200 and 1: %s", code, body.DeletedRuns, raw)
	}
}

// The scope is checked where every transport meets, not only at each
// transport's gate: a caller that reaches the operation without it is refused.
func TestDeleteRunTree_TheOperationItselfRefusesACallerWithoutTheScope(t *testing.T) {
	srv, _ := treeDeleteServer(t)
	run := seedTreeRun(t, srv.store, "acme", "alice", "lead", "", true)
	for name, ctx := range map[string]context.Context{
		"runs:read + runs:create": principalCtx("acme", "alice", auth.ScopeRunsCreate, auth.ScopeRunsRead),
		"substrate:user":          principalCtx("acme", "alice", auth.ScopeUser),
	} {
		_, err := srv.DeleteRunTree(ctx, connector.DeleteRunTreeRequest{RunID: run.ID})
		var hse interface{ HTTPStatus() int }
		if !errors.As(err, &hse) || hse.HTTPStatus() != http.StatusForbidden {
			t.Errorf("%s: err = %v, want a 403 refusal", name, err)
		}
	}
	if !runExists(t, srv.store, run.ID) {
		t.Fatal("the run was deleted")
	}
	for name, ctx := range map[string]context.Context{
		"open mode (no principal)": context.Background(),
		"an admin of no tenant":    principalCtx("", "root", auth.ScopeAdmin),
	} {
		r := seedTreeRun(t, srv.store, "acme", "alice", "lead", "", true)
		if res, err := srv.DeleteRunTree(ctx, connector.DeleteRunTreeRequest{RunID: r.ID}); err != nil || res.DeletedRuns != 1 {
			t.Errorf("%s: deleted %d, err %v; want 1, nil", name, res.DeletedRuns, err)
		}
	}
}

func TestRequiredScopeFor_RunTreeDeleteRoutes(t *testing.T) {
	for _, c := range []struct{ method, path, want string }{
		{"DELETE", "/v1/runs/r_1/tree", auth.ScopeRunsDelete},
		{"DELETE", "/v1/sessions/s_1", auth.ScopeRunsDelete},
		// The draft discard is a different route and keeps its scope, even for
		// a run whose id is the word the other route ends in.
		{"DELETE", "/v1/runs/r_1", auth.ScopeRunsCreate},
		{"DELETE", "/v1/runs/tree", auth.ScopeRunsCreate},
		{"PATCH", "/v1/runs/r_1", auth.ScopeRunsCreate},
		// Nothing else under these prefixes moved.
		{"GET", "/v1/runs/r_1/tree", auth.ScopeRunsRead},
		{"POST", "/v1/runs/r_1/tree", auth.ScopeAdmin},
		{"DELETE", "/v1/runs/r_1/tree/x", auth.ScopeAdmin},
		{"DELETE", "/v1/sessions/s_1/messages", auth.ScopeAdmin},
		{"GET", "/v1/sessions/s_1", auth.ScopeRunsRead},
	} {
		if got := requiredScopeFor(c.method, c.path); got != c.want {
			t.Errorf("requiredScopeFor(%s %s) = %q, want %q", c.method, c.path, got, c.want)
		}
	}
	// The member bypass admits a non-isolated member to substrate:tenant
	// routes. These two must never be among them.
	for _, path := range []string{"/v1/runs/r_1/tree", "/v1/sessions/s_1"} {
		if tenantMemberAccessible("DELETE", path) || userSelfServiceRoute("DELETE", path) || memberReadable(path) {
			t.Errorf("DELETE %s is reachable without runs:delete (member bypass, self-service or member-read)", path)
		}
	}
}

func TestDeleteSession_DeletesEveryRunOfTheSessionWithItsDescendants(t *testing.T) {
	srv, mux := treeDeleteServer(t)
	st := srv.store
	ctx := context.Background()
	first := seedTreeRun(t, st, "acme", "alice", "lead", "", true)
	// A continuation: a second run in the same session.
	second, err := st.CreateRun(ctx, first.SessionID, store.RunIdentity{AgentID: "lead-2", UserID: "alice", TenantID: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	finishTreeRun(t, st, second.ID)
	child := seedTreeRun(t, st, "acme", "alice", "worker", second.ID, true)

	// The parameter is required.
	for _, q := range []string{"", "?cascade=", "?cascade=none", "?cascade=true"} {
		code, body, raw := doDelete(t, mux, tokOperator, "/v1/sessions/"+first.SessionID+q)
		if code != http.StatusBadRequest || body.Code != "cascade_required" {
			t.Errorf("DELETE session%s = %d %q, want 400 cascade_required: %s", q, code, body.Code, raw)
		}
	}
	if !sessionExists(t, st, first.SessionID) {
		t.Fatal("the session was deleted without cascade=children")
	}

	code, body, raw := doDelete(t, mux, tokOperator, "/v1/sessions/"+first.SessionID+"?cascade=children")
	if code != http.StatusOK || body.DeletedRuns != 3 || body.Counts["sessions"] != 2 || body.SessionID != first.SessionID {
		t.Fatalf("DELETE session = %d deleted_runs %d sessions %d; want 200, 3 runs, 2 sessions: %s",
			code, body.DeletedRuns, body.Counts["sessions"], raw)
	}
	for _, id := range []string{first.ID, second.ID, child.ID} {
		if runExists(t, st, id) {
			t.Errorf("run %s is still stored", id)
		}
	}
	if sessionExists(t, st, first.SessionID) || sessionExists(t, st, child.SessionID) {
		t.Error("a session of the tree is still stored")
	}
	if code, again, _ := doDelete(t, mux, tokOperator, "/v1/sessions/"+first.SessionID+"?cascade=children"); code != http.StatusOK || again.DeletedRuns != 0 {
		t.Errorf("second DELETE session = %d deleted_runs %d, want 200 and 0", code, again.DeletedRuns)
	}
}

// Deleting one run of a continued session keeps the session and its other
// run. In the trace index the deleted run's turns go by the run id each row
// carries; a backfilled row carries none, so it stays and is reported.
func TestDeleteRunTree_KeepsASessionThatStillHoldsARun(t *testing.T) {
	srv, mux := treeDeleteServer(t)
	st := srv.store
	ctx := context.Background()
	first := seedTreeRun(t, st, "acme", "alice", "lead", "", true)
	second, err := st.CreateRun(ctx, first.SessionID, store.RunIdentity{AgentID: "lead-2", UserID: "alice", TenantID: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, second.ID, "text", []byte(`{"text":"kept"}`)); err != nil {
		t.Fatal(err)
	}
	finishTreeRun(t, st, second.ID)
	prefix := store.TraceTurnKeyPrefix + first.SessionID + ":"
	setTrace(t, st, "acme", "alice", prefix+"1-1", traceTurnValue{Text: treeCanary, SessionID: first.SessionID, RunID: first.ID})
	setTrace(t, st, "acme", "alice", prefix+"2-1", traceTurnValue{Text: "kept", SessionID: first.SessionID, RunID: second.ID})
	setTrace(t, st, "acme", "alice", prefix+"b7", traceTurnValue{Text: "backfilled", SessionID: first.SessionID})

	code, body, raw := doDelete(t, mux, tokOperator, "/v1/runs/"+first.ID+"/tree")
	if code != http.StatusOK || body.DeletedRuns != 1 || body.Counts["sessions"] != 0 || body.Counts["events"] != 2 || body.Counts["trace_index"] != 1 {
		t.Fatalf("DELETE = %d %v; want 200 with 1 run, 2 events, 1 trace row and no session: %s", code, body.Counts, raw)
	}
	if !sessionExists(t, st, first.SessionID) || !runExists(t, st, second.ID) {
		t.Error("the session or its other run was deleted")
	}
	if holding, total := eventsHolding(t, st, treeCanary); holding != 0 || total != 1 {
		t.Errorf("events: %d rows, %d with the canary; want the other run's 1 and no canary", total, holding)
	}
	keys := treeTraceKeys(t, st, "acme", "alice")
	if keys[prefix+"1-1"] || !keys[prefix+"2-1"] || !keys[prefix+"b7"] {
		t.Errorf("trace index holds %v; want the other run's turn and the backfilled one", keys)
	}
	var unreached *connector.NotReached
	for i := range body.NotReached {
		if body.NotReached[i].Name == "trace_index" {
			unreached = &body.NotReached[i]
		}
	}
	if unreached == nil || unreached.Count == nil || *unreached.Count != 1 {
		t.Errorf("not_reached does not report the 1 backfilled trace row: %s", raw)
	}
}

// A run of another tenant under the tree stops the delete: the caller was
// authorized against the root's tenant only.
func TestDeleteRunTree_RefusesATreeThatCrossesTenants(t *testing.T) {
	srv, mux := treeDeleteServer(t)
	st := srv.store
	parent := seedTreeRun(t, st, "acme", "alice", "lead", "", true)
	foreign := seedTreeRun(t, st, "other", "zed", "worker", parent.ID, true)

	for _, token := range []string{tokOperator, tokLegacy} {
		code, body, raw := doDelete(t, mux, token, "/v1/runs/"+parent.ID+"/tree")
		if code != http.StatusConflict || body.Code != "run_tree_crosses_tenants" {
			t.Errorf("DELETE = %d %q, want 409 run_tree_crosses_tenants: %s", code, body.Code, raw)
		}
	}
	if !runExists(t, st, parent.ID) || !runExists(t, st, foreign.ID) {
		t.Error("a run was deleted across tenants")
	}
}

// The walk stops at the cap with a clear error instead of deleting part of a
// tree, and a row that names itself as parent does not loop it.
func TestDeleteRunTree_RefusesATreeOverTheSizeCap(t *testing.T) {
	if testing.Short() {
		t.Skip("writes maxRunTreeRuns+1 runs")
	}
	srv, mux := treeDeleteServer(t)
	st := srv.store
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "acme", "writer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	root, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "root", UserID: "alice", TenantID: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	finishTreeRun(t, st, root.ID)
	for i := 0; i < maxRunTreeRuns; i++ {
		r, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: fmt.Sprintf("c_%d", i), ParentRunID: root.ID, UserID: "alice", TenantID: "acme"})
		if err != nil {
			t.Fatal(err)
		}
		finishTreeRun(t, st, r.ID)
	}
	code, body, raw := doDelete(t, mux, tokOperator, "/v1/runs/"+root.ID+"/tree")
	if code != http.StatusUnprocessableEntity || body.Code != "run_tree_too_large" {
		t.Fatalf("DELETE of %d runs = %d %q, want 422 run_tree_too_large: %s", maxRunTreeRuns+1, code, body.Code, raw)
	}
	if !runExists(t, st, root.ID) {
		t.Error("the root of an over-size tree was deleted")
	}
}

// One audit record per call: who, which root, how many rows — and no content.
// With no sink configured the delete still happens.
func TestDeleteRunTree_RecordsWhoDeletedWhatAndNoContent(t *testing.T) {
	srv, mux := treeDeleteServer(t)
	st := srv.store
	parent := seedTreeRun(t, st, "acme", "alice", "lead", "", true)
	seedTreeRun(t, st, "acme", "alice", "worker", parent.ID, true)
	unsunk := seedTreeRun(t, st, "acme", "alice", "nosink", "", true)

	// No sink: unlike a subject erasure, not a reason to refuse.
	if code, body, raw := doDelete(t, mux, tokOperator, "/v1/runs/"+unsunk.ID+"/tree"); code != http.StatusOK || body.DeletedRuns != 1 {
		t.Fatalf("DELETE with no audit sink = %d deleted_runs %d, want 200 and 1: %s", code, body.DeletedRuns, raw)
	}

	sink := &recordingSink{}
	srv.SetRunDeleteAudit(sink)
	live := seedTreeRun(t, st, "acme", "alice", "busy", "", false)
	doDelete(t, mux, tokOperator, "/v1/runs/"+parent.ID+"/tree")
	doDelete(t, mux, tokOperator, "/v1/runs/"+live.ID+"/tree")
	doDelete(t, mux, tokMember, "/v1/runs/"+live.ID+"/tree") // 403 at the gate: no record

	if len(sink.events) != 2 {
		t.Fatalf("%d audit records for 2 calls that reached the operation, want 2: %+v", len(sink.events), sink.events)
	}
	done, refused := sink.events[0], sink.events[1]
	if done.Action != "run_tree_delete" || done.ActorTenant != "acme" || done.ActorSubject != "op" ||
		done.TargetTenant != "acme" || done.DeleteRunID != parent.ID || done.DeleteCounts["runs"] != 2 || done.DeleteCounts["events"] != 4 {
		t.Errorf("audit record of the delete = %+v", done)
	}
	if refused.DeleteRefused != "run_tree_live" || refused.DeleteRunID != live.ID || refused.DeleteCounts != nil {
		t.Errorf("audit record of the refused call = %+v", refused)
	}
	raw, _ := json.Marshal(sink.events)
	if strings.Contains(string(raw), treeCanary) {
		t.Errorf("an audit record holds run content: %s", raw)
	}
}

func TestDeleteRunTree_RefusesAMalformedID(t *testing.T) {
	_, mux := treeDeleteServer(t)
	for path, want := range map[string]string{
		"/v1/runs/team:triage/tree":              "invalid_run_id",
		"/v1/sessions/bad%20id?cascade=children": "invalid_session_id",
	} {
		code, body, raw := doDelete(t, mux, tokOperator, path)
		if code != http.StatusBadRequest || body.Code != want {
			t.Errorf("DELETE %s = %d %q, want 400 %s: %s", path, code, body.Code, want, raw)
		}
	}
}
