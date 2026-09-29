package http

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// An armed subscription walks a promoted team with nobody on ctx, so the walk
// runs as confined as whoever PROMOTED the team. These drive the real sweep —
// peek, op=run, admission, the walk, the member's provider call — and read the
// member's identity and operator-key permission at the provider, which is
// where the operator-key backstop and the isolation checks read them.

// promoterHarness is a server whose one team, "armed", is a Starter over the
// tenant channel "in" fanning out to "writer", over a provider that records the
// identity and operator-key permission of every call. The operator-key gate is
// on unless a test turns it off.
type promoterHarness struct {
	t    *testing.T
	srv  *Server
	st   *storesqlite.Store
	prov *identityRecordingProvider
	pub  *channels.StorePublisher
}

func newPromoterHarness(t *testing.T) *promoterHarness {
	t.Helper()
	return newPromoterHarnessGate(t, true)
}

func newPromoterHarnessGate(t *testing.T, gateOn bool) *promoterHarness {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"writer": {Provider: "scripted", Model: "stub-model", SystemPrompt: "write", Tools: []string{}},
		},
		Channels: map[string]config.Channel{
			"in":  {Scope: "tenant", Semantic: "queue", MaxMessages: 100},
			"out": {Scope: "tenant", Semantic: "queue", MaxMessages: 100},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	cfg.Env.OperatorKeyRestriction = gateOn
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "promoter.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	prov := &identityRecordingProvider{scriptedProvider: &scriptedProvider{
		defaultS: []providers.Event{
			{Type: providers.EventText, Text: "written"},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
		},
	}}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	cbus := channels.NewBus()
	pub := &channels.StorePublisher{Store: st, Bus: cbus, Scheduler: channels.NewScheduler(cbus, 100)}
	srv.SetSystemPublisher(pub)
	srv.SetChannelBus(cbus)
	srv.SetTeamDefTool(&builtin.TeamDef{Store: st})
	return &promoterHarness{t: t, srv: srv, st: st, prov: prov, pub: pub}
}

// teamCall runs one TeamDef op through the connector, as a caller presenting
// run and (optionally) principal.
func (h *promoterHarness) teamCall(run tools.RunIdentityValue, principal *auth.Principal, body string) map[string]any {
	h.t.Helper()
	ctx := tools.WithRunIdentity(context.Background(), run)
	if principal != nil {
		ctx = auth.WithPrincipal(ctx, *principal)
	}
	res, err := h.srv.TeamDef(ctx, json.RawMessage(body))
	if err != nil || res.IsError {
		h.t.Fatalf("TeamDef %s: %v %s", body, err, res.Text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		h.t.Fatalf("TeamDef result %s: %v", res.Text, err)
	}
	return out
}

// authorUnpromoted writes the team in acme without arming it and returns its
// def id, so each test chooses who promotes it. Written to the store directly:
// authoring is not under test, and a team's channel ACL may only narrow its
// author's own grants, which these callers do not carry.
func (h *promoterHarness) authorUnpromoted() string {
	h.t.Helper()
	def, err := teamgraph.Parse([]byte(waveTeam))
	if err != nil {
		h.t.Fatal(err)
	}
	row, err := h.st.TeamDefCreate(context.Background(), store.TeamDefRow{
		DefID: "tdf_armed", Name: "armed", TenantID: "acme",
		Definition: json.RawMessage(waveTeam), ContentSHA256: teamgraph.Sign("armed", def),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return row.DefID
}

func (h *promoterHarness) promote(run tools.RunIdentityValue, principal *auth.Principal, defID string) {
	h.t.Helper()
	h.teamCall(run, principal, `{"op":"promote","def_id":"`+defID+`"}`)
}

// walkOnce puts one message on the source, runs one sweep, and returns the
// identities and operator-key permission the walk's member ran under.
func (h *promoterHarness) walkOnce() ([]tools.RunIdentityValue, bool) {
	h.t.Helper()
	h.prov.mu.Lock()
	h.prov.seen = nil
	h.prov.mu.Unlock()
	h.prov.lastOpKeyAllowed.Store(false)
	if _, err := h.pub.PublishNow(context.Background(), "in", "acme", store.MemoryScopeTenant, "",
		json.RawMessage(`{"task":"write"}`), "_system", 0, 0); err != nil {
		h.t.Fatalf("publish: %v", err)
	}
	started, err := h.srv.SweepTeamSubscriptions(context.Background())
	if err != nil || started != 1 {
		h.t.Fatalf("sweep started %d walk(s), err %v; want 1", started, err)
	}
	h.prov.mu.Lock()
	defer h.prov.mu.Unlock()
	if len(h.prov.seen) == 0 {
		h.t.Fatal("the subscription walk never reached its member's provider call")
	}
	return append([]tools.RunIdentityValue(nil), h.prov.seen...), h.prov.lastOpKeyAllowed.Load()
}

func assertWalkConfined(t *testing.T, what string, seen []tools.RunIdentityValue, opKeyAllowed bool) {
	t.Helper()
	if opKeyAllowed {
		t.Errorf("%s: the member's provider call was allowed the operator's key", what)
	}
	for i, id := range seen {
		if !id.OperatorKeyRestricted || !id.Isolated {
			t.Errorf("%s: member call %d ran with restricted=%v isolated=%v, want both", what, i, id.OperatorKeyRestricted, id.Isolated)
		}
	}
}

func assertWalkUnconfined(t *testing.T, what string, seen []tools.RunIdentityValue, opKeyAllowed bool) {
	t.Helper()
	if !opKeyAllowed {
		t.Errorf("%s: the member's provider call was denied the operator's key", what)
	}
	for i, id := range seen {
		if id.OperatorKeyRestricted || id.Isolated {
			t.Errorf("%s: member call %d ran with restricted=%v isolated=%v, want neither", what, i, id.OperatorKeyRestricted, id.Isolated)
		}
	}
}

// A team promoted by a confined caller — a restricted principal, or a run whose
// own bits are confined with no principal (a resumed run) — is walked by the
// sweep denied the operator's key and isolated, though nobody is on the
// sweep's ctx.
func TestTeamSubscription_WalkOfAConfinedPromotersTeamIsConfined(t *testing.T) {
	restricted := auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeUser}}
	for name, promoter := range map[string]struct {
		run       tools.RunIdentityValue
		principal *auth.Principal
	}{
		"restricted principal": {run: tools.RunIdentityValue{AgentID: "a_alice", TenantID: "acme", UserID: "alice"}, principal: &restricted},
		"confined run":         {run: tools.RunIdentityValue{AgentID: "a_confined", TenantID: "acme", UserID: "alice", OperatorKeyRestricted: true, Isolated: true}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newPromoterHarness(t)
			h.promote(promoter.run, promoter.principal, h.authorUnpromoted())
			seen, allowed := h.walkOnce()
			assertWalkConfined(t, name, seen, allowed)
		})
	}
}

