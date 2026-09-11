package teamrun

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/jsonpath"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// The Starter runner — the dispatcher a workflow reads its work from.
//
// It is the one node kind that brings data INTO a walk: it reads a channel,
// turns each message into an agent run, and publishes each result onward. The
// channel is its wire, not its executor — "a node kind is something that RUNS,
// a channel is something that CARRIES".
//
// THIS FILE DOES NOT KNOW WHAT A CHANNEL IS. teamrun's contract is that it is a
// pure graph-walker; the store, the ACL and the cursor all live behind the
// narrow ChannelIO seam below, injected the way SpawnFunc already is. That is
// what keeps this package at three internal dependencies instead of twenty.

// ChannelMessage is one message a Starter read. Payload is the raw JSON the
// publisher wrote; the runner treats it as opaque DATA and never as template
// text — see composeWavePrompt.
type ChannelMessage struct {
	ID      string
	Payload json.RawMessage
}

// ChannelIO is everything a Starter needs from the channel substrate. Three
// methods, deliberately: read, acknowledge, publish. Anything richer would pull
// the cursor model into the walk, and the cursor is exactly what the Starter
// exists to own on the workflow's behalf.
//
// The implementation resolves the channel under the TEAM's ACL
// (Definition.Channels), which is why an agent in a wave needs no channel grant
// in either direction.
type ChannelIO interface {
	// Read returns up to `batch` messages and the cursor that acknowledges
	// them. It blocks up to waitMS for at least `want` messages; returning
	// fewer is not an error, and the caller decides what a short read means.
	Read(ctx context.Context, channel string, want, batch, waitMS int) ([]ChannelMessage, string, error)
	// Ack commits a cursor returned by Read.
	Ack(ctx context.Context, channel, cursor string) error
	// Publish appends one message to a channel.
	Publish(ctx context.Context, channel string, payload json.RawMessage) error
}

