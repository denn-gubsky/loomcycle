// One platform conversation driving one loomcycle interactive run (RFC EN
// §5-6, generalised by RFC EQ).
//
// Messages are handled one at a time per conversation. The run's event stream
// is consumed in the background and rendered turn by turn. The stream is never
// re-attached: loomcycle's run stream has no resume position, so a re-attach
// would replay the whole conversation into the chat. Instead, when the bridge
// has no live stream for a conversation (a restart, a dropped connection, an
// idle cancel), the next message continues the stored loomcycle SESSION in a
// fresh run, which gives the agent its full history and sends nothing twice.

import { randomBytes } from "node:crypto";

import type { AgentEvent, PromptContent, PromptSegment } from "@loomcycle/client";

import { isButtonInput, optionLabels, VERB, type CanonicalCard, type CardAction, type CardButton, type CardInput } from "./cards.js";
import type { Health } from "./health.js";
import type { Speaker } from "./identity.js";
import type { PlatformCapabilities, PlatformInfo } from "./platform.js";
import type { BridgePort, ClientFactory, Logger, RunClient } from "./ports.js";
import { MessageSink, StreamSink, type MessageSinkOptions, type StreamSinkOptions, type TurnSink } from "./renderer.js";
import type { ConversationRecord, ConversationStore } from "./store.js";
import { HELP_TEXT, parseCommand } from "./text.js";
import { statusOf, type TokenBroker } from "./tokens.js";

export interface ConversationDeps<TRef> {
  platform: PlatformInfo;
  capabilities: PlatformCapabilities;
  agent: string;
  store: ConversationStore<TRef>;
  broker: TokenBroker;
  clients: ClientFactory;
  port: BridgePort<TRef>;
  log: Logger;
  health?: Health;
  now?: () => number;
  stream?: StreamSinkOptions;
  message?: MessageSinkOptions;
}

/** What a card action is answered with: a replacement card, or a short
 *  message that leaves the card as it was. */
export type CardResult = { card: CanonicalCard } | { message: string };

/** The untrusted-block label for what a chat participant wrote. */
export const UNTRUSTED_KIND = "user_input";

/** What an untrusted owner hears when a question or review needs typed text. */
export const UNTRUSTED_TYPED_ANSWER_TEXT =
  "This needs a typed answer, which can't be accepted in this chat. Choose an option on the card, or decline it.";

/** At most this many untrusted messages wait for a turn to finish. */
export const MAX_DEFERRED = 20;

interface Live {
  abort: AbortController;
  runId?: string;
  agentId?: string;
  /** The run has finished its turn and waits for the next message. */
  parked: boolean;
  started: Promise<void>;
  markStarted: () => void;
}

interface PendingQuestion {
  runId: string;
  interruptId: string;
  options: string[];
}

/** friendlyError turns a failed run start into something a chat user can act on. */
export function friendlyError(status: number | undefined): string {
  switch (status) {
    case 429:
      return "You're sending messages faster than the agent accepts them. Please wait a minute and try again.";
    case 403:
      return "The agent isn't available to you. Ask your administrator to check the bot's configuration.";
    case 401:
      return "I couldn't sign you in to the agent. Please try again.";
    default:
      return "I couldn't reach the agent. Please try again in a moment.";
  }
}

export function resultCard(message: string): CanonicalCard {
  return { kind: "result", message };
}

export class Conversation<TRef> {
  private live?: Live;
  private sink?: TurnSink;
  private pending?: PendingQuestion;
  /** The actions behind the buttons of the open card, by button id. A restart
   *  ends the live run (see the header), so these need not outlive the process. */
  private buttons = new Map<string, CardAction>();
  /** Untrusted messages that arrived mid-turn, delivered when the turn ends. */
  private deferred: string[] = [];
  private limitShown = false;
  private queue: Promise<unknown> = Promise.resolve();
  private readonly now: () => number;

  constructor(
    private rec: ConversationRecord<TRef>,
    private readonly deps: ConversationDeps<TRef>,
  ) {
    this.now = deps.now ?? Date.now;
  }

  get record(): Readonly<ConversationRecord<TRef>> {
    return this.rec;
  }

  /** True while a run's stream is being consumed. */
  get hasLiveRun(): boolean {
    return this.live !== undefined;
  }

  /** The most recent inbound reference, so replies follow a moved address. */
  updateRef(ref: TRef): void {
    this.rec.ref = ref;
  }