// Promoted by an unrestricted admin, the same walk keeps the operator's key and
// is not isolated — the capture confines only as far as its promoter was.
func TestTeamSubscription_WalkOfAnAdminPromotedTeamIsUnrestricted(t *testing.T) {
	h := newPromoterHarness(t)
	admin := auth.Principal{Subject: "root", Scopes: []string{auth.ScopeAdmin}}
	h.promote(tools.RunIdentityValue{AgentID: "a_root", TenantID: "acme"}, &admin, h.authorUnpromoted())
	seen, allowed := h.walkOnce()
	assertWalkUnconfined(t, "admin-promoted", seen, allowed)
}

// armUncaptured arms the team the way a pre-upgrade deployment has it: an
// active pointer with no promoter capture.
func (h *promoterHarness) armUncaptured() string {
	h.t.Helper()
	defID := h.authorUnpromoted()
	if _, err := h.st.SnapshotRestoreTeamDefActive(context.Background(), store.TeamDefActiveEntry{
		Name: "armed", TenantID: "acme", DefID: defID, PromotedAt: time.Now(),
	}); err != nil {
		h.t.Fatal(err)
	}
	return defID
}

// A team promoted before the capture existed has none. Its walks run
// restricted and isolated — fail closed — and say so ONCE, however many walks
// follow; promoting it again records a capture and the walk runs with it.
func TestTeamSubscription_UncapturedPromoterWalksConfinedAndWarnsOnce(t *testing.T) {
	h := newPromoterHarness(t)
	logged, restore := captureLog(t)
	defer restore()
	defID := h.armUncaptured()

	for i := 1; i <= 2; i++ {
		seen, allowed := h.walkOnce()
		assertWalkConfined(t, "uncaptured walk", seen, allowed)
		if n := strings.Count(logged(), "Promote the team again"); n != 1 {
			t.Errorf("after walk %d the re-promote warning was logged %d times, want once", i, n)
		}
	}

	admin := auth.Principal{Subject: "root", Scopes: []string{auth.ScopeAdmin}}
	h.promote(tools.RunIdentityValue{AgentID: "a_root", TenantID: "acme"}, &admin, defID)
	seen, allowed := h.walkOnce()
	assertWalkUnconfined(t, "re-promoted walk", seen, allowed)
}

// With the operator-key gate off nobody is denied the operator's key, so an
// uncaptured team's walk keeps it too — gate-off stays byte-identical — while
// still running isolated and still warning once.
func TestTeamSubscription_UncapturedPromoterUnderGateOffKeepsTheKeyButIsIsolated(t *testing.T) {
	h := newPromoterHarnessGate(t, false)
	logged, restore := captureLog(t)
	defer restore()
	h.armUncaptured()

	for i := 1; i <= 2; i++ {
		seen, allowed := h.walkOnce()
		if !allowed {
			t.Errorf("walk %d: the member's provider call was denied the operator's key with the gate off", i)
		}
		for j, id := range seen {
			if id.OperatorKeyRestricted || !id.Isolated {
				t.Errorf("walk %d: member call %d ran with restricted=%v isolated=%v, want unrestricted and isolated",
					i, j, id.OperatorKeyRestricted, id.Isolated)
			}
		}
		if n := strings.Count(logged(), "Promote the team again"); n != 1 {
			t.Errorf("after walk %d the re-promote warning was logged %d times, want once", i, n)
		}
	}
}
