package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// A team walk started over POST /v1/_teamdef runs as the CALLER. It used to run
// as the synthetic `http-admin` user: the walk's run and every member run were
// filed under a user nobody is, so the caller's own run-state stream saw
// nothing, and the members read and wrote that synthetic user's memory,
// documents and budget.

// waveTeam is one starter state and a terminal: the smallest walk whose
// members carry the walk id (parent_context.walk_id), which is what the
// walk_id stream filter matches on.
const waveTeam = `{"entry":"wave","states":[` +
	`{"state":"wave","handler":{"kind":"starter","source":{"channel":"in"},` +
	`"fanout":{"agent":"writer","per":"message","max":1},"sink":{"channel":"out"}}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"wave","to":"done","on":"success"}],` +
	`"channels":{"publish":["out"],"subscribe":["in"]}}`

// loopTeam hits its iteration cap on the second entry of `a`, which is what
// makes a walk with interrupt_on_cap pause on a human.
const loopTeam = `{"entry":"a","max_iterations":1,
  "states":[{"state":"a","handler":{"kind":"agent","agent":"writer"}},{"state":"b","handler":{"kind":"agent","agent":"writer"}}],
  "transitions":[{"from":"a","to":"b","on":"success"},{"from":"b","to":"a","on":"success"}]}`

type walkHarness struct {
	t   *testing.T
	srv *Server
	st  store.Store
	bus *runstate.Bus
	pub *channels.StorePublisher
}

// newWalkHarness is a server with a stub provider, a `writer` agent and the
// wave's two tenant channels, with the TeamDef tool wired the way main.go wires
// it — including the pause ask through the real Interruption tool.
func newWalkHarness(t *testing.T) *walkHarness {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:   map[string]config.AgentDef{"writer": {Model: "stub-model", SystemPrompt: "write"}},
		Channels: map[string]config.Channel{
			"in":  {Scope: "tenant", Semantic: "queue", MaxMessages: 100},
			"out": {Scope: "tenant", Semantic: "queue", MaxMessages: 100},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "walk.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: &numberedProvider{}}, []tools.Tool{}, concurrency.New(4, 4, 100*time.Millisecond), st)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	bus := runstate.NewBus()
	srv.SetRunStateBus(bus)
	cbus := channels.NewBus()
	pub := &channels.StorePublisher{Store: st, Bus: cbus, Scheduler: channels.NewScheduler(cbus, 100)}
	srv.SetSystemPublisher(pub)
	srv.SetChannelBus(cbus)
	srv.SetInterruptionBus(cbus)
	it := &builtin.Interruption{Store: st, Bus: cbus}
	td := &builtin.TeamDef{Store: st}
	td.AskHuman = func(ctx context.Context, question string) (string, error) {
		ask, _ := json.Marshal(map[string]any{"op": "ask", "question": question})
		res, err := it.Execute(ctx, ask)
		if err != nil {
			return "", err
		}
		if res.IsError {
			return "", errors.New(res.Text)
		}
		var parsed struct {
			Answer string `json:"answer"`
		}
		_ = json.Unmarshal([]byte(res.Text), &parsed)
		return parsed.Answer, nil
	}
	srv.SetTeamDefTool(td)
	return &walkHarness{t: t, srv: srv, st: st, bus: bus, pub: pub}
}

// feed puts one message on the wave's source in the team's tenant.
func (h *walkHarness) feed(tenant string) {
	h.t.Helper()
	if _, err := h.pub.PublishNow(context.Background(), "in", tenant, store.MemoryScopeTenant, "",
		json.RawMessage(`{"task":"write"}`), "_system", 0, 0); err != nil {
		h.t.Fatalf("publish: %v", err)
	}
}

// seedTenantTeam writes and promotes a team in one tenant.
func seedTenantTeam(t *testing.T, st store.Store, tenant, name, defJSON string) {
	t.Helper()
	def, err := teamgraph.Parse([]byte(defJSON))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	row, err := st.TeamDefCreate(context.Background(), store.TeamDefRow{
		DefID: "tdf_" + tenant + "_" + name, Name: name, TenantID: tenant,
		Definition: json.RawMessage(defJSON), ContentSHA256: teamgraph.Sign(name, def),
	})
	if err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
	if err := st.TeamDefSetActive(context.Background(), tenant, name, row.DefID, "a_test"); err != nil {
		t.Fatalf("promote %s: %v", name, err)
	}
}

func alicePrincipal(ctx context.Context) context.Context {
	return auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeTenant}})
}

// postTeamDef drives POST /v1/_teamdef through its handler with the principal
// (if any) the auth middleware would have stamped, and returns the walk run id.
func (h *walkHarness) postTeamDef(withPrincipal func(context.Context) context.Context, body string) string {
	h.t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/_teamdef", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if withPrincipal != nil {
		r = r.WithContext(withPrincipal(r.Context()))
	}
	rr := httptest.NewRecorder()
	h.srv.handleSubstrateTeamDef(rr, r)
	if rr.Code != http.StatusOK {
		h.t.Fatalf("POST /v1/_teamdef = %d: %s", rr.Code, rr.Body.String())
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil || out.RunID == "" {
		h.t.Fatalf("no run_id in %s (%v)", rr.Body.String(), err)
	}
	return out.RunID
}

// memberOf returns the run the walk spawned, looked up under userID.
func memberOf(t *testing.T, st store.Store, userID, walkID string) (store.Run, bool) {
	t.Helper()
	runs, err := st.ListActiveRunsByUser(context.Background(), userID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.ParentContext != nil && r.ParentContext.WalkID == walkID {
			return r, true
		}
	}
	return store.Run{}, false
}

