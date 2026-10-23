package http

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	bridge "github.com/denn-gubsky/loomcycle/internal/a2a"
	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/api/webhook"
	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/scheduler"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// RFC EZ §1.5: a run a trigger fires has no principal on ctx. It used to run
// on its agent's full grants, bound only by the isolated / operator-key bits
// captured on the definition; the definition owner's RFC EY rules never
// narrowed it, and `run agent:` was never asked for the owner. A definition
// now records its author (acl_owner), and each fire compiles that author's
// CURRENT set and narrows the run by it as a live caller's narrows a live run.
// These tests author definitions as the member `dev` of the aclResumeFixture
// (bound, when bind is set, to a list that withholds Bash) and fire them for
// real.

// devPrincipal is dev's principal with its set compiled, as a live request
// carries it.
func (e *aclResumeEnv) devPrincipal(t *testing.T) auth.Principal {
	t.Helper()
	dev, err := e.srv.ACLResolver().Resolve(context.Background(), auth.Principal{TenantID: "acme", Subject: "dev", TokenDefID: "def_worker", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}})
	if err != nil || dev.Effective == nil {
		t.Fatalf("resolve dev: %v", err)
	}
	return dev
}

// authorCtx is the ctx dev authors a definition under: its principal, on the
// substrate plane of tenant acme.
func (e *aclResumeEnv) authorCtx(t *testing.T) context.Context {
	t.Helper()
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "acme", UserID: "dev"})
	return auth.WithPrincipal(ctx, e.devPrincipal(t))
}

