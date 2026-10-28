package http

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// userReadStore records every user-row read prompt assembly makes.
type userReadStore struct {
	store.Store
	mu    sync.Mutex
	reads []string // "tenant/subject"
}

func (r *userReadStore) UserGet(ctx context.Context, tenant, subject string) (store.UserRow, error) {
	r.mu.Lock()
	r.reads = append(r.reads, tenant+"/"+subject)
	r.mu.Unlock()
	return r.Store.UserGet(ctx, tenant, subject)
}

func (r *userReadStore) userReads() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.reads...)
}

// identityInjectFixture: acme's alice ("Alice Doe", status) is in support,
// which staff contains; globex has a different alice under the same subject.
func identityInjectFixture(t *testing.T, status string) *userReadStore {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "alice", DisplayName: "Alice Doe", AccessMode: "tenant", Status: status}))
	must(st.UserCreate(ctx, store.UserRow{TenantID: "globex", Subject: "alice", DisplayName: "Globex Alice", AccessMode: "tenant", Status: "active"}))
	for _, g := range []string{"support", "staff"} {
		_, err := st.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: g})
		must(err)
	}
	must(st.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "staff", MemberKind: "group", MemberID: "support"}))
	must(st.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "support", MemberKind: "user", MemberID: "alice"}))
	return &userReadStore{Store: st}
}

func resolvedSet(t *testing.T, st store.Store, tenant, subject string) *authz.Effective {
	t.Helper()
	p, err := aclres.New(st, authz.ModeCompat, 0).Resolve(context.Background(), auth.Principal{
		TenantID: tenant, Subject: subject, Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead},
	})
	if err != nil || p.Effective == nil {
		t.Fatalf("resolve %s/%s: %v", tenant, subject, err)
	}
	return p.Effective
}

// A prompt whose only placeholder is {{identity}} renders the run's own
// person: name, status, groups and the tools withheld from this run.
func TestIdentityInjection_RendersTheRunsOwnPerson(t *testing.T) {
	st := identityInjectFixture(t, "active")
	s := &Server{store: st}
	got, _ := s.applyMemoryInjection(context.Background(), config.AgentDef{SystemPrompt: "You help.\n{{identity}}"}, memInject{
		Tenant: "acme", UserID: "alice", AgentName: "a",
		ACL: resolvedSet(t, st, "acme", "alice"), ACLRemovedTools: []string{"Bash"},
	})
	for _, want := range []string{"<identity>", "- name: Alice Doe\n", "- status: active\n", "- groups: staff, support\n", "- tools withheld by access rules: Bash\n"} {
		if !strings.Contains(got.SystemPrompt, want) {
			t.Errorf("missing %q in:\n%s", want, got.SystemPrompt)
		}
	}
	if strings.Contains(got.SystemPrompt, "{{identity}}") || strings.Contains(got.SystemPrompt, "Globex") {
		t.Errorf("unexpanded or another tenant's person:\n%s", got.SystemPrompt)
	}
}

// The note is selected by the store's own pending value, so the runtime's
// literal and the store's constant cannot drift apart unnoticed.
func TestIdentityInjection_PendingUserGetsTheNote(t *testing.T) {
	st := identityInjectFixture(t, store.UserStatusPending)
	s := &Server{store: st}
	got, _ := s.applyMemoryInjection(context.Background(), config.AgentDef{SystemPrompt: "{{identity}}"}, memInject{
		Tenant: "acme", UserID: "alice", ACL: resolvedSet(t, st, "acme", "alice"),
	})
	if !strings.Contains(got.SystemPrompt, "waiting for an operator to approve it") {
		t.Errorf("a pending user has no note:\n%s", got.SystemPrompt)
	}
}

// The user row read is the set's own subject in the run's own tenant. A
// credential of another tenant running here reads no row: the row under the
// same subject id in the run's tenant is somebody else's.
func TestIdentityInjection_ReadsOnlyTheSetsSubjectInTheRunsTenant(t *testing.T) {
	st := identityInjectFixture(t, "active")
	s := &Server{store: st}
	got, _ := s.applyMemoryInjection(context.Background(), config.AgentDef{SystemPrompt: "{{identity}}"}, memInject{
		Tenant: "acme", UserID: "alice", ACL: resolvedSet(t, st, "globex", "alice"),
	})
	if strings.Contains(got.SystemPrompt, "Alice Doe") || strings.Contains(got.SystemPrompt, "- name:") {
		t.Errorf("a globex credential rendered acme's alice:\n%s", got.SystemPrompt)
	}
	if reads := st.userReads(); len(reads) != 0 {
		t.Errorf("user rows read across the tenant line: %v", reads)
	}
	// The wire user id names nobody: with the set's subject a different
	// person, it is the set that decides.
	st2 := identityInjectFixture(t, "active")
	s2 := &Server{store: st2}
	s2.applyMemoryInjection(context.Background(), config.AgentDef{SystemPrompt: "{{identity}}"}, memInject{
		Tenant: "acme", UserID: "mallory", ACL: resolvedSet(t, st2, "acme", "alice"),
	})
	if reads := st2.userReads(); len(reads) != 1 || reads[0] != "acme/alice" {
		t.Errorf("user reads = %v, want [acme/alice]", reads)
	}
}

// No compiled set — open mode, a trigger with no owner — renders nothing,
// and a prompt that does not place the placeholder costs no read.
func TestIdentityInjection_NothingWithoutASetAndNoReadWithoutAReference(t *testing.T) {
	st := identityInjectFixture(t, "active")
	s := &Server{store: st}
	got, _ := s.applyMemoryInjection(context.Background(), config.AgentDef{SystemPrompt: "A {{identity}} B"}, memInject{Tenant: "acme", UserID: "alice"})
	if got.SystemPrompt != "A  B" {
		t.Errorf("no set = %q", got.SystemPrompt)
	}
	s.applyMemoryInjection(context.Background(), config.AgentDef{SystemPrompt: "Plain prompt."}, memInject{
		Tenant: "acme", UserID: "alice", ACL: resolvedSet(t, st, "acme", "alice"),
	})
	if reads := st.userReads(); len(reads) != 0 {
		t.Errorf("user rows read for a prompt with no {{identity}}: %v", reads)
	}
}

// A caller segment (a team node's prompt) is not the agent's own prompt: as
// with the memory variants, {{identity}} there renders nothing rather than
// staying as literal text.
func TestIdentityInjection_CallerSegmentRendersNothing(t *testing.T) {
	st := identityInjectFixture(t, "active")
	s := &Server{store: st}
	sys, user := s.expandCallerSegments(context.Background(), memInject{
		Tenant: "acme", UserID: "alice", ACL: resolvedSet(t, st, "acme", "alice"),
	}, nil, "S {{identity}}", "U {{identity}}", true, true)
	if sys != "S " || user != "U " {
		t.Errorf("caller segments = %q / %q", sys, user)
	}
}
