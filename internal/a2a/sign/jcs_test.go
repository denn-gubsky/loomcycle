package sign

import (
	"testing"
)

// TestCanonicalize_SortsObjectKeysDeterministically asserts JCS object
// key ordering is by code unit and independent of insertion order, since
// signer and verifier must hash byte-identical canonical forms (Go map
// iteration order is otherwise randomised).
func TestCanonicalize_SortsObjectKeysDeterministically(t *testing.T) {
	in := map[string]any{"b": 1, "a": 2, "c": 3}
	got, err := Canonicalize(in)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	want := `{"a":2,"b":1,"c":3}`
	if string(got) != want {
		t.Fatalf("Canonicalize = %q, want %q", got, want)
	}
}

// TestCanonicalize_RFC8785NestedVector pins a small RFC 8785-style
// vector: nested objects/arrays are recursively sorted, whitespace is
// stripped, and integers are emitted literally. A deterministic vector
// guards the canonical form against silent drift.
func TestCanonicalize_RFC8785NestedVector(t *testing.T) {
	in := map[string]any{
		"name": "card",
		"nums": []any{3, 1, 2},
		"meta": map[string]any{"z": true, "a": "x"},
	}
	got, err := Canonicalize(in)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	// Top-level keys sorted (meta,name,nums); meta's keys sorted (a,z);
	// array order preserved; no whitespace.
	want := `{"meta":{"a":"x","z":true},"name":"card","nums":[3,1,2]}`
	if string(got) != want {
		t.Fatalf("Canonicalize = %q, want %q", got, want)
	}
}

// TestCanonicalize_StableAcrossRepeatedCalls asserts the canonical bytes
// are identical across calls on a map (catches any reliance on map order
// leaking through). Runs many iterations so a randomised order would
// almost certainly diverge at least once.
func TestCanonicalize_StableAcrossRepeatedCalls(t *testing.T) {
	in := map[string]any{"one": 1, "two": 2, "three": 3, "four": 4, "five": 5}
	first, err := Canonicalize(in)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	for i := 0; i < 50; i++ {
		got, err := Canonicalize(in)
		if err != nil {
			t.Fatalf("Canonicalize iter %d: %v", i, err)
		}
		if string(got) != string(first) {
			t.Fatalf("canonical form not stable: iter %d = %q, first = %q", i, got, first)
		}
	}
}

// TestCanonicalize_EscapesOnlyMandatoryChars asserts JCS string escaping
// uses only the mandatory escapes and emits other characters (including
// non-ASCII) as literal UTF-8.
func TestCanonicalize_EscapesOnlyMandatoryChars(t *testing.T) {
	in := map[string]any{"s": "a\"b\\c\nördög"}
	got, err := Canonicalize(in)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	want := `{"s":"a\"b\\c\nördög"}`
	if string(got) != want {
		t.Fatalf("Canonicalize = %q, want %q", got, want)
	}
}