// createScheduleAsDev writes a schedule of agent as dev and makes it due now.
func (e *aclResumeEnv) createScheduleAsDev(t *testing.T, name, agent string) string {
	t.Helper()
	e.srv.SetScheduleDefTool(&builtin.ScheduleDef{Store: e.st, Cfg: e.srv.cfg()})
	ctx := tools.WithScheduleDefPolicy(e.authorCtx(t), tools.ScheduleDefPolicyValue{Scopes: []string{"any"}, SelfName: "author"})
	res, err := e.srv.ScheduleDef(ctx, json.RawMessage(`{"op":"create","name":"`+name+`","overlay":{"agent":"`+agent+`","schedule":"0 9 * * 1","prompt":[{"role":"user","content":[{"type":"trusted-text","text":"tick"}]}]}}`))
	if err != nil || res.IsError {
		t.Fatalf("create schedule: %v %s", err, res.Text)
	}
	var created struct {
		DefID string `json:"def_id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &created); err != nil || created.DefID == "" {
		t.Fatalf("create result %s: %v", res.Text, err)
	}
	if err := e.st.ScheduleRunStateSeed(context.Background(), created.DefID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	return created.DefID
}

// startScheduler runs a fast-ticking scheduler over the fixture until the
// returned stop is called (or the test ends). Stop waits for its runs.
func (e *aclResumeEnv) startScheduler(t *testing.T) (stop func()) {
	t.Helper()
	sched := scheduler.New(scheduler.Config{TickInterval: 20 * time.Millisecond}, e.st, e.srv, nil, nil, t.Logf)
	sched.Start(context.Background())
	var stopped bool
	stop = func() {
		if !stopped {
			stopped = true
			sched.Stop()
		}
	}
	t.Cleanup(stop)
	return stop
}

// waitScheduleStatus waits until the schedule's last fire reads status.
func (e *aclResumeEnv) waitScheduleStatus(t *testing.T, defID, status string) store.ScheduleRunStateRow {
	t.Helper()
	var row store.ScheduleRunStateRow
	waitFor(t, "the schedule to record "+status, func() bool {
		r, err := e.st.ScheduleRunStateGet(context.Background(), defID)
		row = r
		return err == nil && r.LastStatus == status
	})
	return row
}

// assertNarrowedByDev fails unless the run's policy and the schema it offered
// the model hold what dev's list leaves.
func (e *aclResumeEnv) assertNarrowedByDev(t *testing.T, p tools.RunPolicy) {
	t.Helper()
	e.assertNarrowedBy(t, p, "dev")
}

// assertNarrowedBy is assertNarrowedByDev for the subject bound to dev's list.
func (e *aclResumeEnv) assertNarrowedBy(t *testing.T, p tools.RunPolicy, subject string) {
	t.Helper()
	got := aclNarrowingOf(p)
	if p.ACL == nil || got.ACLSubject != "acme/"+subject || slices.Contains(got.Tools, "Bash") || !slices.Equal(got.Removed, []string{"Bash"}) ||
		len(got.Volumes) != 1 || !got.Volumes[0].ReadOnly || slices.Contains(got.Memory, "tenant") {
		t.Errorf("the fired run is not narrowed by its owner's list: %+v", got)
	}
	if offered := e.prov.toolsOf("worker"); len(offered) == 0 || slices.Contains(offered[0], "Bash") || !slices.Contains(offered[0], "Probe") {
		t.Errorf("the fired run offered the model %v; Bash must be withheld", offered)
	}
}

// bindRunDeny binds dev to a list that denies running worker.
func (e *aclResumeEnv) bindRunDeny(t *testing.T) {
	t.Helper()
	if _, err := e.st.AclDefPut(context.Background(), store.AclDefRow{TenantID: "acme", Name: "no-worker",
		Rules: json.RawMessage(`[{"effect":"deny","actions":["run"],"resources":["agent:worker"]}]`)}); err != nil {
		t.Fatal(err)
	}
	bindACL(t, e.st, "no-worker")
}

// A schedule written by dev records dev as its owner, whatever an overlay
// says: the owner is server authority, as the isolated bit is.
func TestScheduleDef_RecordsItsAuthorAsACLOwner(t *testing.T) {
	e := aclResumeFixture(t, true, nil)
	e.srv.SetScheduleDefTool(&builtin.ScheduleDef{Store: e.st, Cfg: e.srv.cfg()})
	ctx := tools.WithScheduleDefPolicy(e.authorCtx(t), tools.ScheduleDefPolicyValue{Scopes: []string{"any"}, SelfName: "author"})
	res, err := e.srv.ScheduleDef(ctx, json.RawMessage(`{"op":"create","name":"owned","overlay":{"agent":"worker","schedule":"0 9 * * 1","acl_owner":{"tenant_id":"acme","subject":"root","class":"admin"},"prompt":[{"role":"user","content":[{"type":"trusted-text","text":"tick"}]}]}}`))
	if err != nil || res.IsError {
		t.Fatalf("create: %v %s", err, res.Text)
	}
	var created struct {
		DefID string `json:"def_id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &created); err != nil {
		t.Fatal(err)
	}
	row, err := e.st.ScheduleDefGet(context.Background(), created.DefID)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		ACLOwner *authz.SubjectRef `json:"acl_owner"`
	}
	if err := json.Unmarshal(row.Definition, &body); err != nil {
		t.Fatal(err)
	}
	want := authz.SubjectRef{TenantID: "acme", Subject: "dev", TokenDefID: "def_worker", Class: "member"}
	if body.ACLOwner == nil || *body.ACLOwner != want {
		t.Errorf("acl_owner = %+v, want %+v (the author, not the overlay's)", body.ACLOwner, want)
	}
}

// A schedule fires a run narrowed by its owner's list: Bash withheld from the
// schema, the volume read-only, the tenant keyspace dropped, the set on its
// policy — and the run records its owner for a resume, under the schedule
// transport.
func TestScheduleFire_RunIsNarrowedByItsOwnersACL(t *testing.T) {
	e := aclResumeFixture(t, true, map[string][][]providers.Event{"worker": {probeTurn("tu_s"), endTurn()}})
	defID := e.createScheduleAsDev(t, "nightly", "worker")
	e.startScheduler(t)
	state := e.waitScheduleStatus(t, defID, "completed")
	seen := e.waitProbes(t, 1)
	e.assertNarrowedByDev(t, seen[0])
	run, err := e.st.GetRun(context.Background(), state.LastRunID)
	if err != nil {
		t.Fatal(err)
	}
	if rec, ok := decodeRunConfig(run.RunConfig); !ok || rec.ACLOrigin == nil || rec.ACLOrigin.Subject != "dev" || rec.ACLOrigin.Transport != "schedule" {
		t.Errorf("the fired run's record does not name its owner under the schedule transport: %s", run.RunConfig)
	}
}

