//go:build !windows

package local

import (
	"bytes"
	"context"
	"flag"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/conversion/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/conversion/worker"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
)

// TestMain lets this test binary stand in for `loomcycle worker`: the
// supervisor starts os.Args[0] with "worker" and the flags the real binary
// takes, and the environment it gives every child.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "worker" {
		fs := flag.NewFlagSet("worker", flag.ExitOnError)
		target := fs.String("dispatcher", "", "")
		parent := fs.Int("parent-pid", 0, "")
		_ = fs.Int("oom-score-adj", 0, "")
		id := fs.String("id", "", "")
		_ = fs.Parse(os.Args[2:])
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := worker.Run(ctx, worker.Options{Target: *target, Token: os.Getenv("LOOMCYCLE_WORKER_TOKEN"), ID: *id, ParentPID: *parent}); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type textSink struct{ b strings.Builder }

func (s *textSink) Text(md string) error                { s.b.WriteString(md); return nil }
func (s *textSink) Image(extract.Image) (string, error) { return "", nil }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Children attach and convert; a child killed with SIGKILL costs nothing to
// the pool's other child, and the supervisor starts another in its place.
func TestSupervisor_KeepsThePoolUpWhenAChildIsKilled(t *testing.T) {
	l, socket, cleanup, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	d := dispatch.New(dispatch.Options{Token: token})
	go func() { _ = d.Serve(l) }()
	defer d.Stop()
	sup := Start(Options{Binary: os.Args[0], Socket: socket, Token: token, Workers: 2, MemoryLimitMB: 256, OnRespawn: d.AddRespawn})
	defer sup.Stop()
	waitFor(t, "two workers", func() bool { return d.Stats().Workers == 2 && d.Stats().Idle == 2 })

	extractCSV := func() string {
		t.Helper()
		data := []byte("name,age\nada,36\n")
		var s textSink
		if _, err := d.Client().Extract(context.Background(), "text/csv", bytes.NewReader(data), int64(len(data)), &s, extract.Options{Name: "p.csv"}); err != nil {
			t.Fatal(err)
		}
		return s.b.String()
	}
	if !strings.Contains(extractCSV(), "ada") {
		t.Fatal("no text from a child worker")
	}

	sup.mu.Lock()
	victim := sup.pids[0]
	sup.mu.Unlock()
	if err := syscall.Kill(victim, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the killed child to detach", func() bool { return d.Stats().Workers == 1 })
	if !strings.Contains(extractCSV(), "ada") {
		t.Fatal("the surviving child did not convert")
	}
	waitFor(t, "a replacement child", func() bool { return d.Stats().Workers == 2 && d.Stats().Respawns == 1 })
	sup.mu.Lock()
	replacement := sup.pids[0]
	sup.mu.Unlock()
	if replacement == victim || replacement == 0 {
		t.Fatalf("child 0 is pid %d, the killed one was %d", replacement, victim)
	}
}

// A child gets no setting of the core's: only what a process needs to run,
// its token and its memory limit.
func TestChildEnv_CarriesNoCoreSetting(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-must-not-pass")
	t.Setenv("LOOMCYCLE_PG_DSN", "postgres://must-not-pass")
	env := strings.Join(childEnv(Options{Token: "tok", MemoryLimitMB: 512}), "\n")
	for _, bad := range []string{"ANTHROPIC_API_KEY", "LOOMCYCLE_PG_DSN", "must-not-pass"} {
		if strings.Contains(env, bad) {
			t.Fatalf("the child environment carries %s:\n%s", bad, env)
		}
	}
	for _, want := range []string{"LOOMCYCLE_WORKER_TOKEN=tok", "LOOMCYCLE_WORKER_SLOTS=1", "GOMEMLIMIT=512MiB"} {
		if !strings.Contains(env, want) {
			t.Fatalf("the child environment lacks %s:\n%s", want, env)
		}
	}
}
