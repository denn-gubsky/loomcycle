package engine

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// A server's command line (RFC GW §5.3) is held to the operator's command
// list exactly as exec's is: a worker that lists its commands must not start
// through start what exec refuses. Both engines refuse before anything runs.
func TestStart_CommandIsHeldToTheOperatorsList(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	engines := map[string]Engine{
		"host":      &Host{Commands: []string{"python3"}},
		"container": testContainer(t, func(o *ContainerOptions) { o.CLI = "/nonexistent/cli"; o.Commands = []string{"python3"} }),
	}
	for ename, e := range engines {
		for name, cmd := range map[string]string{
			"not listed": "sh -c 'echo hi'",
			"chained":    "python3 -m http.server; rm -rf .",
			"background": "python3 x & rm y",
			"subshell":   "python3 $(rm y)",
		} {
			p, err := e.Start(ctx, StartRequest{Workspace: "ws1", Root: dir, Dir: dir, Command: cmd})
			if p != nil {
				_ = p.Kill()
			}
			if !errors.Is(err, ErrRefused) {
				t.Errorf("%s %s: err = %v, want ErrRefused", ename, name, err)
			}
		}
	}
}

// A program and a command at once is a caller's mistake, refused.
func TestStart_RefusesBothAProgramAndACommand(t *testing.T) {
	dir := t.TempDir()
	p, err := (&Host{}).Start(context.Background(), StartRequest{Root: dir, Dir: dir, Argv: []string{"true"}, Command: "true"})
	if p != nil {
		_ = p.Kill()
	}
	if err == nil {
		t.Fatal("Start took both Argv and Command")
	}
}

// A server's standard error is joined to its standard output, in the order
// written, so its log is one stream.
func TestHostStart_CommandJoinsStderrToStdout(t *testing.T) {
	dir := t.TempDir()
	p, err := (&Host{}).Start(context.Background(), StartRequest{Root: dir, Dir: dir, Command: "echo one; echo two >&2; echo three"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Kill() }()
	out, _ := io.ReadAll(p.Stdout())
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the command did not end")
	}
	if got := string(out); got != "one\ntwo\nthree\n" {
		t.Errorf("output = %q, want the three lines in order", got)
	}
	if p.Stderr() != "" {
		t.Errorf("stderr tail = %q, want everything on stdout", p.Stderr())
	}
}

// Listed, a command runs.
func TestHostStart_ListedCommandRuns(t *testing.T) {
	dir := t.TempDir()
	p, err := (&Host{Commands: []string{"echo"}}).Start(context.Background(), StartRequest{Root: dir, Dir: dir, Command: "echo listed"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Kill() }()
	out, _ := io.ReadAll(p.Stdout())
	if !strings.Contains(string(out), "listed") {
		t.Errorf("output = %q", out)
	}
}