// A deny on running the agent, bound to the owner, refuses the fire as the
// live intake would refuse dev: no model call, the schedule records the
// failure as acl_denied, and the refusal is audited.
func TestScheduleFire_ADenyOnRunningTheAgentRefusesTheFire(t *testing.T) {
	e := aclResumeFixture(t, false, map[string][][]providers.Event{"worker": {endTurn()}})
	defID := e.createScheduleAsDev(t, "denied", "worker")
	e.bindRunDeny(t)
	stop := e.startScheduler(t)
	state := e.waitScheduleStatus(t, defID, "failed")
	stop()
	if !strings.Contains(state.LastError, "acl_denied") {
		t.Errorf("last_error = %q; want acl_denied", state.LastError)
	}
	if n := e.prov.callsOf("worker"); n != 0 {
		t.Errorf("the refused fire made %d model call(s)", n)
	}
	if !slices.ContainsFunc(e.audit.events, func(ev audit.Event) bool { return ev.Action == "acl_denied" && ev.ActorSubject == "dev" }) {
		t.Errorf("no acl_denied audit event for dev; events: %+v", e.audit.events)
	}
}

// A fire whose owner's set cannot be read is refused with acl_unavailable,
// never run without the set; the slot is given back to be fired again.
func TestScheduleFire_UnreadableOwnerACLRefusesTheFire(t *testing.T) {
	e := aclResumeFixture(t, true, map[string][][]providers.Event{"worker": {endTurn()}})
	defID := e.createScheduleAsDev(t, "held", "worker")
	e.srv.SetACLResolver(aclres.New(failingACLStore{e.st}, authz.ModeCompat, 0))
	stop := e.startScheduler(t)
	state := e.waitScheduleStatus(t, defID, "failed")
	stop()
	if !strings.Contains(state.LastError, "acl_unavailable") || !strings.Contains(state.LastError, "fired again on the next tick") {
		t.Errorf("last_error = %q; want acl_unavailable, retried", state.LastError)
	}
	if state.FireCount != 0 {
		t.Errorf("fire_count = %d; a refused-for-now fire must not use up max_fires", state.FireCount)
	}
	if n := e.prov.callsOf("worker"); n != 0 {
		t.Errorf("the refused fire made %d model call(s)", n)
	}
	if !slices.ContainsFunc(e.audit.events, func(ev audit.Event) bool { return ev.Action == "acl_unavailable" && ev.ActorSubject == "dev" }) {
		t.Errorf("no acl_unavailable audit event for dev; events: %+v", e.audit.events)
	}
}

