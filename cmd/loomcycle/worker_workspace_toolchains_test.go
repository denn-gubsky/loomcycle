package main

import (
	"bytes"
	"strings"
	"testing"
)

// RFC HH §1: a malformed LOOMCYCLE_WORKSPACE_TOOLCHAINS entry stops the
// workspace worker at its start, naming the entry, before it attaches.
func TestRunWorkspaceWorker_AMalformedToolchainEntryStopsTheStart(t *testing.T) {
	t.Setenv("LOOMCYCLE_WORKSPACE_TOOLCHAINS", "python,go=/usr/local/go/bin/go")
	var stderr bytes.Buffer
	if code := runWorkspaceWorker("127.0.0.1:1", "w1", 0, false, nil, &stderr); code != 2 {
		t.Fatalf("exit %d, want 2 (stderr %q)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "/usr/local/go/bin/go") {
		t.Fatalf("stderr %q does not name the entry", stderr.String())
	}
}
