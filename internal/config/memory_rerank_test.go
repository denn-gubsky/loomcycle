package config

import (
	"strings"
	"testing"
)

// TestMemoryRerank_LoadsOnAnAgentAndIsBounded — the per-agent block loads from
// yaml, and out-of-range values fail LOAD naming the agent and field.
func TestMemoryRerank_LoadsOnAnAgentAndIsBounded(t *testing.T) {
	cfg, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  reader:
    memory_rerank: { enabled: true, candidates: 30 }
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	mr := cfg.Agents["reader"].MemoryRerank
	if !mr.On() || mr.Candidates != 30 || mr.MaxChars != 0 {
		t.Errorf("memory_rerank = %+v", mr)
	}
	for _, bad := range []string{"{ candidates: 1 }", "{ candidates: 51 }", "{ max_chars: 50 }", "{ max_chars: 50000 }"} {
		_, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  reader:
    memory_rerank: `+bad+`
`))
		if err == nil || !strings.Contains(err.Error(), `agent "reader": memory_rerank.`) {
			t.Errorf("%s: err = %v, want a bounds error naming the agent", bad, err)
		}
	}
}

// TestMergeMemoryRerank_PerField — a set field wins, an unset one keeps the base;
// an explicit enabled:false switches a parent's rerank off.
func TestMergeMemoryRerank_PerField(t *testing.T) {
	on, off := true, false
	base := &MemoryRerank{Enabled: &on, Candidates: 20, MaxChars: 900}
	got := MergeMemoryRerank(base, &MemoryRerank{Candidates: 30})
	if !got.On() || got.Candidates != 30 || got.MaxChars != 900 {
		t.Errorf("merge = %+v", got)
	}
	if MergeMemoryRerank(base, &MemoryRerank{Enabled: &off}).On() {
		t.Error("enabled:false in the overlay must switch the rerank off")
	}
	if MergeMemoryRerank(nil, nil) != nil || MergeMemoryRerank(&MemoryRerank{}, nil) != nil {
		t.Error("nothing set must merge to nil (byte-stable hash)")
	}
	got.Candidates = 99
	if base.Candidates != 20 {
		t.Error("the merge aliased its base")
	}
}

// TestMemoryRerank_BoundsAreInclusive — the documented ranges [2,50] and
// [200,20000] accept their own endpoints; an off-by-one would refuse a value the
// docs promise.
func TestMemoryRerank_BoundsAreInclusive(t *testing.T) {
	for _, m := range []MemoryRerank{{Candidates: 2}, {Candidates: 50}, {MaxChars: 200}, {MaxChars: 20000}} {
		if err := m.Validate(); err != nil {
			t.Errorf("%+v: %v", m, err)
		}
	}
}
