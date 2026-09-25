package snapshot

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A HookDef travels with the definitions that name it: every version, its
// tenant, its hash and the tenant's active pointer come back on restore.
func TestRoundTrip_WithHookDefs(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()

	def := json.RawMessage(`{"event":"pre","body":{"kind":"http","url":"https://hooks.example/gate"},"fail_mode":"closed"}`)
	v1, err := src.HookDefCreate(ctx, store.HookDefRow{DefID: "hdf_1", Name: "gate", TenantID: "acme", Definition: def, ContentSHA256: "sha256:aa"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.HookDefCreate(ctx, store.HookDefRow{DefID: "hdf_2", Name: "gate", TenantID: "acme", ParentDefID: v1.DefID, Definition: def, ContentSHA256: "sha256:bb", Retired: true}); err != nil {
		t.Fatal(err)
	}
	if err := src.HookDefSetActive(ctx, "acme", "gate", "hdf_1", ""); err != nil {
		t.Fatal(err)
	}

	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Restore(ctx, dst, raw, RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.HookDefsRestored != 2 || result.HookDefActiveRestored != 1 {
		t.Fatalf("restored %d defs / %d pointers; want 2 / 1", result.HookDefsRestored, result.HookDefActiveRestored)
	}
	active, err := dst.HookDefGetActive(ctx, "acme", "gate")
	if err != nil {
		t.Fatalf("active after restore: %v", err)
	}
	if active.DefID != "hdf_1" || active.ContentSHA256 != "sha256:aa" || active.TenantID != "acme" {
		t.Fatalf("active = %+v", active)
	}
	v2, err := dst.HookDefGet(ctx, "hdf_2")
	if err != nil {
		t.Fatal(err)
	}
	if v2.ParentDefID != "hdf_1" || !v2.Retired || v2.Version != 2 {
		t.Fatalf("v2 = %+v; want its lineage, version and retired flag", v2)
	}

	again, err := Restore(ctx, dst, raw, RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if again.HookDefsRestored != 0 || again.HookDefActiveRestored != 0 {
		t.Fatalf("second restore wrote %d / %d; want nothing", again.HookDefsRestored, again.HookDefActiveRestored)
	}
}
