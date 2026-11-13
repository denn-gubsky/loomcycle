package config

import (
	"fmt"
	"time"
)

// MetricsConfig is the `metrics:` block: the process metrics sampler, which
// records loomcycle's CPU and memory into the store while runs are active and
// serves them at /v1/_metrics. It is not the Prometheus /metrics endpoint,
// which has its own listener and credential in the environment.
type MetricsConfig struct {
	// Enabled starts the sampler. Off by default.
	Enabled bool `yaml:"enabled,omitempty"`
	// CollectSystem also samples system-wide CPU and memory (Linux only).
	CollectSystem bool `yaml:"collect_system,omitempty"`
	// SampleInterval is how often a sample is taken while a run is active.
	// Unset = 5s; at least 1s.
	SampleInterval HumanDuration `yaml:"sample_interval,omitempty"`
}

const (
	metricsSampleIntervalDefault = 5 * time.Second
	// A sample is a store write: under a second, a typo such as 50ms for 5s
	// would write many rows a second for as long as a run is active.
	metricsSampleIntervalMin = time.Second
)

// applyMetrics copies the `metrics:` block into the settings the sampler
// reads.
func applyMetrics(cfg *Config) error {
	m := cfg.Metrics
	interval := m.SampleInterval.Duration()
	switch {
	case interval == 0:
		interval = metricsSampleIntervalDefault
	case interval < metricsSampleIntervalMin:
		return fmt.Errorf("metrics.sample_interval: %s is under %s, the shortest interval the sampler takes", interval, metricsSampleIntervalMin)
	}
	cfg.Env.MetricsEnabled = m.Enabled
	cfg.Env.MetricsCollectSystem = m.CollectSystem
	cfg.Env.MetricsSampleInterval = interval
	return nil
}
