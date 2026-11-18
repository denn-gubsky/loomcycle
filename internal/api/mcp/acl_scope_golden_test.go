package mcp

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// RFC EZ P6 step 1: the frozen answers for the MCP per-tool gate. Each
// meta-tool's required scope and run-plane membership were recorded in
// testdata/acl_mcp_tool_golden.txt before the scope ceiling moved into the
// one surface table, and the table must give the same answers. A deliberate
// change regenerates the file with -update-acl-golden; its diff is the
// review.

var updateACLGolden = flag.Bool("update-acl-golden", false, "rewrite testdata/acl_mcp_tool_golden.txt from the current gates")

const aclMCPGoldenFile = "testdata/acl_mcp_tool_golden.txt"

func aclMCPGoldenLines() []string {
	names := []string{"no_such_tool"}
	for name := range handlersByName {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []string
	for _, name := range names {
		scope, gated := toolRequiredScope[name]
		if !gated {
			scope = "-"
		} else if scope == "" {
			scope = "any"
		}
		out = append(out, fmt.Sprintf("%s\t%s\t%t", name, scope, runPlaneTools[name]))
	}
	return out
}

func TestACLScopeGolden_EveryToolKeepsItsGate(t *testing.T) {
	got := aclMCPGoldenLines()
	if *updateACLGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(aclMCPGoldenFile, []byte(strings.Join(got, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(aclMCPGoldenFile)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if strings.Join(got, "\n") == strings.Join(want, "\n") {
		return
	}
	wantSet := map[string]bool{}
	for _, l := range want {
		wantSet[l] = true
	}
	gotSet := map[string]bool{}
	for _, l := range got {
		gotSet[l] = true
		if !wantSet[l] {
			t.Errorf("now:  %s", l)
		}
	}
	for _, l := range want {
		if !gotSet[l] {
			t.Errorf("was:  %s", l)
		}
	}
}
