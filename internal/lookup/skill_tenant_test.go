package lookup_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/skills"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// stubSkillStore implements lookup.SkillStore honouring the RFC N tenant
// axis. Active rows are keyed by (tenant, name).
type stubSkillStore struct {
	active map[string]store.SkillDefRow
}

func skillKey(tenantID, name string) string { return tenantID + "\x00" + name }

func (s *stubSkillStore) SkillDefGetActive(_ context.Context, tenantID, name string) (store.SkillDefRow, error) {
	if row, ok := s.active[skillKey(tenantID, name)]; ok {
		return row, nil
	}
	return store.SkillDefRow{}, &store.ErrNotFound{Kind: "skill_def_active", ID: name}
}

func mustSubstrateSkill(t *testing.T, body string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(lookup.SubstrateSkillDef{Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// staticSetWith builds a skills.Set from a temp dir holding one
// (name, body) SKILL.md — the established test pattern (LoadSet from
// disk; the Set has no exported constructor).
func staticSetWith(t *testing.T, name, body string) *skills.Set {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: " + name + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}
	set, err := skills.LoadSet(root)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	return set
}

// TestSkill_TenantResolutionPrecedence pins the RFC N resolution model
// for skills: a tenant-scoped substrate promotion shadows the shared
// static base by name, while a different tenant resolving the same name
// falls through to the shared static base (it cannot see tenant A's
// private skill).
func TestSkill_TenantResolutionPrecedence(t *testing.T) {
	ss := &stubSkillStore{active: map[string]store.SkillDefRow{
		skillKey("tenant-a", "shared"): {
			DefID:      "sd_a_v1",
			Name:       "shared",
			Definition: mustSubstrateSkill(t, "tenant-A private"),
		},
	}}
	set := staticSetWith(t, "shared", "operator shared base")

	// Tenant A: its substrate promotion shadows the shared static base.
	gotA, ok := lookup.Skill(context.Background(), ss, set, "tenant-a", "shared")
	if !ok {
		t.Fatal("tenant-a resolve !ok")
	}
	if gotA.Body != "tenant-A private" || gotA.Source != "substrate" {
		t.Errorf("tenant-a: got body=%q source=%q, want its own substrate def", gotA.Body, gotA.Source)
	}

	// Tenant B: no private promotion → falls through to the shared static
	// base. It must NOT see tenant A's private substrate def.
	gotB, ok := lookup.Skill(context.Background(), ss, set, "tenant-b", "shared")
	if !ok {
		t.Fatal("tenant-b resolve !ok")
	}
	if gotB.Body != "operator shared base" || gotB.Source != "static" {
		t.Errorf("tenant-b: got body=%q source=%q, want the shared static base (no cross-tenant leak)", gotB.Body, gotB.Source)
	}
}

// TestSkill_DefaultTenantPreservesSubstrateFirstOrder pins the
// back-compat invariant: for the default tenant "", a name present in
// BOTH the shared substrate AND the static set resolves to the SUBSTRATE
// one — identical to the pre-RFC-N "substrate first, then static" order
// (the opposite of the agent plane).
func TestSkill_DefaultTenantPreservesSubstrateFirstOrder(t *testing.T) {
	ss := &stubSkillStore{active: map[string]store.SkillDefRow{
		skillKey("", "dual"): {
			DefID:      "sd_shared_v1",
			Name:       "dual",
			Definition: mustSubstrateSkill(t, "substrate wins"),
		},
	}}
	set := staticSetWith(t, "dual", "static loses")

	got, ok := lookup.Skill(context.Background(), ss, set, "", "dual")
	if !ok {
		t.Fatal("resolve !ok")
	}
	if got.Body != "substrate wins" || got.Source != "substrate" {
		t.Errorf("default tenant precedence broke: got body=%q source=%q, want the shared substrate def", got.Body, got.Source)
	}
}
