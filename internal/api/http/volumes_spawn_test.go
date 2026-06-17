package http

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// bindingNames extracts the surviving binding names for assertions.
func bindingNames(p tools.VolumePolicyValue) map[string]tools.VolumeBinding {
	out := make(map[string]tools.VolumeBinding, len(p.Bindings))
	for _, b := range p.Bindings {
		out[b.Name] = b
	}
	return out
}

// Spawn confinement (RFC AH §4): a child that declares a parent's volume keeps
// it; a child that does NOT declare a parent's volume cannot use it (the
// intersection drops it). This is the analog of
// TestSubAgent_InheritsParentCallerHostAllowlist for the filesystem axis.
func TestSubAgent_VolumeNarrowOnly_ChildNotDeclaringParentVolumeLosesIt(t *testing.T) {
	parent := tools.VolumePolicyValue{Bindings: []tools.VolumeBinding{
		{Name: "repo-a", Root: "/work/a", Default: true},
		{Name: "shared", Root: "/work/shared", ReadOnly: true},
	}}
	// Child declares only repo-a — it must NOT inherit `shared`.
	child := tools.VolumePolicyValue{Bindings: []tools.VolumeBinding{
		{Name: "repo-a", Root: "/work/a", Default: true},
	}}
	got := bindingNames(narrowVolumes(parent, child))
	if _, ok := got["repo-a"]; !ok {
		t.Error("child declaring repo-a should keep it")
	}
	if _, ok := got["shared"]; ok {
		t.Error("child NOT declaring `shared` must not inherit it (narrow-only)")
	}
}

// A child that declares a volume the PARENT lacks cannot gain it — no widening
// on spawn, ever. (ensemble A's child names ensemble B's volume → denied.)
func TestSubAgent_VolumeNarrowOnly_ChildCannotGainVolumeParentLacks(t *testing.T) {
	parent := tools.VolumePolicyValue{Bindings: []tools.VolumeBinding{
		{Name: "repo-a", Root: "/work/a", Default: true},
	}}
	// Child declares repo-b — which the parent does not hold.
	child := tools.VolumePolicyValue{Bindings: []tools.VolumeBinding{
		{Name: "repo-a", Root: "/work/a", Default: true},
		{Name: "repo-b", Root: "/work/b"},
	}}
	got := bindingNames(narrowVolumes(parent, child))
	if _, ok := got["repo-b"]; ok {
		t.Error("child must not gain repo-b — the parent lacks it (no widening on spawn)")
	}
	if _, ok := got["repo-a"]; !ok {
		t.Error("child should retain the shared repo-a")
	}
}

// Where both parent and child hold a volume, the ro/rw axis resolves to the
// MORE restrictive of the two: a rw child under a ro parent is read-only.
func TestSubAgent_VolumeNarrowOnly_MoreRestrictiveModeWins(t *testing.T) {
	parent := tools.VolumePolicyValue{Bindings: []tools.VolumeBinding{
		{Name: "shared", Root: "/work/shared", ReadOnly: true, Default: true},
	}}
	child := tools.VolumePolicyValue{Bindings: []tools.VolumeBinding{
		{Name: "shared", Root: "/work/shared", ReadOnly: false, Default: true}, // child wants rw
	}}
	got := bindingNames(narrowVolumes(parent, child))
	b, ok := got["shared"]
	if !ok {
		t.Fatal("shared should survive the intersection")
	}
	if !b.ReadOnly {
		t.Error("ro parent + rw child must resolve to ro (more restrictive wins)")
	}
}

// childVolumePolicy orchestration (RFC AH §4) — the real spawn path, not just
// the pure narrowVolumes intersection.

// An INACTIVE parent (legacy / no `default` volume) resolves the child as if
// top-level — its own declared volumes apply (there is no parent volume scope
// to narrow against, and the child's volumes come from its own AgentDef).
func TestChildVolumePolicy_InactiveParentResolvesChildAsTopLevel(t *testing.T) {
	s := &Server{cfg: &config.Config{Volumes: map[string]config.Volume{
		"repo-a": {Path: "/work/a"},
	}}}
	got := s.childVolumePolicy(tools.VolumePolicyValue{}, config.AgentDef{Volumes: []string{"repo-a"}})
	if !got.Active {
		t.Fatal("an inactive parent must resolve the child as top-level (its own declared volumes)")
	}
	if _, ok := bindingNames(got)["repo-a"]; !ok {
		t.Error("child declaring repo-a should get it when the parent is unconfined")
	}
}

// An UNBOUND child of a CONFINED parent inherits the parent's policy verbatim
// (it works within the parent's scope, like host-allowlist inheritance).
func TestChildVolumePolicy_ActiveParentUnboundChildInheritsParent(t *testing.T) {
	s := &Server{cfg: &config.Config{Volumes: map[string]config.Volume{"repo-a": {Path: "/work/a"}}}}
	parent := tools.VolumePolicyValue{Active: true, Bindings: []tools.VolumeBinding{
		{Name: "repo-a", Root: "/work/a", Default: true},
	}}
	got := s.childVolumePolicy(parent, config.AgentDef{}) // unbound child
	if !got.Active || len(got.Bindings) != 1 || got.Bindings[0].Name != "repo-a" {
		t.Errorf("an unbound child of a confined parent must inherit the parent verbatim; got %+v", got)
	}
}

// Spawn-widening fix: a bound child that shares NONE of the parent's volumes is
// confined to an Active-EMPTY policy (every file-tool call denied). It must NOT
// silently retain the parent's policy via ctx inheritance, nor fall back to the
// legacy jail. Before the fix the empty result was a no-op and the child kept
// the parent's full bindings.
func TestChildVolumePolicy_BoundChildSharingNoneDeniedEmpty(t *testing.T) {
	s := &Server{cfg: &config.Config{Volumes: map[string]config.Volume{
		"repo-a": {Path: "/work/a"},
		"repo-b": {Path: "/work/b"},
	}}}
	parent := tools.VolumePolicyValue{Active: true, Bindings: []tools.VolumeBinding{
		{Name: "repo-a", Root: "/work/a", Default: true},
	}}
	got := s.childVolumePolicy(parent, config.AgentDef{Volumes: []string{"repo-b"}})
	if !got.Active {
		t.Fatal("the narrowed child policy must stay ACTIVE (tools deny, not fall back to the legacy jail)")
	}
	if len(got.Bindings) != 0 {
		t.Errorf("child sharing none of the parent's volumes must be confined to nothing; got %+v", got.Bindings)
	}
}
