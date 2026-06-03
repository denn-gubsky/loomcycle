package codejs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// TestCapabilities_MetadataViaInput pins that code-js advertises structured
// metadata delivery, so the run-build path suppresses prompt-segment
// serialization (review #3 — the gate is this capability, not a hardcoded
// provider id). Fails if the flag regresses to false.
func TestCapabilities_MetadataViaInput(t *testing.T) {
	p := newTestProvider(t.TempDir())
	if !p.Capabilities().MetadataViaInput {
		t.Error("code-js must advertise MetadataViaInput=true (it reads input.metadata)")
	}
}

// TestBuildInput_MergesCallerMetadata pins that the caller's non-secret
// metadata reaches the JS as input.metadata / input.payload_metadata. Fails on
// the pre-feature buildInput, which hardcoded metadata to {user_id, agent}.
func TestBuildInput_MergesCallerMetadata(t *testing.T) {
	req := providers.Request{Messages: []providers.Message{
		{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "go"}}},
	}}
	meta := providers.RunMeta{
		UserID:          "alice",
		AgentName:       "reviewer",
		Metadata:        map[string]any{"repo": "acme/app", "policy": "strict"},
		PayloadMetadata: map[string]any{"pr": float64(42)},
	}
	in := buildInput(req, meta)

	md, _ := in["metadata"].(map[string]any)
	if md["repo"] != "acme/app" || md["policy"] != "strict" {
		t.Errorf("trusted metadata not merged into input.metadata: %v", md)
	}
	pm, _ := in["payload_metadata"].(map[string]any)
	if pm["pr"] != float64(42) {
		t.Errorf("payload metadata not surfaced as input.payload_metadata: %v", in["payload_metadata"])
	}
}

// TestBuildInput_ReservedKeysWin pins that a caller cannot shadow the
// loop-stamped identity keys via metadata.
func TestBuildInput_ReservedKeysWin(t *testing.T) {
	meta := providers.RunMeta{
		UserID:    "alice",
		AgentName: "reviewer",
		Metadata:  map[string]any{"user_id": "attacker", "agent": "evil"},
	}
	md, _ := buildInput(providers.Request{}, meta)["metadata"].(map[string]any)
	if md["user_id"] != "alice" || md["agent"] != "reviewer" {
		t.Errorf("caller metadata shadowed reserved identity keys: %v", md)
	}
}

// TestBuildInput_NoPayloadMetadataKeyWhenEmpty keeps input.payload_metadata
// absent (not an empty object) when no external projection happened.
func TestBuildInput_NoPayloadMetadataKeyWhenEmpty(t *testing.T) {
	if _, ok := buildInput(providers.Request{}, providers.RunMeta{})["payload_metadata"]; ok {
		t.Error("payload_metadata should be absent when empty")
	}
}

// runOnce drives a single, tool-free Call with the given RunMeta and returns
// the final text (or fails on an EventError). Code agents under test here
// return immediately via `{final_text: ...}`, so one turn suffices.
func runOnce(t *testing.T, p *Provider, meta providers.RunMeta) string {
	t.Helper()
	ctx := providers.WithRunMeta(context.Background(), meta)
	ch, err := p.Call(ctx, providers.Request{
		Model:    "code-js",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "go"}}}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var final, errText string
	for ev := range ch {
		switch ev.Type {
		case providers.EventText:
			final += ev.Text
		case providers.EventError:
			errText = ev.Error
		}
	}
	if errText != "" {
		t.Fatalf("unexpected EventError: %s", errText)
	}
	return final
}

// TestProvider_InlineBodyExecutesOverFilesystem pins the RFC J symmetry: when
// RunMeta carries an inline code_body, the provider runs IT, not the
// filesystem agent_code/<name>/index.js — the seam that lets a code agent run
// with no host FS bind. Fails on the pre-feature provider, which always read
// the filesystem and would return "FROM_FS".
func TestProvider_InlineBodyExecutesOverFilesystem(t *testing.T) {
	root := writeAgent(t, "dual", `function run(input){ return { final_text: "FROM_FS" }; }`)
	p := newTestProvider(root)

	got := runOnce(t, p, providers.RunMeta{
		AgentName: "dual",
		CodeBody:  `function run(input){ return { final_text: "FROM_INLINE" }; }`,
	})
	if got != "FROM_INLINE" {
		t.Fatalf("inline body should win over filesystem; got %q", got)
	}
}

