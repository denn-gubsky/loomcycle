package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// ---- Float32 LE base64 round-trip ----

func TestEncodeFloat32LEBase64_RoundTrip(t *testing.T) {
	cases := [][]float32{
		{},
		{0},
		{1, 2, 3, 4},
		{-1.5, 0.0, math.MaxFloat32, math.SmallestNonzeroFloat32},
		make([]float32, 1536), // realistic embedding dim, all zeros
	}
	for i, in := range cases {
		enc := encodeFloat32LEBase64(in)
		out, err := decodeFloat32LEBase64(enc)
		if err != nil {
			t.Errorf("case %d: decode: %v", i, err)
			continue
		}
		if len(in) == 0 && len(out) == 0 {
			continue
		}
		if !reflect.DeepEqual(in, out) {
			t.Errorf("case %d: round-trip mismatch:\nin:  %v\nout: %v", i, in, out)
		}
	}
}

func TestEncodeFloat32LEBase64_BitStable(t *testing.T) {
	// Bit-stable for non-canonical floats — NaN with a particular
	// payload, denormals, etc. Use math.Float32bits to assert
	// bit-equality (== on NaN is false).
	in := []float32{
		math.Float32frombits(0x7fc00001), // NaN with a payload bit
		math.Float32frombits(0xffc00001), // negative NaN
	}
	enc := encodeFloat32LEBase64(in)
	out, _ := decodeFloat32LEBase64(enc)
	for i := range in {
		if math.Float32bits(in[i]) != math.Float32bits(out[i]) {
			t.Errorf("bit %d: in=%x out=%x", i, math.Float32bits(in[i]), math.Float32bits(out[i]))
		}
	}
}

func TestDecodeFloat32LEBase64_RefusesBadInput(t *testing.T) {
	if _, err := decodeFloat32LEBase64("not-base64!!"); err == nil {
		t.Errorf("expected base64 decode error")
	}
	// 3-byte payload: base64-encoded raw bytes that aren't a multiple of 4.
	bad := []byte{1, 2, 3}
	enc := base64StdEncode(bad)
	if _, err := decodeFloat32LEBase64(enc); err == nil || !strings.Contains(err.Error(), "multiple of 4") {
		t.Errorf("expected length-mismatch error, got %v", err)
	}
}

func base64StdEncode(b []byte) string {
	return encodeFloat32LEBase64Helper(b)
}

// Tiny helper that calls the encoder with raw bytes packaged as
// float32 (re-encodes from raw). Avoids re-importing base64 in the
// test file. We just call the function under test with a slice that
// produces the desired byte count: 3 bytes ≠ multiple of 4.
func encodeFloat32LEBase64Helper(_ []byte) string {
	// 3-byte payload via 0-float32 + literal trim. Simpler: encode
	// a 1-float32 slice (4 bytes) then truncate. We do that by
	// re-encoding via base64.StdEncoding inline.
	// Use the package's own base64 import path through decoding.
	// Since we can't easily produce a 3-byte payload from float32,
	// emit a fixed 3-byte base64 string: AAEC (3 bytes: 00 01 02).
	return "AAEC"
}

// ---- Snapshot capture + restore round-trip ----

// vectorSnapshotStore wraps a real store.Store and overrides the
// vector methods with an in-memory map + SupportsVectors flag.
// Mirrors the pattern used in internal/tools/builtin/memory_vector_test.go
// and internal/api/http/memory_admin_test.go.
type vectorSnapshotStore struct {
	store.Store
	mu       sync.Mutex
	embeds   map[string]store.MemoryEmbedding
	supports bool
}

func newVectorSnapshotStore(s store.Store, supports bool) *vectorSnapshotStore {
	return &vectorSnapshotStore{Store: s, embeds: map[string]store.MemoryEmbedding{}, supports: supports}
}

func vskKey(scope store.MemoryScope, scopeID, key string) string {
	return string(scope) + "|" + scopeID + "|" + key
}

func (v *vectorSnapshotStore) SupportsVectors() bool { return v.supports }

