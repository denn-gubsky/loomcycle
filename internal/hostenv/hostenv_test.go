package hostenv

import (
	"reflect"
	"testing"
)

// TestScrubbed_PassesPathAndTheAllowedNamesOnly — a secret set in this
// process does not reach the child; PATH and the allowed names do, in order,
// and an allowed name that is unset is left out.
func TestScrubbed_PassesPathAndTheAllowedNamesOnly(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("LOOMCYCLE_AUTH_TOKEN", "not-for-children")
	t.Setenv("SOME_API_KEY", "not-for-children")
	t.Setenv("HOSTENV_B", "b")
	t.Setenv("HOSTENV_A", "a")
	got := Scrubbed([]string{"HOSTENV_B", "HOSTENV_UNSET", "HOSTENV_A"})
	want := []string{"PATH=/usr/bin:/bin", "HOSTENV_B=b", "HOSTENV_A=a"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Scrubbed = %v, want %v", got, want)
	}
}

// TestScrubbed_NeverPassesCollectorHeaders: the OTLP collector headers are
// loomcycle's collector credentials; an allowlist naming them passes nothing.
func TestScrubbed_NeverPassesCollectorHeaders(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=s3cr3t")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "authorization=s3cr3t")
	t.Setenv("OTEL_SERVICE_NAME", "svc")
	got := Scrubbed([]string{"OTEL_EXPORTER_OTLP_HEADERS", "otel_exporter_otlp_traces_headers", "OTEL_EXPORTER_OTLP_TRACES_HEADERS", "OTEL_SERVICE_NAME"})
	want := []string{"PATH=/usr/bin", "OTEL_SERVICE_NAME=svc"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Scrubbed = %v, want %v", got, want)
	}
}
