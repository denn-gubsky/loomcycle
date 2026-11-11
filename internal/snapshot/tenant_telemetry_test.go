package snapshot

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

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
		HeadersCredential: "otel", Signals: []string{"traces"}, SamplerRatio: 0.5, UpdatedAt: at, UpdatedBy: "ops"}
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
