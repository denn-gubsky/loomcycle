//go:build linux

// Package metrics — Linux-specific /proc readers for the
// process-resource sampler.
//
// What's read:
//   - /proc/self/status   → VmRSS line for process resident set size
//   - /proc/self/stat     → utime + stime fields for cumulative CPU
//     ticks; delta-from-previous / wall-clock-delta → CPU%
//   - /proc/stat          → first line; system-wide CPU delta
//     (only when collectSystem=true)
//   - /proc/meminfo       → MemTotal + MemAvailable
//     (only when collectSystem=true)
//
// All reads are <50 µs on a healthy host; total per-tick cost
// dominates by the sampler's 5-second interval. Parse errors are
// surfaced as the function's error return — the sampler treats
// them as soft failures (log once, continue).
package metrics

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"time"
)

// ProcMetricsAvailable is true on Linux; the sampler can populate
// RSS + CPU fields.
const ProcMetricsAvailable = true

// USER_HZ — number of jiffies per second on every Linux target
// loomcycle targets. Hard-coded rather than sysconf'd; if a future
// kernel breaks this we revisit.
const userHZ = 100

// procMetrics is one tick's worth of per-process + (optionally)
// system-wide measurements.
type procMetrics struct {
	rssBytes             int64
	cpuPctX100           int
	systemCPUPctX100     *int
	systemMemUsedMB      *int
	systemMemAvailableMB *int
}

// cpuSnapshot is the previous-tick state required to compute
// delta-CPU% on the next tick. The sampler carries this across
// invocations.
type cpuSnapshot struct {
	at              time.Time
	procTicks       uint64 // utime + stime of our process
	systemTotal     uint64 // /proc/stat: sum of all CPU jiffies
	systemIdle      uint64 // /proc/stat: idle jiffies
	systemPopulated bool
}

// readProcMetrics samples /proc once. Returns the new metrics and
// the snapshot to pass into the NEXT call. On first call (prev is
// zero-valued), the delta-based CPU fields return 0 — the second
// call onward produces real CPU% numbers.
//
// Errors are NEVER fatal — the sampler treats them as soft
// failures so a hardened-container `/proc` shape doesn't kill the
// runtime.
func readProcMetrics(collectSystem bool, prev cpuSnapshot) (procMetrics, cpuSnapshot, error) {
	now := time.Now()
	out := procMetrics{}
	next := cpuSnapshot{at: now}

	// 1. /proc/self/status — VmRSS line.
	if rss, err := readProcSelfStatusVmRSS(); err != nil {
		return out, prev, fmt.Errorf("read /proc/self/status: %w", err)
	} else {
		out.rssBytes = rss
	}

	// 2. /proc/self/stat — utime + stime cumulative ticks.
	curProcTicks, err := readProcSelfStatTicks()
	if err != nil {
		return out, prev, fmt.Errorf("read /proc/self/stat: %w", err)
	}
	next.procTicks = curProcTicks
	if !prev.at.IsZero() && curProcTicks >= prev.procTicks {
		wall := now.Sub(prev.at).Seconds()
		if wall > 0 {
			deltaTicks := float64(curProcTicks - prev.procTicks)
			pct := (deltaTicks / float64(userHZ)) / wall * 100.0
			out.cpuPctX100 = int(pct * 100.0)
		}
	}

	// 3. Optionally /proc/stat for system CPU.
	if collectSystem {
		total, idle, err := readProcStatCPU0()
		if err == nil {
			next.systemTotal = total
			next.systemIdle = idle
			next.systemPopulated = true
			if prev.systemPopulated && total >= prev.systemTotal && idle >= prev.systemIdle {
				deltaTotal := total - prev.systemTotal
				deltaIdle := idle - prev.systemIdle
				if deltaTotal > 0 {
					usedJiffies := deltaTotal - deltaIdle
					pct := float64(usedJiffies) / float64(deltaTotal) * 100.0
					v := int(pct * 100.0)
					out.systemCPUPctX100 = &v
				}
			}
		}
		// /proc/meminfo — MemTotal + MemAvailable.
		if total, available, err := readProcMeminfo(); err == nil {
			tot := int(total / 1024 / 1024)
			avail := int(available / 1024 / 1024)
			used := tot - avail
			out.systemMemUsedMB = &used
			out.systemMemAvailableMB = &avail
		}
	}

	return out, next, nil
}

// readProcSelfStatusVmRSS scans /proc/self/status for the VmRSS:
// line and returns the value in bytes. Format example:
//
//	VmRSS:	   45200 kB
//
// (whitespace varies; the kernel writes a single value in kB).
func readProcSelfStatusVmRSS() (int64, error) {
	data, err := readFile("/proc/self/status")
	if err != nil {
		return 0, fmt.Errorf("read /proc/self/status: %w", err)
	}
	return parseVmRSS(data)
}