  onMessage(speaker: Speaker, text: string): Promise<void> {
    return this.serial(() => this.handleMessage(speaker, text));
  }

  onCardAction(speaker: Speaker, input: CardInput): Promise<CardResult> {
    return this.serial(() => this.handleCardAction(speaker, input));
  }

  /** endIfIdle cancels the run when nobody has written for idleMs. The session
   *  is kept, so the next message continues the conversation. */
  endIfIdle(idleMs: number): Promise<void> {
    return this.serial(async () => {
      if (!this.live || this.now() - this.rec.lastActivityAt < idleMs) return;
      this.deps.log.info("conversation idle, ending run", { conversation: this.rec.conversationId, run: this.live.runId });
      await this.endRun("idle");
    });
  }

  /** stop ends the run without starting another (bridge shutdown). */
  stop(): Promise<void> {
    return this.serial(async () => {
      // On shutdown only drop our stream: the run stays parked in loomcycle
      // and the next process continues the session.
      this.live?.abort.abort();
      this.live = undefined;
    });
  }

  private serial<T>(fn: () => Promise<T>): Promise<T> {
    const p = this.queue.then(fn, fn);
    this.queue = p.catch(() => undefined);
    return p;
  }

  private reason(what: string): string {
    return `${this.deps.platform.name}-bridge: ${what}`;
  }

  /** runClient is loomcycle as the conversation's run identity: the person in
   *  a 1:1 chat, the conversation itself in a shared one. */
  private async runClient(): Promise<RunClient> {
    const token = await this.deps.broker.tokenFor(this.rec.runSubject, this.rec.runName);
    this.deps.health?.loomcycle();
    return this.deps.clients(token);
  }

  private async save(): Promise<void> {
    await this.deps.store.put(this.rec);
  }

  private async say(text: string): Promise<void> {
    await this.deps.port.send(this.rec.ref, { type: "message", text });
  }

  private async handleMessage(speaker: Speaker, raw: string): Promise<void> {
    const text = raw.trim();
    if (!text) return;
    this.rec.lastActivityAt = this.now();
    const isOwner = speaker.subject === this.rec.ownerSubject;

    const command = parseCommand(text);
    // In a shared chat the run is the owner's: another member may talk to
    // the agent, but not stop it or throw the conversation away.
    if ((command === "new" || command === "stop") && !isOwner) {
      await this.say(`Only ${this.rec.ownerName}, who started this conversation, can do that.`);
      return;
    }
    switch (command) {
      case "help":
        await this.say(HELP_TEXT);
        return;
      case "stop":
        if (!this.live?.runId || !(await this.cancelTurn("stopped by /stop"))) await this.say("Nothing is running.");
        return;
      case "new":
        await this.endRun("new conversation");
        this.rec.sessionId = undefined;
        this.rec.ownerSubject = speaker.subject;
        this.rec.ownerName = speaker.name;
        await this.save();
        await this.say("Starting fresh: your next message begins a new conversation.");
        return;
    }

    // A plain reply from the owner answers an open free-text question.
    if (this.pending && isOwner && this.pending.runId === this.live?.runId) {
      if (this.pending.options.length > 0) {
        await this.say("Please choose one of the options on the card above.");
        return;
      }
      // An answer reaches the agent as the question's result, not as an
      // untrusted block, so an untrusted speaker cannot type one.
      if (speaker.trust === "untrusted") {
        await this.say(UNTRUSTED_TYPED_ANSWER_TEXT);
        return;
      }
      const p = this.pending;
      this.pending = undefined;
      this.buttons.clear();
      await this.resolve(p, text, speaker.subject);
      await this.save();
      return;
    }

    // A shared chat runs as itself, so the agent needs every speaker named.
    const forAgent = this.rec.scope !== "personal" ? `[${speaker.name}] ${text}` : text;

    if (speaker.trust === "untrusted") {
      await this.deliverUntrusted(forAgent);
      return;
    }

    if (this.live?.runId) {
      try {
        const client = await this.runClient();
        await client.sendRunInput(this.live.runId, forAgent);
        this.live.parked = false;
        await this.save();
        return;
      } catch (err) {
        const s = statusOf(err);
        if (s !== 404 && s !== 409) throw err;
        // The run ended between its last event and this message.
        this.dropLive();
      }
    }
    await this.startRun({ type: "trusted-text", text: forAgent });
  }

