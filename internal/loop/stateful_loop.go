package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/contextplugin"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/statepatch"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// statefulLoop carries what the steps of one stateful run share: the locals of
// runStateful from when the loop was one function. Each field keeps the name
// the local had.
type statefulLoop struct {
	opts      RunOptions
	emit      func(providers.Event)
	toolSpecs []providers.ToolSpec

	// What the run's Context tool returns is kept across steps (see contextMemo).
	helpTool string
	memo     *contextMemo

	schema     map[string]any
	onInvalid  string
	maxRetries int
	maxIter    int

	interactive    bool
	statefulSystem []providers.ContentBlock
	emitTool       []providers.ToolSpec

	sigma              map[string]any
	lastWritten        map[string]int
	preambleTokens     int
	lastIn             int
	lastWindow         int
	footprintMeasured  bool
	seenExhaustedState map[int]bool
	holder             *tools.ExecStateHolder
	// dispatchCtx is the run ctx carrying the live Σ holder, made once at the
	// start as the local was; the action's ctx derives from it.
	dispatchCtx context.Context

	total       providers.Usage
	rec         statefulRecovery
	actionIDTag string

	obs            string
	notRetainedObs string
	task           string

	lastProposed      map[string]any // the last schema the model proposed that differs from the active one
	promptSnapshotted bool           // RFC DI: the first request is recorded once
	// movedForQueue: the run has made its one queue-aware move (see Run).
	movedForQueue bool

	// msgs is the current step's request conversation: rebuilt from (Σ,
	// observation) at the top of every step, grown only by the re-prompts of
	// that step's attempts.
	msgs []providers.Message
}

// runStateful executes the L2 loop. `system` is the resolved preamble P (already
// split from opts.Segments); `initial` is the seed conversation (the task);
// `toolSpecs` is the action-tool catalog; `emit` forwards + persists events.
//
// The driver: set the run up (newStatefulRun), wait for the operator first when
// the run starts parked (startParked), then one step per iteration up to the
// cap, and a max_iterations done when the cap runs out. The order inside a step
// is step's.
func runStateful(ctx context.Context, opts RunOptions, system []providers.ContentBlock, initial []providers.Message, toolSpecs []providers.ToolSpec, iterCap int, emit func(providers.Event)) (RunResult, error) {
	r := newStatefulRun(ctx, opts, system, initial, toolSpecs, iterCap, emit)
	if ex := r.startParked(ctx); ex.ctl == loopReturn {
		return ex.res, ex.err
	}
	for iter := 0; iter < r.maxIter; iter++ {
		if ex := r.step(ctx, iter); ex.ctl == loopReturn {
			return ex.res, ex.err
		}
	}

	r.finish("max_iterations")
	return RunResult{StopReason: "max_iterations", Iterations: r.maxIter, Usage: r.total, State: r.sigma, ProposedSchema: r.lastProposed}, nil
}

// step is one iteration of the stateful loop. Its order, and why where the
// order is a rule:
//
//  1. gates — cancellation, the heartbeat, then the cooperative pause: before
//     any model call, never between an action and its result.
//  2. steer — an autonomous run's queued messages join the task before the
//     request is built from it.
//  3. the request conversation is built from (Σ, task, observation).
//  4. queueMove — the one queue-aware move, which needs that conversation.
//  5. callUntilAccepted — the model call, re-prompted until it returns a usable
//     emit_state; the token budget is asked before every attempt.
//  6. mergePatch then evict — Σ takes the patch, and eviction weighs the grown
//     Σ before the next request goes out.
//  7. record — the context_state event, then the recall and memory harvests.
//  8. endTurn — done (or no action) ends the turn: park, answer a late
//     message, or end the run.
//  9. action — otherwise the named action runs and its output is the next
//     observation.
//
// It returns loopNext to go on to the next iteration and loopReturn to end the
// run with the exit's result.
func (r *statefulLoop) step(ctx context.Context, iter int) loopExit {
	r.stepStarts("gates")
	if ex := r.gates(ctx, iter); ex.ctl != loopProceed {
		return ex
	}
	r.stepStarts("steer")
	r.takeSteer(ctx)
	r.msgs = []providers.Message{statefulUserMessage(r.sigma, r.task, r.memo.render(r.helpTool, r.obs), r.obs)}
	r.stepStarts("queue_move")
	r.queueMove(ctx, iter)
	r.stepStarts("call")
	es, ex := r.callUntilAccepted(ctx, iter)
	if ex.ctl != loopProceed {
		return ex
	}

	r.stepStarts("merge")
	sigmaBefore := r.mergePatch(iter, es)
	r.stepStarts("evict")
	evicted := r.evict(ctx, sigmaBefore)
	r.stepStarts("record")
	r.record(ctx, iter, es, evicted)

	r.stepStarts("end_turn")
	if ex := r.endTurn(ctx, iter, es); ex.ctl != loopProceed {
		return ex
	}
	r.stepStarts("action")
	return r.action(ctx, iter, es)
}

// stepStarts reports that a step starts (statefulStepHook), for the order pin.
func (r *statefulLoop) stepStarts(name string) {
	if r.opts.statefulStepHook != nil {
		r.opts.statefulStepHook(name)
	}
}

