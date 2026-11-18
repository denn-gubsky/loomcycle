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
// HTTPSurface switch) into the one route table (authz.HTTPRoute), every answer
// they gave a request some route serves was recorded in
// testdata/acl_http_surface_golden.txt, HEAD taking GET's answers as the mux
// serves it with the GET handler. The table must give the same answer for
// every recorded request, so no route's scope, run-plane membership or ACL
// question moved with it.
//
// A deliberate change to a route's gate regenerates the file with
//
//	go test ./internal/api/http -run TestACLScopeGolden -update-acl-golden
//
// and the diff is the review: every line that moves is a gate that changed.

var updateACLGolden = flag.Bool("update-acl-golden", false, "rewrite testdata/acl_http_surface_golden.txt from the current gates")

const aclGoldenFile = "testdata/acl_http_surface_golden.txt"

var aclGoldenMethods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}

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
	for _, pattern := range aclRegisteredPatterns(t) {
		if _, p, ok := strings.Cut(pattern, " "); ok {
			pattern = p
		}
		add(wild.ReplaceAllString(pattern, "x"))
		add(wild.ReplaceAllString(pattern, "x/y"))
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

// aclRegisteredPatterns is every literal pattern a route is registered under:
// the HTTP server's own files and the webhook and A2A packages it mounts.
func aclRegisteredPatterns(t *testing.T) []string {
	t.Helper()
	re := regexp.MustCompile(`\.Handle(?:Func)?\("([^"]+)"`)
	var files []string
	for _, g := range []string{"*.go", "../webhook/*.go", "../a2a/*.go"} {
		m, err := filepath.Glob(g)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			if strings.Contains(m[1], "/") {
				seen[m[1]] = true
			}
		}
	}
	// Registered from a computed pattern, which the regexp cannot read: the
	// A2A extended card, behind the HTTP gates (a2a.Server.Mount's adminAuth).
	seen["GET /.well-known/agent-card.json/extended"] = true
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// aclGoldenLine is one request's answers: the scope its route requires, its
// run-plane membership, and its ACL question ("-" for an ungoverned route).
// False for a request no route serves: the mux answers it 404 or 405 before
// any gate runs.
func aclGoldenLine(method, path string) (string, bool) {
	rt, ok := authz.HTTPRoute(method, path)
	if !ok {
		return "", false
	}
	q := "-"
	if s, ok := rt.Question(method, path); ok {
		q = s.Action + " " + s.Type + ":" + s.Name
	}
	scope := rt.Scope
	if scope == "" {
		scope = "any"
	}
	return fmt.Sprintf("%s %s\t%s\t%t\t%s", method, path, scope, rt.RunPlane, q), true
}

func TestACLScopeGolden_EveryRequestKeepsItsGate(t *testing.T) {
	paths := aclGoldenPaths(t)
	if len(paths) < 800 {
		t.Fatalf("only %d sample paths; the sample no longer reads the sources", len(paths))
	}
	var got []string
	for _, p := range paths {
		for _, m := range aclGoldenMethods {
			if line, ok := aclGoldenLine(m, p); ok {
				got = append(got, line)
			}
		}
	}
	if len(got) < 500 {
		t.Fatalf("only %d sampled requests reach a route; the sample no longer covers the routes", len(got))
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
	// Every recorded request is asked again, so the file, not the sample,
	// decides what is checked; a sampled request the file does not hold is a
	// route nobody recorded.
	recorded := map[string]bool{}
	for _, w := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		key, _, _ := strings.Cut(w, "\t")
		recorded[key] = true
		method, path, _ := strings.Cut(key, " ")
		now, ok := aclGoldenLine(method, path)
		switch {
		case !ok:
			t.Errorf("recorded but no longer served: %s", w)
		case now != w:
			t.Errorf("gate moved:\n  was %s\n  now %s", w, now)
		}
	}
	for _, line := range got {
		if key, _, _ := strings.Cut(line, "\t"); !recorded[key] {
			t.Errorf("not recorded: %s (a new route? regenerate the file and review the diff)", line)
		}
	}
}
