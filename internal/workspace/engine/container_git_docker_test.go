package engine_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacetest"
)

// A git workspace's session reads its mirror's objects at /mirror/objects
// and cannot write them (RFC GW §4.2, P11 plan §11 "Isolation"): a command
// could otherwise damage what every workspace of the repository rests on.
func TestContainer_AGitSessionReadsItsMirrorAndCannotWriteIt(t *testing.T) {
	cli, image := workspacetest.RequireDocker(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const key = "0123456789abcdef0123456789abcdef"
	objects := filepath.Join(root, ".loomcycle-mirrors", key, "objects")
	if err := os.MkdirAll(objects, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(objects, "probe"), []byte("mirror\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := engine.NewContainer(engine.ContainerOptions{
		CLI: cli, Image: image, User: fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), WorkerKey: engine.WorkerKey(t.Name(), root),
		MemoryMB: 256, Pids: 128, TmpMB: 16,
		MirrorKeyFor: func(ws string) string {
			if ws == "ws1" {
				return key
			}
			return ""
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	s := &session{t: t, e: c, cli: cli, root: root}
	s.workspace("ws1")
	if res := s.exec("ws1", "cat "+engine.MirrorObjectsMount+"/probe", 30*time.Second); res.ExitCode != 0 || !strings.Contains(string(res.Output), "mirror") {
		t.Fatalf("reading the mirror: %d %q", res.ExitCode, res.Output)
	}
	for _, cmd := range []string{
		"echo x > " + engine.MirrorObjectsMount + "/new",
		"echo x >> " + engine.MirrorObjectsMount + "/probe",
		"rm " + engine.MirrorObjectsMount + "/probe",
	} {
		if res := s.exec("ws1", cmd, 30*time.Second); res.ExitCode == 0 {
			t.Errorf("%q succeeded in the session", cmd)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(objects, "probe")); string(b) != "mirror\n" {
		t.Errorf("the mirror's file is %q", b)
	}
	// A workspace that is no git workspace has no mirror mounted.
	s.workspace("ws2")
	if res := s.exec("ws2", "ls "+engine.MirrorObjectsMount, 30*time.Second); res.ExitCode == 0 {
		t.Errorf("a general workspace sees %s: %q", engine.MirrorObjectsMount, res.Output)
	}
}