// newStatefulRun sets a stateful run up: its patch policy, iteration cap,
// system prompt, seeded Σ and footprint, and its first observation and task.
func newStatefulRun(ctx context.Context, opts RunOptions, system []providers.ContentBlock, initial []providers.Message, toolSpecs []providers.ToolSpec, iterCap int, emit func(providers.Event)) *statefulLoop {
	r := &statefulLoop{opts: opts, emit: emit, toolSpecs: toolSpecs}
	// What the run's Context tool returns is kept across steps (see contextMemo).
	r.helpTool = helpToolName(r.opts.Tools)
	r.memo = &contextMemo{}
	// The model reads only its latest observation, so a help hint shown once
	// would be gone the step after: attach it to every shape failure until the
	// model reads that tool's help. Nothing accumulates here, so the repeat
	// costs no context.
	if r.opts.Dispatcher != nil {
		r.opts.Dispatcher.SetHelpHintsUntilRead(true)
	}
	cx := r.opts.Context
	r.onInvalid = config.ContextDefaultOnInvalidPatch
	r.maxRetries = config.ContextDefaultMaxPatchRetries
	if cx != nil {
		r.schema = cx.StateSchema
		if cx.OnInvalidPatch != nil {
			r.onInvalid = *cx.OnInvalidPatch
		}
		if cx.MaxPatchRetries != nil {
			r.maxRetries = *cx.MaxPatchRetries
		}
	}
	// ⚠️ THE CAP IS COMPUTED ONCE, IN Run, and handed down. This loop used to
	// derive its own from opts.MaxIterations and so missed every lift Run
	// applies — including the one that matters here: an interactive run gets
	// max_iterations raised to the hard ceiling because each PARK consumes an
	// iteration. A stateful turn spends several steps, so a parked chat on the
	// default 16 died after a handful of exchanges reporting max_iterations,
	// which is an answer about the wrong thing.
	r.maxIter = iterCap
	if r.maxIter <= 0 {
		r.maxIter = 16
	}

	r.interactive = r.opts.Interactive && r.opts.SteerQueue != nil
	r.statefulSystem = buildStatefulSystem(system, toolSpecs, r.schema, r.interactive)
	r.emitTool = []providers.ToolSpec{emitStateToolSpec()}

	// Seeded from a resumed run's last recorded Σ (RFC DH P2); empty for a
	// fresh one. Copied rather than aliased so the caller's map is never
	// mutated by a merge — opts is passed by value but the map inside it is not.
	r.sigma = map[string]any{}
	for k, v := range r.opts.InitialState {
		r.sigma[k] = v
	}
	// lastWritten records which step last touched each Σ key, so eviction can
	// break ties by recency WITHIN a retention class. The class is the
	// operator's statement of what matters; recency only orders equals.
	r.lastWritten = map[string]int{}
	// ⚠️ STATEFUL HAD NO FOOTPRINT AT ALL. The transcript is rebuilt from
	// (Σ, observation) each step so it cannot accumulate — which is why the
	// gate was never wired here — but Σ ITSELF accumulates, and
	// statefulUserMessage serialises the whole of it into the prompt every
	// step. Nothing measured it, nothing bounded it, and the run grew until
	// the provider refused.
	r.preambleTokens = estimatePreambleTokens(system, toolSpecs)
	r.lastIn = r.preambleTokens + sigmaTokens(r.sigma)
	r.lastWindow = effectiveWindow(0, r.opts)
	// Same rule as the append/recap gate: EVICT on the estimate if you must,
	// but only REPORT a number a provider returned. A driver that reports no
	// usage would otherwise leave lastIn at the preamble estimate forever and
	// tell the operator its state cannot be reduced — about a Σ that is empty.
	r.footprintMeasured = false
	r.seenExhaustedState = map[int]bool{}
	r.holder = &tools.ExecStateHolder{Sigma: r.sigma}
	r.dispatchCtx = tools.WithExecutionState(ctx, r.holder) // the action sees the live Σ (Context op=state)

	// ⚠️ ACTION IDS ARE PERSISTED, and the step counter restarts at 0 in every
	// run — while a session holds many (each continuation is a new run). A
	// session that later replays as messages (mode auto sends an interactive
	// run to recap) would then carry duplicate tool_use ids, which a provider
	// may reject, and the Web UI pairs calls with results by id. A per-run tag
	// keeps them unique; the in-request retry ids never persist and need none.
	r.actionIDTag = newActionIDTag()

	r.obs = initialObservation(initial)
	if r.opts.InitialObservation != "" {
		r.obs = r.opts.InitialObservation
	}
	// notRetainedObs is the last action's output when that output is a
	// no-trace run's content: the model is shown it, and the harvest below
	// banks the step without it.
	r.notRetainedObs = ""
	// task is shown on every step (see statefulUserMessage). An operator's
	// turn in an interactive run replaces it: that turn is the request now.
	r.task = ""
	if s := statefulTask(initial); s != "" {
		r.task = "Task: " + s
	}
	return r
}

// finish emits the run's ONE terminal done. ⚠️ DONE MEANS THE RUN IS OVER, to
// every consumer that reads it — the Web UI marks the run completed on it, and
// the MCP / connector spawn paths take their final stop reason from it. This
// loop used to emit one at every turn boundary before parking, so the embedded
// terminal showed a parked chat as finished after its first answer and sent
// the operator's next message as a brand-new continuation run. The append loop
// has never emitted a mid-run done: the turn boundary of an interactive run is
// awaiting_input, and per-call usage rides EventUsage.
func (r *statefulLoop) finish(stop string) {
	r.emit(providers.Event{Type: providers.EventDone, StopReason: stop, Usage: &r.total})
}

