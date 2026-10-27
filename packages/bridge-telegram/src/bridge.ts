// The Telegram adapter on the shared core: the webhook route, update
// handling, and start-up (getMe, and setWebhook when asked).

import { consoleLogger, createBridge, type Bridge, type BridgeAdapter, type BridgeContext, type BridgeOptions, type Logger, type RawRequest, type RawResponse } from "@loomcycle/bridge-core";

import { TelegramApi, TelegramError } from "./api.js";
import { parseCallbackData, renderCard } from "./cards.js";
import { WEBHOOK_PATH, type TelegramConfig } from "./config.js";
import {
  addressedText,
  chatGroup,
  adminGroup,
  conversationIdOf,
  personSub,
  hasNonTextContent,
  identify,
  refFor,
  TEXT_ONLY_NOTICE,
  topicOf,
  type AccessPolicy,
  type BotInfo,
  type TgCallbackQuery,
  type TgChatMemberUpdated,
  type TgMessage,
  type TgUpdate,
  type TelegramRef,
} from "./identity.js";
import { MAX_MESSAGE_CHARS, TelegramPort } from "./port.js";
import type { StreamTiming } from "./stream.js";
import { SeenSet, verifySecret } from "./verify.js";

export interface TelegramBridgeOptions extends BridgeOptions<TelegramRef> {
  /** The fetch the Bot API is called with (tests). */
  fetch?: typeof fetch;
  sleep?: (ms: number) => Promise<void>;
  timing?: Partial<StreamTiming>;
}

/** At most this many trusted chats are asked about a person's membership;
 *  past it the bridge asserts no groups for people (a group chat's own user
 *  still gets its chat's). */
const MAX_GROUP_LOOKUPS = 20;
/** How long a person's looked-up groups are reused. */
const GROUP_CACHE_MS = 10 * 60 * 1000;

/** answerCallbackQuery's text limit. */
const MAX_CALLBACK_TEXT = 200;
/** Group speakers whose latest message id is remembered. Forgetting one only
 *  makes a reply prompt open for the whole chat. */
const MAX_LAST_MESSAGES = 10_000;

export function createTelegramBridge(cfg: TelegramConfig, opts: TelegramBridgeOptions = {}): Bridge<TelegramRef> {
  const api = new TelegramApi({ token: cfg.botToken, baseUrl: cfg.apiBaseUrl, fetch: opts.fetch, sleep: opts.sleep });
  const adapter = new TelegramAdapter(cfg, api, opts);
  return createBridge(cfg, adapter, {
    ...opts,
    // A Telegram message has no time limit, so a long answer is one stream.
    stream: { rolloverMs: Number.POSITIVE_INFINITY, ...opts.stream },
  });
}

class TelegramAdapter implements BridgeAdapter<TelegramRef> {
  readonly platform = { name: "telegram", label: "Telegram" };
  readonly capabilities;
  readonly port;
  private readonly tgPort: TelegramPort;
  private readonly policy: AccessPolicy;
  private readonly seen = new SeenSet();
  /** "<conversation id>|<subject>" → that speaker's latest message id there. */
  private readonly lastMessages = new Map<string, number>();
  private bot?: BotInfo;
  private readonly groupCache = new Map<string, { groups: string[]; until: number }>();
  private readonly now?: () => number;
  private ctx?: BridgeContext<TelegramRef>;
  private readonly log: Logger;

  constructor(
    private readonly cfg: TelegramConfig,
    private readonly api: TelegramApi,
    opts: TelegramBridgeOptions,
  ) {
    // The core logs with the same logger (its default is consoleLogger too).
    this.log = opts.logger ?? consoleLogger;
    this.now = opts.now;
    this.capabilities = { streaming: cfg.streaming ? ("personal-only" as const) : ("never" as const), maxMessageChars: MAX_MESSAGE_CHARS };
    this.tgPort = new TelegramPort({
      api,
      log: this.log,
      streaming: cfg.streaming,
      trustedChats: cfg.trustedChats,
      allowedChats: cfg.allowedChats,
      replyTarget: (ref) => this.ownerMessage(ref),
      timing: opts.timing,
      now: opts.now,
    });
    this.port = this.tgPort.asBridgePort();
    this.policy = {
      allowedUsers: cfg.allowedUsers,
      allowedChats: cfg.allowedChats,
      allowAnyone: cfg.allowAnyone,
      trustedChats: cfg.trustedChats,
      groupRequireUsers: cfg.groupRequireUsers,
      gate: cfg.gate?.name ?? "telegram",
    };
  }

