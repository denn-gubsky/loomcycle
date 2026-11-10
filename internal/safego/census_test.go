package safego

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// rawGo is the number of raw `go` statements a file may keep, and why.
type rawGo struct {
	n      int
	reason string
}

// exemptRawGo lists the raw `go` statements that stay raw on purpose.
var exemptRawGo = map[string]rawGo{
	"internal/safego/safego.go": {1, "the helper itself"},

	// Already recover a panic in the goroutine's own frame.
	"internal/scheduler/consolidator.go":            {1, "the consolidation fan-out goroutine recovers and records the failure"},
	"internal/scheduler/scheduler.go":               {1, "fireOne recovers and parks the schedule"},
	"internal/scheduler/tracked.go":                 {1, "runOnceRecovered recovers the run"},
	"internal/api/http/resume.go":                   {1, "the resumed run's goroutine has a panic-safe teardown"},
	"internal/api/http/resident.go":                 {1, "the resident child recovers"},
	"internal/api/http/detached_run.go":             {1, "the detached loop has a panic-safe teardown"},
	"internal/tools/builtin/document_index_text.go": {1, "the reindex pass recovers"},

	// Must not recover: the process ending is the safe outcome.
	"internal/auditchain/store_sink.go": {1, "the durable audit writer: recovered, it would stop draining and lose audit records silently"},
	"internal/pause/manager.go":         {1, "the pause listener closes channels under m.mu with no deferred unlock; recovered, it would leave the pause manager locked"},
	"internal/procgroup/start_linux.go": {1, "the spawner owns a locked OS thread for the process's life; recovered, every later spawn would block on it"},

	// Cannot import this package.
	"internal/providers/anthropic/driver.go":            {1, "nested module internal/providers"},
	"internal/providers/anthropic_oauth_dev/driver.go":  {1, "nested module internal/providers"},
	"internal/providers/anthropic_oauth_dev/oauth.go":   {1, "nested module internal/providers"},
	"internal/providers/anthropic_oauth_dev/refresh.go": {1, "nested module internal/providers"},
	"internal/providers/codejs/provider.go":             {3, "nested module internal/providers"},
	"internal/providers/gemini/driver.go":               {1, "nested module internal/providers"},
	"internal/providers/ollama/driver.go":               {1, "nested module internal/providers"},
	"internal/providers/openai/driver.go":               {1, "nested module internal/providers"},

	// Test support compiled only into tests.
	"internal/auditchain/chaintest/audit.go":             {2, "test support"},
	"internal/store/storetest/auth_lifecycle.go":         {1, "test support"},
	"internal/store/storetest/bench.go":                  {1, "test support"},
	"internal/store/storetest/channel_hooks_contract.go": {1, "test support"},
	"internal/store/storetest/client_defs.go":            {1, "test support"},
	"internal/store/storetest/contract.go":               {9, "test support"},
	"internal/store/storetest/credential_putif.go":       {1, "test support"},
	"internal/store/storetest/device_gate.go":            {1, "test support"},
	"internal/store/storetest/oauth.go":                  {1, "test support"},
	"internal/store/storetest/oidc_gate.go":              {1, "test support"},
	"internal/store/storetest/passkeys_scim.go":          {6, "test support"},
	"internal/store/storetest/password_gate.go":          {3, "test support"},
	"internal/store/storetest/run_config_cas.go":         {1, "test support"},
	"internal/store/storetest/team_webhook_arms.go":      {1, "test support"},
	"internal/store/storetest/webhook_counts.go":         {1, "test support"},
	"internal/store/storetest/webhook_delivery_runs.go":  {4, "test support"},
}

// pendingRawGo is the baseline of raw `go` statements not converted yet: the
// boot's background goroutines in cmd/ and the per-run and fan-out goroutines
// in internal/. It only shrinks — convert a site to Go, or
// add a reasoned entry to exemptRawGo, and lower its count here.
var pendingRawGo = map[string]int{
	// cmd/loomcycle: the boot's background sweepers, subscribers and
	// listeners, converted in a later change.
	"cmd/loomcycle/boot_background.go": 31,
	"cmd/loomcycle/boot_config.go":     1,
	"cmd/loomcycle/boot_listen.go":     7,
	"cmd/loomcycle/boot_metrics.go":    1,
	"cmd/loomcycle/boot_projection.go": 1,
	"cmd/loomcycle/boot_surfaces.go":   2,
	"cmd/loomcycle/boot_triggers.go":   11,
	"cmd/loomcycle/main.go":            1,
	"cmd/loomcycle/openmode.go":        1,

	"internal/api/http/client_tools.go":             1,
	"internal/api/http/connector_impl.go":           3,
	"internal/api/http/llm_gateway_meter.go":        1,
	"internal/api/http/queued_runs.go":              1,
	"internal/api/http/resume.go":                   1,
	"internal/api/http/resume_background.go":        2,
	"internal/api/http/runs_batch.go":               1,
	"internal/api/http/server.go":                   1,
	"internal/api/http/snapshots.go":                1,
	"internal/api/http/teamlocalschedules.go":       1,
	"internal/api/http/teamlocalwebhooks.go":        1,
	"internal/api/http/teamwalk_run.go":             1,
}

// TestCensus_NoNewRawGoStatements fails on a raw `go` statement in non-test
// code under cmd/ and internal/ beyond the lists above: a panic in such a goroutine
// ends the process for every tenant. Start it with safego.Go instead. A file
// with fewer than its allowance fails too, so the baseline is lowered with the
// conversion and never grows back.
func TestCensus_NoNewRawGoStatements(t *testing.T) {
	root := moduleRoot(t)
	got := map[string]int{}
	walk := func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == "testdata" || n == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if _, ok := n.(*ast.GoStmt); ok {
				got[filepath.ToSlash(rel)]++
			}
			return true
		})
		return nil
	}
	for _, dir := range []string{"cmd", "internal"} {
		if err := filepath.WalkDir(filepath.Join(root, dir), walk); err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	allowed := map[string]int{}
	for f, e := range exemptRawGo {
		allowed[f] += e.n
	}
	for f, n := range pendingRawGo {
		allowed[f] += n
	}
	files := map[string]bool{}
	for f := range got {
		files[f] = true
	}
	for f := range allowed {
		files[f] = true
	}
	names := make([]string, 0, len(files))
	for f := range files {
		names = append(names, f)
	}
	sort.Strings(names)
	for _, f := range names {
		switch g, a := got[f], allowed[f]; {
		case g > a:
			t.Errorf("%s: %d raw go statement(s), %d allowed — start the goroutine with safego.Go (or exempt it with a reason in this test)", f, g, a)
		case g < a:
			t.Errorf("%s: %d raw go statement(s), %d allowed — lower its entry in this test so the count cannot grow back", f, g, a)
		}
	}
}

// moduleRoot walks up from the package directory to the root go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the package")
		}
		dir = parent
	}
}