// startParked is the run's wait before its first step. StartParked: a
// re-attached interactive run waits for the operator before spending a model
// call. Mirrors Run's handling — an abandoned park ends the run on the turn it
// had already reached rather than calling the provider.
func (r *statefulLoop) startParked(ctx context.Context) loopExit {
	if r.opts.StartParked && r.interactive {
		next, resumed := parkForStatefulTurn(ctx, &r.opts, 0, r.emit)
		if !resumed {
			stop := r.opts.endTurnOrIdle()
			r.finish(stop)
			return exitReturn(RunResult{StopReason: stop, State: r.sigma}, nil)
		}
		r.obs, r.task = next, next
	}
	return loopExit{}
}

// gates is a step's boundary work before it reads any input: cancellation,
// the heartbeat, and the cooperative pause, each of which can end the run.
func (r *statefulLoop) gates(ctx context.Context, iter int) loopExit {
	if err := ctx.Err(); err != nil {
		return exitReturn(RunResult{StopReason: "cancelled", Iterations: iter, Usage: r.total, State: r.sigma}, err)
	}
	// Same per-iteration pulse the append loop sends; Run's lifetime ticker
	// covers a step that blocks longer than one interval.
	if r.opts.OnHeartbeat != nil {
		r.opts.OnHeartbeat()
	}
	// Cooperative pause (RFC X / F41), at the same boundary the append loop
	// parks at: before a model call, never between an action and its
	// result. ⚠️ THIS LOOP NEVER CHECKED IT, so a runtime pause did not
	// quiesce a stateful run — it kept calling the provider while the
	// runtime reported itself paused, Pause() waited out its whole timeout,
	// and the row never reached pause_state='paused', which is the only
	// state resume re-dispatches. Here Σ is persisted (the last
	// context_state) and so is the pending observation (a tool_result or
	// the operator's user_input), which is what a resume rebuilds from.
	if r.opts.PauseGate != nil && r.opts.PauseGate.PauseRequested() {
		if err := r.opts.PauseGate.Park(ctx); err != nil {
			return exitReturn(RunResult{StopReason: "cancelled", Iterations: iter, Usage: r.total, State: r.sigma}, ctx.Err())
		}
	}
	return loopExit{}
}

// takeSteer hands an autonomous run's queued messages to the task.
func (r *statefulLoop) takeSteer(ctx context.Context) {
	// The append loop reads its queue at the top of every iteration; this
	// loop read it only while parked, so a message sent to an autonomous
	// stateful run was acknowledged and never seen. An interactive run is
	// left alone: its message waits for the park, which hands it over as
	// the operator's turn.
	if r.opts.SteerQueue != nil && !r.opts.interactiveAtBoundary(ctx) {
		for _, text := range takeStatefulSteer(&r.opts, true, r.emit) {
			first := r.obs == r.task // nothing has run yet: the observation IS the task
			if r.task != "" {
				r.task += "\n\n"
			}
			r.task += statefulTaskAddendum + text
			if first {
				r.obs = r.task
			}
		}
	}
}

// queueMove is the queue-aware routing after the start, as in Run: one move at
// a step boundary when the next call would wait too long where the run is.
func (r *statefulLoop) queueMove(ctx context.Context, iter int) {
	if iter > 0 && !r.movedForQueue && r.opts.MoveForQueue != nil &&
		!(r.opts.FallbackPolicy.PinAfterSuccess && r.rec.firstStepSucceeded) {
		r.movedForQueue = moveForQueue(ctx, &r.opts, r.msgs, r.emit)
	}
}

// callUntilAccepted calls the model until it returns an emit_state the run can
// use, re-prompting on prose, a rejected patch, and a turn ended with nothing
// to say, within the patch-retry budget. Each attempt: the token budget, the
// request, the call and its usage, then the answer's checks in that order.
func (r *statefulLoop) callUntilAccepted(ctx context.Context, iter int) (*emitStateOut, loopExit) {
	for attempt := 0; ; attempt++ {
		// Asked here, on every pass, and not once per iteration: a
		// re-prompt is a model call like the first, and some passes come
		// back round without counting as an attempt.
		if err := overBudget(r.opts, r.emit); err != nil {
			return nil, exitReturn(RunResult{StopReason: StopReasonTokenLimit, Iterations: iter, Usage: r.total, State: r.sigma}, err)
		}
		req, ex := r.request(ctx, iter)
		if ex.ctl != loopProceed {
			return nil, ex
		}
		call, err := r.callModel(ctx, req)
		input := call.input
		switch ex := r.callFault(ctx, iter, err); ex.ctl {
		case loopNext:
			attempt--
			continue
		case loopReturn:
			return nil, ex
		}
		if err != nil {
			var ex loopExit
			input, err, ex = r.proseAsAnswer(iter, attempt, call, input, err)
			if ex.ctl != loopProceed {
				return nil, ex
			}
		}
		if err != nil {
			r.repromptProse(call)
			continue
		}
		parsed, ex := r.parseAnswer(iter, attempt, call, input)
		if ex.ctl == loopNext {
			continue
		}
		if ex.ctl != loopProceed {
			return nil, ex
		}
		if r.repromptEmptyTurn(iter, attempt, call, input, parsed) {
			continue
		}
		r.rec.firstStepSucceeded = true
		return parsed, loopExit{}
	}
}

