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

// RequireDocker returns the container CLI and a session image for a test of
// the container engine (RFC GW P3). Without a daemon that answers the test is
// skipped; with LOOMCYCLE_REQUIRE_DOCKER=1 it fails instead, so a CI job that
// must run these tests cannot pass by skipping them. The image is
// LOOMCYCLE_TEST_WORKSPACE_IMAGE or DefaultTestImage, pulled when missing.
func RequireDocker(t *testing.T) (cli, image string) {
	t.Helper()
	skip := t.Skipf
	if os.Getenv("LOOMCYCLE_REQUIRE_DOCKER") == "1" {
		skip = t.Fatalf
	}
	cli = "docker"
	if _, err := exec.LookPath(cli); err != nil {
		skip("no %s on PATH: %v", cli, err)
		return "", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, cli, "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		skip("the container daemon does not answer: %v: %s", err, out)
		return "", ""
	}
	image = os.Getenv("LOOMCYCLE_TEST_WORKSPACE_IMAGE")
	if image == "" {
		image = DefaultTestImage
	}
	if exec.CommandContext(ctx, cli, "image", "inspect", image).Run() == nil {
		return cli, image
	}
	pctx, pcancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer pcancel()
	// #nosec G204 -- a test pulling its own fixed or operator-named image
	if out, err := exec.CommandContext(pctx, cli, "pull", "-q", image).CombinedOutput(); err != nil {
		skip("cannot pull %s: %v: %s", image, err, out)
		return "", ""
	}
	return cli, image
}
