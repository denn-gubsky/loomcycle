package channelhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Asks. A channel hook may need a person: a hook that answers `hold`, or a
// code body that calls Interruption.ask. A channel message belongs to no run,
// and an ask is always filed under one — that is how it is listed, answered
// and found again — so the first ask a message's hooks make opens a run for
// it, `hook:<name>` in the tenant whose definition carries the hook, answered
// by that tenant's operators. The run ends when the message is decided.
//
// A person may take hours, longer than a replica may live. Every answer is
// kept in the message's progress before anything else happens, and replayed
// rather than asked again; an ask still pending when its worker died is
// cancelled when the message is claimed again, and asked afresh. While an ask
// waits, it gives up its concurrency slot.

// HookAgentPrefix labels a hook run, so the runs list reads `hook:<name>`.
const HookAgentPrefix = "hook:"

// RunMinter opens and ends the runs asks are filed under.
type RunMinter interface {
	// Open returns a ctx for asks under runID when that run is still open,
	// or under a new run labelled agent in tenant (its user the system's),
	// and the run's id. The ctx carries the run's id and identity, and the
	// events emitted on it are the run's.
	Open(ctx context.Context, tenant, agent, runID string) (context.Context, string, error)
	// Finish ends a run once the message it was opened for is decided.
	Finish(runID string, status store.RunStatus, reason string)
}

