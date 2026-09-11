package teamrun

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
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

// runStarter executes one starter state: read, dispatch, publish, ack.
//
// This phase dispatches exactly ONE run (the vertical slice). The fan-out —
// dynamic N, the ceiling, wave-size stamping — is the next phase, and the wave
// envelope is already shaped for it so that phase adds width, not a rewrite.
func (r *agentRunner) runStarter(ctx context.Context, st teamgraph.State, task *Task) (Outcome, error) {
	h := st.Handler
	if r.channels == nil {
		return Outcome{}, fmt.Errorf("state %q is a starter but no channel executor is wired", st.ID)
	}

	want := 1
	if h.Source.Wait == teamgraph.WaitAtLeast && h.Source.N > want {
		want = h.Source.N
	}
	msgs, cursor, err := r.channels.Read(ctx, h.Source.Channel, want, h.Source.Batch, h.Source.WaitMS)
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

	// binds project the SOURCE MESSAGE into ${var.*}. The values are UNTRUSTED
	// (a channel message may be agent-written or webhook-relayed); they are safe
	// in a prompt, and trust rules 5b/5c hold them wherever they reach a
	// placeholder argument.
	r.bindFromMessage(st, task, msgs[0])

	waveID := mintWaveID()
	agent := h.Fanout.Agent
	if agent == "" && len(h.Fanout.Agents) > 0 {
		agent = h.Fanout.Agents[0]
	}

	out, runErr := r.dispatchOne(ctx, st, task, agent, waveID, 0, len(msgs), msgs[0])

	// The sink publish is a GUARANTEED path: it has already happened by here,
	// including when the spawn panicked. See dispatchOne.
	if runErr != nil {
		// The wave failed, and the sink says so. The walk still fails — a
		// Starter is a state, and a state whose only run errored has produced
		// nothing to thread onward.
		return Outcome{}, fmt.Errorf("state %q wave: %w", st.ID, runErr)
	}

	// Ack LAST. after_results is at-least-once by construction: a crash between
	// the results and this line redelivers the batch, which is the tradeoff the
	// definition asked for. after_read would have acked before dispatch and
	// lost the batch instead.
	if h.Ack != teamgraph.AckAfterRead && cursor != "" {
		if aerr := r.channels.Ack(ctx, h.Source.Channel, cursor); aerr != nil {
			r.log("teamrun: state %q ack %q: %v", st.ID, h.Source.Channel, aerr)
		}
	}
	return r.captured(st, task, Outcome{Output: out})
}

// dispatchOne spawns one run of the wave and publishes exactly one sink message
// for it, whatever happens.
//
// The publish is deferred so it survives a panic in the spawner: a fan-in
// counting messages cannot tell "still running" from "died", so a wave that can
// produce fewer messages than runs turns a downstream wait into a hang. The
// count is the contract.
func (r *agentRunner) dispatchOne(ctx context.Context, st teamgraph.State, task *Task, agent, waveID string, index, waveSize int, msg ChannelMessage) (out string, err error) {
	published := false
	publish := func(status, output, errText string) {
		if published || st.Handler.Sink == nil {
			return
		}
		published = true
		payload, merr := json.Marshal(SinkMessage{
			Wave: waveID, WaveSize: waveSize, Index: index, Agent: agent,
			Status: status, Output: output, Error: errText,
		})
		if merr != nil {
			r.log("teamrun: state %q sink marshal: %v", st.ID, merr)
			return
		}
		// A survival ctx: the wave's result must reach the sink even when the
		// walk's ctx is already cancelled, for the same reason the scheduler
		// records a result on one — a cancelled parent must not be able to
		// make a fan-in wait forever.
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if perr := r.channels.Publish(pctx, st.Handler.Sink.Channel, payload); perr != nil {
			r.log("teamrun: state %q sink publish: %v", st.ID, perr)
		}
	}
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("agent %q panicked: %v", agent, rec)
			publish(SinkError, "", err.Error())
			return
		}
		if err != nil {
			publish(SinkError, "", err.Error())
			return
		}
		publish(SinkOK, out, "")
	}()

	env := r.envFor(st, task)
	prompt := composeWavePrompt(st.Handler, msg, env)
	// The wave this run belongs to rides ctx to the run-creation seam, which
	// stamps it on the run's ParentContext. A join, not a copy.
	spawnCtx := r.withWave(ctx, task, waveID, index)
	out, err = r.spawn(spawnCtx, agent, prompt, "")
	return out, err
}

// composeWavePrompt builds a wave run's prompt.
//
// THE DATA SLOT IS SUBSTITUTED LAST AND ITS CONTENT IS NEVER SCANNED. The
// message payload is attacker-influenceable — anyone who can reach the channel
// can write it — and prompt assembly resolves {{...}} placeholders under the
// RUNTIME's authority, ungated by the agent's tools or scopes. Splicing the
// payload in as template text would therefore hand an ungated read primitive to
// whoever can publish. So the operator's template is what carries placeholders,
// and the payload arrives after they are all resolved, as data.
//
// That is why the slot is not just another ${...}: those resolve inside the one
// combined pass, and this one must land outside it.
func composeWavePrompt(h teamgraph.Handler, msg ChannelMessage, env Env) Prompt {
	var system, input string
	if h.Prompt != nil {
		system, input = h.Prompt.System, h.Prompt.Input
	}
	return Prompt{
		System: system,
		Input:  input,
		Values: env.Values(),
		// DataSlots are substituted by the assembler AFTER expansion completes.
		DataSlots: map[string]string{
			StarterMessageSlot: string(msg.Payload),
		},
	}
}

// StarterMessageSlot is the reserved slot a Starter's prompt uses to receive
// the source message. Reserved: an operator writing it into a non-starter
// prompt gets nothing, because nothing fills it there.
const StarterMessageSlot = "{{starter.message}}"

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
func (r *agentRunner) withWave(ctx context.Context, task *Task, waveID string, index int) context.Context {
	if r.wave == nil {
		return ctx
	}
	return r.wave(ctx, task.WalkID, waveID, index)
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