  /** deliverUntrusted gives the agent an untrusted speaker's text as an
   *  untrusted block. A live run's input takes only trusted text, so the text
   *  is never sent into one: a parked run is ended and its session continued
   *  with the block; a run mid-turn gets it once the turn ends. */
  private async deliverUntrusted(text: string): Promise<void> {
    const live = this.live;
    if (live?.runId && !live.parked) {
      if (this.deferred.length >= MAX_DEFERRED) {
        await this.say("The agent is still answering. Please wait for it to finish, then send your message again.");
        return;
      }
      this.deferred.push(text);
      await this.save();
      return;
    }
    await this.restartWith(text);
  }

  /** restartWith ends the parked run, if any, and continues the session in a
   *  new run whose prompt is text as an untrusted block. */
  private async restartWith(text: string): Promise<void> {
    if (this.live) {
      const agentId = this.live.agentId;
      this.dropLive();
      await this.finishTurn().catch(() => undefined);
      // startRun ends the run named here before it continues the session.
      this.rec.agentId = agentId;
    }
    await this.startRun({ type: "untrusted-block", kind: UNTRUSTED_KIND, text });
  }

  /** flushDeferred delivers the untrusted messages that waited for a turn. */
  private flushDeferred(): Promise<void> {
    return this.serial(async () => {
      if (this.deferred.length === 0) return;
      if (this.live?.runId && !this.live.parked) return;
      const text = this.deferred.splice(0).join("\n\n");
      await this.restartWith(text);
    });
  }

  private async startRun(block: PromptContent): Promise<void> {
    const client = await this.runClient();
    // A run left by an earlier process, or ended above, is not ours to drive:
    // end it, so two runs never write into one session.
    if (this.rec.agentId) {
      await client.cancelAgent(this.rec.agentId, { reason: this.reason("continued in a new run") }).catch(() => undefined);
      this.rec.agentId = undefined;
      this.rec.runId = undefined;
    }
    const segments: PromptSegment[] = [{ role: "user", content: [block] }];
    const abort = new AbortController();
    let markStarted!: () => void;
    const started = new Promise<void>((r) => (markStarted = r));
    const live: Live = { abort, parked: false, started, markStarted };
    this.live = live;
    this.limitShown = false;
    const events = this.rec.sessionId
      ? client.continueSession({ sessionId: this.rec.sessionId, segments, interactive: true, signal: abort.signal })
      : client.runStreaming({ agent: this.deps.agent, segments, interactive: true, signal: abort.signal });
    void this.pump(events, live);
    // Later messages must see the run id, so wait for the run to announce it.
    await started;
  }

  private async pump(events: AsyncIterable<AgentEvent>, live: Live): Promise<void> {
    try {
      for await (const ev of events) {
        if (this.live !== live) break;
        this.deps.health?.loomcycle();
        await this.onEvent(ev, live);
      }
    } catch (err) {
      if (!live.abort.signal.aborted && this.live === live) {
        this.deps.log.error("run stream failed", {
          conversation: this.rec.conversationId,
          run: live.runId,
          status: statusOf(err),
          error: (err as Error)?.name,
        });
        if (statusOf(err) === 401) this.deps.broker.forget(this.rec.runSubject);
        await this.finishTurn(friendlyError(statusOf(err)), true).catch(() => undefined);
      }
    } finally {
      live.markStarted();
      if (this.live === live) {
        this.live = undefined;
        this.pending = undefined;
        this.buttons.clear();
        await this.finishTurn().catch(() => undefined);
        this.rec.runId = undefined;
        this.rec.agentId = undefined;
        await this.save().catch(() => undefined);
        if (this.deferred.length > 0) void this.flushDeferred().catch(() => undefined);
      }
    }
  }

  private canStream(): boolean {
    const mode = this.deps.capabilities.streaming;
    if (!this.deps.port.createStream || mode === "never") return false;
    return mode === "always" || this.rec.scope === "personal";
  }

  private turn(): TurnSink {
    if (!this.sink) {
      const { port } = this.deps;
      const open = port.createStream?.bind(port);
      this.sink =
        open && this.canStream()
          ? new StreamSink(open, port, this.rec.ref, {
              ...this.deps.stream,
              now: this.deps.stream?.now ?? this.now,
              onStop: () => void this.cancelTurn(`stopped in ${this.deps.platform.label}`),
            })
          : new MessageSink(port, this.rec.ref, { maxMessageChars: this.deps.capabilities.maxMessageChars, ...this.deps.message });
    }
    return this.sink;
  }

