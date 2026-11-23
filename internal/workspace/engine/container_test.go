package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func testContainer(t *testing.T, mod func(*ContainerOptions)) *Container {
	t.Helper()
	opt := ContainerOptions{Image: "img:1", User: "1000:1000", WorkerKey: "k1"}
	if mod != nil {
		mod(&opt)
	}
	c, err := NewContainer(opt)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// hasPair reports whether args holds flag directly followed by value.
func hasPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// Every confinement of RFC GW §4.3 is in a session's run arguments, and the
// workspace is the only mount, at the fixed path.
func TestContainer_RunArgsConfineTheSession(t *testing.T) {
	c := testContainer(t, func(o *ContainerOptions) {
		o.Runtime = "runsc"
		o.MemoryMB = 512
		o.CPUs = 1.5
		o.Pids = 64
		o.TmpMB = 32
	})
	a := c.runArgs("ws1", "/data/ws1")
	for _, p := range [][2]string{
		{"--network", "none"}, {"--cap-drop", "ALL"}, {"--security-opt", "no-new-privileges"}, {"--user", "1000:1000"},
		{"--pids-limit", "64"}, {"--memory", "512m"}, {"--memory-swap", "512m"}, {"--cpus", "1.5"}, {"--runtime", "runsc"},
		{"-v", "/data/ws1:/workspace:rw"}, {"--workdir", "/workspace"}, {"--name", "loomws-k1-ws1"},
		{"--label", "loomcycle.workspace.worker=k1"}, {"--tmpfs", "/tmp:rw,size=32m,mode=1777,nosuid,nodev,exec"},
	} {
		if !hasPair(a, p[0], p[1]) {
			t.Errorf("run args lack %s %s: %v", p[0], p[1], a)
		}
	}
	for _, f := range []string{"--read-only", "--init"} {
		if !slices.Contains(a, f) {
			t.Errorf("run args lack %s: %v", f, a)
		}
	}
	if n := strings.Count(strings.Join(a, " "), " -v "); n != 1 {
		t.Errorf("%d mounts, want only the workspace: %v", n, a)
	}
	for _, f := range []string{"--privileged", "--cap-add", "--env-file", "--network=host", "--pid", "--ipc"} {
		if slices.Contains(a, f) {
			t.Errorf("run args hold %s", f)
		}
	}
	if !slices.Equal(a[len(a)-3:], []string{"img:1", "sleep", "infinity"}) {
		t.Errorf("run args end %v, want the image and sleep infinity", a[len(a)-3:])
	}
}

// Every --env names its value: a bare `--env NAME` would copy the worker's
// own variable (its token) into the session.
func TestContainer_TheSessionEnvironmentIsTheEnginesOwn(t *testing.T) {
	a := testContainer(t, nil).runArgs("ws1", "/d")
	for i, x := range a {
		if (x == "--env" || x == "-e") && (i+1 >= len(a) || !strings.Contains(a[i+1], "=")) {
			t.Errorf("an --env without a value: %v", a)
		}
		if strings.HasPrefix(x, "--env=") || strings.HasPrefix(x, "--env-file") {
			t.Errorf("an unexpected env flag %q", x)
		}
	}
	if !hasPair(a, "--env", "HOME=/workspace") {
		t.Errorf("HOME is not the workspace: %v", a)
	}
}

func TestNewContainer_RefusesWhatCouldWeakenOrConfuseTheSession(t *testing.T) {
	for name, mod := range map[string]func(*ContainerOptions){
		"root user":            func(o *ContainerOptions) { o.User = "0:0" },
		"no user":              func(o *ContainerOptions) { o.User = "" },
		"named user":           func(o *ContainerOptions) { o.User = "nobody" },
		"no image":             func(o *ContainerOptions) { o.Image = "" },
		"image read as flag":   func(o *ContainerOptions) { o.Image = "--privileged" },
		"image with space":     func(o *ContainerOptions) { o.Image = "a b" },
		"runtime read as flag": func(o *ContainerOptions) { o.Runtime = "--x" },
		"runtime with slash":   func(o *ContainerOptions) { o.Runtime = "/usr/bin/runc" },
		"host root with colon": func(o *ContainerOptions) { o.HostRoot = "/a:/b" },
		"bad worker key":       func(o *ContainerOptions) { o.WorkerKey = "a/b" },
	} {
		opt := ContainerOptions{Image: "img:1", User: "1000:1000", WorkerKey: "k1"}
		mod(&opt)
		if _, err := NewContainer(opt); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A directory outside the workspace is refused before anything runs, as are
// a command the operator's list does not name and an empty one.
func TestContainer_RefusesBeforeStartingASession(t *testing.T) {
	c := testContainer(t, func(o *ContainerOptions) { o.CLI = "/nonexistent/cli"; o.Commands = []string{"python3"} })
	ctx := context.Background()
	for name, req := range map[string]ExecRequest{
		"dir outside": {Workspace: "ws1", Root: "/r/ws1", Dir: "/r/ws2", Command: "python3 x", Timeout: time.Second},
		"dir above":   {Workspace: "ws1", Root: "/r/ws1", Dir: "/r", Command: "python3 x", Timeout: time.Second},
		"not listed":  {Workspace: "ws1", Root: "/r/ws1", Dir: "/r/ws1", Command: "rm -rf /", Timeout: time.Second},
		"chained":     {Workspace: "ws1", Root: "/r/ws1", Dir: "/r/ws1", Command: "python3 x; rm y", Timeout: time.Second},
		"empty":       {Workspace: "ws1", Root: "/r/ws1", Dir: "/r/ws1", Command: "  ", Timeout: time.Second},
		"no timeout":  {Workspace: "ws1", Root: "/r/ws1", Dir: "/r/ws1", Command: "python3 x"},
	} {
		_, err := c.Exec(ctx, req)
		if err == nil || strings.Contains(err.Error(), "/nonexistent/cli") {
			t.Errorf("%s: err = %v, want a refusal before the CLI runs", name, err)
		}
		if name == "not listed" && !errors.Is(err, ErrRefused) {
			t.Errorf("%s: err = %v, want ErrRefused", name, err)
		}
	}
}

func TestWorkerKey_DiffersByWorkerAndRoot(t *testing.T) {
	a, b, c := WorkerKey("w1", "/r1"), WorkerKey("w2", "/r1"), WorkerKey("w1", "/r2")
	if a == b || a == c || !plainName(a) {
		t.Errorf("keys %q %q %q", a, b, c)
	}
}
