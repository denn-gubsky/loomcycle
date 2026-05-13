//go:build linux

package metrics

import (
	"strings"
	"testing"
)

// Fixtures embedded as constants so the tests run anywhere, not
// just on a machine whose /proc happens to have the right shape.

const sampleStatusVmRSS = `Name:	loomcycle
Umask:	0022
State:	S (sleeping)
Tgid:	12345
VmPeak:	   53288 kB
VmSize:	   48192 kB
VmLck:	       0 kB
VmRSS:	   45200 kB
VmData:	   12000 kB
Threads:	13
`

const sampleStatusNoVmRSS = `Name:	loomcycle
State:	S (sleeping)
VmSize:	   48192 kB
`

const sampleSelfStat = `12345 (loomcycle) S 1 12345 12345 0 -1 4194304 1234 0 0 0 250 100 0 0 20 0 13 0 12345678 48000000 5650 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0 0 0 0 0 0 0 0 0`

const sampleProcStat = `cpu  100 50 200 4000 30 0 20 0 0 0
cpu0 25 12 50 1000 7 0 5 0 0 0
intr 12345 0 0 0
`

const sampleMeminfo = `MemTotal:        8073648 kB
MemFree:          170380 kB
MemAvailable:     145392 kB
Buffers:            3100 kB
Cached:           159644 kB
`

func TestParseVmRSS_HappyPath(t *testing.T) {
	got, err := parseVmRSS([]byte(sampleStatusVmRSS))
	if err != nil {
		t.Fatalf("parseVmRSS: %v", err)
	}
	want := int64(45200) * 1024
	if got != want {
		t.Errorf("VmRSS = %d bytes, want %d", got, want)
	}
}

func TestParseVmRSS_MalformedNoVmRSSLine(t *testing.T) {
	_, err := parseVmRSS([]byte(sampleStatusNoVmRSS))
	if err == nil {
		t.Fatal("expected error when VmRSS missing")
	}
	if !strings.Contains(err.Error(), "VmRSS line not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseSelfStatTicks_HappyPath(t *testing.T) {
	// utime field 14 = "250", stime field 15 = "100" in fixture.
	// Total = 350.
	got, err := parseSelfStatTicks([]byte(sampleSelfStat))
	if err != nil {
		t.Fatalf("parseSelfStatTicks: %v", err)
	}
	if got != 350 {
		t.Errorf("ticks = %d, want 350", got)
	}
}

func TestParseProcStatCPU0_HappyPath(t *testing.T) {
	total, idle, err := parseProcStatCPU0([]byte(sampleProcStat))
	if err != nil {
		t.Fatalf("parseProcStatCPU0: %v", err)
	}
	wantTotal := uint64(100 + 50 + 200 + 4000 + 30 + 0 + 20 + 0 + 0 + 0)
	if total != wantTotal {
		t.Errorf("total = %d, want %d", total, wantTotal)
	}
	if idle != 4000 {
		t.Errorf("idle = %d, want 4000", idle)
	}
}

func TestParseProcStatCPU0_Malformed(t *testing.T) {
	_, _, err := parseProcStatCPU0([]byte("not the right shape\n"))
	if err == nil {
		t.Fatal("expected error on malformed /proc/stat")
	}
}

func TestParseProcMeminfo_HappyPath(t *testing.T) {
	total, avail, err := parseProcMeminfo([]byte(sampleMeminfo))
	if err != nil {
		t.Fatalf("parseProcMeminfo: %v", err)
	}
	wantTotal := int64(8073648) * 1024
	wantAvail := int64(145392) * 1024
	if total != wantTotal {
		t.Errorf("MemTotal = %d, want %d", total, wantTotal)
	}
	if avail != wantAvail {
		t.Errorf("MemAvailable = %d, want %d", avail, wantAvail)
	}
}

func TestReadProcMetrics_DeltaCPU(t *testing.T) {
	// Mock the file-reader so two sequential calls return two
	// different /proc/self/stat snapshots — second has +50 ticks
	// at +1 wall-clock second → CPU% ≈ 50.0 (50%×100 = 5000 in
	// cpuPctX100 units).
	defer restoreReadFile()
	calls := 0
	readFile = func(path string) ([]byte, error) {
		switch path {
		case "/proc/self/status":
			return []byte(sampleStatusVmRSS), nil
		case "/proc/self/stat":
			calls++
			if calls == 1 {
				return []byte(sampleSelfStat), nil // utime+stime = 350
			}
			// Second call: utime field 14 = "350" (+100), stime field 15 = "100" (unchanged)
			// total = 450 → delta 100 ticks
			return []byte(`12345 (loomcycle) S 1 12345 12345 0 -1 4194304 1234 0 0 0 350 100 0 0 20 0 13 0 12345678 48000000 5650 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0 0 0 0 0 0 0 0 0`), nil
		}
		return nil, nil
	}
	// First call — populates `prev`, returns 0 CPU%.
	m1, snap1, err := readProcMetrics(false, cpuSnapshot{})
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if m1.rssBytes == 0 {
		t.Errorf("RSS not populated on first call: %d", m1.rssBytes)
	}
	if m1.cpuPctX100 != 0 {
		t.Errorf("CPU should be 0 on first call (no prev); got %d", m1.cpuPctX100)
	}
	// Backdate snapshot by 1 second to simulate a 1 sec gap.
	snap1.at = snap1.at.Add(-1_000_000_000) // -1s in nanoseconds
	// Second call. Delta = 450 - 350 = 100 ticks. Wall = ~1s.
	// CPU% = 100 ticks / 100 hz / 1s × 100 = 100%. ×100 = 10000.
	m2, _, err := readProcMetrics(false, snap1)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	// Allow ±10% margin for timing slop.
	if m2.cpuPctX100 < 9000 || m2.cpuPctX100 > 11000 {
		t.Errorf("CPU%%×100 = %d, want ~10000 (delta 100 ticks / 1 sec)", m2.cpuPctX100)
	}
}

func restoreReadFile() {
	readFile = func(path string) ([]byte, error) {
		return nil, nil
	}
}

// TestReadProcMetrics_SurfacesOSError — a permission error from
// the OS (e.g. hardened container blocking /proc) must surface as
// the real OS error in the sampler log, not as a misleading parse
// error like "VmRSS line not found".
func TestReadProcMetrics_SurfacesOSError(t *testing.T) {
	defer restoreReadFile()
	readFile = func(path string) ([]byte, error) {
		return nil, &mockOSError{msg: "permission denied"}
	}
	_, _, err := readProcMetrics(false, cpuSnapshot{})
	if err == nil {
		t.Fatal("expected error from readProcMetrics when /proc unreadable")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error message %q does not mention real OS cause", err.Error())
	}
}

type mockOSError struct{ msg string }

func (e *mockOSError) Error() string { return e.msg }
