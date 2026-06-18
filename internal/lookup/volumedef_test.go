package lookup_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// stubVolumeDefStore is keyed by "tenant\x00name" so the resolver's
// tenant→shared two-read precedence is exercised faithfully.
type stubVolumeDefStore struct {
	rows map[string]store.VolumeDefRow
}

func (s *stubVolumeDefStore) put(tenantID, name, path, mode string) {
	if s.rows == nil {
		s.rows = map[string]store.VolumeDefRow{}
	}
	s.rows[tenantID+"\x00"+name] = store.VolumeDefRow{
		TenantID:   tenantID,
		Name:       name,
		Definition: json.RawMessage(fmt.Sprintf(`{"path":%q,"mode":%q}`, path, mode)),
	}
}

func (s *stubVolumeDefStore) VolumeDefGetByName(_ context.Context, tenantID, name string) (store.VolumeDefRow, error) {
	if row, ok := s.rows[tenantID+"\x00"+name]; ok {
		return row, nil
	}
	return store.VolumeDefRow{}, &store.ErrNotFound{Kind: "volume_def", ID: name}
}

// Static cfg.Volumes is GROUND TRUTH FIRST: a dynamic VolumeDef of the same
// name cannot shadow an operator-declared static volume.
func TestVolumeDef_StaticBeatsDynamic(t *testing.T) {
	cfg := &config.Config{Volumes: map[string]config.Volume{
		"shared-ro": {Path: "/work/reference", Mode: "ro"},
	}}
	st := &stubVolumeDefStore{}
	st.put("acme", "shared-ro", "/pool/acme/shared-ro", "rw") // would-be shadow

	spec, ok := lookup.VolumeDef(context.Background(), cfg, st, "acme", "shared-ro")
	if !ok {
		t.Fatal("expected static volume to resolve")
	}
	if spec.Source != "static" || spec.Path != "/work/reference" || spec.Mode != "ro" {
		t.Errorf("static did not win: %+v", spec)
	}
}

// A tenant-scoped dynamic VolumeDef resolves for that tenant.
func TestVolumeDef_TenantDynamicResolves(t *testing.T) {
	st := &stubVolumeDefStore{}
	st.put("acme", "repo-a", "/pool/acme/repo-a", "rw")

	spec, ok := lookup.VolumeDef(context.Background(), &config.Config{}, st, "acme", "repo-a")
	if !ok {
		t.Fatal("expected tenant dynamic volume to resolve")
	}
	if spec.Source != "dynamic" || spec.Path != "/pool/acme/repo-a" || spec.Mode != "rw" {
		t.Errorf("tenant dynamic resolution = %+v", spec)
	}
}

// A shared dynamic VolumeDef (tenant_id="") resolves as the last tier, both
// for the shared tenant and as the fallback for a named tenant with no own
// row.
func TestVolumeDef_SharedDynamicResolves(t *testing.T) {
	st := &stubVolumeDefStore{}
	st.put("", "common", "/pool/_shared/common", "ro")

	// Shared tenant.
	spec, ok := lookup.VolumeDef(context.Background(), &config.Config{}, st, "", "common")
	if !ok || spec.Source != "dynamic" || spec.Mode != "ro" {
		t.Errorf("shared tenant resolution = %+v ok=%v", spec, ok)
	}
	// Named tenant with no own row falls back to the shared row.
	spec, ok = lookup.VolumeDef(context.Background(), &config.Config{}, st, "acme", "common")
	if !ok || spec.Path != "/pool/_shared/common" {
		t.Errorf("named-tenant shared fallback = %+v ok=%v", spec, ok)
	}
}

// A name present nowhere resolves to (zero, false).
func TestVolumeDef_MissNotFound(t *testing.T) {
	st := &stubVolumeDefStore{}
	if _, ok := lookup.VolumeDef(context.Background(), &config.Config{}, st, "acme", "ghost"); ok {
		t.Error("expected miss to return ok=false")
	}
	// nil store + no static config also misses cleanly.
	if _, ok := lookup.VolumeDef(context.Background(), &config.Config{}, nil, "acme", "ghost"); ok {
		t.Error("expected miss with nil store to return ok=false")
	}
}

// The tenant tier is preferred over the shared tier when both hold the name.
func TestVolumeDef_TenantShadowsShared(t *testing.T) {
	st := &stubVolumeDefStore{}
	st.put("", "work", "/pool/_shared/work", "ro")
	st.put("acme", "work", "/pool/acme/work", "rw")

	spec, ok := lookup.VolumeDef(context.Background(), &config.Config{}, st, "acme", "work")
	if !ok || spec.Path != "/pool/acme/work" || spec.Mode != "rw" {
		t.Errorf("tenant tier should win over shared: %+v ok=%v", spec, ok)
	}
}
