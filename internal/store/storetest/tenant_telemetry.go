package storetest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RunTenantTelemetry asserts the per-tenant telemetry destination contract
// against a backend, each case on a fresh store from factory. It is its own
// runner, beside Run, so each backend's test file calls it once.
func RunTenantTelemetry(t *testing.T, factory Factory) {
	t.Helper()
	for name, fn := range map[string]func(*testing.T, store.Store){
		"PutGetListDelete":     testTenantTelemetryPolicyPutGetListDelete,
		"ContentCaptureFields": testTenantTelemetryPolicyContentCaptureFields,
		"ContentExportMarks":   testTenantTelemetryContentExportMarks,
	} {
		t.Run(name, func(t *testing.T) {
			s, cleanup := factory(t)
			defer cleanup()
			fn(t, s)
		})
	}
}

func testTenantTelemetryPolicyPutGetListDelete(t *testing.T, s store.Store) {
	ctx := context.Background()
	var nf *store.ErrNotFound
	if _, err := s.TenantTelemetryPolicyGet(ctx, "acme"); !errors.As(err, &nf) {
		t.Fatalf("missing policy: err = %v", err)
	}
	at := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	acme := store.TenantTelemetryPolicyRow{
		TenantID: "acme", Endpoint: "https://otel.acme.example:4318", Protocol: "http/protobuf",
		HeadersCredential: "otel-headers", Signals: []string{"traces", "traces"}, SamplerRatio: 0.25,
		UpdatedAt: at, UpdatedBy: "ops",
	}
	if err := s.TenantTelemetryPolicyPut(ctx, acme); err != nil {
		t.Fatal(err)
	}
	got, err := s.TenantTelemetryPolicyGet(ctx, "acme")
	want := acme
	want.Signals = []string{"traces"}
	// A policy written without content capture reads back with it off.
	want.ContentCapture = "off"
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("get = %+v err=%v, want %+v", got, err, want)
	}

	// Replaced whole, signals emptied: the row stays and exports nothing.
	acme.Endpoint, acme.Protocol, acme.HeadersCredential, acme.Signals, acme.SamplerRatio = "https://otel.acme.example:4317", "grpc", "", nil, 1
	if err := s.TenantTelemetryPolicyPut(ctx, acme); err != nil {
		t.Fatal(err)
	}
	got, err = s.TenantTelemetryPolicyGet(ctx, "acme")
	if err != nil || got.Protocol != "grpc" || got.HeadersCredential != "" || got.Signals == nil || len(got.Signals) != 0 || got.SamplerRatio != 1 {
		t.Fatalf("replaced = %+v err=%v", got, err)
	}
	if err := s.TenantTelemetryPolicyPut(ctx, store.TenantTelemetryPolicyRow{TenantID: "beta", Endpoint: "https://b.example", Protocol: "http/protobuf", Signals: []string{"traces"}}); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]store.TenantTelemetryPolicyRow{
		"no tenant":    {Endpoint: "https://x.example", Protocol: "grpc"},
		"no endpoint":  {TenantID: "gamma", Protocol: "grpc"},
		"ratio over 1": {TenantID: "gamma", Endpoint: "https://x.example", Protocol: "grpc", SamplerRatio: 1.5},
		"empty signal": {TenantID: "gamma", Endpoint: "https://x.example", Protocol: "grpc", Signals: []string{""}},
		"bad capture":  {TenantID: "gamma", Endpoint: "https://x.example", Protocol: "grpc", ContentCapture: "always"},
		"negative cap": {TenantID: "gamma", Endpoint: "https://x.example", Protocol: "grpc", ContentMaxChars: -1},
	} {
		if err := s.TenantTelemetryPolicyPut(ctx, bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	list, err := s.TenantTelemetryPolicyList(ctx)
	if err != nil || len(list) != 2 || list[0].TenantID != "acme" || list[1].TenantID != "beta" || list[1].UpdatedAt.IsZero() {
		t.Fatalf("list = %+v err=%v", list, err)
	}

	had, err := s.TenantTelemetryPolicyDelete(ctx, "acme")
	if err != nil || !had {
		t.Fatalf("delete: had=%v err=%v", had, err)
	}
	if had, err := s.TenantTelemetryPolicyDelete(ctx, "acme"); err != nil || had {
		t.Fatalf("second delete: had=%v err=%v", had, err)
	}
	if _, err := s.TenantTelemetryPolicyGet(ctx, "acme"); !errors.As(err, &nf) {
		t.Fatalf("deleted policy: err = %v", err)
	}
}

func testTenantTelemetryPolicyContentCaptureFields(t *testing.T, s store.Store) {
	ctx := context.Background()
	row := store.TenantTelemetryPolicyRow{
		TenantID: "acme", Endpoint: "https://otel.acme.example:4318", Protocol: "http/protobuf",
		Signals: []string{"traces"}, SamplerRatio: 1, ContentCapture: "on", ContentMaxChars: 2048,
		UpdatedAt: time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC),
	}
	if err := s.TenantTelemetryPolicyPut(ctx, row); err != nil {
		t.Fatal(err)
	}
	got, err := s.TenantTelemetryPolicyGet(ctx, "acme")
	if err != nil || got.ContentCapture != "on" || got.ContentMaxChars != 2048 {
		t.Fatalf("get = %+v err=%v, want capture on at 2048", got, err)
	}
	row.ContentCapture, row.ContentMaxChars = "off", 0
	if err := s.TenantTelemetryPolicyPut(ctx, row); err != nil {
		t.Fatal(err)
	}
	if got, err := s.TenantTelemetryPolicyGet(ctx, "acme"); err != nil || got.ContentCapture != "off" || got.ContentMaxChars != 0 {
		t.Fatalf("replaced = %+v err=%v, want capture off", got, err)
	}
}

func testTenantTelemetryContentExportMarks(t *testing.T, s store.Store) {
	ctx := context.Background()
	var nf *store.ErrNotFound
	if _, err := s.TenantTelemetryContentExportGet(ctx, "acme"); !errors.As(err, &nf) {
		t.Fatalf("no export yet: err = %v", err)
	}
	t0 := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	for _, at := range []time.Time{t0.Add(time.Hour), t0, t0.Add(3 * time.Hour), t0.Add(2 * time.Hour)} {
		if err := s.TenantTelemetryContentExportMark(ctx, "acme", at); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.TenantTelemetryContentExportGet(ctx, "acme")
	if err != nil || !got.FirstAt.Equal(t0) || !got.LastAt.Equal(t0.Add(3*time.Hour)) || got.TenantID != "acme" {
		t.Fatalf("record = %+v err=%v, want first %v last %v", got, err, t0, t0.Add(3*time.Hour))
	}
	if _, err := s.TenantTelemetryContentExportGet(ctx, "beta"); !errors.As(err, &nf) {
		t.Fatalf("another tenant: err = %v", err)
	}
	// A policy delete leaves the record: the content is still in the
	// tenant's backend.
	if err := s.TenantTelemetryPolicyPut(ctx, store.TenantTelemetryPolicyRow{TenantID: "acme", Endpoint: "https://x.example", Protocol: "grpc", Signals: []string{"traces"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TenantTelemetryPolicyDelete(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TenantTelemetryContentExportGet(ctx, "acme"); err != nil {
		t.Fatalf("after a policy delete: %v", err)
	}
	if err := s.TenantTelemetryContentExportMark(ctx, "", t0); err == nil {
		t.Error("a mark with no tenant was accepted")
	}
}
