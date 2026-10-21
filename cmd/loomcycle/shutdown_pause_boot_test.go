//go:build unix

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// These boot the real binary (RFC ES F1): a SIGTERM pauses the runs in flight
// instead of abandoning them. A run inside a Bash call has the call
// interrupted — its process group killed — and parks; the next boot on the same
// data dir resumes it to completion.

// shutdownPauseScript sleeps in Bash, recording its shell's pid (the process
// group id: Bash puts each command in its own group). Interrupted, it catches
// the failed call and runs a second one, so the resumed run ends on an answer
// that names the interruption.
const shutdownPauseScript = `
function run(input) {
  try {
    Bash({command: "echo $$ > sleeper.pid; sleep 30"});
    return "slept through";
  } catch (e) {
    Bash({command: "echo again"});
    return "resumed after interruption";
  }
}
`

func shutdownPauseYAML(volume string) string {
	var b strings.Builder
	b.WriteString("provider_priority: [mock]\n")
	b.WriteString("tiers:\n  middle:\n    - { provider: mock, model: mock-generic }\n")
	fmt.Fprintf(&b, "volumes:\n  default: { path: %q, mode: rw, default: true }\n", volume)
	b.WriteString("agents:\n  sleeper:\n    provider: code-js\n    tools: [Bash]\n    code: |\n")
	for _, line := range strings.Split(strings.TrimSpace(shutdownPauseScript), "\n") {
		b.WriteString("      " + line + "\n")
	}
	return b.String()
}

type shutdownPauseBoot struct {
	bin, dir, dataDir, volume, cfgPath string
	port                               int
}

func newShutdownPauseBoot(t *testing.T) *shutdownPauseBoot {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and boots the binary; skipped in -short")
	}
	dir := t.TempDir()
	b := &shutdownPauseBoot{
		bin:     builtBootBinary(t),
		dir:     dir,
		dataDir: filepath.Join(dir, "data"),
		volume:  filepath.Join(dir, "work"),
		cfgPath: filepath.Join(dir, "loomcycle.yaml"),
		port:    freePort(t),
	}
	for _, d := range []string{b.dataDir, b.volume, filepath.Join(dir, "home")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(b.cfgPath, []byte(shutdownPauseYAML(b.volume)), 0o600); err != nil {
		t.Fatal(err)
	}
	return b
}

type runningBoot struct {
	cmd     *exec.Cmd
	out     *lockedBuffer
	done    chan struct{} // closed once the process has exited
	waitErr error         // its exit, read after done
	base    string
}

// wait reports whether the process exited within d, and its exit.
func (rb *runningBoot) wait(d time.Duration) (bool, error) {
	select {
	case <-rb.done:
		return true, rb.waitErr
	default:
	}
	select {
	case <-rb.done:
		return true, rb.waitErr
	case <-time.After(d):
		return false, nil
	}
}

// start boots the binary and waits for /healthz. The environment is explicit:
// nothing is inherited, so no real config or credentials are read.
func (b *shutdownPauseBoot) start(t *testing.T) *runningBoot {
	return b.startOn(t, b.port)
}

// startOn is start on port, with extra environment (KEY=value).
func (b *shutdownPauseBoot) startOn(t *testing.T, port int, extra ...string) *runningBoot {
	t.Helper()
	env := append([]string{
		"HOME=" + filepath.Join(b.dir, "home"),
		"PATH=/usr/bin:/bin",
		"LOOMCYCLE_DATA_DIR=" + b.dataDir,
		"LOOMCYCLE_LISTEN_ADDR=" + net.JoinHostPort("127.0.0.1", fmt.Sprint(port)),
		"LOOMCYCLE_MOCK_ENABLED=1",
		"LOOMCYCLE_CODE_AGENTS_ENABLED=1",
		"LOOMCYCLE_BASH_ENABLED=1",
		"LOOMCYCLE_SHUTDOWN_PAUSE_TIMEOUT_MS=6000",
	}, extra...)
	cmd := exec.Command(b.bin, "--config", b.cfgPath)
	cmd.Dir = b.dir
	cmd.Env = env
	out := &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	rb := &runningBoot{cmd: cmd, out: out, done: make(chan struct{}), base: fmt.Sprintf("http://127.0.0.1:%d", port)}
	go func() { rb.waitErr = cmd.Wait(); close(rb.done) }()
	t.Cleanup(func() {
		if exited, _ := rb.wait(0); !exited {
			_ = cmd.Process.Kill()
			<-rb.done
		}
	})
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if exited, err := rb.wait(200 * time.Millisecond); exited {
			t.Fatalf("binary exited before serving: %v\n%s", err, out.String())
		}
		resp, err := client.Get(rb.base + "/healthz")
		if err != nil {
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return rb
		}
	}
	t.Fatalf("binary did not serve /healthz within 90s\n%s", out.String())
	return nil
}

