package config

import (
	"strings"
	"testing"
)

func cxMode(m string) *Context { return &Context{Mode: &m} }

func bptr(b bool) *bool { return &b }
func iptr(i int) *int   { return &i }

// The advisory that would have told the operator before any of this happened:
// compaction.autocompact_at_pct was set to 70 on an agent whose mode routes
// around the compaction gate entirely, and nothing said so.
func TestInertContextSettings_AutocompactThresholdIsDeadOutsideAppend(t *testing.T) {
	for _, mode := range []string{ContextModeRecap, ContextModeStateful, ContextModeAuto} {
		a := AgentDef{
			Context:    cxMode(mode),
			Compaction: &Compaction{AutoCompactAtPct: iptr(70)},
		}
		got := InertContextSettings(a)
		if len(got) != 1 || got[0].Setting != "compaction.autocompact_at_pct" {
			t.Fatalf("mode %s: want the autocompact advisory, got %+v", mode, got)
		}
		if !strings.Contains(got[0].Reason, mode) {
			t.Errorf("mode %s: the reason does not name the mode that disables it: %q", mode, got[0].Reason)
		}
	}
}

// ⚠️ auto is inert UNCONDITIONALLY, which is stronger than "it depends how it
// resolves". Both resolutions bypass the compaction gate: recap takes the other
// branch, and stateful returns before the gate exists. So this needs no
// knowledge of the provider or of whether the run is interactive.
func TestInertContextSettings_AutoNeedsNoResolutionToBeInert(t *testing.T) {
	a := AgentDef{Context: cxMode(ContextModeAuto), Compaction: &Compaction{AutoCompactAtPct: iptr(70)}}
	if got := InertContextSettings(a); len(got) == 0 {
		t.Fatal("mode auto reported nothing inert — but auto resolves only to recap " +
			"or stateful, and BOTH bypass the compaction threshold")
	}
}

// append is the one mode where the compaction path is live. Reporting it there
// would be a false positive on the default configuration, which is how an
// advisory channel becomes noise.
func TestInertContextSettings_AppendModeReportsNothing(t *testing.T) {
	for _, cx := range []*Context{nil, cxMode(ContextModeAppend)} {
		a := AgentDef{Context: cx, Compaction: &Compaction{AutoCompactAtPct: iptr(70), MemoryFlush: bptr(true)}}
		if got := InertContextSettings(a); len(got) != 0 {
			t.Errorf("append mode reported %+v — the compaction path is live there", got)
		}
	}
}

// memory_flush installs the banking callback, but the recap and stateful paths
// both gate the actual bank on context.harvest_to_memory. The flag alone banks
// nothing, and nothing said so.
func TestInertContextSettings_MemoryFlushNeedsHarvestToMemory(t *testing.T) {
	a := AgentDef{Context: cxMode(ContextModeRecap), Compaction: &Compaction{MemoryFlush: bptr(true)}}
	got := InertContextSettings(a)
	if len(got) != 1 || got[0].Setting != "compaction.memory_flush" {
		t.Fatalf("want the memory_flush advisory, got %+v", got)
	}
	if got[0].Fix != "context.harvest_to_memory" {
		t.Errorf("fix = %q, want context.harvest_to_memory — an advisory that does not "+
			"name the live knob leaves the operator hunting", got[0].Fix)
	}
}

// With harvest_to_memory set the spans ARE banked, so memory_flush is merely
// redundant rather than dead. Reporting redundancy as a defect trains an
// operator to ignore the channel.
func TestInertContextSettings_MemoryFlushIsQuietWhenHarvestIsOn(t *testing.T) {
	a := AgentDef{
		Context:    &Context{Mode: cxMode(ContextModeRecap).Mode, HarvestToMemory: bptr(true)},
		Compaction: &Compaction{MemoryFlush: bptr(true)},
	}
	for _, s := range InertContextSettings(a) {
		if s.Setting == "compaction.memory_flush" {
			t.Errorf("memory_flush reported inert while harvest_to_memory is on: %+v", s)
		}
	}
}

// An agent that sets neither produces nothing at all.
func TestInertContextSettings_AFullyLiveAgentIsSilent(t *testing.T) {
	a := AgentDef{Context: cxMode(ContextModeRecap)}
	if got := InertContextSettings(a); len(got) != 0 {
		t.Errorf("a recap agent with no compaction block reported %+v", got)
	}
}

// The rendered boot advisory must name the agent, the dead setting and the fix
// — the three things an operator needs to act without opening the source.
func TestContextCompactionWarnings_NameTheAgentSettingAndFix(t *testing.T) {
	a := AgentDef{
		Context:    cxMode(ContextModeRecap),
		Compaction: &Compaction{AutoCompactAtPct: iptr(70), MemoryFlush: bptr(true)},
	}
	w := contextCompactionWarnings("chat/local", a)
	if len(w) != 2 {
		t.Fatalf("want 2 advisories, got %d: %v", len(w), w)
	}
	joined := strings.Join(w, "\n")
	for _, want := range []string{
		`agent "chat/local"`,
		"compaction.autocompact_at_pct",
		"context.autorecap_at_pct",
		"compaction.memory_flush",
		"context.harvest_to_memory",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("advisories do not mention %q:\n%s", want, joined)
		}
	}
}

// Stateful distils per step by construction — there is no threshold to move, so
// the advisory must NOT invent one. A fix that points nowhere is worse than no
// fix: it sends the operator to set a key that will not help.
func TestInertContextSettings_StatefulOffersNoFalseThresholdFix(t *testing.T) {
	a := AgentDef{Context: cxMode(ContextModeStateful), Compaction: &Compaction{AutoCompactAtPct: iptr(70)}}
	got := InertContextSettings(a)
	if len(got) != 1 {
		t.Fatalf("want 1 advisory, got %+v", got)
	}
	if got[0].Fix != "" {
		t.Errorf("stateful offered fix %q — there is no threshold knob in that mode", got[0].Fix)
	}
}