  /** groupsFor is what loomcycle is told about a run identity's groups
   *  (RFC FA §3.7d). A group chat's own user is in that chat's group. A
   *  person (a private chat) is in the group of each trusted chat they are a
   *  member of, and its admins group when they administer it, looked up with
   *  getChatMember. A lookup that fails, or more trusted chats than are
   *  worth asking about, asserts nothing, so loomcycle never removes a group
   *  on a guess. Cached per person for a while: a token is asked for rarely,
   *  but a restart asks for all of them at once. */
  async groupsFor(subject: string, ref: TelegramRef): Promise<string[] | undefined> {
    if (ref.chatId < 0) return [chatGroup(ref.chatId)];
    if (this.cfg.trustedChats.size === 0) return [];
    if (this.cfg.trustedChats.size > MAX_GROUP_LOOKUPS) return undefined;
    const now = (this.now ?? Date.now)();
    const hit = this.groupCache.get(subject);
    if (hit && hit.until > now) return hit.groups;
    const out: string[] = [];
    for (const chat of [...this.cfg.trustedChats].sort()) {
      const m = await this.api.call<{ status?: string; is_member?: boolean }>("getChatMember", { chat_id: chat, user_id: ref.chatId });
      const status = m?.status ?? "";
      const member = status === "creator" || status === "administrator" || status === "member" || (status === "restricted" && m?.is_member === true);
      if (member) out.push(chatGroup(chat));
      if (status === "creator" || status === "administrator") out.push(adminGroup(chat));
    }
    this.groupCache.set(subject, { groups: out, until: now + GROUP_CACHE_MS });
    if (this.groupCache.size > MAX_LAST_MESSAGES) this.groupCache.delete(this.groupCache.keys().next().value!);
    return out;
  }

  async start(ctx: BridgeContext<TelegramRef>): Promise<void> {
    this.ctx = ctx;
    const me = await this.api.call<{ id: number; username?: string; can_read_all_group_messages?: boolean }>("getMe");
    if (typeof me?.id !== "number" || !me.username) throw new Error("telegram getMe returned no bot id or username");
    this.bot = { id: me.id, username: me.username };
    if (me.can_read_all_group_messages) {
      ctx.log.warn("the bot's privacy mode is off; turn it on with @BotFather (/setprivacy) so it sees only what is addressed to it in groups", {
        bot: me.username,
      });
    }
    await ctx.serve({ [`POST ${WEBHOOK_PATH}`]: (req) => this.webhook(req) });
    if (this.cfg.setWebhook && this.cfg.publicUrl) {
      const allowed = ["message", "callback_query", "my_chat_member"];
      await this.api.call("setWebhook", {
        url: `${this.cfg.publicUrl}${WEBHOOK_PATH}`,
        secret_token: this.cfg.webhookSecret,
        allowed_updates: allowed,
      });
      ctx.log.info("telegram webhook set", { allowed_updates: allowed });
    }
  }

  private async webhook(req: RawRequest): Promise<RawResponse> {
    if (!this.cfg.dev && !verifySecret(req, this.cfg.webhookSecret)) return { status: 401 };
    let update: TgUpdate;
    try {
      update = JSON.parse(req.body.toString("utf8")) as TgUpdate;
    } catch {
      // A 2xx, so Telegram does not retry an update nobody can read.
      this.log.warn("unreadable telegram update");
      return { status: 200 };
    }
    if (typeof update?.update_id !== "number" || !this.seen.firstTime(update.update_id)) return { status: 200 };
    // Answer now, work after: Telegram retries a slow webhook.
    void this.handle(update).catch((err: unknown) => {
      this.log.error("telegram update failed", {
        update: update.update_id,
        ...(err instanceof TelegramError ? { method: err.method, code: err.code } : { error: (err as Error)?.name }),
      });
    });
    return { status: 200 };
  }

  private async handle(u: TgUpdate): Promise<void> {
    if (u.message) await this.onMessage(u.message);
    else if (u.callback_query) await this.onCallback(u.callback_query);
    else if (u.my_chat_member) await this.onMyChatMember(u.my_chat_member);
  }

