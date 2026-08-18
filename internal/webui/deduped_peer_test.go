package webui

// deduped_peer_test.go — the web app's dependency on a DEDUPED package must satisfy
// the peer range of every package consumed from source.
//
// WHY THIS TEST EXISTS, from a bug that reached a release. @loomcycle/memory-view is
// compiled from SOURCE through a Vite alias, and web/vite.config.ts lists
// @loomcycle/client in `resolve.dedupe`. Dedupe makes web/node_modules' copy the one
// that ends up in the BUNDLE — so web's version, not the package's own, is what runs
// in the browser.
//
// Nothing caught the mismatch, and the two obvious candidates each cannot:
//
//   - `tsc --noEmit` (which web's build DOES run) resolves @loomcycle/client from the
//     memory-view SOURCE file, i.e. packages/memory-view/node_modules — the correct,
//     newer copy. It typechecks clean against a version the bundle will not contain.
//   - `vite build` strips types with esbuild and performs no such check.
//
// So web pinned ^1.49.0 while memory-view declared peer ^1.55.0, the build was green,
// and the console threw `e.backfillEmbeddings is not a function` at an operator —
// two methods that landed in the client at 1.55.0 were simply absent from the bundle.
//
// A Go test rather than a lint rule because `go test ./...` is the gate everybody
// already runs, including CI, and this needs no node_modules to be installed.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// sourceConsumedPackages are the packages web compiles from source via a Vite alias.
// Extend this when another one is added — the same trap applies to each.
var sourceConsumedPackages = []string{
	"../../packages/memory-view/package.json",
	"../../packages/library/package.json",
	"../../packages/explorer/package.json",
}

type pkgJSON struct {
	Name             string            `json:"name"`
	Version          string            `json:"version"`
	Dependencies     map[string]string `json:"dependencies"`
	PeerDependencies map[string]string `json:"peerDependencies"`
}

func readPkg(t *testing.T, path string) pkgJSON {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("%s not present: %v", path, err)
	}
	var p pkgJSON
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return p
}

// minVersion extracts the lower bound of a simple npm range ("^1.56.0", "~1.2.3",
// ">=1.2.3", "1.2.3"). Ranges this does not understand return ok=false and are
// SKIPPED rather than guessed at — a wrong comparison would be worse than none.
func minVersion(rng string) (maj, min, patch int, ok bool) {
	s := strings.TrimSpace(rng)
	// A disjunction ("^18.0.0 || ^19.0.0") is satisfied by any branch, so its lower
	// bound is the LOWEST branch. Taking that keeps react/react-dom guarded instead of
	// skipped. It means the check only catches web being too OLD, never too new — which
	// is the direction that removes methods from the bundle.
	if i := strings.Index(s, "||"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	for _, p := range []string{"^", "~", ">=", ">", "=", "v"} {
		s = strings.TrimPrefix(s, p)
	}
	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	nums := make([]int, 3)
	for i, p := range parts {
		// Drop any prerelease/build suffix.
		if idx := strings.IndexAny(p, "-+"); idx >= 0 {
			p = p[:idx]
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, 0, 0, false
		}
		nums[i] = n
	}
	return nums[0], nums[1], nums[2], true
}

func TestWebDedupedDeps_SatisfySourcePackagePeers(t *testing.T) {
	web := readPkg(t, "../../web/package.json")
	deduped := dedupedFromViteConfig(t)
	if len(deduped) == 0 {
		t.Fatal("parsed no dedupe list from web/vite.config.ts — the guard would pass vacuously")
	}

	for _, path := range sourceConsumedPackages {
		pkg := readPkg(t, path)
		for dep, peerRange := range pkg.PeerDependencies {
			if !deduped[dep] {
				// Not deduped: the package's own copy is bundled, so web's version
				// does not govern. Nothing to check.
				continue
			}
			webRange, present := web.Dependencies[dep]
			if !present {
				t.Errorf("%s peers on deduped %q but web/package.json does not depend on it — "+
					"dedupe would resolve it from web/node_modules, which has no copy",
					pkg.Name, dep)
				continue
			}
			pMaj, pMin, pPatch, okP := minVersion(peerRange)
			wMaj, wMin, wPatch, okW := minVersion(webRange)
			if !okP || !okW {
				t.Logf("skipping %s %s: unparsed range (peer %q, web %q)", pkg.Name, dep, peerRange, webRange)
				continue
			}
			peer := [3]int{pMaj, pMin, pPatch}
			have := [3]int{wMaj, wMin, wPatch}
			if less(have, peer) {
				t.Errorf("web/package.json has %s %q but %s peers on %q.\n"+
					"  %s is DEDUPED in web/vite.config.ts, so web's copy is what lands in the\n"+
					"  bundle — the older methods are simply absent at runtime, and neither\n"+
					"  `tsc --noEmit` (resolves the package's own newer copy) nor `vite build`\n"+
					"  (no typecheck) will tell you. Bump web to at least %q.",
					dep, webRange, pkg.Name, peerRange, dep, peerRange)
			}
		}
	}
}

func less(a, b [3]int) bool {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// dedupedFromViteConfig reads the resolve.dedupe list out of web/vite.config.ts.
// Parsed from the real config rather than duplicated here, so removing a name from
// the dedupe list correctly relaxes this test instead of leaving it asserting a rule
// the build no longer follows.
func dedupedFromViteConfig(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "web", "vite.config.ts"))
	if err != nil {
		t.Skipf("web/vite.config.ts unreadable: %v", err)
	}
	src := string(b)
	i := strings.Index(src, "dedupe:")
	if i < 0 {
		return nil
	}
	open := strings.Index(src[i:], "[")
	close := strings.Index(src[i:], "]")
	if open < 0 || close < 0 || close < open {
		return nil
	}
	out := map[string]bool{}
	for _, raw := range strings.Split(src[i+open+1:i+close], ",") {
		s := strings.TrimSpace(strings.Trim(strings.TrimSpace(raw), `"'`))
		if s != "" && !strings.HasPrefix(s, "//") {
			out[s] = true
		}
	}
	return out
}

// TestWebDedupedDeps_GuardActuallyCompares proves the comparison is not vacuous —
// an older web range against a newer peer must be reported.
func TestWebDedupedDeps_GuardActuallyCompares(t *testing.T) {
	cases := []struct {
		web, peer string
		wantLess  bool
	}{
		{"^1.49.0", "^1.55.0", true}, // the bug that shipped
		{"^1.56.0", "^1.55.0", false},
		{"^1.55.0", "^1.55.0", false},
		{"^1.55.0", "^1.55.1", true},
		{"^2.0.0", "^1.55.0", false},
	}
	for _, c := range cases {
		wM, wm, wp, ok1 := minVersion(c.web)
		pM, pm, pp, ok2 := minVersion(c.peer)
		if !ok1 || !ok2 {
			t.Fatalf("failed to parse %q / %q", c.web, c.peer)
		}
		got := less([3]int{wM, wm, wp}, [3]int{pM, pm, pp})
		if got != c.wantLess {
			t.Errorf("web %s vs peer %s: less=%v, want %v", c.web, c.peer, got, c.wantLess)
		}
	}
	if _, _, _, ok := minVersion("workspace:*"); ok {
		t.Error("an unparseable range must report ok=false rather than a guessed number")
	}
	// A disjunction resolves to its lowest branch, so react's real peer is covered
	// rather than skipped.
	if maj, _, _, ok := minVersion("^18.0.0 || ^19.0.0"); !ok || maj != 18 {
		t.Errorf("disjunction should resolve to its lowest branch, got maj=%d ok=%v", maj, ok)
	}
}
