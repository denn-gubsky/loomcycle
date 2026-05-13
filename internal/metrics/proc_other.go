//go:build !linux

// Package metrics — non-Linux stub for /proc/* readers.
//
// loomcycle's metrics sampler reads process RSS + CPU% from
// /proc/self/status and /proc/self/stat (Linux-specific kernel
// interfaces). On macOS / Windows / FreeBSD, those paths don't
// exist; this build-tagged stub returns zero values so the sampler
// still records the platform-independent fields (active_runs,
// goroutine count, Go heap metrics).
package metrics

import "time"

// ProcMetricsAvailable advertises whether per-process /proc-based
// metrics will be populated on this platform. Build-tag-split:
// true on Linux, false everywhere else.
const ProcMetricsAvailable = false

// procMetrics is the per-tick output of readProcMetrics. Empty on
// non-Linux platforms.
type procMetrics struct {
	rssBytes             int64
	cpuPctX100           int
	systemCPUPctX100     *int
	systemMemUsedMB      *int
	systemMemAvailableMB *int
}

// cpuSnapshot is the previous-tick state needed for delta-CPU%
// computation. Empty on non-Linux.
type cpuSnapshot struct {
	at time.Time
}

// readProcMetrics returns zero metrics on non-Linux platforms.
// The caller (sampler) still writes a row with the
// platform-independent fields populated — RSS/CPU columns just
// land as 0 / NULL.
func readProcMetrics(_ bool, prev cpuSnapshot) (procMetrics, cpuSnapshot, error) {
	return procMetrics{}, prev, nil
}
