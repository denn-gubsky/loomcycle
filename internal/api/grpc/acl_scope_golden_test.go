package grpc

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// RFC EZ P6 step 1: the frozen answers for the gRPC gates. Each RPC's
// required scope, run-plane membership and ACL question were recorded in
// testdata/acl_grpc_surface_golden.txt before the scope ceiling moved into
// the one surface table, and the table must give the same answers. A
// deliberate change regenerates the file with -update-acl-golden; its diff is
// the review.

var updateACLGolden = flag.Bool("update-acl-golden", false, "rewrite testdata/acl_grpc_surface_golden.txt from the current gates")

const aclGRPCGoldenFile = "testdata/acl_grpc_surface_golden.txt"

func aclGRPCGoldenLines() []string {
	names := append(serviceRPCs(), "NoSuchRPC")
	sort.Strings(names)
	var out []string
	for _, name := range names {
		q := "-"
		if s, ok := authz.GRPCSurface(name); ok {
			q = s.Action + " " + s.Type + ":" + s.Name
		}
		scope := requiredScopeForRPC(grpcMethodPrefix + name)
		if scope == "" {
			scope = "any"
		}
		out = append(out, fmt.Sprintf("%s\t%s\t%t\t%s", name, scope, grpcRunPlaneRPCs[name], q))
	}
	return out
}

func TestACLScopeGolden_EveryRPCKeepsItsGate(t *testing.T) {
	got := aclGRPCGoldenLines()
	if *updateACLGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(aclGRPCGoldenFile, []byte(strings.Join(got, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(aclGRPCGoldenFile)
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

// RFC EZ P6: every RPC the service registers has a row in the route table,
// and the table holds no row for an RPC the service does not register. An
// RPC without a row would be held to substrate:admin and asked nothing.
func TestACLRoutes_EveryRPCHasOneRow(t *testing.T) {
	served := map[string]bool{}
	for _, name := range serviceRPCs() {
		served[name] = true
		if _, ok := authz.GRPCRoute(name); !ok {
			t.Errorf("%s is served but has no row in authz's RPC table", name)
		}
	}
	for name := range authz.GRPCRoutes() {
		if !served[name] {
			t.Errorf("row %s names an RPC the service does not register", name)
		}
	}
}
