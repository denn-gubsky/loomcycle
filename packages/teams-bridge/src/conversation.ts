// One Teams conversation driving one loomcycle interactive run (RFC EN §5-6).
//
// Messages are handled one at a time per conversation. The run's event stream
// is consumed in the background and rendered turn by turn. The stream is never
// re-attached: loomcycle's run stream has no resume position, so a re-attach
// would replay the whole conversation into Teams. Instead, when the bridge has
// no live stream for a conversation (a restart, a dropped connection, an idle
// cancel), the next message continues the stored loomcycle SESSION in a fresh
// run, which gives the agent its full history and sends nothing twice.

import type { AgentEvent, PromptSegment } from "@loomcycle/client";

import { optionLabels, questionCard, resultCard, reviewCard, VERB, type Card, type CardAction } from "./cards.js";
import type { Speaker } from "./identity.js";
import type { ClientFactory, ConversationRef, Logger, RunClient, TeamsPort } from "./ports.js";
import { MessageSink, StreamSink, type MessageSinkOptions, type StreamSinkOptions, type TurnSink } from "./renderer.js";
import type { ConversationRecord, ConversationStore } from "./store.js";
import { HELP_TEXT, parseCommand } from "./text.js";
import { statusOf, type TokenBroker } from "./tokens.js";

export interface ConversationDeps {
  agent: string;
  store: ConversationStore;
  broker: TokenBroker;
  clients: ClientFactory;
  port: TeamsPort;
  log: Logger;
  now?: () => number;
  stream?: StreamSinkOptions;
  message?: MessageSinkOptions;
}

/** What a card click is answered with: a replacement card, or a short
 *  message that leaves the card as it was. */
export type CardResult = { card: Card } | { message: string };

interface Live {
  abort: AbortController;
  runId?: string;
  agentId?: string;
  started: Promise<void>;
  markStarted: () => void;
}

interface PendingQuestion {
  runId: string;
  interruptId: string;
  options: string[];
}

/** friendlyError turns a failed run start into something a Teams user can act on. */
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

export class Conversation {
  private live?: Live;
  private sink?: TurnSink;
  private pending?: PendingQuestion;
  private limitShown = false;
  private queue: Promise<unknown> = Promise.resolve();
  private readonly now: () => number;

  constructor(
    private rec: ConversationRecord,
    private readonly deps: ConversationDeps,
  ) {
    this.now = deps.now ?? Date.now;
  }

  get record(): Readonly<ConversationRecord> {
    return this.rec;
  }

  /** True while a run's stream is being consumed. */
  get hasLiveRun(): boolean {
    return this.live !== undefined;
  }

  /** The most recent inbound reference, so replies follow a moved service URL. */
  updateRef(ref: ConversationRef): void {
    this.rec.ref = ref;
  }

  onMessage(speaker: Speaker, text: string): Promise<void> {
    return this.serial(() => this.handleMessage(speaker, text));
  }

  onCardAction(speaker: Speaker, action: CardAction): Promise<CardResult> {
    return this.serial(() => this.handleCardAction(speaker, action));
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

  private ownerClient(): Promise<RunClient> {
    return this.deps.broker
      .tokenFor(this.rec.ownerSubject, this.rec.ownerName)
      .then((token) => this.deps.clients(token));
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
        if (this.live?.runId) await this.cancelTurn("stopped by /stop");
        else await this.say("Nothing is running.");
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
      const p = this.pending;
      this.pending = undefined;
      await this.resolve(p, text, speaker.subject);
      await this.save();
      return;
    }

    // In a shared chat the agent must know who is speaking, and that it is
    // not necessarily the person whose data and tools the run uses.
    const forAgent =
      this.rec.scope !== "personal" && !isOwner
        ? `[Message from ${speaker.name}, another member of this ${this.rec.scope === "channel" ? "channel" : "chat"}; not ${this.rec.ownerName}, who owns this conversation]\n${text}`
        : text;

    if (this.live?.runId) {
      try {
        const client = await this.ownerClient();
        await client.sendRunInput(this.live.runId, forAgent);
        await this.save();
        return;
      } catch (err) {
        const s = statusOf(err);
        if (s !== 404 && s !== 409) throw err;
        // The run ended between its last event and this message.
        this.dropLive();
      }
    }
    await this.startRun(forAgent);
  }

