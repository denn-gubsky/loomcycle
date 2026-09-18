import type { AgentEvent, RunOverrideOptions } from "./types.js";

/** The client-side operations an {@link InteractiveSession} routes through.
 *  Supplied by LoomcycleClient.interactiveSession / attachInteractiveSession —
 *  the session itself holds no transport logic. */
export interface InteractiveSessionOps {
  sendRunInput: (
    runId: string,
    text: string,
    opts?: { overrides?: RunOverrideOptions },
  ) => Promise<{ delivered: boolean }>;
  retuneRun: (
    runId: string,
    overrides: RunOverrideOptions,
  ) => Promise<{ retuned: boolean }>;
  cancelAgent: (agentId: string) => Promise<unknown>;
}

/** A high-level driver for an interactive agentic session (RFC AI) — the
 *  adapter port of the Web UI's run terminal. It wraps the underlying event
 *  stream (a fresh `interactive` run, or a `streamRunByID` re-attach), taps
 *  each frame to track the run's tracking IDs + parked state, and routes
 *  operator input + cancel through the client.
 *
 *  Typical loop:
 *  ```ts
 *  const sess = client.interactiveSession({ agent: "chat/medium", segments: [...] });
 *  for await (const ev of sess.events()) {
 *    if (ev.type === "text") process.stdout.write(ev.text ?? "");
 *    if (ev.type === "awaiting_input") await sess.send(await prompt("you> "));
 *  }
 *  ```
 *  `send()` does NOT open a new stream — the operator's turn and the model's
 *  response arrive on the same `events()` iterator. */
export class InteractiveSession {
  /** The run's id, from the first `agent` frame (set up-front on re-attach). */
  runId = "";
  /** The run's agent_id (for cancel), from the first `agent` frame. */
  agentId = "";
  /** The run's session_id, from the `session` / `agent` frames. */
  sessionId = "";
  /** True after an `awaiting_input` frame; cleared on the next activity. */
  awaitingInput = false;

  constructor(
    private readonly source: AsyncIterable<AgentEvent>,
    private readonly ops: InteractiveSessionOps,
  ) {}

  /** The merged event stream. Consume with `for await`. Each frame updates the
   *  session's runId / agentId / sessionId / awaitingInput BEFORE it is
   *  yielded, so a consumer that reacts to `awaiting_input` can immediately
   *  `send()`. */
  async *events(): AsyncIterable<AgentEvent> {
    for await (const ev of this.source) {
      if (ev.type === "agent") {
        if (ev.run_id) this.runId = ev.run_id;
        if (ev.agent_id) this.agentId = ev.agent_id;
        if (ev.session_id) this.sessionId = ev.session_id;
      } else if (ev.type === "session" && ev.session_id) {
        this.sessionId = ev.session_id;
      }
      this.awaitingInput = ev.type === "awaiting_input";
      yield ev;
    }
  }

  /** Steer the live run with an operator message. The response arrives on the
   *  `events()` stream (NOT a new stream). Returns the server's `delivered`
   *  flag. Throws if the run_id isn't known yet — for a fresh session, consume
   *  `events()` until the `agent` frame (or the first `awaiting_input`) first;
   *  a re-attached session has the run_id up front. */
  async send(
    text: string,
    opts?: { overrides?: RunOverrideOptions },
  ): Promise<boolean> {
    if (!this.runId) {
      throw new Error(
        "loomcycle: run_id not known yet — consume events() until the `agent` frame before send()",
      );
    }
    const { delivered } = await this.ops.sendRunInput(this.runId, text, opts);
    this.awaitingInput = false;
    return delivered;
  }

  /** Change this run's settings WITHOUT sending it a turn — switch a parked
   *  chat to another model, raise its iteration bound.
   *
   *  `send(text, { overrides })` is the other half: retune and speak in one
   *  atomic call. Use THIS one when a message in the transcript would be an
   *  artefact of changing a setting rather than something the operator said.
   *
   *  Does not clear `awaitingInput`: a parked run is still parked after a
   *  retune, because nothing was delivered for it to answer. */
  async retune(overrides: RunOverrideOptions): Promise<boolean> {
    if (!this.runId) {
      throw new Error(
        "loomcycle: run_id not known yet — consume events() until the `agent` frame before retune()",
      );
    }
    const { retuned } = await this.ops.retuneRun(this.runId, overrides);
    return retuned;
  }

  /** Cancel the run (and its sub-agents). No-op if the agent_id isn't known
   *  yet (nothing to cancel). */
  async cancel(): Promise<void> {
    if (this.agentId) await this.ops.cancelAgent(this.agentId);
  }
}
