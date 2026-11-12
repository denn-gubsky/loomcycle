package connector

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

func usageEv(typ providers.EventType, in, out int) providers.Event {
	return providers.Event{Type: typ, Usage: &providers.Usage{InputTokens: in, OutputTokens: out, Model: "m"}}
}

func TestRunUsage_NoCallsReportsNil(t *testing.T) {
	var u RunUsage
	u.Observe(providers.Event{Type: providers.EventText, Text: "x"})
	if got := u.Usage(); got != nil {
		t.Errorf("Usage() = %+v, want nil", got)
	}
}

func TestRunUsage_WithoutDoneSumsTheCalls(t *testing.T) {
	var u RunUsage
	u.Observe(usageEv(providers.EventUsage, 10, 1))
	u.Observe(usageEv(providers.EventUsage, 20, 2))
	got := u.Usage()
	if got == nil || got.InputTokens != 30 || got.OutputTokens != 3 || got.Model != "m" {
		t.Errorf("Usage() = %+v, want 30/3 on model m", got)
	}
}

func TestRunUsage_DoneTotalWinsOverTheSum(t *testing.T) {
	var u RunUsage
	u.Observe(usageEv(providers.EventUsage, 10, 1))
	u.Observe(usageEv(providers.EventDone, 99, 9))
	got := u.Usage()
	if got == nil || got.InputTokens != 99 || got.OutputTokens != 9 {
		t.Errorf("Usage() = %+v, want the done total 99/9", got)
	}
}
