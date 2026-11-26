package config

import (
	"strings"
	"testing"
)

// Absent, a push may move only the workspace's own branch, named under
// refs/heads/, at most 64 MiB, and an existing forge branch is refused at
// open (RFC GW §6.2; the owner's rulings of 2026-11-26, P11 plan §7.1).
func TestWorkspacePush_DefaultsAreTheRFCs(t *testing.T) {
	cfg, err := loadConversion(t, "")
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Workspace.Resolved().Push
	if len(p.RefPrefixes) != 1 || p.RefPrefixes[0] != "refs/heads/" || p.MaxBytes != 64<<20 || p.AdoptExisting {
		t.Fatalf("push = %+v", p)
	}
	if p.AllowsTags() {
		t.Fatal("tags allowed by default")
	}
}

// What a workspace's own branch may be named: a full ref under a heads
// prefix the operator lists; refs/tags/ in the list allows tag pushes and
// names no branch.
func TestWorkspacePush_BranchNamesAndTags(t *testing.T) {
	p := WorkspacePushConfig{RefPrefixes: []string{"refs/heads/loom/", "refs/tags/"}}.Resolved()
	if !p.AllowsTags() {
		t.Fatal("refs/tags/ listed but tags not allowed")
	}
	for name, want := range map[string]bool{
		"loom/fix-1": true, "loom/a/b": true, "feature": false, "loom": false, "": false,
	} {
		if got := p.BranchAllowed(name); got != want {
			t.Errorf("BranchAllowed(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestWorkspacePush_RefusesWhatItCannotHold(t *testing.T) {
	for name, block := range map[string]string{
		"a prefix outside heads and tags": "    ref_prefixes: [refs/notes/]\n",
		"a prefix without its slash":      "    ref_prefixes: [refs/heads]\n",
		"a prefix with ..":                "    ref_prefixes: [refs/heads/../]\n",
		"a prefix listed twice":           "    ref_prefixes: [refs/heads/, refs/heads/]\n",
		"an empty list":                   "    ref_prefixes: []\n",
		"a negative size":                 "    max_bytes: -1\n",
		"a size over 1 GiB":               "    max_bytes: 1073741825\n",
		"an unknown key":                  "    force: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadConversion(t, "workspace:\n  push:\n"+block)
			if err == nil || !strings.Contains(err.Error(), "workspace.push") {
				t.Fatalf("err = %v, want a refusal naming workspace.push", err)
			}
		})
	}
	cfg, err := loadConversion(t, "workspace:\n  push:\n    ref_prefixes: [refs/heads/loom/, refs/tags/]\n    max_bytes: 1048576\n    adopt_existing: true\n")
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.Workspace.Resolved().Push; !p.AdoptExisting || p.MaxBytes != 1<<20 || len(p.RefPrefixes) != 2 {
		t.Fatalf("push = %+v", p)
	}
}
