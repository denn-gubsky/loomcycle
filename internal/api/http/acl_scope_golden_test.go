package http

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// RFC EZ P6 step 1: the frozen answers. Before the scope ceiling moved from
// the hand-kept HTTP switches (requiredScopeFor, runPlaneRoute, the
// HTTPSurface switch) into the one route table, every answer they gave was
// recorded in testdata/acl_http_surface_golden.txt. The table must give the
// same answer for every recorded request, so no route's scope, run-plane
// membership or ACL question moved with it.
//
// A deliberate change to a route's gate regenerates the file with
//
//	go test ./internal/api/http -run TestACLScopeGolden -update-acl-golden
//
// and the diff is the review: every line that moves is a gate that changed.

var updateACLGolden = flag.Bool("update-acl-golden", false, "rewrite testdata/acl_http_surface_golden.txt from the current gates")

const aclGoldenFile = "testdata/acl_http_surface_golden.txt"

var aclGoldenMethods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"}

// aclGoldenPaths is the request sample: every pattern the mux registers, with
// its wildcards filled one and two segments deep; every path literal the gates
// and the surface table name, bare and with one to three segments after it;
// and the run, session, agent and user paths with every suffix a gate tests
// for. It is derived from the sources, so a route added later joins it.
func aclGoldenPaths(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" {
			p = "/"
		}
		seen[p] = true
	}
	read := func(name string) string {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	server := read("server.go")
	wild := regexp.MustCompile(`\{[^}]+\}`)
	for _, m := range muxPatternRe.FindAllStringSubmatch(server, -1) {
		pattern := m[2]
		add(wild.ReplaceAllString(pattern, "x"))
		add(strings.ReplaceAll(wild.ReplaceAllString(pattern, "x"), "x", "x/y"))
		if strings.HasSuffix(pattern, "/") {
			add(pattern + "x")
			add(pattern + "x/y")
		}
	}
	lit := regexp.MustCompile(`"(/[A-Za-z0-9_.:/{}\-]*)"`)
	var suffixes []string
	for _, src := range []string{read("auth_principal.go"), read("../../authz/surfaces.go"), read("../memberpath/memberpath.go"), read("../memberpath/twins.go"), server} {
		for _, m := range lit.FindAllStringSubmatch(src, -1) {
			p := wild.ReplaceAllString(m[1], "x")
			if strings.Count(p, "/") == 1 && !strings.HasPrefix(p, "/v1") {
				suffixes = append(suffixes, p)
			}
			add(p)
			base := strings.TrimSuffix(p, "/")
			for _, tail := range []string{"/x", "/x/y", "/x/y/z"} {
				add(base + tail)
			}
		}
	}
	suffixes = append(suffixes, "", "/tree", "/start", "/cancel", "/stream", "/input", "/inbox", "/file", "/interrupts/x/resolve")
	for _, base := range []string{"/v1/runs/", "/v1/sessions/", "/v1/agents/", "/v1/users/"} {
		for _, mid := range []string{"x", "x/y", "tree"} {
			for _, s := range suffixes {
				add(base + mid + s)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// aclGoldenLine is one request's answers: the scope the route requires, its
// run-plane membership, and the ACL question (or "-" for an ungoverned route).
func aclGoldenLine(method, path string) string {
	q := "-"
	if s, ok := authz.HTTPSurface(method, path); ok {
		q = s.Action + " " + s.Type + ":" + s.Name
	}
	scope := requiredScopeFor(method, path)
	if scope == "" {
		scope = "any"
	}
	return fmt.Sprintf("%s %s\t%s\t%t\t%s", method, path, scope, runPlaneRoute(method, path), q)
}

func TestACLScopeGolden_EveryRequestKeepsItsGate(t *testing.T) {
	paths := aclGoldenPaths(t)
	if len(paths) < 900 {
		t.Fatalf("only %d sample paths; the sample no longer reads the sources", len(paths))
	}
	var got []string
	for _, p := range paths {
		for _, m := range aclGoldenMethods {
			got = append(got, aclGoldenLine(m, p))
		}
	}
	if *updateACLGolden {
		if err := os.MkdirAll(filepath.Dir(aclGoldenFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(aclGoldenFile, []byte(strings.Join(got, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(aclGoldenFile)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		key, _, _ := strings.Cut(line, "\t")
		want[key] = line
	}
	checked, bad := 0, 0
	for _, line := range got {
		key, _, _ := strings.Cut(line, "\t")
		w, ok := want[key]
		if !ok {
			continue // a request the sample did not hold when the file was written
		}
		checked++
		if w != line {
			bad++
			if bad <= 50 {
				t.Errorf("gate moved:\n  was %s\n  now %s", w, line)
			}
		}
	}
	if bad > 50 {
		t.Errorf("... and %d more", bad-50)
	}
	if checked < len(want)*9/10 {
		t.Errorf("only %d of the %d recorded requests are still sampled; regenerate the file deliberately", checked, len(want))
	}
}