// request builds one attempt's request: the context-transform chain over a
// copy, the forced emit_state call, the sampling, and the run's first request
// recorded once.
func (r *statefulLoop) request(ctx context.Context, iter int) (providers.Request, loopExit) {
	// RFC DG: say on the WIRE what the system prompt has only ever
	// asked for. Narrowing `tools` to emit_state was never enough —
	// the run that produced this RFC had exactly one tool offered and
	// replied in prose anyway.
	//
	// A provider without the parameter DROPS it and the step proceeds
	// unforced, which is why this is an optimisation and not a
	// precondition: the contract still lives in the prompt, and a
	// model that ignores it is still re-prompted rather than fatal.
	//
	// ⚠️ THE CONTEXT-TRANSFORM CHAIN RUNS HERE TOO, on a copy, exactly as
	// Run applies it to the append loop's request. This request used to
	// go out untransformed, so the `redact` plugin never saw a stateful
	// run — and Σ and the observation are where tool output, and any
	// secret in it, lands.
	reqSystem, reqMsgs := r.statefulSystem, r.msgs
	if len(r.opts.ContextPlugins) > 0 && r.opts.Provider.ID() != codeJSProviderID {
		cs, cm, perr := contextplugin.Apply(ctx, r.opts.ContextPlugins, r.statefulSystem, r.msgs)
		if perr != nil {
			r.emit(providers.Event{Type: providers.EventError, Error: "context transform: " + perr.Error()})
			return providers.Request{}, exitReturn(RunResult{StopReason: "error", Iterations: iter, Usage: r.total, State: r.sigma}, perr)
		}
		reqSystem, reqMsgs = cs, cm
	}
	req := providers.Request{Model: r.opts.Model, System: reqSystem, Messages: reqMsgs, Tools: r.emitTool,
		MaxTokens:        r.opts.MaxTokens,
		MaxContextTokens: r.opts.MaxContextTokens, // RFC CJ — without it Ollama ignored a per-agent context size
		Effort:           r.opts.Effort,
		ToolChoice:       providers.ToolChoice{Mode: providers.ToolChoiceTool, Name: emitStateToolName},
		OnEvent:          r.emit, // a driver's retry-while-rate-limited event reaches the stream
	}
	applyStatefulSampling(&req, r.opts.Sampling)
	if !r.promptSnapshotted {
		// RFC DI: the stateful loop's first request, as sent — its system
		// carries the state instructions the main loop never adds.
		r.promptSnapshotted = true
		r.emit(providers.Event{Type: providers.EventPromptSnapshot, PromptSnapshot: providers.NewPromptSnapshot(req.System, req.Messages)})
	}
	return req, loopExit{}
}

// callModel makes one attempt's model call and accounts for what it spent:
// the run's total, the footprint eviction measures, and a per-call usage event.
func (r *statefulLoop) callModel(ctx context.Context, req providers.Request) (emitStateCall, error) {
	call, err := callForEmitState(ctx, r.opts.Provider, req, r.opts.Now, r.opts.PriceCall)
	usage := call.usage
	addUsage(&r.total, usage)
	if usage != nil {
		r.total.Provider = r.opts.Provider.ID() // the SERVING provider, across a fallback
		// The provider's own count of the whole request, which is what
		// the eviction threshold must measure against — the same
		// numerator the append/recap gate uses.
		if in := usage.InputTokens + usage.CacheReadTokens + usage.CacheCreationTokens; in > 0 {
			r.lastIn = in
			r.footprintMeasured = true
		}
		r.lastWindow = effectiveWindow(usage.MaxContextTokens, r.opts)

		// ⚠️ THIS LOOP NEVER REPORTED A SINGLE TOKEN IT SPENT. It
		// accumulated `total` and attached it to the final EventDone,
		// and emitted no per-call EventUsage at all — which is the
		// event three separate subsystems key off:
		//
		//   the UI gauge        → a stateful run read "0 in / 0 out"
		//                         for its whole life
		//   the RFC AV ledger   → recordCallUsage never fired, so
		//                         token_usage had no rows, /v1/_usage
		//                         was blind to stateful runs, and
		//                         runs.cost summed an empty set to NULL
		//   the RFC AW budgets  → limits.Add never fired, so a
		//                         mode:stateful agent spent tokens that
		//                         counted against NO per-scope budget
		//
		// The last one is the reason this is not a display bug. An
		// operator who set a hard token limit was not protected from
		// this mode, and nothing said so.
		//
		// Stamped like the append loop's: the effective window so the
		// gauge has a denominator, and the SERVING provider so the
		// ledger records which key paid across a mid-run fallback.
		iterUsage := *usage
		iterUsage.MaxContextTokens = r.lastWindow
		iterUsage.Provider = r.opts.Provider.ID()
		r.emit(providers.Event{Type: providers.EventUsage, Usage: &iterUsage})
	}
	return call, err
}

// callFault answers a transport or provider fault: loopNext sends the same
// step again (not counted as an attempt), loopReturn ends the run. A call that
// came back clears the pair's retry count and stall, and proceeds.
func (r *statefulLoop) callFault(ctx context.Context, iter int, err error) loopExit {
	if err != nil && !errors.Is(err, errNoEmitState) {
		// Transport or provider fault — nothing the MODEL can correct, but
		// the append loop's recovery applies unchanged, and this loop used
		// to have none: one 429 or "overloaded" killed a stateful run that
		// an append run on the same agent would ride out. Neither retry
		// consumes a max_patch_retries attempt — that budget is for the
		// model's mistakes, and this was not one.
		switch recoverStatefulCall(ctx, &r.opts, &r.rec, err, r.emit, r.msgs) {
		case statefulCallRetry:
			return loopExit{ctl: loopNext}
		case statefulCallCancelled:
			return exitReturn(RunResult{StopReason: "cancelled", Iterations: iter, Usage: r.total, State: r.sigma}, ctx.Err())
		}
		r.emit(providers.Event{Type: providers.EventError, Error: "stateful step failed: " + err.Error()})
		return exitReturn(RunResult{StopReason: "error", Iterations: iter, Usage: r.total, State: r.sigma}, err)
	}
	// The call reached the provider and came back: the pair is healthy
	// enough to answer, whatever the model then did with the answer.
	r.rec.sameProviderRetries = 0
	if r.opts.ClearStall != nil {
		r.opts.ClearStall(r.opts.Provider.ID(), r.opts.Model)
	}
	return loopExit{}
}