func (v *vectorSnapshotStore) MemoryEmbedSet(ctx context.Context, scope store.MemoryScope, scopeID, key string, e store.MemoryEmbedding) error {
	if !v.supports {
		return store.ErrVectorUnsupported
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.embeds[vskKey(scope, scopeID, key)] = e
	return nil
}

func (v *vectorSnapshotStore) MemoryEmbedGet(ctx context.Context, scope store.MemoryScope, scopeID, key string) (store.MemoryEmbedding, error) {
	if !v.supports {
		return store.MemoryEmbedding{}, store.ErrVectorUnsupported
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	e, ok := v.embeds[vskKey(scope, scopeID, key)]
	if !ok {
		return store.MemoryEmbedding{}, &store.ErrNotFound{Kind: "memory_embedding", ID: key}
	}
	return e, nil
}

func (v *vectorSnapshotStore) MemoryEmbedSearch(ctx context.Context, scope store.MemoryScope, scopeID, keyPrefix string, query []float32, topK int) ([]store.MemorySearchEntry, error) {
	return nil, errors.New("MemoryEmbedSearch not used by snapshot tests")
}

func (v *vectorSnapshotStore) MemoryEmbedListByModel(ctx context.Context, scope store.MemoryScope, scopeID, currentProvider, currentModel string, limit int) ([]store.MemoryEntry, error) {
	return nil, errors.New("MemoryEmbedListByModel not used by snapshot tests")
}

func (v *vectorSnapshotStore) MemoryEmbedStats(ctx context.Context, scope store.MemoryScope) (store.MemoryEmbedStats, error) {
	return store.MemoryEmbedStats{}, errors.New("MemoryEmbedStats not used by snapshot tests")
}

// preloadOneEmbedded writes one k/v row + a matching embedding so
// capture has something to round-trip.
func preloadOneEmbedded(t *testing.T, vs *vectorSnapshotStore, scope store.MemoryScope, scopeID, key string, vec []float32) {
	t.Helper()
	ctx := context.Background()
	if err := vs.Store.MemorySet(ctx, scope, scopeID, key, json.RawMessage(`"v"`), 0); err != nil {
		t.Fatal(err)
	}
	emb := store.MemoryEmbedding{
		Provider:  "openai",
		Model:     "text-embedding-3-large",
		Dimension: len(vec),
		Vector:    vec,
		EmbedText: key,
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := vs.MemoryEmbedSet(ctx, scope, scopeID, key, emb); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureMemory_EmbeddingPopulatedWhenStoreHasOne(t *testing.T) {
	base, cleanup := newTestStore(t)
	defer cleanup()
	vs := newVectorSnapshotStore(base, true)

	preloadOneEmbedded(t, vs, store.MemoryScopeAgent, "qa-agent", "rec1",
		[]float32{1, 0, 0, 0})

	_, jsonBytes, err := Capture(context.Background(), vs, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The captured JSON must include the embedding payload. Sample
	// a couple of distinguishing strings rather than full structural
	// JSON comparison (the wire shape is exercised separately below).
	body := string(jsonBytes)
	if !strings.Contains(body, `"provider":"openai"`) {
		t.Errorf("embedding provider missing from snapshot")
	}
	if !strings.Contains(body, `"model":"text-embedding-3-large"`) {
		t.Errorf("embedding model missing from snapshot")
	}
	if strings.Contains(body, `"embedding":null`) {
		t.Errorf("expected populated embedding, got null in snapshot")
	}
}

func TestCaptureMemory_EmbeddingNullWhenBackendUnsupported(t *testing.T) {
	base, cleanup := newTestStore(t)
	defer cleanup()
	vs := newVectorSnapshotStore(base, false) // unsupported

	if err := base.MemorySet(context.Background(),
		store.MemoryScopeAgent, "qa-agent", "rec1", json.RawMessage(`"v"`), 0); err != nil {
		t.Fatal(err)
	}

	_, jsonBytes, err := Capture(context.Background(), vs, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Should still see "embedding":null — the row exists but no
	// vector support means the field stays nil.
	if !strings.Contains(string(jsonBytes), `"embedding":null`) {
		t.Errorf("expected null embedding on unsupported backend: %s", jsonBytes)
	}
}

func TestRestore_EmbeddingRoundTripsToSupportedBackend(t *testing.T) {
	srcBase, cleanupSrc := newTestStore(t)
	defer cleanupSrc()
	src := newVectorSnapshotStore(srcBase, true)
	preloadOneEmbedded(t, src, store.MemoryScopeAgent, "qa-agent", "rec1",
		[]float32{0.1, 0.2, 0.3, 0.4})

	// Capture → JSON.
	_, jsonBytes, err := Capture(context.Background(), src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Fresh destination, also supporting vectors.
	dstBase, cleanupDst := newTestStore(t)
	defer cleanupDst()
	dst := newVectorSnapshotStore(dstBase, true)

	res, err := Restore(context.Background(), dst, jsonBytes, RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.MemoryRestored != 1 {
		t.Errorf("MemoryRestored=%d want 1", res.MemoryRestored)
	}
	if len(res.Warnings) > 0 {
		t.Errorf("unexpected warnings: %v", res.Warnings)
	}

	// Verify the embedding landed in the destination.
	got, err := dst.MemoryEmbedGet(context.Background(),
		store.MemoryScopeAgent, "qa-agent", "rec1")
	if err != nil {
		t.Fatalf("MemoryEmbedGet on dst: %v", err)
	}
	want := []float32{0.1, 0.2, 0.3, 0.4}
	if !reflect.DeepEqual(got.Vector, want) {
		t.Errorf("vector round-trip:\nwant %v\ngot  %v", want, got.Vector)
	}
	if got.Provider != "openai" || got.Model != "text-embedding-3-large" {
		t.Errorf("metadata not restored: %+v", got)
	}
	if got.EmbedText != "rec1" {
		t.Errorf("embed_text not restored: %q", got.EmbedText)
	}
}

func TestRestore_EmbeddingDroppedWithWarningWhenDstUnsupported(t *testing.T) {
	srcBase, cleanupSrc := newTestStore(t)
	defer cleanupSrc()
	src := newVectorSnapshotStore(srcBase, true)
	preloadOneEmbedded(t, src, store.MemoryScopeAgent, "qa-agent", "rec1",
		[]float32{1, 0, 0, 0})
	preloadOneEmbedded(t, src, store.MemoryScopeAgent, "qa-agent", "rec2",
		[]float32{0, 1, 0, 0})

	_, jsonBytes, err := Capture(context.Background(), src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Destination: vector-unsupported.
	dstBase, cleanupDst := newTestStore(t)
	defer cleanupDst()
	dst := newVectorSnapshotStore(dstBase, false)

	res, err := Restore(context.Background(), dst, jsonBytes, RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Both k/v rows still restored.
	if res.MemoryRestored != 2 {
		t.Errorf("MemoryRestored=%d want 2 (k/v should still land)", res.MemoryRestored)
	}
	// Two warnings — one per dropped embedding.
	embedWarnings := 0
	for _, w := range res.Warnings {
		if strings.Contains(w, "embedding dropped") {
			embedWarnings++
		}
	}
	if embedWarnings != 2 {
		t.Errorf("got %d 'embedding dropped' warnings, want 2: %v", embedWarnings, res.Warnings)
	}
	// The k/v rows must be retrievable on the destination.
	got, err := dst.MemoryGet(context.Background(), store.MemoryScopeAgent, "qa-agent", "rec1")
	if err != nil {
		t.Fatalf("k/v row missing on dst: %v", err)
	}
	if string(got.Value) != `"v"` {
		t.Errorf("k/v value not restored: %s", got.Value)
	}
}

func TestRestore_BadBase64InEmbeddingRecordsWarningAndContinues(t *testing.T) {
	// Build an envelope with a corrupted embedding payload by hand.
	srcBase, cleanupSrc := newTestStore(t)
	defer cleanupSrc()
	src := newVectorSnapshotStore(srcBase, true)
	preloadOneEmbedded(t, src, store.MemoryScopeAgent, "qa-agent", "rec1",
		[]float32{1, 2, 3, 4})

	_, jsonBytes, err := Capture(context.Background(), src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the vector base64 in the envelope.
	corrupted := strings.Replace(string(jsonBytes),
		`"vector":"`, `"vector":"!!not-base64!!`, 1)

	dstBase, cleanupDst := newTestStore(t)
	defer cleanupDst()
	dst := newVectorSnapshotStore(dstBase, true)

	res, err := Restore(context.Background(), dst, []byte(corrupted), RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// k/v row landed.
	if res.MemoryRestored != 1 {
		t.Errorf("MemoryRestored=%d want 1", res.MemoryRestored)
	}
	// A warning mentions the embedding decode failure.
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "embedding") && (strings.Contains(w, "base64") || strings.Contains(w, "decode")) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected base64-decode warning, got: %v", res.Warnings)
	}
}