// A schedule from the operator's yaml has no owner: it fires as before,
// unnarrowed, though every subject of its tenant may be bound to a list.
func TestScheduleFire_YamlScheduleWithNoOwnerRunsAsBefore(t *testing.T) {
	e := aclResumeFixture(t, true, map[string][][]providers.Event{"worker": {probeTurn("tu_y"), endTurn()}})
	cfg := e.srv.cfg()
	cfg.ScheduledRuns = map[string]config.ScheduledRun{"yaml-nightly": {
		Agent: "worker", Schedule: "0 9 * * 1", Enabled: true,
		Prompt: []config.ScheduledRunSegment{{Role: "user", Content: []config.ScheduledRunSegmentContent{{Type: "trusted-text", Text: "tick"}}}},
	}}
	tool := &builtin.ScheduleDef{Store: e.st, Cfg: cfg}
	if _, err := tool.BootstrapStaticSchedules(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, err := e.st.ScheduleDefGetActive(context.Background(), "", "yaml-nightly")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(row.Definition), "acl_owner") {
		t.Fatalf("a yaml schedule recorded an owner: %s", row.Definition)
	}
	if err := e.st.ScheduleRunStateSeed(context.Background(), row.DefID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	e.startScheduler(t)
	e.waitScheduleStatus(t, row.DefID, "completed")
	if seen := e.waitProbes(t, 1); seen[0].ACL != nil || !slices.Contains(seen[0].Tools, "Bash") {
		t.Errorf("a yaml schedule with no owner fired narrowed: %+v", aclNarrowingOf(seen[0]))
	}
}

const ownerWebhookSecret = "owner-webhook-secret"

// createWebhookAsDev writes a webhook spawning agent as dev and returns a
// mux serving the receiver.
func (e *aclResumeEnv) createWebhookAsDev(t *testing.T, name, agent string) *http.ServeMux {
	t.Helper()
	cfg := e.srv.cfg()
	e.srv.SetWebhookDefTool(&builtin.WebhookDef{Store: e.st, Cfg: cfg})
	ctx := tools.WithWebhookDefPolicy(e.authorCtx(t), tools.WebhookDefPolicyValue{Scopes: []string{"any"}, SelfName: "author"})
	res, err := e.srv.WebhookDef(ctx, json.RawMessage(`{"op":"create","name":"`+name+`","overlay":{"enabled":true,"delivery":"spawn","agent":"`+agent+`","auth":{"kind":"hmac","header":"X-Test-Signature","signing_secret_env":"LOOMCYCLE_TEST_WH_SECRET"},"payload_mapping":{"goal":"$.goal"}}}`))
	if err != nil || res.IsError {
		t.Fatalf("create webhook: %v %s", err, res.Text)
	}
	rec := webhook.New(webhook.Deps{
		Store: e.st, Cfg: cfg, Runner: e.srv,
		EnvAllowlist: map[string]bool{"LOOMCYCLE_TEST_WH_SECRET": true},
		Getenv: func(k string) string {
			if k == "LOOMCYCLE_TEST_WH_SECRET" {
				return ownerWebhookSecret
			}
			return ""
		},
	})
	mux := http.NewServeMux()
	rec.Mount(mux)
	return mux
}

func deliverSigned(mux *http.ServeMux, name string) *httptest.ResponseRecorder {
	body := []byte(`{"goal":"handle the delivery"}`)
	mac := hmac.New(sha256.New, []byte(ownerWebhookSecret))
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/_webhooks/acme/"+name, bytes.NewReader(body))
	req.Header.Set("X-Test-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// A delivery to dev's webhook spawns a run narrowed by dev's list, recorded
// under the webhook transport.
func TestWebhookDelivery_RunIsNarrowedByItsOwnersACL(t *testing.T) {
	e := aclResumeFixture(t, true, map[string][][]providers.Event{"worker": {probeTurn("tu_w"), endTurn()}})
	mux := e.createWebhookAsDev(t, "owned", "worker")
	w := deliverSigned(mux, "owned")
	if w.Code != http.StatusAccepted {
		t.Fatalf("delivery status = %d: %s", w.Code, w.Body.String())
	}
	var accepted struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &accepted); err != nil || accepted.RunID == "" {
		t.Fatalf("delivery response %s: %v", w.Body.String(), err)
	}
	run := waitRunStatus(t, e.st, accepted.RunID, store.RunCompleted)
	e.assertNarrowedByDev(t, e.waitProbes(t, 1)[0])
	if rec, ok := decodeRunConfig(run.RunConfig); !ok || rec.ACLOrigin == nil || rec.ACLOrigin.Subject != "dev" || rec.ACLOrigin.Transport != "webhook" {
		t.Errorf("the delivered run's record does not name its owner under the webhook transport: %s", run.RunConfig)
	}
}

// A deny on running the agent, bound to the owner, refuses the delivery: a
// 403 the sender does not retry, and no model call.
func TestWebhookDelivery_ADenyOnRunningTheAgentRefusesTheDelivery(t *testing.T) {
	e := aclResumeFixture(t, false, map[string][][]providers.Event{"worker": {endTurn()}})
	mux := e.createWebhookAsDev(t, "denied", "worker")
	e.bindRunDeny(t)
	if w := deliverSigned(mux, "denied"); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "agent_not_allowed") {
		t.Fatalf("delivery status = %d: %s; want 403 agent_not_allowed", w.Code, w.Body.String())
	}
	if n := e.prov.callsOf("worker"); n != 0 {
		t.Errorf("the refused delivery made %d model call(s)", n)
	}
}

// A delivery whose owner's set cannot be read is refused with a retryable
// 503, never run without the set.
func TestWebhookDelivery_UnreadableOwnerACLRefusesTheDelivery(t *testing.T) {
	e := aclResumeFixture(t, true, map[string][][]providers.Event{"worker": {endTurn()}})
	mux := e.createWebhookAsDev(t, "held", "worker")
	e.srv.SetACLResolver(aclres.New(failingACLStore{e.st}, authz.ModeCompat, 0))
	if w := deliverSigned(mux, "held"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("delivery status = %d: %s; want 503", w.Code, w.Body.String())
	}
	if n := e.prov.callsOf("worker"); n != 0 {
		t.Errorf("the refused delivery made %d model call(s)", n)
	}
}

// executeA2A runs one message to worker through the A2A executor as the peer
// credential p, to the end of its event stream.
func (e *aclResumeEnv) executeA2A(t *testing.T, p auth.Principal, taskID string) []a2asdk.Event {
	t.Helper()
	ex := bridge.NewExecutor(e.srv, nil, e.st, "worker")
	ctx := bridge.WithPeerPrincipal(context.Background(), p)
	var events []a2asdk.Event
	ex.Execute(ctx, &a2asrv.ExecutorContext{
		TaskID:  a2asdk.TaskID(taskID),
		Message: a2asdk.NewMessage(a2asdk.MessageRoleUser, a2asdk.NewTextPart("go")),
	})(func(ev a2asdk.Event, err error) bool {
		if err != nil {
			t.Logf("a2a stream error: %v", err)
			return false
		}
		events = append(events, ev)
		return true
	})
	return events
}

// An A2A peer's run has no principal on ctx either: it is narrowed by the
// peer credential's own list, recorded under the a2a transport.
func TestA2ARun_IsNarrowedByThePeersACL(t *testing.T) {
	e := aclResumeFixture(t, true, map[string][][]providers.Event{"worker": {probeTurn("tu_a"), endTurn()}})
	e.executeA2A(t, e.devPrincipal(t), "task-owned")
	run := waitRunStatus(t, e.st, mustRunIDByAgent(t, e.st, "task-owned"), store.RunCompleted)
	e.assertNarrowedByDev(t, e.waitProbes(t, 1)[0])
	if rec, ok := decodeRunConfig(run.RunConfig); !ok || rec.ACLOrigin == nil || rec.ACLOrigin.Subject != "dev" || rec.ACLOrigin.Transport != "a2a" {
		t.Errorf("the A2A run's record does not name the peer under the a2a transport: %s", run.RunConfig)
	}
}

// A deny on running the agent, bound to the peer, refuses its run.
func TestA2ARun_ADenyOnRunningTheAgentRefusesIt(t *testing.T) {
	e := aclResumeFixture(t, false, map[string][][]providers.Event{"worker": {endTurn()}})
	e.bindRunDeny(t)
	e.executeA2A(t, e.devPrincipal(t), "task-denied")
	if n := e.prov.callsOf("worker"); n != 0 {
		t.Errorf("the refused A2A run made %d model call(s)", n)
	}
	if !slices.ContainsFunc(e.audit.events, func(ev audit.Event) bool { return ev.Action == "acl_denied" && ev.ActorSubject == "dev" }) {
		t.Errorf("no acl_denied audit event for the peer; events: %+v", e.audit.events)
	}
}

func mustRunIDByAgent(t *testing.T, st store.Store, agentID string) string {
	t.Helper()
	var id string
	waitFor(t, "the run of "+agentID, func() bool {
		r, err := st.GetRunByAgentID(context.Background(), agentID)
		id = r.ID
		return err == nil
	})
	return id
}

// principalCtx is the ctx subject authors a definition under, with its set
// compiled.
func (e *aclResumeEnv) principalCtx(t *testing.T, subject string) context.Context {
	t.Helper()
	token := "def_" + subject
	if subject == "dev" {
		token = "def_worker" // dev's own token, as authorCtx presents it
	}
	p, err := e.srv.ACLResolver().Resolve(context.Background(), auth.Principal{TenantID: "acme", Subject: subject, TokenDefID: token, Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}})
	if err != nil || p.Effective == nil {
		t.Fatalf("resolve %s: %v", subject, err)
	}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "acme", UserID: subject})
	return tools.WithScheduleDefPolicy(auth.WithPrincipal(ctx, p), tools.ScheduleDefPolicyValue{Scopes: []string{"any"}, SelfName: "author"})
}