  private async startRun(text: string): Promise<void> {
    const client = await this.ownerClient();
    // A run left by an earlier process is not ours to drive: end it, so two
    // runs never write into one session.
    if (this.rec.agentId) {
      await client.cancelAgent(this.rec.agentId, { reason: "teams-bridge: continued in a new run" }).catch(() => undefined);
      this.rec.agentId = undefined;
      this.rec.runId = undefined;
    }
    const segments: PromptSegment[] = [{ role: "user", content: [{ type: "trusted-text", text }] }];
    const abort = new AbortController();
    let markStarted!: () => void;
    const started = new Promise<void>((r) => (markStarted = r));
    const live: Live = { abort, started, markStarted };
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
        if (statusOf(err) === 401) this.deps.broker.forget(this.rec.ownerSubject);
        await this.finishTurn(friendlyError(statusOf(err)), true).catch(() => undefined);
      }
    } finally {
      live.markStarted();
      if (this.live === live) {
        this.live = undefined;
        this.pending = undefined;
        await this.finishTurn().catch(() => undefined);
        this.rec.runId = undefined;
        this.rec.agentId = undefined;
        await this.save().catch(() => undefined);
      }
    }
  }

  private turn(): TurnSink {
    if (!this.sink) {
      const { port } = this.deps;
      this.sink =
        this.rec.scope === "personal"
          ? new StreamSink(port, this.rec.ref, {
              ...this.deps.stream,
              now: this.deps.stream?.now ?? this.now,
              onStop: () => void this.cancelTurn("stopped in Teams"),
            })
          : new MessageSink(port, this.rec.ref, this.deps.message);
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
        this.pending = { runId: live.runId, interruptId: intr.interrupt_id, options };
        const card = questionCard({
          runId: live.runId,
          interruptId: intr.interrupt_id,
          question: intr.question ?? "",
          context:
            this.rec.scope === "personal" ? intr.context : [intr.context, `Only ${this.rec.ownerName} can answer.`].filter(Boolean).join("\n\n"),
          options,
        });
        await this.deps.port.send(this.rec.ref, { type: "message", card });
        return;
      }
      case "awaiting_review":
        if (!live.runId) return;
        await this.finishTurn();
        await this.deps.port.send(this.rec.ref, {
          type: "message",
          card: reviewCard({ runId: live.runId, round: ev.awaiting_review?.round }),
        });
        return;
      case "awaiting_input":
        await this.finishTurn();
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

  private async handleCardAction(speaker: Speaker, action: CardAction): Promise<CardResult> {
    if (speaker.subject !== this.rec.ownerSubject) {
      return { message: `Only ${this.rec.ownerName} can answer this.` };
    }
    if (!this.live?.runId || action.runId !== this.live.runId) {
      return { card: resultCard("This is no longer open.") };
    }
    this.rec.lastActivityAt = this.now();
    const runId = action.runId;
    try {
      const client = await this.ownerClient();
      switch (action.verb) {
        case VERB.answer: {
          if (!action.interruptId) return { card: resultCard("This is no longer open.") };
          if (!action.answer) return { message: "Type an answer first." };
          this.pending = undefined;
          await client.resolveInterrupt(runId, action.interruptId, { answer: action.answer, resolvedBy: speaker.subject });
          return { card: resultCard(`Answered: ${action.answer}`) };
        }
        case VERB.decline:
          if (!action.interruptId) return { card: resultCard("This is no longer open.") };
          this.pending = undefined;
          await client.cancelInterrupt(runId, action.interruptId, { resolvedBy: speaker.subject });
          return { card: resultCard("Declined.") };
        case VERB.approve:
          await client.reviewRun(runId, "approve");
          return { card: resultCard("Approved.") };
        case VERB.reject:
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
      const client = await this.ownerClient();
      await client.resolveInterrupt(p.runId, p.interruptId, { answer, resolvedBy: by });
    } catch (err) {
      const s = statusOf(err);
      if (s === 404 || s === 409) await this.say("That question is no longer open.");
      else throw err;
    }
  }

  private async cancelTurn(reason: string): Promise<void> {
    const runId = this.live?.runId;
    if (!runId) return;
    try {
      const client = await this.ownerClient();
      await client.cancelTurn(runId, { reason });
    } catch (err) {
      // 409: the run was not mid-turn. Nothing to stop.
      if (statusOf(err) !== 409) {
        this.deps.log.warn("cancel turn failed", { conversation: this.rec.conversationId, run: runId, status: statusOf(err) });
      }
    }
  }

  private dropLive(): void {
    this.live?.abort.abort();
    this.live = undefined;
    this.pending = undefined;
    this.rec.runId = undefined;
    this.rec.agentId = undefined;
  }

  private async endRun(reason: string): Promise<void> {
    const agentId = this.live?.agentId ?? this.rec.agentId;
    this.dropLive();
    await this.finishTurn().catch(() => undefined);
    if (agentId) {
      const client = await this.ownerClient();
      await client.cancelAgent(agentId, { reason: `teams-bridge: ${reason}` }).catch(() => undefined);
    }
    await this.save();
  }
}
