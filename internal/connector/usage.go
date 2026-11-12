package connector

import "github.com/denn-gubsky/loomcycle/internal/providers"

// RunUsage folds a run's events into the usage a spawn result reports: the
// run's cumulative usage, the figures its row (get_run) holds. EventUsage is
// per model call, so keeping the last one under-reported a multi-call run
// (RFC GK D5); the run's closing EventDone carries the loop's own total and
// wins. A run that ends without one (cancelled, failed) reports the sum of
// the calls that completed, which is what its row records then too.
type RunUsage struct {
	sum   providers.Usage
	calls int
	total *providers.Usage
}

// Observe takes one event of the run's stream.
func (u *RunUsage) Observe(ev providers.Event) {
	if ev.Usage == nil {
		return
	}
	switch ev.Type {
	case providers.EventUsage:
		c := ev.Usage
		u.sum.InputTokens += c.InputTokens
		u.sum.OutputTokens += c.OutputTokens
		u.sum.CacheCreationTokens += c.CacheCreationTokens
		u.sum.CacheReadTokens += c.CacheReadTokens
		// As the loop's total does: the last call's model, provider and paying key.
		u.sum.Model, u.sum.Provider = c.Model, c.Provider
		u.sum.CredentialSource, u.sum.CredentialScopeID = c.CredentialSource, c.CredentialScopeID
		u.calls++
	case providers.EventDone:
		t := *ev.Usage
		u.total = &t
	}
}

// Usage is the run's cumulative usage; nil when no call reported any.
func (u *RunUsage) Usage() *providers.Usage {
	if u.total != nil {
		t := *u.total
		return &t
	}
	if u.calls == 0 {
		return nil
	}
	s := u.sum
	return &s
}
