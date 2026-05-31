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

// ---- MemoryBackendDef resolver ----
//
// RFC I MR-3a / mirrors the WebhookDef resolver tests (webhook_test.go).
// Reuses the a2aJSONTagsOf + assertTagSetsEqual helpers from a2a_test.go
// (same lookup_test package).

type stubMemoryBackendStore struct {
	defs map[string]store.MemoryBackendDefRow
}

func (s *stubMemoryBackendStore) MemoryBackendDefGetActive(_ context.Context, name string) (store.MemoryBackendDefRow, error) {
	if row, ok := s.defs[name]; ok {
		return row, nil
	}
	return store.MemoryBackendDefRow{}, &store.ErrNotFound{Kind: "memory_backend_def_active", ID: name}
}

func TestMemoryBackend_EquivalenceYamlVsSubstrate(t *testing.T) {
	yamlBackend := config.MemoryBackend{
		Name: "primary",
		Kind: "mem9",
		Config: config.MemoryBackendConfig{
			BaseURL:    "https://mem9.example.com",
			APIVersion: "v1",
			APIKeyEnv:  "LOOMCYCLE_MEM9_KEY",
		},
		TenancyStrategy: config.MemoryBackendTenancy{
			Kind:          "shared_key_with_prefix",
			PrefixPattern: "tenant/{tenant_id}/",
		},
		FallbackOnError:            "inprocess",
		HealthCheckIntervalSeconds: 30,
	}

	substrateShape := lookup.SubstrateMemoryBackendDef{
		Name: yamlBackend.Name,
		Kind: yamlBackend.Kind,
		Config: lookup.SubstrateMemoryBackendConfig{
			BaseURL:    "https://mem9.example.com",
			APIVersion: "v1",
			APIKeyEnv:  "LOOMCYCLE_MEM9_KEY",
		},
		TenancyStrategy: lookup.SubstrateMemoryBackendTenancy{
			Kind:          "shared_key_with_prefix",
			PrefixPattern: "tenant/{tenant_id}/",
		},
		FallbackOnError:            "inprocess",
		HealthCheckIntervalSeconds: 30,
	}
	defJSON, err := json.Marshal(substrateShape)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	ss := &stubMemoryBackendStore{
		defs: map[string]store.MemoryBackendDefRow{
			"primary": {DefID: "mb_v1", Name: "primary", Version: 1, Definition: defJSON, CreatedAt: time.Now()},
		},
	}
	resolved, ok := lookup.MemoryBackend(context.Background(), ss, &config.Config{}, "primary")
	if !ok {
		t.Fatal("resolver returned !ok")
	}
	if !reflect.DeepEqual(resolved, yamlBackend) {
		t.Errorf("substrate-resolved backend != yaml backend:\n got %+v\nwant %+v", resolved, yamlBackend)
	}
}

func TestMemoryBackend_StaticBeforeSubstrate(t *testing.T) {
	cfg := &config.Config{
		MemoryBackends: map[string]config.MemoryBackend{
			"backend": {Kind: "inprocess"},
		},
	}
	ss := &stubMemoryBackendStore{
		defs: map[string]store.MemoryBackendDefRow{
			"backend": {DefID: "mb_v1", Name: "backend", Definition: json.RawMessage(`{"kind":"mem9"}`)},
		},
	}
	got, ok := lookup.MemoryBackend(context.Background(), ss, cfg, "backend")
	if !ok {
		t.Fatal("resolver returned !ok")
	}
	if got.Kind != "inprocess" {
		t.Errorf("Kind = %q, want inprocess (static must win)", got.Kind)
	}
}

// TestMemoryBackend_DriftDetection pins the SubstrateMemoryBackendDef
// top-level field set against an explicit `want` enumeration. A field
// added to or removed from SubstrateMemoryBackendDef without updating
// this map fails CI.
//
// The complementary direction (mergedMemoryBackendDef ↔
// SubstrateMemoryBackendDef) lives in the builtin package
// (TestMergedMemoryBackendDef_DriftDetection_VsLookupSubstrate). This
// `want` map is the canonical field-set both the substrate-read shape
// here and the substrate-write `mergedMemoryBackendDef` must match.
func TestMemoryBackend_DriftDetection(t *testing.T) {
	want := map[string]bool{
		"name":                          true,
		"kind":                          true,
		"config":                        true,
		"tenancy_strategy":              true,
		"fallback_on_error":             true,
		"health_check_interval_seconds": true,
	}
	have := a2aJSONTagsOf(reflect.TypeOf(lookup.SubstrateMemoryBackendDef{}))
	assertTagSetsEqual(t, "SubstrateMemoryBackendDef", want, have)

	// Third arm of the 3-way drift: config.MemoryBackend (the runtime
	// consumer / yaml-operator shape) carries the SAME json tag set. With
	// the builtin-package merged↔substrate test, this closes the loop
	// merged ↔ substrate ↔ config so a field added to one mirror but not
	// the other two fails CI. RFC I MR-3a.
	cfgTags := a2aJSONTagsOf(reflect.TypeOf(config.MemoryBackend{}))
	assertTagSetsEqual(t, "config.MemoryBackend", want, cfgTags)
}
