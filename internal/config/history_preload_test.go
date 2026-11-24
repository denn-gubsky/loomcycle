package config

import (
	"strings"
	"testing"
	"time"
)

func TestHistoryPreload_LoadsFromYAMLAndAnEmptyBlockMeansOn(t *testing.T) {
	cfg, err := loadSpeech(t, `
agents:
  chatty:
    system_prompt: hi
    history_preload:
      sessions: 5
      within: 7d
      include: summary+tail
      tail_turns: 2
      max_tokens: 900
  defaults:
    system_prompt: hi
    history_preload: {}
  plain:
    system_prompt: hi
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := cfg.Agents["chatty"].HistoryPreload
	if got == nil || got.Sessions != 5 || got.Within != "7d" || got.Include != HistoryPreloadIncludeSummaryTail || got.TailTurns != 2 || got.MaxTokens != 900 {
		t.Fatalf("chatty history_preload = %+v", got)
	}
	if got.Effective().WithinDuration() != 7*24*time.Hour {
		t.Errorf("within = %v, want 7d", got.Effective().WithinDuration())
	}
	def := cfg.Agents["defaults"].HistoryPreload
	if def == nil {
		t.Fatal("history_preload: {} loaded as absent; an empty block must turn the preload on")
	}
	eff := def.Effective()
	if eff.Sessions != 3 || eff.Include != "summary" || eff.TailTurns != 4 || eff.MaxTokens != 1500 || eff.WithinDuration() != 14*24*time.Hour {
		t.Errorf("defaults = %+v within %v", eff, eff.WithinDuration())
	}
	if cfg.Agents["plain"].HistoryPreload != nil {
		t.Error("an agent without the block has a preload")
	}
}

func TestHistoryPreload_RefusesOutOfRangeValuesAtLoad(t *testing.T) {
	for _, tc := range []struct{ block, want string }{
		{"sessions: 21", "history_preload.sessions"},
		{"tail_turns: 21", "history_preload.tail_turns"},
		{"max_tokens: 8001", "history_preload.max_tokens"},
		{"include: everything", "history_preload.include"},
		{"within: soon", "history_preload.within"},
		{"within: 0", "history_preload.within"},
		{"bogus: 1", "bogus"},
	} {
		t.Run(tc.block, func(t *testing.T) {
			_, err := loadSpeech(t, `
agents:
  a:
    system_prompt: hi
    history_preload:
      `+tc.block+`
`)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

func TestMergeHistoryPreload_AnEmptyOverlayTurnsItOnAndSetFieldsWin(t *testing.T) {
	if MergeHistoryPreload(nil, nil) != nil {
		t.Error("nil+nil is not nil")
	}
	if got := MergeHistoryPreload(nil, &HistoryPreload{}); got == nil {
		t.Error("an empty overlay over no block left it off")
	}
	base := &HistoryPreload{Sessions: 2, Within: "3d"}
	got := MergeHistoryPreload(base, &HistoryPreload{Within: "1d", MaxTokens: 100})
	if got.Sessions != 2 || got.Within != "1d" || got.MaxTokens != 100 {
		t.Errorf("merged = %+v", got)
	}
	if got == base || base.Within != "3d" {
		t.Error("merge aliased or mutated its base")
	}
}