// proseAsAnswer handles a reply that called no emit_state once the attempts
// are spent: its text becomes the final answer, or the run fails. Before
// then it returns err unchanged, and the caller re-prompts.
func (r *statefulLoop) proseAsAnswer(iter, attempt int, call emitStateCall, input json.RawMessage, err error) (json.RawMessage, error, loopExit) {
	// ⚠️ THE SAME BUDGET AS A REJECTED PATCH, and it used to get
	// none. `on_invalid_patch` / `max_patch_retries` covered a patch
	// that arrived and failed validation; a model that answered in
	// prose — the ordinary behaviour of a local model handed a
	// conversational question — got one shot and killed the run on
	// step zero, with its reply discarded unread.
	//
	// Both are the same condition from the runtime's side: the
	// output was not usable, and the model is the one who can fix
	// it. So it is re-prompted with what it actually said.
	if r.onInvalid == "fail" || attempt >= r.maxRetries {
		// ⚠️ PROSE IS AN ANSWER, once the model has been asked
		// for the call and still replied in words. Measured live
		// (ornith-1.5 behind chat/local): a step found the right
		// value, wrote "The value is Helix" as text, did so again
		// when re-prompted, and the run FAILED with the correct
		// answer discarded. The retries above still ask for
		// emit_state first; only when the budget is spent is the
		// last reply taken as `{done: true, final: <text>}`, which
		// then ends the turn exactly as a real final does — the
		// interactive park included. Not under on_invalid_patch:
		// fail, which asks for the strict behaviour. A reply with
		// no text still fails: there is nothing to show.
		if r.onInvalid != "fail" && call.text != "" {
			input, _ = json.Marshal(map[string]any{"done": true, "final": call.text})
			err = nil
		} else {
			msg := fmt.Sprintf("stateful step failed: model did not call emit_state after %d attempt(s) — %s. "+
				"context.mode: stateful requires a model that reliably emits tool calls; "+
				"raise context.max_patch_retries, or move this agent to a model that does%s",
				attempt+1, producedInstead(call), unforcedNote(r.opts))
			r.emit(providers.Event{Type: providers.EventError, Error: msg})
			return input, err, exitReturn(RunResult{StopReason: "error", Iterations: iter, Usage: r.total, State: r.sigma}, errors.New(msg))
		}
	}
	return input, err, loopExit{}
}

// repromptProse shows the model its own reply, then says what was required.
func (r *statefulLoop) repromptProse(call emitStateCall) {
	// Show it its own reply, then say what was required. An EMPTY
	// assistant turn is never appended — the providers reject one,
	// so a model that returned nothing gets the instruction alone.
	// A reasoning trace is not replayed either: it is not an
	// assistant turn the API will accept back.
	if call.text != "" {
		r.msgs = append(r.msgs, providers.Message{Role: "assistant",
			Content:   []providers.ContentBlock{{Type: "text", Text: call.text}},
			Reasoning: call.reasoning, ReasoningSignature: call.reasoningSig})
	}
	r.msgs = append(r.msgs, providers.Message{Role: "user",
		Content: []providers.ContentBlock{{Type: "text", Text: "That reply was not usable: in structured " +
			"execution mode the ONLY accepted response is a single `emit_state` tool call, and prose is " +
			"discarded. Send the same content again as `emit_state` — put your answer in `final` with " +
			"`done: true` if you are finished, otherwise name the next `action`."}}})
}

// parseAnswer parses and validates the emit_state. A bad one is sent back for
// a correction (loopNext) while attempts remain, and ends the run after.
func (r *statefulLoop) parseAnswer(iter, attempt int, call emitStateCall, input json.RawMessage) (*emitStateOut, loopExit) {
	parsed, perr := parseEmitState(input)
	var verr error
	if perr == nil {
		liftNestedProtocolFields(parsed, r.schema)
		verr = statepatch.ValidatePatch(r.schema, parsed.Patch)
	}
	if perr != nil || verr != nil {
		cause := perr
		if cause == nil {
			cause = verr
		}
		if r.onInvalid == "fail" || attempt >= r.maxRetries {
			msg := fmt.Sprintf("stateful patch rejected after %d attempt(s): %v", attempt+1, cause)
			r.emit(providers.Event{Type: providers.EventError, Error: msg})
			return nil, exitReturn(RunResult{StopReason: "invalid_patch", Iterations: iter, Usage: r.total, State: r.sigma}, errors.New(msg))
		}
		// Rollback-retry: show the model its rejected emit_state + the reason,
		// as a proper tool_use/tool_result pair, and ask for a correction.
		tid := retryToolUseID(call.callID, iter, attempt)
		r.msgs = append(r.msgs,
			providers.Message{Role: "assistant", Content: []providers.ContentBlock{{Type: "tool_use", ToolUseID: tid, ToolName: emitStateToolName, ToolInput: input}},
				Reasoning: call.reasoning, ReasoningSignature: call.reasoningSig},
			providers.Message{Role: "user", Content: []providers.ContentBlock{{Type: "tool_result", ToolUseID: tid, Text: "emit_state rejected: " + cause.Error() + ". Emit a corrected emit_state."}}})
		return nil, loopExit{ctl: loopNext}
	}
	return parsed, loopExit{}
}

