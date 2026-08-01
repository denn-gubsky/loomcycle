package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// TestCoreBlock_TenantScopeIsSharedAcrossUsers is the P4 "tenant core blocks"
// deliverable, asserted rather than built.
//
// `tenant` has been in validCoreBlockScopes since P1, so a tenant block loads and
// validates — but coreBlockScopeID returned "" for it under a comment reading
// "tenant scope has no scope_id convention in P1 (the entity tier lands later)".
// That comment is what made the deliverable look unbuilt. It is stale: "" IS the
// tenant convention in the k/v plane, where the tenant_id COLUMN carries the
// identity and scope_id is empty — the same asymmetry P4b settled for Memory
// against SQL Memory, which needs a non-empty id because it becomes a schema name.
//
// So this asserts the behaviour the deliverable asked for, in the only way that
// distinguishes "works" from "loads": two DIFFERENT users of one tenant must read
// the SAME block.
func TestCoreBlock_TenantScopeIsSharedAcrossUsers(t *testing.T) {
	s, mi := ontologyFixture(t)
	ctx := context.Background()

	// Seed the block the way an operator would: out-of-band, at tenant scope with
	// the k/v plane's empty scope_id.
	val, _ := json.Marshal("Deploys go through staging. Never push to main.")
	if err := s.store.MemorySet(ctx, mi.Tenant, store.MemoryScopeTenant, "",
		meminject.CoreBlockKeyPrefix+"conventions", val, 0); err != nil {
		t.Fatalf("seed tenant core block: %v", err)
	}

	def := config.AgentDef{
		SystemPrompt: "You are an agent.\n\n{{memory:core_blocks}}",
		CoreBlocks: []config.CoreBlock{
			{Label: "conventions", Scope: "tenant", ReadOnly: true},
		},
	}

	// alice reads it...
	got, _ := s.applyMemoryInjection(ctx, def, mi)
	if !strings.Contains(got.SystemPrompt, "Never push to main") {
		t.Fatalf("the tenant core block never reached alice's prompt:\n%s", got.SystemPrompt)
	}

	// ...and so does bob, from the same tenant. This is the assertion that a
	// per-user scope_id would fail: the block would fork one copy per person and an
	// operator editing "the" block would fix it for exactly one of them.
	other := mi
	other.UserID = "bob"
	gotBob, _ := s.applyMemoryInjection(ctx, def, other)
	if !strings.Contains(gotBob.SystemPrompt, "Never push to main") {
		t.Errorf("a second user of the same tenant did not read the block — it forked per user:\n%s", gotBob.SystemPrompt)
	}
}

// TestCoreBlock_TenantScopeIsTenantIsolated: shared across the tenant is the point;
// shared across TENANTS would be a cross-tenant leak of operator config. The
// tenant_id column is what separates them, so this is the assertion that it is
// actually being used rather than incidentally empty.
func TestCoreBlock_TenantScopeIsTenantIsolated(t *testing.T) {
	s, mi := ontologyFixture(t)
	ctx := context.Background()

	val, _ := json.Marshal("acme-only convention")
	if err := s.store.MemorySet(ctx, mi.Tenant, store.MemoryScopeTenant, "",
		meminject.CoreBlockKeyPrefix+"conventions", val, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	def := config.AgentDef{
		SystemPrompt: "{{memory:core_blocks}}",
		CoreBlocks:   []config.CoreBlock{{Label: "conventions", Scope: "tenant"}},
	}
	stranger := mi
	stranger.Tenant = "other-tenant"

	got, _ := s.applyMemoryInjection(ctx, def, stranger)
	if strings.Contains(got.SystemPrompt, "acme-only convention") {
		t.Errorf("another tenant read this tenant's core block — the tenant_id column is not isolating:\n%s", got.SystemPrompt)
	}
}

// TestCoreBlock_TenantScopeIDIsEmptyByConvention pins the mapping itself, because
// it is the piece most likely to be "fixed" by a future reader who sees "" and
// assumes it is a stub.
//
// SQL Memory refuses an empty scope id (it becomes half a schema name and a
// database role); the k/v plane requires it for tenant, because tenant_id already
// carries the identity there. Two planes, two conventions, both correct — the
// asymmetry that produced the invisible-dirent bug when one leaked into the other.
func TestCoreBlock_TenantScopeIDIsEmptyByConvention(t *testing.T) {
	mi := memInject{Tenant: "acme", UserID: "alice", AgentName: "curator"}
	cases := []struct {
		scope  string
		wantID string
		wantOK bool
		why    string
	}{
		{"agent", "curator", true, "the agent name locates the block"},
		{"user", "alice", true, "the user id locates the block"},
		{"tenant", "", true, "EMPTY BY CONVENTION and resolvable — tenant_id carries the identity"},
		{"nonsense", "", false, "an unknown scope is not resolvable"},
	}
	for _, c := range cases {
		gotID, gotOK := coreBlockScopeID(c.scope, mi)
		if gotID != c.wantID || gotOK != c.wantOK {
			t.Errorf("coreBlockScopeID(%q) = (%q, %v), want (%q, %v) — %s",
				c.scope, gotID, gotOK, c.wantID, c.wantOK, c.why)
		}
	}

	// The identity being MISSING is a different thing from tenant's empty id, and
	// must still be unresolvable — otherwise an agent-scope block on a run with no
	// agent name would read whatever sits at scope_id "".
	empty := memInject{Tenant: "acme"}
	for _, scope := range []string{"agent", "user"} {
		if _, ok := coreBlockScopeID(scope, empty); ok {
			t.Errorf("%s scope with no identity must be unresolvable, not a read at scope_id \"\"", scope)
		}
	}
}

// TestCoreBlock_TenantWriteStillNeedsTheGrant is the safety half, and the reason a
// tenant core block is not simply "a user block with a wider audience".
//
// Everything written here is read by every other agent and user in the tenant — the
// poisoning surface tenant scope was made default-deny for. READING one is safe
// (the operator authored it, and the render path is a server-side read). WRITING
// one must still pass the same `memory_scopes: [tenant]` gate every other tenant
// memory write does, and the rendering fix must not have opened a back door.
//
// Asserted against the Memory TOOL, which is where the gate lives — not against the
// injector, which has no say in it.
func TestCoreBlock_TenantWriteStillNeedsTheGrant(t *testing.T) {
	s, mi := ontologyFixture(t)
	mem := &builtin.Memory{Store: s.store}

	write, _ := json.Marshal(map[string]any{
		"op": "set", "scope": "tenant",
		"key": meminject.CoreBlockKeyPrefix + "conventions", "value": "injected",
	})

	// No tenant grant: the fixture's agent declares none.
	ctx := tools.WithRunIdentity(context.Background(),
		tools.RunIdentityValue{UserID: mi.UserID, TenantID: mi.Tenant})
	res, err := mem.Execute(ctx, write)
	if err == nil && !res.IsError {
		t.Fatal("an ungranted agent wrote a TENANT core block — every agent in the tenant reads that")
	}

	// With the grant it succeeds, so the refusal above is the gate and not some
	// unrelated failure. A test that only proves "it errored" would pass against a
	// typo in the op name.
	granted := tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{
		AllowedScopes: []string{"agent", "user", "tenant"},
	})
	if res, err := mem.Execute(granted, write); err != nil || res.IsError {
		t.Errorf("the same write must succeed WITH the grant: %v %s", err, res.Text)
	}
}
