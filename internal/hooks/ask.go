package hooks

import (
	"context"
	"encoding/json"
)

// A hook that asks a person — a code body's Interruption.ask, or a channel
// hook's hold — may wait far longer than any process can promise to live.
// An AskSession is how the caller that drives such a hook makes the wait
// survivable: the asks already answered are kept durably and replayed rather
// than asked again, the run an ask is filed under is opened only when the
// first ask is made, and the caller's concurrency slot is given up while a
// person decides.
//
// A run's own hooks need none of this (their asks are the run's, which the
// loop already owns), so a nil session means: ask in the ctx as it is.

// AskRecord is one Interruption call a hook made and its answer, as it is
// replayed on every later run of the hook's body.
type AskRecord struct {
	Input   json.RawMessage `json:"input"`
	Text    string          `json:"text"`
	IsError bool            `json:"is_error,omitempty"`
}

// AskSession is the durable context of one hook decision's asks.
type AskSession interface {
	// Recorded is what was already asked and answered for this decision.
	Recorded() []AskRecord
	// Record keeps one answered ask before the body runs again. An error
	// fails the hook: an answer that is not kept would be asked again.
	Record(AskRecord) error
	// Begin returns the ctx an ask is made under — the run it is filed
	// under, opened on first use — derived from ctx.
	Begin(ctx context.Context) (context.Context, error)
	// Wait is called before an ask blocks for a person; the func it returns
	// is called when the answer is in. The caller frees what a waiting ask
	// must not hold (a concurrency slot) and takes it back.
	Wait() (resume func())
	// Anchor is the body's clock for this decision (unix millis): now on the
	// first run, and the same on every later one, in whatever process, so a
	// body that reads the time replays the question it asked.
	Anchor(now int64) int64
}

type askSessionKey struct{}

// WithAskSession attaches the session a hook's asks go through.
func WithAskSession(ctx context.Context, s AskSession) context.Context {
	return context.WithValue(ctx, askSessionKey{}, s)
}

// AskSessionFrom returns the ctx's ask session, or nil.
func AskSessionFrom(ctx context.Context) AskSession {
	s, _ := ctx.Value(askSessionKey{}).(AskSession)
	return s
}
