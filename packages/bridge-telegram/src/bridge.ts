// The Telegram adapter on the shared core: the webhook route, update
// handling, and start-up (getMe, and setWebhook when asked).

import { consoleLogger, createBridge, type Bridge, type BridgeAdapter, type BridgeContext, type BridgeOptions, type Logger, type RawRequest, type RawResponse } from "@loomcycle/bridge-core";

import { TelegramApi, TelegramError } from "./api.js";
import { parseCallbackData, renderCard } from "./cards.js";
import { WEBHOOK_PATH, type TelegramConfig } from "./config.js";
import {
  addressedText,
  identify,
  refFor,
  topicOf,
  type AccessPolicy,
  type BotInfo,
  type TgCallbackQuery,
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

/** answerCallbackQuery's text limit. */
const MAX_CALLBACK_TEXT = 200;

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
  private bot?: BotInfo;
  private ctx?: BridgeContext<TelegramRef>;
  private readonly log: Logger;

  constructor(
    private readonly cfg: TelegramConfig,
    private readonly api: TelegramApi,
    opts: TelegramBridgeOptions,
  ) {
    // The core logs with the same logger (its default is consoleLogger too).
    this.log = opts.logger ?? consoleLogger;
    this.capabilities = { streaming: cfg.streaming ? ("personal-only" as const) : ("never" as const), maxMessageChars: MAX_MESSAGE_CHARS };
    this.tgPort = new TelegramPort({ api, log: this.log, streaming: cfg.streaming, trustedChats: cfg.trustedChats, timing: opts.timing, now: opts.now });
    this.port = this.tgPort.asBridgePort();
    this.policy = {
      allowedUsers: cfg.allowedUsers,
      allowedChats: cfg.allowedChats,
      allowAnyone: cfg.allowAnyone,
      trustedChats: cfg.trustedChats,
      groupRequireUsers: cfg.groupRequireUsers,
    };
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
      const allowed = ["message", "callback_query"];
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
  }

  private async onMessage(msg: TgMessage): Promise<void> {
    const bot = this.bot;
    const ctx = this.ctx;
    if (!bot || !ctx || !msg.chat) return;
    // The bot's own messages and every other bot's are dropped before
    // anything else: a bridge that answers a bot loops on the model keys.
    if (!msg.from || msg.from.is_bot) return;
    const text = addressedText(msg, bot);
    if (text === undefined) return;
    const thread = topicOf(msg);
    await ctx.router.onMessage(identify(msg.chat, msg.from, thread, this.policy), refFor(msg.chat, thread), text);
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