// repromptEmptyTurn sends back a step that ends the turn with nothing to show,
// while attempts remain. It reports whether it did.
func (r *statefulLoop) repromptEmptyTurn(iter, attempt int, call emitStateCall, input json.RawMessage, parsed *emitStateOut) bool {
	// ⚠️ A TURN THAT ENDS WITH NOTHING TO SAY. A turn ends only when the
	// model says so — `done`, or an answer in `final` — and a step that
	// ends one with no `final` is sent back, on the same budget as a
	// rejected patch. Not treated as not-done: a model that cannot
	// produce `final` would then loop.
	//
	// Two shapes reach here, and they need different words:
	//
	//   - UNFINISHED: no action, no `done`, no `final` — only a patch and
	//     a reasoning that says what it will do next. Observed live on
	//     ornith-1.5:35b in 4 of 9 replays of one step: "I will run a web
	//     search for the exact figures", and no `action`. This used to END
	//     THE TURN and show that plan to the operator as the answer, so
	//     the chat stopped mid-task twice in a row.
	//   - FINISHED WITHOUT AN ANSWER: `done` with `final` empty. The
	//     reasoning fallback used to show the model's inner monologue
	//     ("Operator said continue. The state shows…") as the reply.
	//
	// Once the budget is spent the turn still ends and the reasoning
	// fallback applies, so a model that cannot comply is never stuck.
	if endsTurn(parsed) && strings.TrimSpace(parsed.Final) == "" &&
		r.onInvalid != "fail" && attempt < r.maxRetries {
		tid := retryToolUseID(call.callID, iter, attempt)
		why := "it ends your turn with no `final`, so nothing would be shown. Send it again with your answer in the top-level " +
			"`final` field of emit_state — next to `patch`, not inside it."
		if !parsed.Done && parsed.Action == nil {
			why = "it names no `action` and gives no `final`, so your turn would end with nothing shown. If you meant to run " +
				"a tool next — as your reasoning describes — name it in `action` as {\"tool\": <name>, \"input\": {…}}. If you are " +
				"finished, put your answer in the top-level `final` field with `done: true`."
		}
		r.msgs = append(r.msgs,
			providers.Message{Role: "assistant", Content: []providers.ContentBlock{{Type: "tool_use", ToolUseID: tid, ToolName: emitStateToolName, ToolInput: input}},
				Reasoning: call.reasoning, ReasoningSignature: call.reasoningSig},
			providers.Message{Role: "user", Content: []providers.ContentBlock{{Type: "tool_result", ToolUseID: tid, Text: "emit_state not accepted: " + why}}})
		return true
	}
	return false
}

// mergePatch merges the accepted patch into Σ and returns Σ's size before it.
func (r *statefulLoop) mergePatch(iter int, es *emitStateOut) int {
	sigmaBefore := sigmaTokens(r.sigma)
	r.sigma = statepatch.Merge(r.sigma, es.Patch)
	for k := range es.Patch {
		r.lastWritten[k] = iter
	}
	r.holder.Sigma = r.sigma
	return sigmaBefore
}

// evict drops Σ's least significant entries when the next request would be
// over the backstop, and returns the keys it dropped.
func (r *statefulLoop) evict(ctx context.Context, sigmaBefore int) []string {
	// ⚠️ STRUCTURAL COMPACTION. Summarising is the wrong operation on a
	// state object: Σ is validated against state_schema, and prose is not a
	// Σ — the next patch would have nothing well-formed to merge into. The
	// structural equivalent is EVICTION of the least significant entries.
	var evicted []string
	// ⚠️ lastIn MEASURED THE REQUEST THIS STEP ANSWERED, which carried the
	// previous Σ. A large patch merged just now was never weighed before
	// the next request went out, so one big write could take the prompt
	// past the window with no eviction first. The gate counts the growth;
	// what is REPORTED stays the provider's own number.
	gateIn := r.lastIn
	if grown := r.lastIn - sigmaBefore + sigmaTokens(r.sigma); grown > gateIn {
		gateIn = grown
	}
	if aboveBackstop(r.opts.Compaction, gateIn, r.lastWindow) && backstopAvailable(r.opts.Compaction) {
		// The budget is Σ's share of the window. The preamble is fixed for
		// the run and the observation is one step, so Σ is the only part
		// eviction can move.
		budget := r.lastWindow*compactionKeptTailBudgetPct/100 - r.preambleTokens
		if plan := sigmaEvictionPlan(r.sigma, r.schema, r.lastWritten, budget); len(plan) > 0 {
			// Bank BEFORE dropping. Σ is the run's working memory, and
			// silently losing it is worse than a large prompt — a later
			// recall must be able to fetch back what was evicted.
			bankEvictedSigma(ctx, r.opts, r.emit, r.sigma, plan)
			for _, k := range plan {
				delete(r.sigma, k)
				delete(r.lastWritten, k)
			}
			r.holder.Sigma = r.sigma
			evicted = plan
			r.lastIn = r.preambleTokens + sigmaTokens(r.sigma)
		} else if r.footprintMeasured && aboveBackstop(r.opts.Compaction, r.lastIn, r.lastWindow) {
			// Reported only on the provider's own number — the grown
			// estimate may open the gate, but it is not a finding.
			// Nothing evictable and still over: every key is core, or the
			// preamble alone is the problem. Either way the run is heading
			// for the provider's limit and must say so.
			reportExhausted(r.emit, r.seenExhaustedState, r.opts.Compaction, r.lastIn, r.lastWindow,
				[]providers.ContextTierVerdict{{
					Mode:   "stateful",
					Reason: providers.DistillDeclineSplitDeclined,
					Message: "stateful state cannot be reduced: every key is retention " +
						"core (the default) — declare x-retention: scratch or derived on " +
						"the state_schema properties that are safe to drop",
				}})
		}
	}
	return evicted
}

