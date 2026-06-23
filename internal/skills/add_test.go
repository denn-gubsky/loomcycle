package skills

import "testing"

func TestSet_Add(t *testing.T) {
	set, err := LoadSet("") // empty root → non-nil empty set
	if err != nil {
		t.Fatal(err)
	}
	set.Add(&Skill{Name: "x", Body: "first"})
	if sk, ok := set.Get("x"); !ok || sk.Body != "first" {
		t.Fatalf("Get after Add = (%+v, %v), want body=first", sk, ok)
	}
	// Add overwrites by name (inline-overlays-root semantics).
	set.Add(&Skill{Name: "x", Body: "second"})
	if sk, _ := set.Get("x"); sk.Body != "second" {
		t.Errorf("Add did not overwrite: body=%q, want second", sk.Body)
	}
	// Nameless / nil are no-ops, not panics.
	set.Add(&Skill{Name: ""})
	set.Add(nil)
	var nilSet *Set
	nilSet.Add(&Skill{Name: "y"}) // nil receiver no-op
}
