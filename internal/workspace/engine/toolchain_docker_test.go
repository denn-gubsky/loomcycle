package engine_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacetest"
)

// RFC HH §1: the container probe reports, from one confined session of the
// image, exactly the declared toolchains whose binaries the image has: the
// first found of several counts, and one it lacks is left out.
func TestContainer_ProbeReportsTheDeclaredToolchainsTheImageHas(t *testing.T) {
	cli, image := workspacetest.RequireDocker(t)
	c, err := engine.NewContainer(engine.ContainerOptions{
		CLI: cli, Image: image, User: fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		WorkerKey: engine.WorkerKey(t.Name(), t.TempDir()), MemoryMB: 256, Pids: 128, TmpMB: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Probe(context.Background(), []engine.Toolchain{
		{Name: "python", Binaries: []string{"python3", "python"}},
		{Name: "box", Binaries: []string{"no-such-tool", "busybox"}},
		{Name: "shell", Binaries: []string{"sh"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// busybox:1.36, the test image, has sh and busybox and no python.
	if !slices.Equal(got, []string{"box", "shell"}) {
		t.Fatalf("probe = %v, want [box shell]", got)
	}
}
