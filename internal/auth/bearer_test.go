package auth

import "testing"

func TestCompareBearer_EqualReturnsTrue(t *testing.T) {
	if !CompareBearer("Bearer abc123", "Bearer abc123") {
		t.Fatal("equal tokens should compare true")
	}
}

func TestCompareBearer_DifferentContentSameLengthReturnsFalse(t *testing.T) {
	if CompareBearer("Bearer aaaaaaaa", "Bearer bbbbbbbb") {
		t.Fatal("different content (same length) should compare false")
	}
}

func TestCompareBearer_DifferentLengthReturnsFalse(t *testing.T) {
	// The whole point of the helper: even when one side is empty
	// or wildly shorter than the other, the function must still
	// return false. The hash-then-CTC approach guarantees this
	// without leaking the length difference via timing.
	if CompareBearer("", "Bearer abc123") {
		t.Fatal("empty got should compare false")
	}
	if CompareBearer("Bearer abc123", "") {
		t.Fatal("empty want should compare false")
	}
	if CompareBearer("a", "abcdefghijklmnopqrstuvwxyz") {
		t.Fatal("very-different lengths should compare false")
	}
}

func TestCompareBearer_BothEmptyReturnsTrue(t *testing.T) {
	// Empty == empty is technically equal. Callers (the auth
	// middleware) gate this case separately by checking the
	// token-not-set "open mode" branch BEFORE calling
	// CompareBearer; the test documents the helper's pure
	// behavior.
	if !CompareBearer("", "") {
		t.Fatal("both empty should compare true")
	}
}