// record emits the step's context_state, then banks the step for recall and
// for persistent memory.
func (r *statefulLoop) record(ctx context.Context, iter int, es *emitStateOut, evicted []string) {
	// Model-proposed schema (RFC CR): recorded for the operator to review +
	// adopt (by forking the agent def's context.state_schema). INERT — it does
	// not change validation this run. Only surfaced when it differs from the
	// active schema, so an agent restating the adopted schema is not noise.
	var proposed map[string]any
	if len(es.ProposeSchema) > 0 && schemasDiffer(es.ProposeSchema, r.schema) {
		proposed = es.ProposeSchema
		r.lastProposed = es.ProposeSchema
	}
	r.emit(providers.Event{Type: providers.EventContextState,
		ContextState: &providers.ContextStateEventInfo{State: r.sigma, Patch: es.Patch, Iter: iter, Action: actionName(es), Reasoning: es.Reasoning, ProposedSchema: proposed, Evicted: evicted}})

	// Recall harvest (RFC CT): stateful is the most lossy mode — only Σ and the
	// latest observation are fed, so each step's reasoning + observation are
	// otherwise discarded. Embed them into the run-scoped index so a later
	// Recall(query) can fetch the original detail back. nil-safe when recall off.
	bankedObs := r.obs
	if r.notRetainedObs != "" && r.obs == r.notRetainedObs {
		bankedObs = ""
	}
	stepSpan := []providers.Message{
		{Role: "assistant", Reasoning: es.Reasoning,
			Content: []providers.ContentBlock{{Type: "text", Text: bankedObs}}},
	}
	r.opts.RecallIndex.Harvest(ctx, stepSpan)
	// Persistent-memory harvest (RFC CT P2): bank the same evicted step for the
	// consolidator when the agent opted in. No-op unless context.harvest_to_memory.
	harvestToMemory(ctx, r.opts, r.emit, stepSpan)
}

// endTurn is the turn boundary: done flag, or no action named. It proceeds
// when the step named an action; otherwise it parks or takes a late message
// (loopNext) or ends the run (loopReturn).
func (r *statefulLoop) endTurn(ctx context.Context, iter int, es *emitStateOut) loopExit {
	// Turn boundary: done flag, or no action named.
	if endsTurn(es) {
		final := es.Final
		if final == "" {
			// A fallback, and a bend in the contract that reasoning is
			// discarded — kept because an answer found only there is still
			// better shown than lost.
			final = es.Reasoning
		}
		if strings.TrimSpace(final) == "" {
			final = emptyTurnNote(es.Patch)
		}
		r.emit(providers.Event{Type: providers.EventText, Text: final})

		// ⚠️ RFC DH P1: AN INTERACTIVE RUN PARKS HERE INSTEAD OF ENDING, and
		// this one line is the whole of the reported bug. The loop reached
		// the model's `done` and returned, so a terminal chat stopped after
		// every answer no matter what `interactive` said — the flag reached
		// a loop with nowhere to put it.
		//
		// The park belongs at `done` and not at every step: the step loop
		// is internal machinery the operator never sees, and the thing they
		// wait for is the answer. That is the one place the two loops
		// already agreed, which is why this is a substitution rather than a
		// new concept.
		//
		// interactiveAtBoundary (not the static flag) so a run promoted or
		// demoted mid-flight is honoured, exactly as the append loop does.
		if r.opts.interactiveAtBoundary(ctx) && r.opts.SteerQueue != nil {
			next, resumed := parkForStatefulTurn(ctx, &r.opts, iter, r.emit)
			if resumed {
				r.obs, r.task = next, next
				r.memo.clearWork()
				return loopExit{ctl: loopNext}
			}
			// Cancelled while parked, or the queue closed: the run ends on
			// the turn it had already completed.
		} else {
			// A run that ends here reads its queue once more first: a
			// message sent while this answer was being produced was
			// acknowledged, so it is answered — as the next operator turn,
			// the way the park applies one — and that answer is the run's.
			var said []string
			if settleSteerAtFinish(&r.opts, func() bool {
				said = takeStatefulSteer(&r.opts, iter+1 < r.maxIter, r.emit)
				return len(said) > 0
			}) {
				next := statefulOperatorTurn(said, r.interactive)
				r.obs, r.task = next, next
				r.memo.clearWork()
				return loopExit{ctl: loopNext}
			}
		}
		stop := r.opts.endTurnOrIdle()
		r.finish(stop)
		return exitReturn(RunResult{StopReason: stop, FinalText: final, Iterations: iter + 1, Usage: r.total, State: r.sigma, ProposedSchema: r.lastProposed}, nil)
	}
	return loopExit{}
}

