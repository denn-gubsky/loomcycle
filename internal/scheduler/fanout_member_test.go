package scheduler

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/errclassify"
	"github.com/denn-gubsky/loomcycle/internal/errkind"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// classifiedToolFailure is the tool_result event a run's loop emits when a
// tool call fails with err: the Agent tool classifies a refused spawn through
// errclassify, and the loop carries that classification on ErrorInfo.
func classifiedToolFailure(t *testing.T, err error) providers.Event {
	t.Helper()
	info, ok := errclassify.CategoryOf(err)
	if !ok {
		t.Fatalf("errclassify does not classify %v", err)
	}
	return providers.Event{
		Type:      providers.EventToolResult,
		ToolUse:   &providers.ToolUse{ID: "tu_1", Name: "Agent"},
		Text:      err.Error(),
		IsError:   true,
		ErrorInfo: &info,
	}
}

// TestFanout_SubRunOperatorKeyRefusalIsDeferred: with the operator key
// restricted, the bundled code-js consolidator needs no key itself, so the
// refusal lands on the extractor it spawns. The pass catches it, holds its
// watermark and completes, and RunOnce returns nil — the schedule read
// "completed" for a tenant that was never consolidated, and nothing named it.
//
// Now the refusal on the pass's event stream defers the pass, and the tenant is
// logged once for the tick however many of its passes were refused.
//
// Fails-before: last_status is completed and no line names tenant acme.
func TestFanout_SubRunOperatorKeyRefusalIsDeferred(t *testing.T) {
	for _, refusal := range []error{resolve.ErrOperatorKeyRestricted, providers.ErrOperatorKeyForbidden} {
		t.Run(refusal.Error(), func(t *testing.T) {
			sched, fr, st, logs := fanoutFixture(t, operatorFanoutDef(nil), func(c *Config) {
				c.OperatorKeyRestriction = true
			})
			sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
			fr.onCallbacks = func(in runner.RunInput, cb runner.RunCallbacks) {
				if in.TenantID == "acme" {
					cb.OnEvent(classifiedToolFailure(t, refusal))
				}
			}
			seedSettledSession(t, st, "default", "alice")
			seedSettledSession(t, st, "acme", "sam")
			seedSettledSession(t, st, "acme", "tia")

			fireT(t, sched)

			if got := len(fr.Calls()); got != 3 {
				t.Fatalf("RunOnce calls = %d, want 3; logs:\n%s", got, logs.all())
			}
			state := scheduleState(t, sched, "sd-test")
			if state.LastStatus != "skipped" {
				t.Errorf("last_status = %q (%q), want skipped — acme's passes ran but consolidated nothing; logs:\n%s",
					state.LastStatus, state.LastError, logs.all())
			}
			if !strings.Contains(state.LastError, "2 deferred by a token budget or the operator-key restriction") {
				t.Errorf("last_error = %q, want it to count acme's two deferred passes", state.LastError)
			}
			var tenantLines []string
			for _, line := range strings.Split(logs.all(), "\n") {
				if strings.Contains(line, "operator_key_restricted") {
					tenantLines = append(tenantLines, line)
				}
			}
			if len(tenantLines) != 1 || !strings.Contains(tenantLines[0], `tenant "acme": 2 pass(es) deferred`) {
				t.Errorf("want exactly one line naming tenant acme and its 2 deferred passes, got %q", tenantLines)
			}
		})
	}
}

// TestFanout_OtherPermissionRefusalInsidePassStaysCompleted: only the
// operator-key refusal defers a pass. A permission failure that no key fixes —
// a scope the agent was not granted — is the pass's own business, as it was.
func TestFanout_OtherPermissionRefusalInsidePassStaysCompleted(t *testing.T) {
	sched, fr, st, logs := fanoutFixture(t, operatorFanoutDef(nil), func(c *Config) {
		c.OperatorKeyRestriction = true
	})
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
	fr.onCallbacks = func(_ runner.RunInput, cb runner.RunCallbacks) {
		cb.OnEvent(providers.Event{
			Type:    providers.EventToolResult,
			ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Memory"},
			IsError: true,
			ErrorInfo: &errkind.Info{
				Category:    errkind.CategoryPermission,
				Description: "scope \"tenant\" is not in memory_scopes",
			},
		})
	}
	seedSettledSession(t, st, "acme", "sam")

	fireT(t, sched)

	if state := scheduleState(t, sched, "sd-test"); state.LastStatus != "completed" {
		t.Errorf("last_status = %q (%q), want completed; logs:\n%s", state.LastStatus, state.LastError, logs.all())
	}
}

