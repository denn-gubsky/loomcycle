package http

import (
	"testing"

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

// An unbound parent (legacy jail, no policy) yields a child with no named
// volumes — a bound child cannot widen past the parent's absent policy; it
// inherits the same unbound legacy-jail fallback.
func TestSubAgent_VolumeNarrowOnly_UnboundParentYieldsUnboundChild(t *testing.T) {
	parent := tools.VolumePolicyValue{} // unbound
	child := tools.VolumePolicyValue{Bindings: []tools.VolumeBinding{
		{Name: "repo-a", Root: "/work/a", Default: true},
	}}
	got := narrowVolumes(parent, child)
	if len(got.Bindings) != 0 {
		t.Errorf("unbound parent must yield an unbound child; got %d bindings", len(got.Bindings))
	}
}