// startRun posts the run and reads its stream in the background, as a client
// would; the stream ends when the server goes away.
func (rb *runningBoot) startRun(t *testing.T, agentID string) {
	t.Helper()
	body := fmt.Sprintf(`{"agent":"sleeper","prompt":"go","user_id":"u","agent_id":%q}`, agentID)
	req, err := http.NewRequest(http.MethodPost, rb.base+"/v1/runs", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/runs: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("POST /v1/runs = %d", resp.StatusCode)
	}
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
		}
	}()
}

func TestBoot_ShutdownPausesRunAndRestartResumesIt(t *testing.T) {
	b := newShutdownPauseBoot(t)
	first := b.start(t)
	first.startRun(t, "shutdown-pause-1")

	pgid := waitForSleeper(t, b.volume, first)
	shutDownParked(t, first, pgid)

	run := readRun(t, b.dataDir, "shutdown-pause-1")
	if run.PauseState != store.PauseStatePaused || run.Status != store.RunRunning {
		t.Fatalf("after shutdown the run is status=%s pause_state=%q, want running/paused\n%s",
			run.Status, run.PauseState, first.out.String())
	}

	// A second boot on the same data dir resumes the run, which now runs to
	// its answer.
	second := b.start(t)
	for deadline := time.Now().Add(60 * time.Second); ; {
		status := runStatus(t, second.base, run.ID)
		if status == string(store.RunCompleted) {
			break
		}
		if status == string(store.RunFailed) || status == string(store.RunCancelled) || time.Now().After(deadline) {
			t.Fatalf("resumed run status=%q, want completed\n%s", status, second.out.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := second.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if exited, _ := second.wait(15 * time.Second); !exited {
		t.Fatalf("second shutdown did not finish within 15s\n%s", second.out.String())
	}
	if text := runText(t, b.dataDir, run.ID); !strings.Contains(text, "resumed after interruption") {
		t.Errorf("resumed run's answer = %q, want the interrupted branch's answer", text)
	}
}

func readRun(t *testing.T, dataDir, agentID string) store.Run {
	t.Helper()
	st, err := storesqlite.Open(filepath.Join(dataDir, "loomcycle.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	run, err := st.GetRunByAgentID(context.Background(), agentID)
	if err != nil {
		t.Fatalf("read run %s: %v", agentID, err)
	}
	return run
}

// runText is the run's streamed answer text, from its stored events.
func runText(t *testing.T, dataDir, runID string) string {
	t.Helper()
	st, err := storesqlite.Open(filepath.Join(dataDir, "loomcycle.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	events, err := st.GetRunEventsSince(context.Background(), runID, 0, 10000)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var b strings.Builder
	for _, e := range events {
		if e.Type == "text" || e.Type == "done" {
			b.Write(e.Payload)
		}
	}
	return b.String()
}

func runStatus(t *testing.T, base, runID string) string {
	t.Helper()
	resp, err := http.Get(base + "/v1/runs/" + runID)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var buf strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		buf.WriteString(sc.Text())
	}
	s := buf.String()
	i := strings.Index(s, `"status":"`)
	if i < 0 {
		return ""
	}
	s = s[i+len(`"status":"`):]
	if j := strings.IndexByte(s, '"'); j >= 0 {
		return s[:j]
	}
	return ""
}

// waitForSleeper waits for the run's Bash call to be running — its shell has
// written its pid, which is its process group id — and returns that group.
func waitForSleeper(t *testing.T, volume string, rb *runningBoot) int {
	t.Helper()
	pidFile := filepath.Join(volume, "sleeper.pid")
	for deadline := time.Now().Add(30 * time.Second); ; {
		if raw, err := os.ReadFile(pidFile); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && n > 0 {
				if err := syscall.Kill(-n, 0); err != nil {
					t.Fatalf("the sleep's process group %d is not alive before shutdown: %v", n, err)
				}
				return n
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Bash call never started\n%s", rb.out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// shutDownParked sends SIGTERM and asserts a bounded clean exit that paused the
// run, interrupted its Bash call, and took the call's process group with it.
func shutDownParked(t *testing.T, rb *runningBoot, pgid int) {
	t.Helper()
	sent := time.Now()
	if err := rb.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// The shutdown is bounded: a 6 s pause, the listeners' 5 s drains
	// alongside it, then the flushes. Well under the sleep's 30 s.
	exited, err := rb.wait(15 * time.Second)
	if !exited {
		t.Fatalf("shutdown did not finish within 15s\n%s", rb.out.String())
	}
	if err != nil {
		t.Fatalf("shutdown exited with %v\n%s", err, rb.out.String())
	}
	t.Logf("shutdown took %s", time.Since(sent).Round(time.Millisecond))
	if log := rb.out.String(); !strings.Contains(log, "1 run(s) paused") || !strings.Contains(log, "1 had their tool calls interrupted") {
		t.Errorf("shutdown log does not report the run paused and its tool call interrupted\n%s", log)
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	t.Fatalf("the sleep's process group %d outlived the shutdown\n%s", pgid, rb.out.String())
}