func TestTeamDefRun_HTTPWalkRunsAsTheCaller(t *testing.T) {
	h := newWalkHarness(t)
	seedTenantTeam(t, h.st, "acme", "wave", waveTeam)
	h.feed("acme")

	// Alice watches her own run states, the way a client watches a walk.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events := make(chan connector.RunStateEvent, 64)
	go func() {
		_ = h.srv.StreamUserRunStates(ctx, connector.StreamUserRunStatesRequest{
			UserID: "alice", TenantID: "acme", TenantScoped: true,
		}, func(evt connector.RunStateEvent) error {
			events <- evt
			return nil
		})
	}()
	waitForSubscriber(t, h.bus)

	walkID := h.postTeamDef(alicePrincipal, `{"op":"run","name":"wave","input":"go"}`)

	walk, err := h.st.GetRun(context.Background(), walkID)
	if err != nil {
		t.Fatalf("walk run: %v", err)
	}
	if walk.UserID != "alice" || walk.TenantID != "acme" {
		t.Errorf("walk run filed under (tenant %q, user %q), want (acme, alice)", walk.TenantID, walk.UserID)
	}
	member, ok := memberOf(t, h.st, "alice", walkID)
	if !ok {
		t.Fatalf("no member run of walk %s under alice — the walk's agents ran as someone else", walkID)
	}
	if member.TenantID != "acme" {
		t.Errorf("member tenant = %q, want acme", member.TenantID)
	}

	// Her stream, narrowed to this walk by the filter the walk_id route applies,
	// carries the walk's member.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case evt := <-events:
			if walkIDMatches(walkID, evt.ParentContext) && evt.RunID == member.ID {
				return
			}
		case <-deadline:
			t.Fatalf("alice's run-state stream never saw walk %s's member %s", walkID, member.ID)
		}
	}
}

// With no principal (open mode) there is no caller to run as, so the walk
// keeps the synthetic operator identity.
func TestTeamDefRun_HTTPOpenModeWalkKeepsTheSyntheticUser(t *testing.T) {
	h := newWalkHarness(t)
	seedTenantTeam(t, h.st, "", "wave", waveTeam)
	h.feed("")

	walkID := h.postTeamDef(nil, `{"op":"run","name":"wave","input":"go"}`)

	walk, err := h.st.GetRun(context.Background(), walkID)
	if err != nil {
		t.Fatalf("walk run: %v", err)
	}
	if walk.UserID != substrateAdminUserID {
		t.Errorf("open-mode walk user = %q, want the synthetic %q", walk.UserID, substrateAdminUserID)
	}
	if _, ok := memberOf(t, h.st, substrateAdminUserID, walkID); !ok {
		t.Errorf("no member run of walk %s under %q", walkID, substrateAdminUserID)
	}
}

// A walk's pause is an Interruption on the walk's run. It now belongs to the
// caller — it is in HER inbox — and the resolve route still answers it for
// her and for an admin, and still refuses another tenant: the resolve is
// authorized by the run's tenant, not by the user id the walk runs as.
func TestTeamDefRun_HTTPWalkPauseBelongsToTheCallerAndResolves(t *testing.T) {
	admin := func(ctx context.Context) context.Context {
		return auth.WithPrincipal(ctx, auth.Principal{Subject: "root", Scopes: []string{auth.ScopeAdmin}})
	}
	for name, resolver := range map[string]func(context.Context) context.Context{
		"the caller": alicePrincipal,
		"an admin":   admin,
	} {
		t.Run(name, func(t *testing.T) {
			h := newWalkHarness(t)
			seedTenantTeam(t, h.st, "acme", "loop", loopTeam)
			walkID := h.postTeamDef(alicePrincipal, `{"op":"run","name":"loop","input":"go","interrupt_on_cap":true,"mode":"detach"}`)

			pending := waitPendingInterrupt(t, h.st, walkID)
			if pending.UserID != "alice" {
				t.Errorf("the pause is owned by %q, want alice", pending.UserID)
			}
			inbox, err := h.st.InterruptListByUser(context.Background(), "alice", "acme", store.InterruptStatusPending)
			if err != nil || len(inbox) != 1 || inbox[0].InterruptID != pending.InterruptID {
				t.Errorf("alice's inbox = %+v (%v), want the walk's pause", inbox, err)
			}

			evil := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "evil", Subject: "alice", Scopes: []string{auth.ScopeTenant}})
			if _, err := h.srv.ResolveInterrupt(evil, walkID, pending.InterruptID, "", "abort", "", ""); !errors.Is(err, connector.ErrInterruptNotFound) {
				t.Errorf("another tenant resolving the pause: err = %v, want not found", err)
			}
			if _, err := h.srv.ResolveInterrupt(resolver(context.Background()), walkID, pending.InterruptID, "", "abort", "", ""); err != nil {
				t.Fatalf("resolve: %v", err)
			}
			waitRunEnded(t, h.st, walkID)
		})
	}
}

func waitPendingInterrupt(t *testing.T, st store.Store, runID string) store.InterruptRow {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := st.InterruptListByRun(context.Background(), runID, store.InterruptStatusPending)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > 0 {
			return rows[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("walk %s never paused", runID)
	return store.InterruptRow{}
}

func waitRunEnded(t *testing.T, st store.Store, runID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if run, err := st.GetRun(context.Background(), runID); err == nil && run.Status != store.RunRunning {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("walk %s did not end after its pause was answered", runID)
}