// TestIsOperatorKeyRefusal_MatchesOnlyTheOperatorKeyClassification pins the
// signal to the classifier's own output for both layers of the restriction,
// and to nothing else that shares its category.
func TestIsOperatorKeyRefusal_MatchesOnlyTheOperatorKeyClassification(t *testing.T) {
	for _, err := range []error{resolve.ErrOperatorKeyRestricted, providers.ErrOperatorKeyForbidden} {
		info, _ := errclassify.CategoryOf(err)
		if !isOperatorKeyRefusal(&info) {
			t.Errorf("isOperatorKeyRefusal(classification of %v) = false, want true", err)
		}
	}
	for _, info := range []*errkind.Info{
		nil,
		{Category: errkind.CategoryPermission, Description: "a scope was not granted"},
		{Category: errkind.CategoryBusiness},
	} {
		if isOperatorKeyRefusal(info) {
			t.Errorf("isOperatorKeyRefusal(%+v) = true, want false", info)
		}
	}
}

// userListCountingStore counts UserList reads, and fails them when err is set.
type userListCountingStore struct {
	store.Store
	mu    *sync.Mutex
	calls *int
	err   error
}

func (s userListCountingStore) UserList(ctx context.Context, tenantID string) ([]store.UserRow, error) {
	s.mu.Lock()
	*s.calls++
	s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	return s.Store.UserList(ctx, tenantID)
}

func createMember(t *testing.T, st store.Store, tenantID, subject, accessMode string) {
	t.Helper()
	if err := st.UserCreate(context.Background(), store.UserRow{
		TenantID: tenantID, Subject: subject, AccessMode: accessMode, Status: "active",
	}); err != nil {
		t.Fatalf("UserCreate(%s/%s): %v", tenantID, subject, err)
	}
}

// TestFanout_IsolatedMemberPassRunsIsolated: a pass runs as its target user,
// and an isolated member's own runs are confined to that user's scopes. The
// sweep could not tell who was isolated — that lives on the member's token — so
// an isolated member's pass ran with the tenant's whole reach. The member's
// stored access_mode says it, and is read once for the whole tick.
//
// Fails-before: sam's pass carries Isolated=false, and UserList is never read.
func TestFanout_IsolatedMemberPassRunsIsolated(t *testing.T) {
	sched, fr, st, logs := fanoutFixture(t, operatorFanoutDef(nil), nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
	var (
		mu    sync.Mutex
		calls int
	)
	sched.store = userListCountingStore{Store: st, mu: &mu, calls: &calls}

	createMember(t, st, "acme", "sam", "isolated")
	createMember(t, st, "acme", "tia", "tenant")
	// The same subject in another tenant is another member.
	createMember(t, st, "beta", "sam", "tenant")
	seedSettledSession(t, st, "acme", "sam")
	seedSettledSession(t, st, "acme", "tia")
	seedSettledSession(t, st, "beta", "sam")
	seedSettledSession(t, st, "default", "alice") // no users row

	fireT(t, sched)

	got := map[string]bool{}
	for _, c := range fr.Calls() {
		got[c.TenantID+"/"+c.UserID] = c.Isolated
	}
	want := map[string]bool{"acme/sam": true, "acme/tia": false, "beta/sam": false, "default/alice": false}
	if len(got) != len(want) {
		t.Fatalf("dispatched %v, want %v; logs:\n%s", got, want, logs.all())
	}
	for target, w := range want {
		if got[target] != w {
			t.Errorf("pass for %s: Isolated = %v, want %v", target, got[target], w)
		}
	}
	if calls != 1 {
		t.Errorf("UserList read %d time(s) for one tick, want 1", calls)
	}
}

// TestFanout_IsolatedDefStaysIsolatedForATenantMember: the member's mode only
// ever narrows. A def captured as isolated still confines every pass.
func TestFanout_IsolatedDefStaysIsolatedForATenantMember(t *testing.T) {
	def := fanoutDef(nil)
	def.TenantID = "acme"
	def.Isolated = true
	sched, fr, st, _ := fanoutFixture(t, def, nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
	createMember(t, st, "acme", "tia", "tenant")
	seedSettledSession(t, st, "acme", "tia")

	fireT(t, sched)

	calls := fr.Calls()
	if len(calls) != 1 || !calls[0].Isolated {
		t.Fatalf("calls = %+v, want one isolated pass", calls)
	}
}

// TestFanout_MemberLookupFaultSkipsTheTick: without the access modes an
// isolated member's pass would run unconfined, so a lookup fault dispatches
// nothing and the schedule reads failed, as an enumeration fault does.
//
// Fails-before: the pass is dispatched (the lookup does not exist).
func TestFanout_MemberLookupFaultSkipsTheTick(t *testing.T) {
	sched, fr, st, _ := fanoutFixture(t, operatorFanoutDef(nil), nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
	var (
		mu    sync.Mutex
		calls int
	)
	sched.store = userListCountingStore{Store: st, mu: &mu, calls: &calls, err: errors.New("boom-users")}
	seedSettledSession(t, st, "acme", "sam")

	fireT(t, sched)

	if n := len(fr.Calls()); n != 0 {
		t.Errorf("RunOnce calls = %d, want 0 — a pass must not run without its member's access mode", n)
	}
	state := scheduleState(t, sched, "sd-test")
	if state.LastStatus != "failed" || !strings.Contains(state.LastError, "boom-users") {
		t.Errorf("last_status = %q (%q), want failed naming the lookup fault", state.LastStatus, state.LastError)
	}
}
