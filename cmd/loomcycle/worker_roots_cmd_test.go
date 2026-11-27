package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --personal and --root are a workspace worker's: a conversion worker
// given them refuses to start rather than ignore a folder the user offered.
func TestWorkerCmd_RootFlagsNeedTheWorkspaceRole(t *testing.T) {
	for _, args := range [][]string{
		{"--personal"},
		{"--root", "work=" + t.TempDir()},
		{"--role", "conversion", "--personal", "--root", "work=" + t.TempDir()},
	} {
		var stderr bytes.Buffer
		if code := runWorker(args, &stderr); code != 2 || !strings.Contains(stderr.String(), "need --role workspace") {
			t.Errorf("%v = %d %q, want 2 and the refusal", args, code, stderr.String())
		}
	}
}

// A --root names a folder as name=path; ~ is the home folder and a relative
// path is made absolute, so the start checks see the folder meant.
func TestWorkerCmd_RootFlagParsesToAnAbsolutePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	wd, _ := os.Getwd()
	for in, want := range map[string]string{
		"work=~/code":    filepath.Join(home, "code"),
		"work=~":         home,
		"work=rel/dir":   filepath.Join(wd, "rel/dir"),
		"work = /abs/x ": "/abs/x",
	} {
		r, err := parseWorkerRoot(in)
		if err != nil || r.Name != "work" || r.Path != want {
			t.Errorf("%q = %+v, %v; want work %s", in, r, err, want)
		}
	}
	for _, bad := range []string{"work", "=/x", "work=", ""} {
		if _, err := parseWorkerRoot(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
