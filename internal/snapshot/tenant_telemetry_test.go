package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func TestSnapshotTenantTelemetry_RoundTripsAndTheLiveRowStands(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()

	// No policy: the envelope has no section at all.
	if raw := mustCapture(t, src); strings.Contains(string(raw), `"tenant_telemetry"`) {
		t.Fatal("a capture with no policy carries the section")
	}

	at := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	acme := store.TenantTelemetryPolicyRow{TenantID: "acme", Endpoint: "https://otel.acme.example", Protocol: "http/protobuf",
		HeadersCredential: "otel", Signals: []string{"traces"}, SamplerRatio: 0.5, ContentCapture: "on", ContentMaxChars: 2048,
		UpdatedAt: at, UpdatedBy: "ops"}
	beta := store.TenantTelemetryPolicyRow{TenantID: "beta", Endpoint: "https://otel.beta.example:4317", Protocol: "grpc",
		Signals: []string{"traces"}, SamplerRatio: 1, UpdatedAt: at, UpdatedBy: "admin"}
	for _, r := range []store.TenantTelemetryPolicyRow{acme, beta} {
		if err := src.TenantTelemetryPolicyPut(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	// The target already has its own policy for beta: it stands.
	live := beta
	live.Endpoint, live.UpdatedBy = "https://otel.beta-new.example", "beta-ops"
	if err := dst.TenantTelemetryPolicyPut(ctx, live); err != nil {
		t.Fatal(err)
	}

	raw := mustCapture(t, src)
	var e Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	if e.Sections.TenantTelemetry == nil || len(e.Sections.TenantTelemetry.Entries) != 2 {
		t.Fatalf("section = %+v", e.Sections.TenantTelemetry)
	}

	res := mustRestore(t, dst, raw, RestoreOptions{Validators: passValidators()})
	if res.TenantTelemetryRestored != 1 || !reflect.DeepEqual(res.Refresh.TelemetryTenants, []string{"acme"}) {
		t.Errorf("restored %d, refresh %v", res.TenantTelemetryRestored, res.Refresh.TelemetryTenants)
	}
	if !hasWarning(res, "tenant telemetry policy acme", `"otel"`) {
		t.Errorf("no warning that acme's header credential must be stored again:\n%s", strings.Join(res.Warnings, "\n"))
	}
	got, err := dst.TenantTelemetryPolicyGet(ctx, "acme")
	if err != nil || !reflect.DeepEqual(got, acme) {
		t.Errorf("acme on the target = %+v (err %v), want %+v", got, err, acme)
	}
	if got, _ := dst.TenantTelemetryPolicyGet(ctx, "beta"); got.Endpoint != live.Endpoint || got.UpdatedBy != "beta-ops" {
		t.Errorf("the target's own beta policy was overwritten: %+v", got)
	}

	// A re-restore writes nothing.
	if res := mustRestore(t, dst, raw, RestoreOptions{Validators: passValidators()}); res.TenantTelemetryRestored != 0 || len(res.Refresh.TelemetryTenants) != 0 {
		t.Errorf("re-restore: restored %d, refresh %v", res.TenantTelemetryRestored, res.Refresh.TelemetryTenants)
	}
}

// A policy restores only when this host's rules admit it: one the validator
// refuses is skipped with its reason, and with no validator nothing restores.
func TestSnapshotTenantTelemetry_RestoreHoldsEachPolicyToTheTargetsRules(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	ctx := context.Background()
	for _, r := range []store.TenantTelemetryPolicyRow{
		{TenantID: "acme", Endpoint: "https://otel.acme.example", Protocol: "http/protobuf", Signals: []string{"traces"}, SamplerRatio: 1},
		{TenantID: "evil", Endpoint: "https://exfil.example", Protocol: "http/protobuf", Signals: []string{"traces"}, SamplerRatio: 1},
	} {
		if err := src.TenantTelemetryPolicyPut(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	raw := mustCapture(t, src)

	dst, dstClose := newTestStore(t)
	defer dstClose()
	v := passValidators()
	v[migrations.SectionTenantTelemetry] = func(body json.RawMessage) error {
		var e TenantTelemetryEntry
		if err := json.Unmarshal(body, &e); err != nil {
			return err
		}
		if e.Endpoint == "https://exfil.example" {
			return errors.New("endpoint host \"exfil.example\" is not one the operator allows telemetry to be sent to")
		}
		return nil
	}
	res := mustRestore(t, dst, raw, RestoreOptions{Validators: v})
	if res.TenantTelemetryRestored != 1 || !hasWarning(res, "tenant telemetry policy evil", "not one the operator allows") {
		t.Errorf("restored %d; warnings:\n%s", res.TenantTelemetryRestored, strings.Join(res.Warnings, "\n"))
	}
	if _, err := dst.TenantTelemetryPolicyGet(ctx, "evil"); err == nil {
		t.Error("a policy the target's rules refuse was restored")
	}

	bare, bareClose := newTestStore(t)
	defer bareClose()
	v = passValidators()
	delete(v, migrations.SectionTenantTelemetry)
	res = mustRestore(t, bare, raw, RestoreOptions{Validators: v})
	if res.TenantTelemetryRestored != 0 || !hasWarning(res, "tenant_telemetry", "skipped") {
		t.Errorf("no validator: restored %d; warnings:\n%s", res.TenantTelemetryRestored, strings.Join(res.Warnings, "\n"))
	}
	if rows, _ := bare.TenantTelemetryPolicyList(ctx); len(rows) != 0 {
		t.Errorf("restored without a validator: %+v", rows)
	}
}