// journal is what one hook has asked about one message, kept in the
// message's progress: the hook's place in the chain, and each answered ask.
type journal struct {
	Pos  int               `json:"pos"`
	Asks []hooks.AskRecord `json:"asks,omitempty"`
	// Chain identifies the chain the progress is a place in (see
	// chainSignature).
	Chain string `json:"chain,omitempty"`
	// Anchor is the hook body's clock for this decision (see
	// hooks.AskSession.Anchor).
	Anchor int64 `json:"anchor,omitempty"`
	// HeldBy marks a hold made while nobody can be asked (no Interruption
	// configured): the message waits out its deadline.
	HeldBy string `json:"held_by,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func (j *job) loadJournal() {
	j.jrnl = journal{Pos: -1}
	if len(j.progress.Journal) > 0 {
		_ = json.Unmarshal(j.progress.Journal, &j.jrnl)
	}
}

func (j *job) journalJSON() json.RawMessage {
	if j.jrnl.Pos < 0 && j.jrnl.Chain == "" && j.jrnl.HeldBy == "" {
		return nil
	}
	b, _ := json.Marshal(j.jrnl)
	return b
}

// askSession is the AskSession of one hook's decision on one message.
type askSession struct {
	j        *job
	ctx      context.Context // the job's, for saving
	pos      int
	hookName string
	body     json.RawMessage // the body as saved at this point in the chain
}

func (j *job) session(ctx context.Context, pos int, hookName string, body json.RawMessage) *askSession {
	return &askSession{j: j, ctx: ctx, pos: pos, hookName: hookName, body: body}
}

var _ hooks.AskSession = (*askSession)(nil)

func (s *askSession) Recorded() []hooks.AskRecord {
	if s.j.jrnl.Pos != s.pos {
		return nil
	}
	return s.j.jrnl.Asks
}

func (s *askSession) Anchor(now int64) int64 {
	if s.j.jrnl.Pos != s.pos {
		s.j.jrnl = journal{Pos: s.pos, Chain: s.j.jrnl.Chain}
	}
	if s.j.jrnl.Anchor == 0 {
		s.j.jrnl.Anchor = now // kept with the first answer
	}
	return s.j.jrnl.Anchor
}

func (s *askSession) Record(r hooks.AskRecord) error {
	if s.j.jrnl.Pos != s.pos {
		s.j.jrnl = journal{Pos: s.pos, Chain: s.j.jrnl.Chain}
	}
	s.j.jrnl.Asks = append(s.j.jrnl.Asks, r)
	return s.j.saveProgress(s.ctx, s.pos, s.body, 0, time.Time{}, "")
}

func (s *askSession) Begin(ctx context.Context) (context.Context, error) {
	return s.j.openRun(ctx, s.hookName)
}

func (s *askSession) Wait() func() { return s.j.w.yield(s.j) }

// openRun returns ctx under the message's hook run, opening it on first use
// and keeping its id in the message's progress.
func (j *job) openRun(ctx context.Context, hookName string) (context.Context, error) {
	if j.w.cfg.Runs == nil {
		return nil, errors.New("asks are not available here: no run to file them under")
	}
	rctx, id, err := j.w.cfg.Runs.Open(ctx, j.msg.HookTenant, HookAgentPrefix+hookName, j.progress.RunID)
	if err != nil {
		return nil, err
	}
	if id != j.progress.RunID {
		j.progress.RunID = id
		if err := j.saveProgress(ctx, j.progress.ChainPos, j.progress.Body, j.progress.Attempts, time.Time{}, j.progress.LastError); err != nil {
			return nil, err
		}
	}
	j.runEmit = tools.EventEmitter(rctx)
	return rctx, nil
}

// yield gives up a waiting job's slots — the worker's and its channel's — so
// a person's time does not starve every other message; the func it returns
// takes them back.
func (w *Worker) yield(j *job) func() {
	<-j.chSem
	<-w.sem
	w.inFlight.Add(-1)
	select {
	case w.freed <- struct{}{}:
	default:
	}
	return func() {
		w.sem <- struct{}{}
		j.chSem <- struct{}{}
		w.inFlight.Add(1)
	}
}

// cancelStaleAsks cancels what the message's run still has pending. The
// message is claimed again only once its previous worker is gone, so nobody
// is waiting for those answers; the hook asks afresh.
func (j *job) cancelStaleAsks(ctx context.Context) {
	if j.progress.RunID == "" {
		return
	}
	rows, err := j.w.cfg.Store.InterruptListByRun(ctx, j.progress.RunID, store.InterruptStatusPending)
	if err != nil {
		log.Printf("channelhooks: %s message %s: list pending asks: %v", j.msg.Channel, j.msg.ID, err)
		return
	}
	for _, r := range rows {
		if err := j.w.cfg.Store.InterruptFinish(ctx, r.InterruptID, store.InterruptStatusCancelled, store.InterruptResolvedByHookRestart); err != nil {
			log.Printf("channelhooks: %s message %s: cancel stale ask %s: %v", j.msg.Channel, j.msg.ID, r.InterruptID, err)
		}
	}
}

// Hold answers.
const (
	holdRelease = "release"
	holdDrop    = "drop"
)

// holdAsk is the question a hold puts to a person.
type holdAsk struct {
	Op        string   `json:"op"`
	Question  string   `json:"question"`
	Options   []string `json:"options"`
	Context   string   `json:"context,omitempty"` // the message, as JSON text
	TimeoutMS int      `json:"timeout_ms,omitempty"`
}

// maxHoldContextBody bounds the body a hold's question shows the person.
const maxHoldContextBody = 4 << 10

// askHold asks a person whether to deliver a message a hook holds. It returns
// "release" or "drop"; a decline, a timeout or a failure to ask is an error,
// and a hold nobody answered drops the message.
func (j *job) askHold(ctx context.Context, pos int, hookName, name, reason string, body json.RawMessage, savedBody json.RawMessage) (string, error) {
	c := map[string]any{"channel": j.msg.Channel, "message_id": j.msg.ID, "hook": name}
	if reason != "" {
		c["reason"] = reason
	}
	if len(body) <= maxHoldContextBody {
		c["body"] = body
	} else {
		c["body_bytes"] = len(body)
	}
	ctxJSON, _ := json.Marshal(c)
	in := holdAsk{Op: "ask", Options: []string{holdRelease, holdDrop}, Context: string(ctxJSON),
		Question: fmt.Sprintf("Hook %s is holding a message on channel %s. Deliver it?", name, j.msg.Channel)}
	// The journal keys the ask by the question, not the timeout: the time
	// left changes between a worker and the one that replays its answer.
	key, _ := json.Marshal(in)
	if !j.msg.ExpiresAt.IsZero() {
		// Answered before the message expires, or it is lost unseen.
		left := time.Until(j.msg.ExpiresAt.Add(-margin(j.msg.ExpiresAt.Sub(j.msg.PublishedAt))))
		if left < time.Second {
			return "", errors.New("the message expires before anyone could answer")
		}
		in.TimeoutMS = int(left / time.Millisecond)
	}
	input, _ := json.Marshal(in)

	sess := j.session(ctx, pos, hookName, savedBody)
	for _, rec := range sess.Recorded() {
		if bytes.Equal(bytes.TrimSpace(rec.Input), key) {
			return holdAnswer(rec.Text, rec.IsError)
		}
	}
	askCtx := tools.WithInterruptionPolicy(ctx, tools.InterruptionPolicyValue{Enabled: true, Kinds: []string{"question"}})
	askCtx, err := sess.Begin(askCtx)
	if err != nil {
		return "", err
	}
	resume := sess.Wait()
	res, err := j.w.cfg.Interruption.Execute(askCtx, input)
	resume()
	if err != nil {
		return "", err
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err := sess.Record(hooks.AskRecord{Input: key, Text: res.Text, IsError: res.IsError}); err != nil {
		return "", err
	}
	return holdAnswer(res.Text, res.IsError)
}

func holdAnswer(text string, isError bool) (string, error) {
	if isError {
		return "", errors.New(text)
	}
	var r struct {
		Answer   string `json:"answer"`
		Declined bool   `json:"declined"`
	}
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		return "", fmt.Errorf("unreadable answer: %w", err)
	}
	if r.Declined {
		return "", errors.New("the person declined to answer")
	}
	if r.Answer != holdRelease && r.Answer != holdDrop {
		return "", fmt.Errorf("answer %q is not %q or %q", r.Answer, holdRelease, holdDrop)
	}
	return r.Answer, nil
}

// finishRun ends the message's hook run, if it has one, with how the message
// was decided.
func (j *job) finishRun(reason string) {
	if j.progress.RunID == "" || j.w.cfg.Runs == nil {
		return
	}
	j.w.cfg.Runs.Finish(j.progress.RunID, store.RunCompleted, reason)
}

// emitDecision puts a decision into the hook run's transcript, when the
// message has one.
func (j *job) emitDecision(d providers.HookDecisionInfo) {
	if j.runEmit == nil {
		return
	}
	j.runEmit(providers.Event{Type: providers.EventHookDecision, HookDecision: &d})
}
