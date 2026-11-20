package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHost_ReportsTheExitCodeAndCombinedOutput(t *testing.T) {
	h := &Host{}
	res, err := h.Exec(context.Background(), ExecRequest{Dir: t.TempDir(), Command: "echo out; echo err >&2; exit 3", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || res.TimedOut || res.Truncated {
		t.Errorf("result = %+v, want exit 3, not timed out, not truncated", res)
	}
	if got := string(res.Output); !strings.Contains(got, "out\n") || !strings.Contains(got, "err\n") {
		t.Errorf("output = %q, want both streams", got)
	}
}

func TestHost_RunsInTheGivenDirectoryWithAScrubbedEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LOOMCYCLE_TEST_SECRET_TOKEN", "not-for-the-child")
	res, err := (&Host{}).Exec(context.Background(), ExecRequest{Dir: dir, Command: "pwd -P; env", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(dir)
	if !strings.HasPrefix(string(res.Output), real+"\n") {
		t.Errorf("output = %q, want it to start in %s", res.Output, real)
	}
	if strings.Contains(string(res.Output), "LOOMCYCLE_TEST_SECRET_TOKEN") {
		t.Errorf("the worker's environment reached the command: %q", res.Output)
	}
}

// The timeout kills the whole process group: a child the command put in the
// background dies with it, so nothing the command started outlives the call.
func TestHost_TimeoutKillsTheWholeProcessGroup(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	res, err := (&Host{}).Exec(context.Background(), ExecRequest{Dir: dir,
		Command: "(sleep 1; touch survived) & sleep 30", Timeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.ExitCode != -1 {
		t.Errorf("result = %+v, want timed out with exit -1", res)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Errorf("Exec took %v, want it to end at the timeout", el)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "survived")); err == nil {
		t.Error("a background child of the command outlived the timeout")
	}
}

func TestHost_OutputIsCutAtTheBoundWithAMarker(t *testing.T) {
	res, err := (&Host{MaxOutput: 1000}).Exec(context.Background(), ExecRequest{Dir: t.TempDir(),
		Command: "head -c 5000 /dev/zero | tr '\\0' x", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.ExitCode != 0 {
		t.Fatalf("result = truncated %v exit %d, want truncated and exit 0", res.Truncated, res.ExitCode)
	}
	if !strings.HasPrefix(string(res.Output), strings.Repeat("x", 1000)+"\n[output truncated at 1000 bytes]") {
		t.Errorf("output = %q", res.Output)
	}
}

func TestHost_AllowlistRefusesAnUnlistedCommandAndShellControl(t *testing.T) {
	h := &Host{Commands: []string{"echo"}}
	dir := t.TempDir()
	for _, c := range []string{"ls", "echo hi; touch x", "echo hi && touch x", "echo $(touch x)", "echo hi > x", "echo `touch x`"} {
		_, err := h.Exec(context.Background(), ExecRequest{Dir: dir, Command: c, Timeout: 5 * time.Second})
		if !errors.Is(err, ErrRefused) {
			t.Errorf("%q: err = %v, want ErrRefused", c, err)
		}
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("a refused command ran: the directory holds %d entries", len(ents))
	}
	res, err := h.Exec(context.Background(), ExecRequest{Dir: dir, Command: "echo listed", Timeout: 5 * time.Second})
	if err != nil || string(res.Output) != "listed\n" {
		t.Errorf("a listed command: %q, %v", res.Output, err)
	}
}

func TestHost_CancelledCallerEndsTheCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	_, err := (&Host{}).Exec(ctx, ExecRequest{Dir: t.TempDir(), Command: "sleep 30", Timeout: time.Minute})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
