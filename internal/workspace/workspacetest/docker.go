package workspacetest

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// DefaultTestImage is the session image the container engine's tests run: a
// small one with a shell, wget, nslookup and find, which is all they use.
const DefaultTestImage = "busybox:1.36"

// dockerCLI is the container command the tests drive.
const dockerCLI = "docker"

// RequireDocker returns the container CLI and a session image for a test of
// the container engine (RFC GW P3). Without a daemon that answers the test is
// skipped; with LOOMCYCLE_REQUIRE_DOCKER=1 it fails instead, so a CI job that
// must run these tests cannot pass by skipping them. The image is
// DefaultTestImage, pulled when missing.
func RequireDocker(t *testing.T) (cli, image string) {
	t.Helper()
	skip := t.Skipf
	if os.Getenv("LOOMCYCLE_REQUIRE_DOCKER") == "1" {
		skip = t.Fatalf
	}
	if _, err := exec.LookPath(dockerCLI); err != nil {
		skip("no %s on PATH: %v", dockerCLI, err)
		return "", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, dockerCLI, "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		skip("the container daemon does not answer: %v: %s", err, out)
		return "", ""
	}
	if exec.CommandContext(ctx, dockerCLI, "image", "inspect", DefaultTestImage).Run() == nil {
		return dockerCLI, DefaultTestImage
	}
	pctx, pcancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer pcancel()
	if out, err := exec.CommandContext(pctx, dockerCLI, "pull", "-q", DefaultTestImage).CombinedOutput(); err != nil {
		skip("cannot pull %s: %v: %s", DefaultTestImage, err, out)
		return "", ""
	}
	return dockerCLI, DefaultTestImage
}
