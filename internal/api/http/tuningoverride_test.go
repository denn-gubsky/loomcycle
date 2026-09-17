package http

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/steer"
)

func intPtrT(v int) *int    { return &v }
func boolPtrT(b bool) *bool { return &b }

// Every tuning field has a MEANINGFUL ZERO — 0 retries, inject nothing, omit
// the guide — which is why they are pointers. A plain value could raise them and
// never turn them off, and turning one off for a single run is the commonest
// reason to reach for them.
func TestTuningOverride_ZeroValuesAreExpressible(t *testing.T) {
	def := config.AgentDef{
		RetryAttempts:         intPtrT(5),
		MemoryInjectMaxTokens: 4000,
		MemoryIndexMaxBytes:   99999,
		InjectToolGuide:       true,
	}

	got := applyTuningOverride(def, &tuningOverride{
		RetryAttempts:         intPtrT(0),
		MemoryInjectMaxTokens: intPtrT(0),
		MemoryIndexMaxBytes:   intPtrT(0),
		InjectToolGuide:       boolPtrT(false),
	})

	if got.RetryAttempts == nil || *got.RetryAttempts != 0 {
		t.Errorf("retry_attempts = %v, want 0 — 'do not retry' must be expressible", got.RetryAttempts)
	}
	if got.MemoryInjectMaxTokens != 0 {
		t.Errorf("memory_inject_max_tokens = %d, want 0 — 'inject nothing' must be expressible", got.MemoryInjectMaxTokens)
	}
	if got.MemoryIndexMaxBytes != 0 {
		t.Errorf("memory_index_max_bytes = %d, want 0", got.MemoryIndexMaxBytes)
	}
	if got.InjectToolGuide {
		t.Error("inject_tool_guide = true, want it turned off")
	}
}

// An absent field leaves the definition's own value alone — the distinction a
// pointer exists to carry.
func TestTuningOverride_AbsentLeavesTheDefinitionsValue(t *testing.T) {
	def := config.AgentDef{MemoryInjectMaxTokens: 4000, InjectToolGuide: true}

	got := applyTuningOverride(def, &tuningOverride{RetryAttempts: intPtrT(2)})
	if got.MemoryInjectMaxTokens != 4000 || !got.InjectToolGuide {
		t.Errorf("an absent field changed the definition: %+v", got)
	}
}

func TestTuningOverride_LeavesTheDefinitionUntouched(t *testing.T) {
	def := config.AgentDef{MemoryInjectMaxTokens: 4000, InjectToolGuide: true}
	_ = applyTuningOverride(def, &tuningOverride{
		MemoryInjectMaxTokens: intPtrT(1), InjectToolGuide: boolPtrT(false),
	})
	if def.MemoryInjectMaxTokens != 4000 || !def.InjectToolGuide {
		t.Errorf("the caller's definition was mutated: %+v", def)
	}
}

// THE CROSSING for tuning, and the one that would have caught the resume gap.
//
// retry_attempts reaches the loop as MaxSameProviderRetries via
// retryAttemptsForAgent(agentDef, ...) — a consumer that reads the DEFINITION.
// Before the single-effective-def collapse, resume passed the un-overridden name
// to exactly that call, so a restored tuning override was recorded, restored,
// and then ignored. Asserted across a resume for that reason.
func TestTuningOverride_SurvivesResumeAndReachesItsConsumer(t *testing.T) {
	srv, ts, prov, st := routedServer(t)
	srv.SetSteerRegistry(steer.NewRegistry(0))

	code, body := postRoutedRun(t, ts,
		`{"agent":"router","retry_attempts":7,"memory_inject_max_tokens":123,`+routedSegments+`}`)
	if code != 200 {
		t.Fatalf("run: %d %s", code, body)
	}
	prov.waitForRequests(t, 1)

	run := onlyRun(t, st, extractSessionID(body))
	rec, ok := decodeRunConfig(run.RunConfig)
	if !ok || rec.Tuning == nil {
		t.Fatalf("the run row records no tuning override (%+v)", rec.Tuning)
	}
	if rec.Tuning.RetryAttempts == nil || *rec.Tuning.RetryAttempts != 7 {
		t.Errorf("persisted retry_attempts = %v, want 7", rec.Tuning.RetryAttempts)
	}
	if rec.Tuning.MemoryInjectMaxTokens == nil || *rec.Tuning.MemoryInjectMaxTokens != 123 {
		t.Errorf("persisted memory_inject_max_tokens = %v, want 123", rec.Tuning.MemoryInjectMaxTokens)
	}

	// And the restored record reaches the definition the resumed run runs
	// under — asserted through the same accessor the loop's retry budget uses.
	restored, _ := decodeRunConfig(mustGetRun(t, st, run.ID).RunConfig)
	eff, err := srv.effectiveDef(context.Background(), srv.cfg().Agents["router"], runOverrides{
		Routing: restored.Routing, Resources: restored.Resources, Tuning: restored.Tuning,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.retryAttemptsForAgent(eff, ""); got != 7 {
		t.Errorf("a resumed run's retry budget = %d, want 7 — the restored override did not "+
			"reach the consumer that reads the definition", got)
	}
}

// The tuning fields are accepted on the wire at all. Cheap, but the §4 tuning
// row had no phase in the RFC and was the one part of the overridable table
// that was never wired, so "does the endpoint take them" is worth pinning.
func TestTuningOverride_AcceptedOnTheWire(t *testing.T) {
	_, ts, prov, _ := routedServer(t)

	code, body := postRoutedRun(t, ts,
		`{"agent":"router","inject_tool_guide":false,"memory_index_max_bytes":16,`+routedSegments+`}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", code, strings.TrimSpace(body))
	}
	prov.waitForRequests(t, 1)
}
