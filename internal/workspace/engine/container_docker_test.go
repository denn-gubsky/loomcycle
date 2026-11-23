package engine_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacetest"
)

// The tests below run real sessions and need a container daemon
// (workspacetest.RequireDocker). Each states a confinement of RFC GW §4.3
// that the host engine does not give.

type session struct {
	t    *testing.T
	e    engine.Engine
	cli  string
	root string // the worker's root, symlink-free
	key  string
}

func newSession(t *testing.T) *session {
	t.Helper()
	cli, image := workspacetest.RequireDocker(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := engine.WorkerKey(t.Name(), root)
	c, err := engine.NewContainer(engine.ContainerOptions{
		CLI: cli, Image: image, User: fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), WorkerKey: key,
		MemoryMB: 256, Pids: 128, TmpMB: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &session{t: t, e: c, cli: cli, root: root, key: key}
}

// workspace makes a workspace's directory as the worker does and returns it.
func (s *session) workspace(id string) string {
	s.t.Helper()
	dir := filepath.Join(s.root, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		s.t.Fatal(err)
	}
	return dir
}

func (s *session) exec(id, command string, timeout time.Duration) engine.ExecResult {
	s.t.Helper()
	dir := filepath.Join(s.root, id)
	res, err := s.e.Exec(context.Background(), engine.ExecRequest{Workspace: id, Root: dir, Dir: dir, Command: command, Timeout: timeout})
	if err != nil {
		s.t.Fatalf("exec %q: %v", command, err)
	}
	return res
}

// A session has no network: no route anywhere, and both a name lookup and a
// connection to an address fail at once (RFC GW §4.3, §11 "No name but
// egress"; spike C measured 16-40 s on a network that only dropped the
// traffic). The time is taken inside the session, so a slow daemon does not
// count.
func TestContainer_ACommandReachesNoNetworkAndFailsAtOnce(t *testing.T) {
	s := newSession(t)
	s.workspace("ws1")
	if res := s.exec("ws1", "tail -n +2 /proc/net/route; tail -n +2 /proc/net/ipv6_route 2>/dev/null | grep -v ' lo$'", 60*time.Second); strings.TrimSpace(string(res.Output)) != "" {
		t.Errorf("the session has routes:\n%s", res.Output)
	}
	for _, cmd := range []string{"nslookup example.com", "wget -q -T 5 -O /dev/null http://1.1.1.1/", "wget -q -T 5 -O /dev/null http://example.com/"} {
		timed := `a=$(cut -d' ' -f1 /proc/uptime); ` + cmd + ` >/dev/null 2>&1; rc=$?; b=$(cut -d' ' -f1 /proc/uptime); echo "$rc $a $b"`
		res := s.exec("ws1", timed, 60*time.Second)
		var rc int
		var a, b float64
		if _, err := fmt.Sscanf(string(res.Output), "%d %f %f", &rc, &a, &b); err != nil {
			t.Fatalf("%s: cannot read %q: %v", cmd, res.Output, err)
		}
		if rc == 0 || b-a >= 1 {
			t.Errorf("%s: exit %d after %.2fs, want a failure in under a second", cmd, rc, b-a)
		}
	}
}

// Nothing of the worker's environment reaches a session: not the command's
// environment, not any process's in the session, and the worker's own
// process is not in the session's process table to be read through /proc.
func TestContainer_ACommandCannotReadTheWorkersEnvironment(t *testing.T) {
	s := newSession(t)
	s.workspace("ws1")
	const canary = "loomcycle-p3-canary-7f3a9c"
	t.Setenv("LOOMCYCLE_WORKER_TOKEN", canary)
	t.Setenv("LOOMCYCLE_TEST_SECRET_TOKEN", canary)
	res := s.exec("ws1", `env; for f in /proc/[0-9]*/environ /proc/[0-9]*/cmdline; do cat "$f"; echo; done 2>/dev/null`, 30*time.Second)
	if strings.Contains(string(res.Output), canary) || strings.Contains(string(res.Output), "LOOMCYCLE_") {
		t.Fatalf("the worker's environment reached the session:\n%s", res.Output)
	}
	if !strings.Contains(string(res.Output), "HOME=/workspace") {
		t.Errorf("the session's environment = %q, want the engine's own (HOME=/workspace)", res.Output)
	}
	ppid := fmt.Sprintf("/proc/%d/", os.Getpid())
	if res := s.exec("ws1", "cat "+ppid+"environ", 30*time.Second); res.ExitCode == 0 && strings.Contains(string(res.Output), canary) {
		t.Errorf("the worker's process is readable from the session: %q", res.Output)
	}
}

// A command writes nothing outside /workspace: the root is read-only, and
// /tmp is the session's own, not the worker's, and ends with the session.
func TestContainer_ACommandCannotWriteOutsideTheWorkspace(t *testing.T) {
	s := newSession(t)
	dir := s.workspace("ws1")
	for _, target := range []string{"/x", "/etc/x", "/usr/x", "/bin/x", "/home/x", "/var/x"} {
		if res := s.exec("ws1", "echo x > "+target, 30*time.Second); res.ExitCode == 0 {
			t.Errorf("writing %s succeeded", target)
		}
	}
	mark := "loom-p3-tmp-" + s.key
	res := s.exec("ws1", "echo in > /workspace/kept && echo t > /tmp/"+mark, 30*time.Second)
	if res.ExitCode != 0 {
		t.Fatalf("writing in the workspace and /tmp: %d %q", res.ExitCode, res.Output)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "kept")); err != nil || string(b) != "in\n" {
		t.Errorf("the workspace's file on the worker = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(os.TempDir(), mark)); err == nil {
		t.Errorf("the session's /tmp is the worker's")
	}
	if err := s.e.Forget(context.Background(), "ws1"); err != nil {
		t.Fatal(err)
	}
	if res := s.exec("ws1", "cat /tmp/"+mark+"; cat /workspace/kept", 30*time.Second); strings.Contains(string(res.Output), "t\n") || !strings.Contains(string(res.Output), "in\n") {
		t.Errorf("after a new session: %q, want /tmp empty and the workspace kept", res.Output)
	}
}

// A session sees its own workspace only: another workspace of the same worker
// is neither under / nor reachable by the path the worker keeps it at.
func TestContainer_ACommandCannotSeeAnotherWorkspace(t *testing.T) {
	s := newSession(t)
	other := s.workspace("ws-other")
	s.workspace("ws1")
	const marker = "loom-p3-other-marker"
	if err := os.WriteFile(filepath.Join(other, marker), []byte("secret of another workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Errors are dropped: a refusal names the path it was asked for.
	res := s.exec("ws1", "cat "+filepath.Join(other, marker)+" 2>/dev/null; find / -name "+marker+" 2>/dev/null; ls -a /workspace", 120*time.Second)
	if strings.Contains(string(res.Output), "secret") || strings.Contains(string(res.Output), marker) {
		t.Fatalf("another workspace is visible from the session:\n%s", res.Output)
	}
}

// A command past its timeout is killed inside the session with everything it
// started; killing the CLI client alone would leave it running there. The
// session itself stays, so the next command does not start a new one.
func TestContainer_ATimeoutKillsTheCommandInsideTheSession(t *testing.T) {
	s := newSession(t)
	dir := s.workspace("ws1")
	s.exec("ws1", "true", 30*time.Second)
	started := s.sessionStarted()
	res := s.exec("ws1", "(sleep 2; touch /workspace/survived) & sleep 60", 500*time.Millisecond)
	if !res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("result = %+v, want timed out with exit -1", res)
	}
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(filepath.Join(dir, "survived")); err == nil {
		t.Fatal("a child of the killed command ran on in the session")
	}
	if got := s.exec("ws1", "ps 2>/dev/null | grep -c '[s]leep 60' || true", 30*time.Second); strings.TrimSpace(string(got.Output)) != "0" {
		t.Errorf("the killed command is still in the session's process table: %q", got.Output)
	}
	if s.sessionStarted() != started {
		t.Error("the timeout removed the session; a kill inside it was expected")
	}
}

// A command whose op the core gave up on (its context ended) is killed too.
func TestContainer_ACancelledCommandIsKilledInsideTheSession(t *testing.T) {
	s := newSession(t)
	dir := s.workspace("ws1")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(time.Second, cancel)
	_, err := s.e.Exec(ctx, engine.ExecRequest{Workspace: "ws1", Root: dir, Dir: dir, Command: "(sleep 3; touch /workspace/survived) & sleep 60", Timeout: time.Minute})
	if err == nil {
		t.Fatal("a cancelled exec returned no error")
	}
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(filepath.Join(dir, "survived")); err == nil {
		t.Fatal("a child of the cancelled command ran on in the session")
	}
}

// A session that is gone (reaped, crashed) is started again at the next
// command over the same directory, its files intact.
func TestContainer_AGoneSessionIsStartedAgainWithItsFiles(t *testing.T) {
	s := newSession(t)
	s.workspace("ws1")
	s.exec("ws1", "echo kept > /workspace/f", 30*time.Second)
	if out, err := exec.Command(s.cli, "rm", "-f", "loomws-"+s.key+"-ws1").CombinedOutput(); err != nil {
		t.Fatalf("removing the session: %v %s", err, out)
	}
	if res := s.exec("ws1", "cat /workspace/f", 30*time.Second); res.ExitCode != 0 || string(res.Output) != "kept\n" {
		t.Fatalf("after the session went: %d %q", res.ExitCode, res.Output)
	}
}

// A session runs as the worker's user, never root, with no capability and
// no way to gain privileges, at the fixed mount path.
func TestContainer_ASessionHoldsNoPrivilege(t *testing.T) {
	s := newSession(t)
	s.workspace("ws1")
	if err := os.Mkdir(filepath.Join(s.root, "ws1", "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.root, "ws1")
	res, err := s.e.Exec(context.Background(), engine.ExecRequest{Workspace: "ws1", Root: dir, Dir: filepath.Join(dir, "sub"), Timeout: 30 * time.Second,
		Command: "id -u; pwd; grep -E '^(CapEff|CapPrm|CapBnd|NoNewPrivs)' /proc/self/status"})
	if err != nil {
		t.Fatal(err)
	}
	out := string(res.Output)
	want := []string{fmt.Sprintf("%d\n", os.Getuid()), "/workspace/sub\n", "CapEff:\t0000000000000000", "CapPrm:\t0000000000000000", "CapBnd:\t0000000000000000", "NoNewPrivs:\t1"}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("session status lacks %q:\n%s", w, out)
		}
	}
}

// Forget ends the session and what runs in it; the next command starts a
// new one.
func TestContainer_ForgetEndsTheSession(t *testing.T) {
	s := newSession(t)
	s.workspace("ws1")
	s.exec("ws1", "true", 30*time.Second)
	if err := s.e.Forget(context.Background(), "ws1"); err != nil {
		t.Fatal(err)
	}
	if s.sessionStarted() != "" {
		t.Fatal("the session is still there after Forget")
	}
}

// sessionStarted is when ws1's session was started; "" when there is none.
func (s *session) sessionStarted() string {
	s.t.Helper()
	out, err := exec.Command(s.cli, "inspect", "--format", "{{.State.StartedAt}}", "loomws-"+s.key+"-ws1").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