func parseVmRSS(data []byte) (int64, error) {
	prefix := []byte("VmRSS:")
	idx := bytes.Index(data, prefix)
	if idx < 0 {
		return 0, fmt.Errorf("VmRSS line not found in /proc/self/status")
	}
	// After the prefix, skip whitespace and find the numeric value.
	rest := data[idx+len(prefix):]
	// Trim leading whitespace.
	start := 0
	for start < len(rest) && (rest[start] == ' ' || rest[start] == '\t') {
		start++
	}
	end := start
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if start == end {
		return 0, fmt.Errorf("VmRSS line malformed: no numeric value after `VmRSS:`")
	}
	kb, err := strconv.ParseInt(string(rest[start:end]), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse VmRSS value %q: %w", string(rest[start:end]), err)
	}
	return kb * 1024, nil
}

// readProcSelfStatTicks parses utime (field 14) + stime (field 15)
// from /proc/self/stat. The file is a single line of
// space-separated fields. Field indexing is 1-based per `man 5
// proc`; field 2 is `comm` in parens which may contain spaces, so
// we find the closing `)` and split the rest.
func readProcSelfStatTicks() (uint64, error) {
	data, err := readFile("/proc/self/stat")
	if err != nil {
		return 0, fmt.Errorf("read /proc/self/stat: %w", err)
	}
	return parseSelfStatTicks(data)
}

func parseSelfStatTicks(data []byte) (uint64, error) {
	closeIdx := bytes.LastIndexByte(data, ')')
	if closeIdx < 0 || closeIdx+1 >= len(data) {
		return 0, fmt.Errorf("/proc/self/stat: closing `)` of comm not found")
	}
	tail := data[closeIdx+1:]
	// Now fields 3.. are space-separated. utime is field 14
	// overall, which is the (14 - 2) = 12th field of `tail` after
	// trimming the leading space. Iterate counting fields.
	fields := bytes.Fields(tail)
	// fields[0] = field 3 (state). utime is field 14 → index 11.
	// stime is field 15 → index 12.
	if len(fields) < 13 {
		return 0, fmt.Errorf("/proc/self/stat: not enough fields after comm (%d)", len(fields))
	}
	utime, err := strconv.ParseUint(string(fields[11]), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse utime %q: %w", string(fields[11]), err)
	}
	stime, err := strconv.ParseUint(string(fields[12]), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse stime %q: %w", string(fields[12]), err)
	}
	return utime + stime, nil
}

// readProcStatCPU0 parses the first "cpu " line of /proc/stat,
// returning (total jiffies, idle jiffies). The line format is:
//
//	cpu user nice system idle iowait irq softirq steal guest guest_nice
//
// Total is the sum of all the numeric fields; idle is the 4th
// numeric (after the "cpu" label).
func readProcStatCPU0() (uint64, uint64, error) {
	data, err := readFile("/proc/stat")
	if err != nil {
		return 0, 0, fmt.Errorf("read /proc/stat: %w", err)
	}
	return parseProcStatCPU0(data)
}

func parseProcStatCPU0(data []byte) (uint64, uint64, error) {
	nl := bytes.IndexByte(data, '\n')
	if nl < 0 {
		nl = len(data)
	}
	first := data[:nl]
	fields := bytes.Fields(first)
	if len(fields) < 5 || !bytes.Equal(fields[0], []byte("cpu")) {
		return 0, 0, fmt.Errorf("/proc/stat: expected `cpu ...` on first line")
	}
	var total, idle uint64
	for i := 1; i < len(fields); i++ {
		v, err := strconv.ParseUint(string(fields[i]), 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("parse /proc/stat cpu field %d (%q): %w", i, string(fields[i]), err)
		}
		total += v
		if i == 4 { // idle is the 4th numeric field
			idle = v
		}
	}
	return total, idle, nil
}

// readProcMeminfo returns (MemTotal, MemAvailable) in bytes.
func readProcMeminfo() (int64, int64, error) {
	data, err := readFile("/proc/meminfo")
	if err != nil {
		return 0, 0, fmt.Errorf("read /proc/meminfo: %w", err)
	}
	return parseProcMeminfo(data)
}

func parseProcMeminfo(data []byte) (int64, int64, error) {
	total, err := parseMeminfoField(data, "MemTotal:")
	if err != nil {
		return 0, 0, err
	}
	avail, err := parseMeminfoField(data, "MemAvailable:")
	if err != nil {
		return 0, 0, err
	}
	return total * 1024, avail * 1024, nil
}

func parseMeminfoField(data []byte, prefix string) (int64, error) {
	idx := bytes.Index(data, []byte(prefix))
	if idx < 0 {
		return 0, fmt.Errorf("/proc/meminfo: %s not found", prefix)
	}
	rest := data[idx+len(prefix):]
	start := 0
	for start < len(rest) && (rest[start] == ' ' || rest[start] == '\t') {
		start++
	}
	end := start
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if start == end {
		return 0, fmt.Errorf("/proc/meminfo: no numeric after %s", prefix)
	}
	kb, err := strconv.ParseInt(string(rest[start:end]), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s value %q: %w", prefix, string(rest[start:end]), err)
	}
	return kb, nil
}

// readFile is the test-injectable file reader. Tests override it
// to feed parsers fixture bytes; production calls os.ReadFile.
// Returns the raw OS error on failure so operator logs surface
// the actual cause (e.g. "permission denied" on a hardened
// container) rather than a downstream parse error like "VmRSS
// line not found in /proc/self/status".
var readFile = func(path string) ([]byte, error) {
	return os.ReadFile(path)
}
