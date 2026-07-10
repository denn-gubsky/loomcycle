package teamgraph

import "testing"

func TestSign_DeterministicAndColorExcluded(t *testing.T) {
	base, _ := Parse([]byte(sdlcJSON))
	h1 := Sign("sdlc", base)
	h2 := Sign("sdlc", base)
	if h1 != h2 {
		t.Fatalf("Sign not deterministic: %q vs %q", h1, h2)
	}
	if len(h1) != len("sha256:")+64 {
		t.Errorf("hash shape wrong: %q", h1)
	}

	// Adding a color scheme must NOT change the hash (presentation, not content).
	colored := base
	colored.Colors = &Colors{
		States:      map[string]string{"review": "pink"},
		Transitions: map[string]string{"success": "#2f9e44"},
	}
	if Sign("sdlc", colored) != h1 {
		t.Errorf("colours must be excluded from the content hash")
	}

	// A different name → different hash.
	if Sign("other", base) == h1 {
		t.Errorf("name is content-identifying; hash should differ")
	}
	// A graph change → different hash.
	changed := base
	changed.MaxIterations = 99
	if Sign("sdlc", changed) == h1 {
		t.Errorf("a graph change should change the hash")
	}
}