// action runs the step's named action; its output is the next observation.
// It returns loopNext, or loopReturn when the dispatcher says the run keeps
// repeating a call that cannot succeed.
func (r *statefulLoop) action(ctx context.Context, iter int, es *emitStateOut) loopExit {
	// Execute the named action → next observation.
	tid := fmt.Sprintf("es-%s-act-%d", r.actionIDTag, iter)
	switch {
	case r.opts.Dispatcher == nil:
		r.obs = renderToolResultText(statefulRefusal(tools.CategoryBusiness, "no tools are available to run action "+es.Action.Tool,
			"Omit `action` and put your answer in `final`."))
	// ⚠️ THE ACTION NAME WENT STRAIGHT TO THE DISPATCHER, unchecked. A model
	// that named something that is not a tool got back the dispatcher's
	// "tool not found: X" as its observation — an answer that says the name
	// was wrong and not one word about which names are right.
	//
	// Reported from a live chat: the model named `emit_state` as its
	// action. That is the worst case of the class, because emit_state is
	// not a tool at all — it is the channel the model is ALREADY speaking
	// through, offered as the only entry in `tools` on every step. Asking
	// to "run" it is a category error the runtime was in the best position
	// to name and instead forwarded to a lookup that could only miss.
	case es.Action.Tool == emitStateToolName:
		r.obs = emitActionRefusal(r.emit, tid, es.Action, statefulRefusal(tools.CategoryValidation,
			"`"+emitStateToolName+"` is not an action — it is how you reply.",
			"Every step you make exactly one "+emitStateToolName+" call; its `action` field names "+
				"a DIFFERENT tool for the runtime to run for you. "+statefulActionHint(r.toolSpecs)))
	case !offersTool(r.toolSpecs, es.Action.Tool):
		// Refused HERE rather than dispatched, so the observation can name
		// the alternatives. The dispatcher knows every tool in the process;
		// only this loop knows which ones THIS agent was offered.
		r.obs = emitActionRefusal(r.emit, tid, es.Action, statefulRefusal(tools.CategoryValidation,
			"no tool named `"+es.Action.Tool+"` is available to this agent.", statefulActionHint(r.toolSpecs)))
	case missingRequiredInput(r.toolSpecs, es.Action.Tool, es.Action.Input) != "":
		// Refused before dispatch, generally rather than per tool: the
		// schema already says what the input needs, and naming it here
		// costs the model one step instead of a guess. Observed live as
		// `{"tool":"Interruption","input":{}}` — a fumble of the same class
		// as naming emit_state, with a real tool.
		r.obs = emitActionRefusal(r.emit, tid, es.Action, statefulRefusal(tools.CategoryValidation,
			missingRequiredInput(r.toolSpecs, es.Action.Tool, es.Action.Input), ""))
	default:
		return r.dispatchAction(ctx, iter, tid, es)
	}
	return loopExit{ctl: loopNext}
}

// dispatchAction runs an offered tool through the append loop's dispatch and
// keeps its output as the next observation.
func (r *statefulLoop) dispatchAction(ctx context.Context, iter int, tid string, es *emitStateOut) loopExit {
	tu := providers.ToolUse{ID: tid, Name: es.Action.Tool, Input: es.Action.Input}
	r.emit(providers.Event{Type: providers.EventToolCall, ToolUse: &tu})
	// ⚠️ THROUGH THE APPEND LOOP'S DISPATCH, not the dispatcher directly.
	// This used to call Dispatcher.Execute, which skipped everything
	// executePendingTools adds around a call: the operator's Pre-hooks
	// (a deny did not apply to a stateful agent), Post-hooks, the
	// tool-use id the parallel_spawn ledger keys on, and the RFC DA
	// classification of a failure. It also emits the tool_result.
	hookIdent := HookIdentity(ctx, r.opts.AgentName, iter)
	// What the append loop stamps per iteration, so Context op=self in a
	// stateful action reports the provider/model it is actually running
	// on (after any fallback), its sampling, and how full the window is.
	// Stamped here rather than at the top of the step because a fallback
	// during this step's call can change the first two.
	actCtx := tools.WithResolvedProvider(r.dispatchCtx, r.opts.Provider.ID())
	actCtx = tools.WithResolvedModel(actCtx, r.opts.Model)
	actCtx = tools.WithResolvedSampling(actCtx, r.opts.Sampling)
	actCtx = tools.WithMaxContextTokens(actCtx, r.opts.MaxContextTokens)
	actCtx = tools.WithContextUsage(actCtx, r.lastIn, r.lastWindow)
	actCtx = tools.WithEffectiveContextWindow(actCtx, r.lastWindow)
	// Run lifted the cap to the hard ceiling exactly when the run is
	// iteration-unbounded, so the ceiling identifies that case here.
	actCtx = tools.WithIterationBudget(actCtx, iter+1, r.maxIter, r.maxIter >= maxIterationsHardCeiling)
	// A shutdown interrupts the action as it does the append loop's
	// tools, so the run parks at its next step (RFC ES F1).
	actCtx, stopInterrupt := interruptibleTools(actCtx, r.opts.PauseGate)
	blocks := executePendingTools(actCtx, r.opts.Dispatcher, []providers.ToolUse{tu}, 1, r.opts.Hooks, hookIdent, r.emit)
	stopInterrupt()
	// A failure is already the structured error object, isError and all.
	r.obs = blocks[0].Text
	r.notRetainedObs = ""
	if blocks[0].NotRetained {
		r.notRetainedObs = r.obs
	}
	switch {
	case r.helpTool != "" && tu.Name == r.helpTool:
		if !blocks[0].IsError {
			r.memo.add(tu.Input, r.obs)
		}
	default:
		r.memo.addWork(tu.Name, tu.Input, r.obs, blocks[0].IsError)
	}
	if why, stop := r.opts.Dispatcher.RepeatedFailure(); stop {
		msg := "run stopped: " + why + " after being told it cannot succeed as sent"
		r.emit(providers.Event{Type: providers.EventError, Error: msg})
		return exitReturn(RunResult{StopReason: StopReasonRepeatedFailedCall, Iterations: iter + 1, Usage: r.total, State: r.sigma}, errors.New(msg))
	}
	return loopExit{ctl: loopNext}
}