// TestProvider_EmptyBodyFallsBackToFilesystem pins the fallback: an empty
// code_body preserves the legacy filesystem path verbatim.
func TestProvider_EmptyBodyFallsBackToFilesystem(t *testing.T) {
	root := writeAgent(t, "fsonly", `function run(input){ return { final_text: "FROM_FS" }; }`)
	p := newTestProvider(root)

	got := runOnce(t, p, providers.RunMeta{AgentName: "fsonly", CodeBody: ""})
	if got != "FROM_FS" {
		t.Fatalf("empty code_body should fall back to the filesystem; got %q", got)
	}
}

// TestProvider_InlineBodyNeedsNoFilesystem proves the cloud case: an inline
// body runs even when no agent_code/<name>/index.js exists anywhere.
func TestProvider_InlineBodyNeedsNoFilesystem(t *testing.T) {
	p := newTestProvider(t.TempDir()) // empty root — no index.js for "ghost"
	got := runOnce(t, p, providers.RunMeta{
		AgentName: "ghost",
		CodeBody:  `function run(input){ return { final_text: "NO_FS_NEEDED" }; }`,
	})
	if got != "NO_FS_NEEDED" {
		t.Fatalf("inline body should run with no filesystem entry; got %q", got)
	}
}

// TestCompiler_CacheKeyedByContentHashNotName pins the cache-key fix: a new
// inline body under the SAME agent name must compile fresh, never serving the
// previously-cached program. Fails on the pre-feature by-name cache, which
// returned the first program for the second (different) body.
func TestCompiler_CacheKeyedByContentHashNotName(t *testing.T) {
	c := newCompiler(t.TempDir())

	a, err := c.loadSource("agent", `function run(){ return {final_text:"A"}; }`)
	if err != nil {
		t.Fatalf("loadSource A: %v", err)
	}
	b, err := c.loadSource("agent", `function run(){ return {final_text:"B"}; }`)
	if err != nil {
		t.Fatalf("loadSource B: %v", err)
	}
	if a.hash == b.hash {
		t.Fatal("different bodies under the same name must hash differently")
	}
	if a.prog == b.prog {
		t.Fatal("different bodies must compile to distinct programs (by-name cache regression)")
	}
	// Identical bytes under a different name share the cached program (hash key).
	a2, err := c.loadSource("other-agent", `function run(){ return {final_text:"A"}; }`)
	if err != nil {
		t.Fatalf("loadSource A2: %v", err)
	}
	if a2.prog != a.prog {
		t.Fatal("identical bytes should hit the content-hash cache regardless of name")
	}
}

// TestValidate_AcceptsAndHashes / _RejectsSyntaxError pin the shared authorship
// check used by the AgentDef create/fork gate.
func TestValidate_AcceptsAndHashes(t *testing.T) {
	h, err := Validate(`function run(input){ return {final_text:"ok"}; }`)
	if err != nil {
		t.Fatalf("valid body should compile: %v", err)
	}
	if len(h) != 64 {
		t.Fatalf("hash should be 64 hex chars, got %d (%q)", len(h), h)
	}
}

func TestValidate_RejectsSyntaxError(t *testing.T) {
	if _, err := Validate(`function run(input){ return {final_text: }`); err == nil {
		t.Fatal("a syntactically broken body must be rejected")
	}
}

// TestCompiler_FilesystemPathCachedByName pins the per-turn-read fix: after the
// first load, the compiled program is memoized by agent name, so a subsequent
// load does NOT touch the filesystem. We prove "no re-read" by deleting the
// file after the first load — a cached program is still returned. Fails on the
// regression where load() did an unconditional os.ReadFile every call (the
// second load would hit os.IsNotExist and error).
func TestCompiler_FilesystemPathCachedByName(t *testing.T) {
	root := writeAgent(t, "fscached", `function run(){ return {final_text:"x"}; }`)
	c := newCompiler(root)

	first, err := c.load("fscached")
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	// Remove the file: a correct by-name cache must not need it again.
	if err := os.Remove(filepath.Join(root, "fscached", "index.js")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	second, err := c.load("fscached")
	if err != nil {
		t.Fatalf("second load re-read the filesystem (per-turn regression): %v", err)
	}
	if first != second {
		t.Fatal("by-name cache should return the identical compiled program")
	}
}
