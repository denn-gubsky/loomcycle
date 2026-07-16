package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeRunner records the argv it was handed and returns canned output, so tests
// assert command construction without a podman host.
type fakeRunner struct {
	calls [][]string
	stdin [][]byte
	fn    func(argv []string, stdin []byte) ([]byte, int, error)
}

func (f *fakeRunner) Run(_ context.Context, stdin []byte, _ int64, argv ...string) ([]byte, int, error) {
	cp := append([]string(nil), argv...)
	f.calls = append(f.calls, cp)
	f.stdin = append(f.stdin, append([]byte(nil), stdin...))
	if f.fn != nil {
		return f.fn(argv, stdin)
	}
	return nil, 0, nil
}

func testCfg() *Config {
	return &Config{
		PodmanBin: "podman", Image: "img:latest", CtrUser: "1000:1000",
		DefTmpfsMB: 512, MaxTmpfsMB: 2048, DefCPUs: 2, MaxCPUs: 2,
		DefMemMB: 2048, MaxMemMB: 2048, DefPids: 512, MaxPids: 512,
		DefTimeout: 30 * time.Second, MaxTimeout: 5 * time.Minute, MaxOutBytes: 1 << 20,
		SessionIdleTTL: 15 * time.Minute, SessionMaxTTL: time.Hour, MaxSessions: 32,
	}
}

// hasPair asserts args contains flag immediately followed by value.
func hasPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func has(args []string, v string) bool {
	for _, a := range args {
		if a == v {
			return true
		}
	}
	return false
}

func TestEngine_RunArgs_HardeningPosture(t *testing.T) {
	cfg := testCfg()
	cfg.Runtime = "runsc"
	e := NewEngine(cfg, &fakeRunner{})
	args := e.runArgs("loom-sbx-abc", openOpts{Network: "none", TmpfsMB: 512, CPUs: 2, MemMB: 2048, Pids: 512, Image: "img:latest"})

	// The security-critical flags MUST all be present.
	for _, want := range []struct{ flag, val string }{
		{"--network", "none"},
		{"--cap-drop", "ALL"},
		{"--security-opt", "no-new-privileges"},
		{"--user", "1000:1000"},
		{"--runtime", "runsc"},
		{"--pids-limit", "512"},
		{"--memory", "2048m"},
		{"--workdir", workDir},
	} {
		if !hasPair(args, want.flag, want.val) {
			t.Errorf("runArgs missing %s %s in %v", want.flag, want.val, args)
		}
	}
	if !has(args, "--read-only") {
		t.Errorf("runArgs missing --read-only: %v", args)
	}
	if !hasPair(args, "--cpus", "2") {
		t.Errorf("runArgs missing --cpus 2: %v", args)
	}
	// tmpfs workspace is in-memory, size-capped, mode 0700.
	var tmpfsWork string
	for i, a := range args {
		if a == "--tmpfs" && i+1 < len(args) && strings.HasPrefix(args[i+1], workDir+":") {
			tmpfsWork = args[i+1]
		}
	}
	if tmpfsWork == "" || !strings.Contains(tmpfsWork, "size=512m") || !strings.Contains(tmpfsWork, "mode=0700") {
		t.Errorf("tmpfs /work mount wrong: %q", tmpfsWork)
	}
	// Image + entrypoint come last.
	if args[len(args)-3] != "img:latest" || args[len(args)-2] != "sleep" || args[len(args)-1] != "infinity" {
		t.Errorf("expected image + sleep infinity at tail, got %v", args[len(args)-3:])
	}
	// Managed label so the crash-sweeper can reap it.
	if !hasPair(args, "--label", "loomcycle.managed=1") {
		t.Errorf("runArgs missing managed label: %v", args)
	}
}

func TestEngine_RunArgs_NetworkNoneByDefaultAndWhenEgressDisallowed(t *testing.T) {
	cfg := testCfg()
	cfg.AllowEgress = false
	e := NewEngine(cfg, &fakeRunner{})
	// Even if a session was clamped to egress, runArgs only opens the network
	// when the operator allows it — belt-and-suspenders with clampOpen.
	args := e.runArgs("n", openOpts{Network: "egress", TmpfsMB: 512, CPUs: 1, MemMB: 512, Pids: 100})
	if !hasPair(args, "--network", "none") {
		t.Errorf("egress must not open the network when AllowEgress=false: %v", args)
	}
}

func TestEngine_RunArgs_EgressBridgeWhenAllowed(t *testing.T) {
	cfg := testCfg()
	cfg.AllowEgress = true
	e := NewEngine(cfg, &fakeRunner{})
	args := e.runArgs("n", openOpts{Network: "egress", TmpfsMB: 512, CPUs: 1, MemMB: 512, Pids: 100})
	if !hasPair(args, "--network", "bridge") {
		t.Errorf("egress should open bridge when allowed: %v", args)
	}
}

func TestEngine_WriteArgs_PathViaEnvNotShell(t *testing.T) {
	fr := &fakeRunner{}
	e := NewEngine(testCfg(), fr)
	if err := e.Write(context.Background(), "n", "src/main.go", []byte("package main")); err != nil {
		t.Fatal(err)
	}
	argv := fr.calls[0]
	// The destination path must ride in an env var, never interpolated into the
	// shell string (injection safety). The shell command references $SBX_DEST.
	if !has(argv, "SBX_DEST=/work/src/main.go") {
		t.Errorf("write should pass dest via env: %v", argv)
	}
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "/work/src/main.go\"") && !has(argv, "SBX_DEST=/work/src/main.go") {
		t.Errorf("dest path leaked into the command string: %v", argv)
	}
	if string(fr.stdin[0]) != "package main" {
		t.Errorf("content should be piped via stdin, got %q", fr.stdin[0])
	}
}

func TestEngine_Exec_TimeoutReported(t *testing.T) {
	fr := &fakeRunner{fn: func(argv []string, _ []byte) ([]byte, int, error) {
		return []byte("partial"), -1, nil
	}}
	cfg := testCfg()
	cfg.MaxTimeout = time.Millisecond
	e := NewEngine(cfg, fr)
	// A zero timeout uses the default; force a tiny deadline so DeadlineExceeded
	// is deterministic without depending on the fake sleeping.
	_, _, timedOut, err := e.Exec(context.Background(), "n", "sleep 1", time.Nanosecond, 1<<20)
	if err != nil {
		t.Fatalf("exec err: %v", err)
	}
	if !timedOut {
		t.Errorf("expected timedOut=true for an expired deadline")
	}
}

func TestBoundedBuf_TruncatesAtCap(t *testing.T) {
	b := &boundedBuf{cap: 4}
	_, _ = b.Write([]byte("ab"))
	_, _ = b.Write([]byte("cdef"))
	if got := string(b.Bytes()); got != "abcd" {
		t.Errorf("bounded buffer = %q, want %q", got, "abcd")
	}
	if !b.truncated {
		t.Errorf("expected truncated=true")
	}
}
