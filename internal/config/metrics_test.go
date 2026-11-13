package config

import (
	"strings"
	"testing"
	"time"
)

// The process metrics sampler is off unless the yaml turns it on, samples
// every 5s, and leaves system-wide CPU and memory out.
func TestMetrics_OffByDefault(t *testing.T) {
	cfg, err := loadYAMLString(t, "agents: {}\n")
	if err != nil {
		t.Fatal(err)
	}
	if e := cfg.Env; e.MetricsEnabled || e.MetricsCollectSystem || e.MetricsSampleInterval != 5*time.Second {
		t.Errorf("default = enabled %v, system %v, interval %v; want off, off, 5s", e.MetricsEnabled, e.MetricsCollectSystem, e.MetricsSampleInterval)
	}
}

func TestMetrics_BlockSetsTheSampler(t *testing.T) {
	cfg, err := loadYAMLString(t, "metrics: {enabled: true, collect_system: true, sample_interval: 1s}\n")
	if err != nil {
		t.Fatal(err)
	}
	if e := cfg.Env; !e.MetricsEnabled || !e.MetricsCollectSystem || e.MetricsSampleInterval != time.Second {
		t.Errorf("block = enabled %v, system %v, interval %v; want on, on, 1s", e.MetricsEnabled, e.MetricsCollectSystem, e.MetricsSampleInterval)
	}
	cfg, err = loadYAMLString(t, "metrics: {enabled: true}\n")
	if err != nil {
		t.Fatal(err)
	}
	if e := cfg.Env; !e.MetricsEnabled || e.MetricsCollectSystem || e.MetricsSampleInterval != 5*time.Second {
		t.Errorf("enabled alone = system %v, interval %v; want off, 5s", e.MetricsCollectSystem, e.MetricsSampleInterval)
	}
}

// An interval under a second would write a sample row many times a second
// (a typo such as 50ms for 5s); the variable floored it silently, the yaml
// says so.
func TestMetrics_RefusesAnIntervalItCannotApply(t *testing.T) {
	for block, want := range map[string]string{
		"metrics: {sample_interval: 50ms}": "metrics.sample_interval",
		"metrics: {sample_interval: 5000}": "not a duration",
		"metrics: {sample_interval: -5s}":  "negative",
		"metrics: {enabled: 1}":            "into bool",
	} {
		if _, err := loadYAMLString(t, block+"\n"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want one naming %q", block, err, want)
		}
	}
}

// The three variables are retired: none is read, and each is reported.
func TestMetrics_TheYAMLIsTheOnlySource(t *testing.T) {
	t.Setenv("LOOMCYCLE_METRICS_ENABLED", "1")
	t.Setenv("LOOMCYCLE_METRICS_COLLECT_SYSTEM", "1")
	t.Setenv("LOOMCYCLE_METRICS_SAMPLE_INTERVAL_MS", "1000")
	cfg, err := loadYAMLString(t, "agents: {}\n")
	if err != nil {
		t.Fatal(err)
	}
	if e := cfg.Env; e.MetricsEnabled || e.MetricsCollectSystem || e.MetricsSampleInterval != 5*time.Second {
		t.Errorf("with the retired variables set = enabled %v, system %v, interval %v; want the yaml's defaults", e.MetricsEnabled, e.MetricsCollectSystem, e.MetricsSampleInterval)
	}
	for _, name := range []string{"LOOMCYCLE_METRICS_ENABLED", "LOOMCYCLE_METRICS_COLLECT_SYSTEM", "LOOMCYCLE_METRICS_SAMPLE_INTERVAL_MS"} {
		requireRetiredAdvisory(t, cfg, name)
	}
}
