package codejs

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// These tests pin the replay-determinism fix for the Go-map key-order bug:
// caller metadata arrives as Go map[string]any (key order already lost +
// randomized per goja conversion), so without stableJSValue an agent that
// JSON.stringify(s) input.metadata emitted byte-different bytes across replay
// turns → spurious code_agent_replay_divergence. stableJSValue materializes
// every map with SORTED keys, so input.* is byte-stable across turns.

// TestStableJSValue_MetadataKeysSorted: top-level input.metadata keys (incl. the
// loop-stamped user_id/agent) come out sorted, regardless of Go-map order.
func TestStableJSValue_MetadataKeysSorted(t *testing.T) {
	p := newTestProvider(t.TempDir())
	got := runOnce(t, p, providers.RunMeta{
		AgentName: "x",
		UserID:    "u",
		CodeBody:  `function run(input){ return { final_text: Object.keys(input.metadata).join(",") }; }`,
		Metadata:  map[string]any{"zebra": 1, "mango": 2, "alpha": 3},
	})
	// buildInput adds user_id + agent; all keys sorted:
	const want = "agent,alpha,mango,user_id,zebra"
	if got != want {
		t.Fatalf("input.metadata keys not sorted/stable: got %q want %q", got, want)
	}
}

// TestStableJSValue_NestedObjectKeysSorted: recursion sorts nested object keys.
func TestStableJSValue_NestedObjectKeysSorted(t *testing.T) {
	p := newTestProvider(t.TempDir())
	got := runOnce(t, p, providers.RunMeta{
		AgentName: "x",
		CodeBody:  `function run(input){ return { final_text: JSON.stringify(input.metadata.nested) }; }`,
		Metadata:  map[string]any{"nested": map[string]any{"charlie": 1, "alpha": 2, "bravo": 3}},
	})
	const want = `{"alpha":2,"bravo":3,"charlie":1}`
	if got != want {
		t.Fatalf("nested object keys not sorted: got %q want %q", got, want)
	}
}

// TestStableJSValue_ArrayOfObjectsKeysSorted: recursion descends through arrays
// (the ats-filter-batch shape: metadata.matches = [{...}, ...]) and sorts each
// element-object's keys while preserving array order.
func TestStableJSValue_ArrayOfObjectsKeysSorted(t *testing.T) {
	p := newTestProvider(t.TempDir())
	got := runOnce(t, p, providers.RunMeta{
		AgentName: "x",
		CodeBody:  `function run(input){ return { final_text: JSON.stringify(input.metadata.matches) }; }`,
		Metadata: map[string]any{"matches": []any{
			map[string]any{"score": 1, "id": "a", "name": "first"},
			map[string]any{"score": 2, "id": "b", "name": "second"},
		}},
	})
	const want = `[{"id":"a","name":"first","score":1},{"id":"b","name":"second","score":2}]`
	if got != want {
		t.Fatalf("array-of-objects not stably key-sorted: got %q want %q", got, want)
	}
}

// TestStableJSValue_StableAcrossRuntimeBuilds is the direct replay-stability
// property: two Call invocations (each a fresh goja runtime, as replay turns
// are) serialize the SAME metadata to identical bytes. This is what prevents
// the divergence guard from firing on a serialized tool input.
func TestStableJSValue_StableAcrossRuntimeBuilds(t *testing.T) {
	p := newTestProvider(t.TempDir())
	meta := providers.RunMeta{
		AgentName: "x",
		CodeBody:  `function run(input){ return { final_text: JSON.stringify(input.metadata) }; }`,
		Metadata: map[string]any{
			"k9": 1, "k1": 2, "k7": 3, "k3": 4, "k5": 5,
			"k2": 6, "k8": 7, "k4": 8, "k6": 9, "k0": 10,
		},
	}
	a := runOnce(t, p, meta)
	b := runOnce(t, p, meta)
	if a != b {
		t.Fatalf("metadata serialization unstable across runtime builds (replay would diverge):\n  turn1=%s\n  turn2=%s", a, b)
	}
}
