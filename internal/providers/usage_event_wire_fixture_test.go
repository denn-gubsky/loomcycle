package providers

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// webUsageEventsFixture is read by web/src/lib/runMetrics.test.ts.
const webUsageEventsFixture = "../../web/src/lib/__fixtures__/usage-events.json"

// The Web UI's run metrics (context gauge, tokens per second) read fields off
// the usage frame that only this struct defines — the cache counters, the
// window, the serving provider and the per-call timing. Produced from the real
// struct, as the tool-event fixture is, so the TS test cannot share a wrong
// belief about a field name with the code it checks. Regenerate with
// LOOMCYCLE_UPDATE_FIXTURES=1 when the Usage shape changes.
//
// Two calls: a cloud call with prompt caching and no phase timing, then a
// local call that reports its decode phase and waited in a queue.
func TestUsageEventWireShape_MatchesTheWebFixture(t *testing.T) {
	frames := []Event{
		{Type: EventUsage, Usage: &Usage{
			InputTokens: 1200, OutputTokens: 150, CacheReadTokens: 800, CacheCreationTokens: 300,
			Model: "cloud-model", Provider: "cloud", MaxContextTokens: 200000,
			Timing: &CallTiming{DurationMs: 3000, TTFTMs: 900},
		}},
		{Type: EventUsage, Usage: &Usage{
			InputTokens: 2500, OutputTokens: 400,
			Model: "local-model", Provider: "local", MaxContextTokens: 32768,
			Timing: &CallTiming{DurationMs: 9000, TTFTMs: 2000, PrefillMs: 1500, DecodeMs: 5000, QueueMs: 2500},
		}},
	}
	got, err := json.MarshalIndent(frames, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if os.Getenv("LOOMCYCLE_UPDATE_FIXTURES") == "1" {
		if err := os.WriteFile(webUsageEventsFixture, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(webUsageEventsFixture)
	if err != nil {
		t.Skipf("web fixture not readable from here: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the usage wire shape changed; the Web UI's run metrics are tested against "+
			"the old one. Regenerate with LOOMCYCLE_UPDATE_FIXTURES=1 and re-run the web "+
			"tests.\n got: %s\nwant: %s", got, want)
	}
}