  /** onMyChatMember: a person who blocks the bot in their private chat has
   *  left it, and the bridge, as the gate for Telegram, tells loomcycle so
   *  (RFC FA §3.7c): the user is disabled at once and removed per the gate's
   *  policy. Coming back takes an operator (reactivate: operator). */
  private async onMyChatMember(u: TgChatMemberUpdated): Promise<void> {
    const gate = this.ctx?.gate;
    if (!gate || u.chat?.type !== "private" || !u.from || u.from.is_bot || u.chat.id !== u.from.id) return;
    if (u.new_chat_member?.status !== "kicked") return;
    try {
      await gate.revoke(gate.subjectOf(personSub(u.from.id)));
      this.groupCache.delete(gate.subjectOf(personSub(u.from.id)));
      this.log.info("telegram user blocked the bot; revoked in loomcycle", { chat: u.chat.id });
    } catch (err) {
      this.log.warn("telegram revoke failed", { status: (err as { status?: number })?.status });
    }
  }

  private async onMessage(msg: TgMessage): Promise<void> {
    const bot = this.bot;
    const ctx = this.ctx;
    if (!bot || !ctx || !msg.chat) return;
    // Telegram's own notice, in the old chat, that the group became a
    // supergroup: not a person's message, so it never reaches the router.
    if (typeof msg.migrate_to_chat_id === "number") {
      this.log.warn("telegram group moved to a new chat id", { chat: msg.chat.id, new_chat: msg.migrate_to_chat_id });
      await this.tgPort.tellMigrated(msg.chat.id, msg.migrate_to_chat_id);
      return;
    }
    // The bot's own messages and every other bot's are dropped before
    // anything else: a bridge that answers a bot loops on the model keys.
    if (!msg.from || msg.from.is_bot) return;
    const thread = topicOf(msg);
    const id = identify(msg.chat, msg.from, thread, this.policy);
    const ref = refFor(msg.chat, thread);
    const text = addressedText(msg, bot);
    if (text === undefined) {
      // A private chat is told why a photo went unanswered. A group stays
      // silent: its media is mostly not for the bot. A stranger hears only
      // the refusal, which the router gives once.
      if (msg.chat.type !== "private" || !hasNonTextContent(msg)) return;
      if (!id.ok) await ctx.router.onMessage(id, ref, "");
      else await this.tgPort.send(ref, { type: "message", text: TEXT_ONLY_NOTICE });
      return;
    }
    if (id.ok && id.scope === "group") this.rememberMessage(`${id.conversationId}|${id.speaker.subject}`, msg.message_id);
    await ctx.router.onMessage(id, ref, text);
  }

  private rememberMessage(key: string, messageId: number): void {
    this.lastMessages.delete(key);
    this.lastMessages.set(key, messageId);
    if (this.lastMessages.size > MAX_LAST_MESSAGES) this.lastMessages.delete(this.lastMessages.keys().next().value!);
  }

  /** ownerMessage is the latest message of the conversation's owner, the one
   *  person who may answer its cards. */
  private ownerMessage(ref: TelegramRef): number | undefined {
    const cid = conversationIdOf(ref.chatId, ref.threadId);
    const owner = this.ctx?.router.get(cid)?.record.ownerSubject;
    return owner === undefined ? undefined : this.lastMessages.get(`${cid}|${owner}`);
  }

  private async onCallback(cq: TgCallbackQuery): Promise<void> {
    const ctx = this.ctx;
    const msg = cq.message;
    const answer = (text?: string) =>
      this.api.call("answerCallbackQuery", { callback_query_id: cq.id, ...(text ? { text: text.slice(0, MAX_CALLBACK_TEXT) } : {}) });
    if (!ctx || !msg?.chat || !cq.from || cq.from.is_bot) {
      await answer();
      return;
    }
    const thread = topicOf(msg);
    const ref = refFor(msg.chat, thread);
    const result = await ctx.router.onCardAction(identify(msg.chat, cq.from, thread, this.policy), ref, parseCallbackData(cq.data));
    // Always answered: until it is, the button shows a spinner.
    await answer(result && "message" in result ? result.message : undefined);
    if (result && "card" in result) {
      // No keyboard passed: the used card's buttons are removed.
      const { html, plain } = renderCard(result.card);
      await this.tgPort.editMessage(msg.chat.id, msg.message_id, { html, plain });
    }
  }
}