// SinkMessage is what the RUNTIME publishes for every spawned run — one
// message each, on a guaranteed path, so a downstream fan-in count is never
// short. A timed-out or crashed agent publishes `status: error` rather than
// vanishing: a wait unblocked by failure beats a wait that hangs on it, and a
// downstream judge sees the failure as content.
type SinkMessage struct {
	Wave     string `json:"wave"`
	WaveSize int    `json:"wave_size"`
	Index    int    `json:"index"`
	Agent    string `json:"agent"`
	Status   string `json:"status"`
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Sink statuses. `ok` and `error` today; `timeout` arrives with the per-run
// wall clock in the fan-out phase.
const (
	SinkOK    = "ok"
	SinkError = "error"
)

// runStarter executes one starter state: read, dispatch the wave, publish one
// sink message per run, ack.
func (r *agentRunner) runStarter(ctx context.Context, st teamgraph.State, task *Task) (Outcome, error) {
	h := st.Handler
	if r.channels == nil {
		return Outcome{}, fmt.Errorf("state %q is a starter but no channel executor is wired", st.ID)
	}

	width, err := r.waveWidth(st)
	if err != nil {
		return Outcome{}, err
	}
	want := 1
	if h.Source.Wait == teamgraph.WaitAtLeast && h.Source.N > want {
		want = h.Source.N
	}
	msgs, cursor, err := r.channels.Read(ctx, h.Source.Channel, want, width, h.Source.WaitMS)
	if err != nil {
		return Outcome{}, fmt.Errorf("state %q read %q: %w", st.ID, h.Source.Channel, err)
	}
	if len(msgs) == 0 {
		// Nothing arrived inside the wait. A walk that silently proceeded on an
		// empty wave would hand the next state an answer nobody produced, so
		// this is an error — the RFC's "expiring the wait is a walk error,
		// never a silent proceed".
		return Outcome{}, fmt.Errorf("state %q: no message on %q within the wait", st.ID, h.Source.Channel)
	}
	// The read is already bounded by width, but a store that returned more
	// would otherwise widen the wave past the author's ceiling.
	if len(msgs) > width {
		msgs = msgs[:width]
	}

	results, waveErr := r.runWave(ctx, st, task, msgs)

	// Ack LAST, and only on success. after_results is at-least-once by
	// construction: a crash — or a failed wave — between the results and this
	// line redelivers the batch, which is the tradeoff the definition asked
	// for. after_read acks on read instead and loses the batch to a crash.
	if waveErr == nil && h.Ack != teamgraph.AckAfterRead && cursor != "" {
		if aerr := r.channels.Ack(ctx, h.Source.Channel, cursor); aerr != nil {
			r.log("teamrun: state %q ack %q: %v", st.ID, h.Source.Channel, aerr)
		}
	}
	if waveErr != nil {
		return Outcome{}, fmt.Errorf("state %q wave: %w", st.ID, waveErr)
	}

	// The state's output is ALWAYS the results envelope, even for a wave of
	// one. A Starter's output shape must not depend on how many messages
	// happened to arrive, or a downstream consolidator works on Tuesday and
	// breaks on Wednesday. It is the same envelope a parallel handler produces,
	// so one consolidator agent reads either.
	envelope, err := resultsEnvelope(results)
	if err != nil {
		return Outcome{}, err
	}
	return r.captured(st, task, Outcome{Output: envelope})
}

// waveWidth is how many runs this state may dispatch at once: the author's
// ceiling, bounded by the deployment's.
//
// A definition that asks for more than the deployment allows is REFUSED rather
// than quietly given less. Silently capping is the shape of bug that has cost
// this codebase real time — an operator sets a number, the runtime uses a
// different one, and nothing says so.
func (r *agentRunner) waveWidth(st teamgraph.State) (int, error) {
	h := st.Handler
	if h.Fanout.Per == teamgraph.FanoutPerOnce {
		// One run holding the whole batch; the read is bounded by `batch` alone.
		w := h.Source.Batch
		if w <= 0 {
			w = defaultStarterBatch
		}
		return w, nil
	}
	w := h.Fanout.Max
	if b := h.Source.Batch; b > 0 && b < w {
		// Reading fewer than the ceiling is legitimate: the ceiling says "never
		// more than this", the batch says "take this many at a time".
		w = b
	}
	if r.maxWave > 0 && h.Fanout.Max > r.maxWave {
		return 0, fmt.Errorf("state %q fanout max=%d exceeds this deployment's ceiling of %d — "+
			"lower the definition's max, or raise the substrate's", st.ID, h.Fanout.Max, r.maxWave)
	}
	return w, nil
}

// defaultStarterBatch is how many messages a per=once wave reads when the
// definition says nothing. Matches the Channel tool's own subscribe default, so
// an author who reasons about one reasons about both.
const defaultStarterBatch = 10

// runWave dispatches the wave and returns one result per spawned run, in index
// order. Every result has a sink message published for it, whatever happened.
//
// Concurrency and the wait threshold mirror runParallel — a Starter wave IS a
// fan-out, and having two answers for "how many must succeed" would be two
// places to get it wrong.
func (r *agentRunner) runWave(ctx context.Context, st teamgraph.State, task *Task, msgs []ChannelMessage) ([]agentResult, error) {
	h := st.Handler
	waveID := mintWaveID()

	// per=once is ONE run holding every message; per=message is one run each.
	dispatches := len(msgs)
	if h.Fanout.Per == teamgraph.FanoutPerOnce {
		dispatches = 1
	}
	agentFor := func(i int) string {
		if h.Fanout.Agent != "" {
			return h.Fanout.Agent
		}
		// A list of agents fans the SAME message set across them; with
		// per=message and more messages than agents it cycles, so `agents` is
		// "these roles" rather than "this many runs".
		return h.Fanout.Agents[i%len(h.Fanout.Agents)]
	}

	// binds project THE source message, so they run only where there is one.
	if h.Fanout.Per != teamgraph.FanoutPerOnce {
		r.bindFromMessage(st, task, msgs[0])
	}
	env := r.envFor(st, task)

	need, err := requiredSuccesses(h.Fanout.Wait, dispatches)
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([]agentResult, dispatches)
	sem := make(chan struct{}, parallelConcurrency(dispatches))
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0

	for i := 0; i < dispatches; i++ {
		i := i
		agent := agentFor(i)
		slots := waveSlots(h, msgs, i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-runCtx.Done():
				// Cancelled before it ever ran — still a result, and still a
				// sink message, because the count is the contract.
				res := agentResult{Index: i, Agent: agent, Ok: false, Error: runCtx.Err().Error()}
				results[i] = res
				r.publishSink(ctx, st, waveID, dispatches, res)
				return
			}
			results[i] = r.dispatchOne(runCtx, st, env, agent, waveID, i, dispatches, slots)
			if results[i].Ok {
				mu.Lock()
				successes++
				if successes >= need {
					cancel() // enough succeeded → stop the rest (no-op for wait:all)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if successes < need {
		wait := h.Fanout.Wait
		if wait == "" {
			wait = teamgraph.WaitAll
		}
		var fails []string
		for _, res := range results {
			if !res.Ok {
				fails = append(fails, fmt.Sprintf("%s: %s", res.Agent, res.Error))
			}
		}
		return results, fmt.Errorf("%d of %d runs succeeded, need %d (wait=%q): %s",
			successes, dispatches, need, wait, strings.Join(fails, "; "))
	}
	return results, nil
}

// dispatchOne spawns one run of the wave and publishes exactly one sink message
// for it, whatever happens.
//
// The publish is deferred so it survives a panic in the spawner: a fan-in
// counting messages cannot tell "still running" from "died", so a wave that can
// produce fewer messages than runs turns a downstream wait into a hang. The
// count is the contract.
func (r *agentRunner) dispatchOne(ctx context.Context, st teamgraph.State, env Env, agent, waveID string, index, waveSize int, slots map[string]string) (res agentResult) {
	res = agentResult{Index: index, Agent: agent}
	defer func() {
		if rec := recover(); rec != nil {
			res = agentResult{Index: index, Agent: agent, Ok: false,
				Error: fmt.Sprintf("agent %q panicked: %v", agent, rec)}
		}
		r.publishSink(ctx, st, waveID, waveSize, res)
	}()

	prompt := Prompt{Values: env.Values(), DataSlots: slots}
	if st.Handler.Prompt != nil {
		prompt.System, prompt.Input = st.Handler.Prompt.System, st.Handler.Prompt.Input
	}
	// The wave this run belongs to rides ctx to the run-creation seam, which
	// stamps it on the run's ParentContext. A join, not a copy.
	out, err := r.spawn(r.withWave(ctx, env.WalkID, waveID, index), agent, prompt, "")
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Ok, res.Output = true, out
	return res
}

// publishSink writes one run's outcome to the sink. Called from dispatchOne's
// defer, so it runs on every path including a panic.
func (r *agentRunner) publishSink(ctx context.Context, st teamgraph.State, waveID string, waveSize int, res agentResult) {
	if st.Handler.Sink == nil {
		return
	}
	msg := SinkMessage{
		Wave: waveID, WaveSize: waveSize, Index: res.Index, Agent: res.Agent,
		Status: SinkOK, Output: res.Output,
	}
	if !res.Ok {
		msg.Status, msg.Output, msg.Error = SinkError, "", res.Error
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		r.log("teamrun: state %q sink marshal: %v", st.ID, err)
		return
	}
	// A survival ctx: the wave's result must reach the sink even when the
	// walk's ctx is already cancelled — by a sibling hitting the wait
	// threshold, or by the parent run. A cancelled parent must not be able to
	// make a downstream fan-in wait forever.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if perr := r.channels.Publish(pctx, st.Handler.Sink.Channel, payload); perr != nil {
		r.log("teamrun: state %q sink publish: %v", st.ID, perr)
	}
}

// waveSlots builds the data slots for one dispatch: the single message for
// per=message, the whole batch as a JSON array for per=once.
func waveSlots(h teamgraph.Handler, msgs []ChannelMessage, i int) map[string]string {
	if h.Fanout.Per == teamgraph.FanoutPerOnce {
		parts := make([]json.RawMessage, 0, len(msgs))
		for _, m := range msgs {
			parts = append(parts, m.Payload)
		}
		all, err := json.Marshal(parts)
		if err != nil {
			all = []byte("[]")
		}
		return map[string]string{StarterMessagesSlot: string(all)}
	}
	return map[string]string{StarterMessageSlot: string(msgs[i].Payload)}
}

// The reserved slots a Starter's prompt receives its payload in. Reserved: an
// operator writing one into a prompt that fills no slots gets it back
// unchanged, because nothing fills it there.
//
// Two, because the two fan-out shapes hand the agent different things: one
// message each (per=message), or the whole batch as a JSON array (per=once).
// Naming them separately means a template says which shape it expects, instead
// of a singular name quietly holding a list.
const (
	StarterMessageSlot  = "{{starter.message}}"
	StarterMessagesSlot = "{{starter.messages}}"
)

// logf reports what a Starter could not do but must not fail for.
func (r *agentRunner) log(format string, args ...any) {
	if r.logf != nil {
		r.logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// bindFromMessage projects the source message into ${var.*} per `binds`, using
// the same strict-subset JSONPath the webhook projector uses — one dialect in
// the runtime, not two. A path that does not resolve binds nothing, matching
// capture's posture toward a document nobody promised.
func (r *agentRunner) bindFromMessage(st teamgraph.State, task *Task, msg ChannelMessage) {
	if len(st.Handler.Binds) == 0 {
		return
	}
	var doc interface{}
	if err := json.Unmarshal(msg.Payload, &doc); err != nil {
		return
	}
	for name, path := range st.Handler.Binds {
		segs, err := jsonpath.Parse(path)
		if err != nil {
			continue
		}
		if v, ok := jsonpath.Eval(doc, segs); ok {
			task.SetVar(name, jsonpath.Stringify(v))
		}
	}
}

// withWave puts this run's wave identity on ctx for the run-creation seam.
func (r *agentRunner) withWave(ctx context.Context, walkID, waveID string, index int) context.Context {
	if r.wave == nil {
		return ctx
	}
	return r.wave(ctx, walkID, waveID, index)
}

// mintWaveID / mintWalkID return fresh correlation ids. Short and opaque: they
// are join keys, not things anyone types.
func mintWaveID() string { return mintID("wav_") }
func mintWalkID() string { return mintID("wlk_") }

func mintID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}