  /** finishTurn ends the open turn. note is shown in it; with always, a note is
   *  sent even when no turn is open. */
  private async finishTurn(note?: string, always = false): Promise<void> {
    const s = this.sink;
    this.sink = undefined;
    if (s) await s.finish(note);
    else if (note && always) await this.say(note);
  }

  /** drawButtons replaces the open card's buttons. */
  private drawButtons(actions: Array<{ label: string; action: CardAction }>): CardButton[] {
    this.buttons.clear();
    return actions.map(({ label, action }) => {
      const id = randomBytes(6).toString("base64url");
      this.buttons.set(id, action);
      return { id, label, action };
    });
  }

  private async onEvent(ev: AgentEvent, live: Live): Promise<void> {
    switch (ev.type) {
      case "session":
        if (ev.session_id) {
          this.rec.sessionId = ev.session_id;
          await this.save();
        }
        return;
      case "agent":
        // Only the run's own frame: a sub-agent's has a parent.
        if (ev.parent_agent_id || !ev.run_id) return;
        live.runId = ev.run_id;
        live.agentId = ev.agent_id;
        this.rec.runId = ev.run_id;
        this.rec.agentId = ev.agent_id;
        if (ev.session_id) this.rec.sessionId = ev.session_id;
        await this.save();
        live.markStarted();
        return;
      case "text":
        if (ev.text) await this.turn().text(ev.text);
        return;
      case "tool_call":
        this.turn().tool(ev.tool_use?.name ?? "a tool");
        return;
      case "limit":
        if (!this.limitShown && ev.limit) {
          this.limitShown = true;
          await this.turn().notice(ev.limit.message || "This conversation is close to its token budget.");
        }
        return;
      case "interruption_pending": {
        const intr = ev.interruption;
        if (!intr || !live.runId) return;
        await this.finishTurn();
        const options = optionLabels(intr.options);
        const runId = live.runId;
        const interruptId = intr.interrupt_id;
        this.pending = { runId, interruptId, options };
        const base = { runId, interruptId };
        const buttons = this.drawButtons([
          ...(options.length > 0
            ? options.map((o) => ({ label: o, action: { verb: VERB.answer, ...base, answer: o } }))
            : [{ label: "Send", action: { verb: VERB.answer, ...base } }]),
          { label: "Decline", action: { verb: VERB.decline, ...base } },
        ]);
        const card: CanonicalCard = {
          kind: "question",
          runId,
          interruptId,
          question: intr.question ?? "",
          context:
            this.rec.scope === "personal" ? intr.context : [intr.context, `Only ${this.rec.ownerName} can answer.`].filter(Boolean).join("\n\n"),
          options,
          buttons,
        };
        await this.deps.port.send(this.rec.ref, { type: "message", card });
        return;
      }
      case "awaiting_review": {
        if (!live.runId) return;
        await this.finishTurn();
        const base = { runId: live.runId };
        const buttons = this.drawButtons([
          { label: "Approve", action: { verb: VERB.approve, ...base } },
          { label: "Send back", action: { verb: VERB.reject, ...base } },
        ]);
        await this.deps.port.send(this.rec.ref, {
          type: "message",
          card: { kind: "review", runId: live.runId, round: ev.awaiting_review?.round, buttons },
        });
        return;
      }
      case "awaiting_input":
        live.parked = true;
        await this.finishTurn();
        if (this.deferred.length > 0) void this.flushDeferred().catch(() => undefined);
        return;
      case "turn_cancelled":
        await this.finishTurn("_Stopped._");
        return;
      case "error":
        this.deps.log.warn("run reported an error", { conversation: this.rec.conversationId, run: live.runId });
        await this.finishTurn(`The agent stopped with an error${live.runId ? ` (run ${live.runId})` : ""}.`, true);
        return;
      default:
        return;
    }
  }

