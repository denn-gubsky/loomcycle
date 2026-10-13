package tools

import "testing"

// Add must not clear read-only. Callers add from a mode read a moment before
// (a stored row, a create's answer); without this, that stale mode undid a
// SetReadOnly that ran in between and handed write access back.
func TestEphemeralVolumeSet_AddNeverMakesAReadOnlyVolumeWritable(t *testing.T) {
	set := NewEphemeralVolumeSet()
	set.Add("repo", EphemeralVolumeRef{Root: "/a"})
	if !set.SetReadOnly("repo") {
		t.Fatal("SetReadOnly did not find the volume")
	}
	set.Add("repo", EphemeralVolumeRef{Root: "/a"}) // the stale, writable mode
	if ref, _ := set.Get("repo"); !ref.ReadOnly {
		t.Error("Add made a read-only volume writable")
	}
	// Adding read-only over a writable entry does take effect.
	set.Add("notes", EphemeralVolumeRef{Root: "/b"})
	set.Add("notes", EphemeralVolumeRef{Root: "/b", ReadOnly: true})
	if ref, _ := set.Get("notes"); !ref.ReadOnly {
		t.Error("Add did not make a writable volume read-only")
	}
	if set.SetReadOnly("absent") {
		t.Error("SetReadOnly reported a volume that is not there")
	}
	if got := set.Names(); len(got) != 2 || got[0] != "notes" || got[1] != "repo" {
		t.Errorf("Names = %v, want [notes repo]", got)
	}
}
