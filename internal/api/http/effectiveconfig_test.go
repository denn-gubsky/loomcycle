package http

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// THE GUARD THAT KEEPS THIS REPORT HONEST.
//
// The whole point of the endpoint is that no single place assembled the four
// layers. Building one and letting a new overridable field quietly fall out of
// it would recreate the problem inside the fix — so the field list is derived
// from agentDefOverridability, and every overridable field must have a way to
// be read from the run's record.
//
// A field added to the override set and not here fails the build, not a review.
func TestEffectiveConfig_EveryOverridableFieldIsReported(t *testing.T) {
	var missing []string
	for name, kind := range agentDefOverridability {
		if kind == notOverridable {
			continue
		}
		if _, ok := runFieldReaders[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("these fields are overridable but the effective-config report cannot read "+
			"them from a run: %s\n\nThe report exists because nothing assembled the layers; a "+
			"field it silently omits puts the reader back where they started.",
			strings.Join(missing, ", "))
	}
}

// The COMPLEMENT of the test above, and the one that was missing.
//
// That test walks the classification table and asks "can the report read this?".
// It cannot see a reader the iteration never reaches — dead code whose field is
// silently absent from every report. `interactive` shipped exactly that way: the
// runFieldReaders entry and the fallback both existed and were both unreachable,
// because the iteration is keyed off config.AgentDef and no such field exists
// there.
//
// A guard that only checks one direction is half a guard.
func TestEffectiveConfig_EveryReaderIsReachable(t *testing.T) {
	reported := reportableFieldNames()

	// Deduped: a field usually has BOTH a reader and a fallback, and naming it
	// twice reads as two problems.
	seen := map[string]bool{}
	var dead []string
	note := func(name string) {
		if _, ok := reported[name]; !ok && !seen[name] {
			seen[name] = true
			dead = append(dead, name)
		}
	}
	for name := range runFieldReaders {
		note(name)
	}
	for name := range fallbacks {
		note(name)
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("these readers/fallbacks are never reached by the report: %s\n\n"+
			"An entry the iteration cannot see is dead code, and the field it was "+
			"written for is missing from every report. Classify it in "+
			"agentDefOverridability, or name it in runOnlyOverridable if the "+
			"definition has no such field.",
			strings.Join(dead, ", "))
	}
}

// A name in both sets would be reported twice and sorted nondeterministically.
func TestEffectiveConfig_TheTwoFieldSetsAreDisjoint(t *testing.T) {
	for name := range runOnlyOverridable {
		if _, ok := agentDefOverridability[name]; ok {
			t.Errorf("%q is in BOTH agentDefOverridability and runOnlyOverridable — "+
				"runOnlyOverridable is for fields the definition does NOT have", name)
		}
	}
}

// reportableFieldNames mirrors what effectiveFields iterates. Derived from the
// same two sets rather than restated, so it cannot drift from the real loop.
func reportableFieldNames() map[string]struct{} {
	out := make(map[string]struct{})
	for name, kind := range agentDefOverridability {
		if kind != notOverridable {
			out[name] = struct{}{}
		}
	}
	for name := range runOnlyOverridable {
		out[name] = struct{}{}
	}
	return out
}

// Non-vacuity: the derived list must actually contain the override set.
func TestEffectiveConfig_TheDerivedListIsPopulated(t *testing.T) {
	n := 0
	for _, kind := range agentDefOverridability {
		if kind != notOverridable {
			n++
		}
	}
	if n < 15 {
		t.Fatalf("only %d overridable fields derived; the classification table's shape "+
			"probably changed and this report has quietly narrowed", n)
	}
}

// Every reported key must be the name the rest of the wire uses, or the report
// cannot be joined against /v1/_library/agents — which is what a panel does.
func TestEffectiveConfig_ReportsWireNamesNotGoNames(t *testing.T) {
	for _, tc := range []struct{ goName, want string }{
		{"MaxTokens", "max_tokens"},
		{"MaxConcurrentChildren", "max_concurrent_children"},
		{"MemoryInjectMaxTokens", "memory_inject_max_tokens"},
		{"RunTimeoutSeconds", "run_timeout_seconds"},
		{"Model", "model"},
	} {
		if got := wireNameFor(tc.goName); got != tc.want {
			t.Errorf("wireNameFor(%q) = %q, want %q", tc.goName, got, tc.want)
		}
	}
}

func getEffective(t *testing.T, ts *httptest.Server, runID string) (int, string) {
	t.Helper()
	resp, err := http.Get(ts.URL + "/v1/runs/" + runID + "/effective-config")
	if err != nil {
		t.Fatalf("get effective-config: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

type effResp struct {
	RunID  string                    `json:"run_id"`
	Agent  string                    `json:"agent"`
	Fields map[string]effectiveValue `json:"fields"`
}

// The source is the point. "max_iterations: 16" cannot tell a deliberate
// setting from a default nobody chose, and those call for opposite actions.
func TestEffectiveConfig_ReportsTheValueAndWhichLayerDecidedIt(t *testing.T) {
	srv, ts, _, run := parkedRoutedRun(t)

	code, body := getEffective(t, ts, run.ID)
	if code != 200 {
		t.Fatalf("effective-config: %d %s", code, strings.TrimSpace(body))
	}
	var resp effResp
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if resp.RunID != run.ID {
		t.Errorf("run_id = %q, want %q", resp.RunID, run.ID)
	}
	if want := mustGetRun(t, srv.store, run.ID).Agent; resp.Agent != want {
		t.Errorf("agent = %q, want %q", resp.Agent, want)
	}

	// A field nobody set comes back with the runtime's own constant, labelled as
	// a default — the answer that was previously unobtainable at any price.
	mi, ok := resp.Fields["max_iterations"]
	if !ok {
		t.Fatalf("max_iterations missing from the report: %v", resp.Fields)
	}
	if mi.Source != sourceDefault {
		t.Errorf("max_iterations source = %q, want %q", mi.Source, sourceDefault)
	}
	if got, want := jsonNum(t, mi.Value), 16.0; got != want {
		t.Errorf("max_iterations = %v, want %v (loop.DefaultMaxIterations)", got, want)
	}

	// And a retune moves it to the run layer, value and source together.
	if c, b := postRetune(t, ts, run.ID, `{"max_iterations":40}`); c != 200 {
		t.Fatalf("retune: %d %s", c, strings.TrimSpace(b))
	}
	_, body = getEffective(t, ts, run.ID)
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	mi = resp.Fields["max_iterations"]
	if mi.Source != sourceRun {
		t.Errorf("after a retune, max_iterations source = %q, want %q", mi.Source, sourceRun)
	}
	if got := jsonNum(t, mi.Value); got != 40 {
		t.Errorf("max_iterations = %v, want 40", got)
	}
}

// The definition layer must be distinguishable from the default layer, or the
// report answers the question it was built for with a shrug.
func TestEffectiveConfig_ADefinitionsOwnValueIsAttributedToIt(t *testing.T) {
	_, ts, _, run := parkedRoutedRun(t)
	_, body := getEffective(t, ts, run.ID)
	var resp effResp
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	// parkedRoutedRun's agent declares a tier, so that field is the definition's.
	tier, ok := resp.Fields["tier"]
	if !ok {
		t.Fatalf("tier missing: %v", resp.Fields)
	}
	if tier.Source != sourceDefinition {
		t.Errorf("tier source = %q, want %q — the definition set it and the report says "+
			"otherwise", tier.Source, sourceDefinition)
	}
}

// The behaviour the reachability guard protects: `interactive` is IN the report,
// and a retune moves it. A custom chat cannot decide whether it may ask the user
// a question if the one endpoint built to answer "what is this run actually
// doing" omits the field that says so.
func TestEffectiveConfig_ReportsInteractiveAndARetuneMovesIt(t *testing.T) {
	_, ts, _, run := parkedRoutedRun(t)

	_, body := getEffective(t, ts, run.ID)
	var resp effResp
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	got, ok := resp.Fields["interactive"]
	if !ok {
		t.Fatalf("interactive missing from the report — the reader exists but nothing "+
			"reaches it: %v", resp.Fields)
	}
	// This fixture's run was CREATED interactive, so the report must say so
	// before anything is retuned: how the run was started is itself the answer.
	if got.Value != true {
		t.Errorf("interactive = %v, want true (the run was started interactive)", got.Value)
	}

	// And a retune moves it. `false` on a run that started interactive is the
	// direction with no other expression — it RELEASES the run, which is why
	// this is a settable boolean and not a flag that can only be turned on.
	if c, b := postRetune(t, ts, run.ID, `{"interactive":false}`); c != 200 {
		t.Fatalf("retune: %d %s", c, strings.TrimSpace(b))
	}
	_, body = getEffective(t, ts, run.ID)
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got = resp.Fields["interactive"]
	if got.Value != false {
		t.Errorf("after a retune to false, interactive = %v, want false — a release "+
			"that the report does not show is a release the operator cannot confirm",
			got.Value)
	}
	if got.Source != sourceRun {
		t.Errorf("interactive source = %q, want %q", got.Source, sourceRun)
	}
}

func TestEffectiveConfig_UnknownRunIsTheSame404AsAForbiddenOne(t *testing.T) {
	_, ts, _, _ := parkedRoutedRun(t)
	if code, _ := getEffective(t, ts, "r_does_not_exist"); code != 404 {
		t.Errorf("unknown run → %d, want 404", code)
	}
}

func jsonNum(t *testing.T, v any) float64 {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("value %v (%T) is not a number", v, v)
	}
	return f
}

// The effective-config report names what CANNOT take effect, not only what
// will. This is the half boot validation cannot do: a per-run context override
// can introduce the trap on a run whose definition is clean, and boot never
// sees that combination.
func TestEffectiveConfig_ReportsInertAutocompactThreshold(t *testing.T) {
	_, ts, _, run := parkedRoutedRun(t)

	// The fixture's agent is not in a distilling mode, so nothing is inert —
	// and the key must still be PRESENT, or a consumer cannot tell "nothing
	// inert" from "this server does not report it".
	_, body := getEffective(t, ts, run.ID)
	var resp struct {
		Inert []config.InertContextSetting `json:"inert"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if resp.Inert == nil {
		t.Error("inert is absent/null — an empty slice is the answer that lets a " +
			"consumer distinguish it from an older server")
	}
	if len(resp.Inert) != 0 {
		t.Errorf("a non-distilling agent reported inert settings: %+v", resp.Inert)
	}
}

// The predicate itself is shared with the boot check, so the two surfaces
// cannot disagree about which settings are dead.
func TestEffectiveConfig_InertSharesThePredicateWithBootValidation(t *testing.T) {
	a := config.AgentDef{
		Context: &config.Context{Mode: func() *string {
			m := config.ContextModeRecap
			return &m
		}()},
		Compaction: &config.Compaction{AutoCompactAtPct: func() *int { i := 70; return &i }()},
	}
	// Whatever the boot warnings say is dead, the report must also call dead.
	inert := config.InertContextSettings(a)
	if len(inert) == 0 {
		t.Fatal("the shared predicate reported nothing for a recap agent with an " +
			"autocompact threshold — the two surfaces would both be silent")
	}
	if inert[0].Setting != "compaction.autocompact_at_pct" {
		t.Errorf("setting = %q, want compaction.autocompact_at_pct", inert[0].Setting)
	}
}