  /** actionOf turns a card input into the action behind it. A button id the
   *  open card did not draw is no action at all.
   *
   *  An answer and feedback reach the agent as trusted text, so from an
   *  untrusted speaker only what the bridge drew is taken: a button by its
   *  id, with an option the agent itself offered, and nothing typed. */
  private actionOf(input: CardInput, speaker: Speaker): CardAction | undefined {
    const typed = speaker.trust === "trusted";
    if (!isButtonInput(input)) {
      if (!typed) return undefined;
      return input;
    }
    const drawn = this.buttons.get(input.buttonId);
    if (!drawn) return undefined;
    return {
      ...drawn,
      answer: drawn.answer ?? (typed ? input.answer?.trim() || undefined : undefined),
      feedback: drawn.feedback ?? (typed ? input.feedback?.trim() || undefined : undefined),
    };
  }

  private async handleCardAction(speaker: Speaker, input: CardInput): Promise<CardResult> {
    if (speaker.subject !== this.rec.ownerSubject) {
      return { message: `Only ${this.rec.ownerName} can answer this.` };
    }
    const action = this.actionOf(input, speaker);
    if (!action || !this.live?.runId || action.runId !== this.live.runId) {
      return { card: resultCard("This is no longer open.") };
    }
    this.rec.lastActivityAt = this.now();
    const runId = action.runId;
    try {
      const client = await this.runClient();
      switch (action.verb) {
        case VERB.answer: {
          if (!action.interruptId) return { card: resultCard("This is no longer open.") };
          if (!action.answer) return { message: speaker.trust === "untrusted" ? UNTRUSTED_TYPED_ANSWER_TEXT : "Type an answer first." };
          this.pending = undefined;
          this.buttons.clear();
          await client.resolveInterrupt(runId, action.interruptId, { answer: action.answer, resolvedBy: speaker.subject });
          return { card: resultCard(`Answered: ${action.answer}`) };
        }
        case VERB.decline:
          if (!action.interruptId) return { card: resultCard("This is no longer open.") };
          this.pending = undefined;
          this.buttons.clear();
          await client.cancelInterrupt(runId, action.interruptId, { resolvedBy: speaker.subject });
          return { card: resultCard("Declined.") };
        case VERB.approve:
          this.buttons.clear();
          await client.reviewRun(runId, "approve");
          return { card: resultCard("Approved.") };
        case VERB.reject:
          this.buttons.clear();
          await client.reviewRun(runId, "reject", action.feedback ? { feedback: action.feedback } : {});
          return { card: resultCard(action.feedback ? "Sent back with your feedback." : "Rejected.") };
      }
    } catch (err) {
      const s = statusOf(err);
      if (s === 404 || s === 409) return { card: resultCard("This is no longer open.") };
      this.deps.log.error("card action failed", { conversation: this.rec.conversationId, run: runId, status: s });
      return { message: "Could not reach the agent. Please try again." };
    } finally {
      await this.save().catch(() => undefined);
    }
  }

  private async resolve(p: PendingQuestion, answer: string, by: string): Promise<void> {
    try {
      const client = await this.runClient();
      await client.resolveInterrupt(p.runId, p.interruptId, { answer, resolvedBy: by });
    } catch (err) {
      const s = statusOf(err);
      if (s === 404 || s === 409) await this.say("That question is no longer open.");
      else throw err;
    }
  }

  /** cancelTurn stops the turn in progress. It reports false when there was
   *  none: the run was parked, waiting for the next message. */
  private async cancelTurn(reason: string): Promise<boolean> {
    const runId = this.live?.runId;
    if (!runId) return false;
    try {
      const client = await this.runClient();
      await client.cancelTurn(runId, { reason });
      return true;
    } catch (err) {
      // 409: the run was not mid-turn. Nothing to stop.
      if (statusOf(err) === 409) return false;
      this.deps.log.warn("cancel turn failed", { conversation: this.rec.conversationId, run: runId, status: statusOf(err) });
      return true;
    }
  }

  private dropLive(): void {
    this.live?.abort.abort();
    this.live = undefined;
    this.pending = undefined;
    this.buttons.clear();
    this.rec.runId = undefined;
    this.rec.agentId = undefined;
  }

  private async endRun(reason: string): Promise<void> {
    const agentId = this.live?.agentId ?? this.rec.agentId;
    this.dropLive();
    this.deferred = [];
    await this.finishTurn().catch(() => undefined);
    if (agentId) {
      const client = await this.runClient();
      await client.cancelAgent(agentId, { reason: this.reason(reason) }).catch(() => undefined);
    }
    await this.save();
  }
}
