package lookup_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// ---- DocumentSourceDef resolver (RFC CE) ----
//
// Mirrors the MemoryBackendDef resolver tests (memory_backend_test.go). Reuses
// the a2aJSONTagsOf + assertTagSetsEqual helpers from a2a_test.go (same
// lookup_test package).

type stubDocumentSourceStore struct {
	defs map[string]store.DocumentSourceDefRow
}

// DocumentSourceDefGetActive ignores tenantID — these resolver tests exercise
// the precedence/equivalence/drift logic with the shared "" tenant; per-tenant
// isolation is covered by the store contract test.
func (s *stubDocumentSourceStore) DocumentSourceDefGetActive(_ context.Context, _, name string) (store.DocumentSourceDefRow, error) {
	if row, ok := s.defs[name]; ok {
		return row, nil
	}
	return store.DocumentSourceDefRow{}, &store.ErrNotFound{Kind: "document_source_def_active", ID: name}
}

func TestDocumentSource_EquivalenceYamlVsSubstrate(t *testing.T) {
	yamlSource := config.DocumentSource{
		Config: config.DocumentSourceConfig{
			BaseURL:    "https://peer.example.com",
			APIVersion: "v1",
			APIKeyEnv:  "LOOMCYCLE_PEER_DOC_KEY",
		},
		TenancyStrategy: config.DocumentSourceTenancy{
			Kind:       "key_per_tenant",
			EnvPattern: "LOOMCYCLE_PEER_{tenant_id}_KEY",
		},
	}
	substrateShape := lookup.SubstrateDocumentSourceDef{
		Name: "primary",
		Config: lookup.SubstrateDocumentSourceConfig{
			BaseURL:    "https://peer.example.com",
			APIVersion: "v1",
			APIKeyEnv:  "LOOMCYCLE_PEER_DOC_KEY",
		},
		TenancyStrategy: lookup.SubstrateDocumentSourceTenancy{
			Kind:       "key_per_tenant",
			EnvPattern: "LOOMCYCLE_PEER_{tenant_id}_KEY",
		},
	}
	defJSON, err := json.Marshal(substrateShape)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ss := &stubDocumentSourceStore{
		defs: map[string]store.DocumentSourceDefRow{
			"primary": {DefID: "ds_v1", Name: "primary", Version: 1, Definition: defJSON, CreatedAt: time.Now()},
		},
	}
	resolved, ok := lookup.DocumentSource(context.Background(), ss, &config.Config{}, "", "primary")
	if !ok {
		t.Fatal("resolver returned !ok")
	}
	// ToConfigDef drops Name (config.DocumentSource has none), so the substrate
	// resolves to exactly the yaml shape.
	if !reflect.DeepEqual(resolved, yamlSource) {
		t.Errorf("substrate-resolved source != yaml source:\n got %+v\nwant %+v", resolved, yamlSource)
	}
}

func TestDocumentSource_RetiredActiveDoesNotResolve(t *testing.T) {
	ss := &stubDocumentSourceStore{
		defs: map[string]store.DocumentSourceDefRow{
			"peer": {DefID: "ds_v1", Name: "peer", Retired: true, Definition: json.RawMessage(`{"config":{"base_url":"https://retired.example.com"}}`)},
		},
	}
	// A retired def that is still the active pointer must NOT resolve — retiring
	// the active source deactivates it.
	if _, ok := lookup.DocumentSource(context.Background(), ss, &config.Config{}, "", "peer"); ok {
		t.Errorf("a retired active def must not resolve")
	}
	// Static yaml still wins even when a retired substrate pointer exists.
	cfg := &config.Config{DocumentSources: map[string]config.DocumentSource{
		"peer": {Config: config.DocumentSourceConfig{BaseURL: "https://static.example.com"}},
	}}
	got, ok := lookup.DocumentSource(context.Background(), ss, cfg, "", "peer")
	if !ok || got.Config.BaseURL != "https://static.example.com" {
		t.Errorf("static should resolve past a retired substrate pointer, got ok=%v %q", ok, got.Config.BaseURL)
	}
}

func TestDocumentSource_StaticBeforeSubstrate(t *testing.T) {
	cfg := &config.Config{
		DocumentSources: map[string]config.DocumentSource{
			"peer": {Config: config.DocumentSourceConfig{BaseURL: "https://static.example.com"}},
		},
	}
	ss := &stubDocumentSourceStore{
		defs: map[string]store.DocumentSourceDefRow{
			"peer": {DefID: "ds_v1", Name: "peer", Definition: json.RawMessage(`{"config":{"base_url":"https://substrate.example.com"}}`)},
		},
	}
	got, ok := lookup.DocumentSource(context.Background(), ss, cfg, "", "peer")
	if !ok {
		t.Fatal("resolver returned !ok")
	}
	if got.Config.BaseURL != "https://static.example.com" {
		t.Errorf("base_url = %q, want the static one (static must win over shared substrate)", got.Config.BaseURL)
	}
}

func TestDocumentSource_TenantSubstrateBeforeStatic(t *testing.T) {
	cfg := &config.Config{
		DocumentSources: map[string]config.DocumentSource{
			"peer": {Config: config.DocumentSourceConfig{BaseURL: "https://static.example.com"}},
		},
	}
	ss := &stubDocumentSourceStore{
		defs: map[string]store.DocumentSourceDefRow{
			"peer": {DefID: "ds_v1", Name: "peer", TenantID: "acme", Definition: json.RawMessage(`{"config":{"base_url":"https://tenant.example.com"}}`)},
		},
	}
	// A non-"" tenant consults its own substrate FIRST (step 1), overriding the
	// shared static base.
	got, ok := lookup.DocumentSource(context.Background(), ss, cfg, "acme", "peer")
	if !ok {
		t.Fatal("resolver returned !ok")
	}
	if got.Config.BaseURL != "https://tenant.example.com" {
		t.Errorf("base_url = %q, want the tenant substrate one (tenant substrate must win over static)", got.Config.BaseURL)
	}
}

// TestDocumentSource_DriftDetection pins the SubstrateDocumentSourceDef top-level
// field set against an explicit `want`. A field added to or removed from the
// substrate-read shape without updating this fails CI. The complementary
// direction (mergedDocumentSourceDef ↔ SubstrateDocumentSourceDef) lives in the
// builtin package (TestMergedDocumentSourceDef_DriftDetection_VsLookupSubstrate).
func TestDocumentSource_DriftDetection(t *testing.T) {
	want := map[string]bool{
		"name":             true,
		"config":           true,
		"tenancy_strategy": true,
	}
	have := a2aJSONTagsOf(reflect.TypeOf(lookup.SubstrateDocumentSourceDef{}))
	assertTagSetsEqual(t, "SubstrateDocumentSourceDef", want, have)

	// The third arm — config.DocumentSource (the runtime consumer / yaml shape) —
	// carries the SAME field set MINUS `name` (the name is the map key / def
	// column, never a field on config.DocumentSource). So the merged↔substrate↔
	// config loop still closes: a field added to config's config/tenancy_strategy
	// without the other two mirrors fails CI here.
	wantCfg := map[string]bool{"config": true, "tenancy_strategy": true}
	cfgTags := a2aJSONTagsOf(reflect.TypeOf(config.DocumentSource{}))
	assertTagSetsEqual(t, "config.DocumentSource", wantCfg, cfgTags)
}