// addHookAs edits defID's hooks as subject and returns the new version's id.
func (e *aclResumeEnv) addHookAs(t *testing.T, subject, defID string) string {
	t.Helper()
	res, err := e.srv.ScheduleDef(e.principalCtx(t, subject), json.RawMessage(`{"op":"add_hook","def_id":"`+defID+`","hook":{"kind":"memory.set","scope":"user","key":"k"}}`))
	if err != nil || res.IsError {
		t.Fatalf("add_hook as %s: %v %s", subject, err, res.Text)
	}
	var out struct {
		DefID string `json:"def_id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil || out.DefID == "" || out.DefID == defID {
		t.Fatalf("add_hook result %s: %v", res.Text, err)
	}
	return out.DefID
}

func scheduleOwnerOf(t *testing.T, st store.Store, defID string) *authz.SubjectRef {
	t.Helper()
	row, err := st.ScheduleDefGet(context.Background(), defID)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		ACLOwner *authz.SubjectRef `json:"acl_owner"`
	}
	if err := json.Unmarshal(row.Definition, &body); err != nil {
		t.Fatal(err)
	}
	return body.ACLOwner
}

// A version is bound to whoever wrote it. When a narrower principal edits a
// schedule a broader one wrote, the new version fires under the EDITOR's
// rules: keeping the original owner's would let the editor make the trigger
// run with authority it does not hold itself.
func TestScheduleDef_AHookEditBindsTheNextFireToTheEditorsACL(t *testing.T) {
	e := aclResumeFixture(t, false, map[string][][]providers.Event{"worker": {probeTurn("tu_e"), endTurn()}})
	defID := e.createScheduleAsDev(t, "edited", "worker") // dev: no list bound
	if _, err := e.st.AclBindingPut(context.Background(), store.AclBindingRow{TenantID: "acme", ACLName: "dev-limits", SubjectKind: "user", SubjectID: "ed"}); err != nil {
		t.Fatal(err)
	}
	edited := e.addHookAs(t, "ed", defID)
	if o := scheduleOwnerOf(t, e.st, edited); o == nil || o.Subject != "ed" {
		t.Fatalf("the edited version's owner = %+v, want ed", o)
	}
	if err := e.st.ScheduleRunStateSeed(context.Background(), edited, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	e.startScheduler(t)
	e.waitScheduleStatus(t, edited, "completed")
	e.assertNarrowedBy(t, e.waitProbes(t, 1)[0], "ed")
}

// The owner's own edit keeps the owner.
func TestScheduleDef_TheOwnersHookEditKeepsItsOwner(t *testing.T) {
	e := aclResumeFixture(t, true, nil)
	defID := e.createScheduleAsDev(t, "self-edited", "worker")
	edited := e.addHookAs(t, "dev", defID)
	want := authz.SubjectRef{TenantID: "acme", Subject: "dev", TokenDefID: "def_worker", Class: "member"}
	if o := scheduleOwnerOf(t, e.st, edited); o == nil || *o != want {
		t.Errorf("the owner's own edit recorded owner %+v, want %+v", o, want)
	}
}
