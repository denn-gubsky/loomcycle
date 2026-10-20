// The Slack platform adapter on the shared core.
//
// Slack posts two kinds of request: events (POST /slack/events) and button
// presses (POST /slack/actions). Each is verified against the raw bytes,
// acknowledged at once (Slack wants an answer within three seconds and
// retries otherwise), and handled after the answer.

import {
  consoleLogger,
  createBridge,
  splitMessage,
  type Bridge,
  type BridgeAdapter,
  type BridgeContext,
  type BridgeOptions,
  type BridgePort,
  type Logger,
  type RawRequest,
  type RawResponse,
  type Route,
} from "@loomcycle/bridge-core";

import { SlackApi, slackErrorCode } from "./api.js";
import { renderCard } from "./cards.js";
import type { SlackConfig } from "./config.js";
import { cleanName, identify, refOf, teamsAllowed, type InboundFields, type SlackRef, type TrustPolicy } from "./identity.js";
import { readAction, readEvent, type BlockActionsPayload, type EventEnvelope, type SelfIds } from "./inbound.js";
import { SeenSet } from "./seen.js";
import { SlackStream } from "./stream.js";
import { escapeMrkdwn, MAX_MESSAGE_CHARS, neutralise } from "./text.js";
import { verifySlackRequest } from "./verify.js";

export interface SlackBridgeOptions extends BridgeOptions<SlackRef> {
  /** Slack's side of every outbound call (tests, a proxy). */
  fetch?: typeof fetch;
  sleep?: (ms: number) => Promise<void>;
  /** How often a streamed reply is appended to. */
  streamIntervalMs?: number;
}

export interface SlackAdapter extends BridgeAdapter<SlackRef> {
  /** Learns the bot's own user id with auth.test; start() calls it. */
  learnSelf(): Promise<SelfIds>;
  /** The two routes, served by start(); exposed for a server of your own and for tests. */
  routes(ctx: Pick<BridgeContext<SlackRef>, "router" | "log">): Record<string, Route>;
}

const SLACK_HOOKS_ORIGIN = "https://hooks.slack.com";
/** Slack retries an event for a few minutes; ids are kept well past that. */
const SEEN_TTL_MS = 60 * 60_000;
const NAME_TTL_MS = 60 * 60_000;
const MAX_NAMES = 5000;

/** A link the agent writes is never unfurled: an unfurl fetches the URL from
 *  Slack's side, which a prompt-injected agent could use to send data out
 *  with no click. Every message the bridge posts carries this; Slack's
 *  streaming methods take no such flag, and do not unfurl. */
const NO_UNFURL = { unfurl_links: false, unfurl_media: false } as const;

const ok: RawResponse = { status: 200 };
const unauthorized: RawResponse = { status: 401 };
const badRequest: RawResponse = { status: 400 };

export function createSlackAdapter(cfg: SlackConfig, opts: SlackBridgeOptions = {}): SlackAdapter {
  const now = opts.now ?? Date.now;
  const log: Logger = opts.logger ?? consoleLogger;
  const api = new SlackApi({ baseUrl: cfg.apiBaseUrl, token: cfg.botToken, fetch: opts.fetch, sleep: opts.sleep });
  const policy: TrustPolicy = { allowedTeams: cfg.allowedTeams, sharedTrust: cfg.sharedTrust };
  const seen = new SeenSet(SEEN_TTL_MS, 10_000, now);
  const names = new Map<string, { name: string; at: number }>();
  const apiOrigin = new URL(cfg.apiBaseUrl).origin;
  let self: SelfIds | undefined;
  let namesRefused = false;

  const port: BridgePort<SlackRef> = {
    async send(ref, activity) {
      // Slack's Web API has no typing indicator for a bot outside an
      // assistant thread; the turn's own message is the signal.
      if (activity.type === "typing") return;
      const where = { channel: ref.channel, ...(ref.threadTs ? { thread_ts: ref.threadTs } : {}) };
      if ("card" in activity) {
        const c = renderCard(activity.card, ref.channelType);
        await api.call("chat.postMessage", { ...where, text: c.text, blocks: c.blocks, ...NO_UNFURL });
        return;
      }
      for (const part of splitMessage(activity.text, MAX_MESSAGE_CHARS)) {
        await api.call("chat.postMessage", { ...where, markdown_text: neutralise(part), ...NO_UNFURL });
      }
    },
    createStream(ref) {
      return new SlackStream(api, {
        channel: ref.channel,
        // A stream must be a thread reply: in a DM it threads under the
        // message it answers.
        threadTs: ref.threadTs ?? ref.messageTs ?? "",
        maxChars: MAX_MESSAGE_CHARS,
        intervalMs: opts.streamIntervalMs,
        fallback: (text) => port.send(ref, { type: "message", text }),
        log,
        conversation: ref.channel,
        now,
      });
    },
  };

  async function displayName(f: InboundFields): Promise<string> {
    // Nobody outside the fence costs a Slack call.
    if (namesRefused || !teamsAllowed(f, policy.allowedTeams)) return f.user;
    const hit = names.get(f.user);
    if (hit && now() - hit.at < NAME_TTL_MS) return hit.name;
    try {
      const r = await api.call("users.info", { user: f.user });
      const u = (r.user ?? {}) as { name?: string; profile?: { display_name?: string; real_name?: string } };
      const name = cleanName(u.profile?.display_name || u.profile?.real_name || u.name, f.user);
      if (names.size >= MAX_NAMES) names.clear();
      names.set(f.user, { name, at: now() });
      return name;
    } catch (err) {
      const code = slackErrorCode(err);
      // Without the users:read scope, people are named by their id.
      if (code === "missing_scope") namesRefused = true;
      log.warn("could not read a Slack user's name", { user: f.user, error: code ?? (err as Error)?.name });
      return f.user;
    }
  }

  function authentic(req: RawRequest): boolean {
    return cfg.shared.dev || verifySlackRequest(req, cfg.signingSecret, now());
  }

  /** A response_url is Slack's own; anything else (possible only in dev mode,
   *  where requests are unchecked) would make the bridge post anywhere. */
  function usableResponseUrl(raw: string | undefined): string | undefined {
    if (!raw) return undefined;
    try {
      const u = new URL(raw);
      return u.origin === SLACK_HOOKS_ORIGIN || u.origin === apiOrigin ? raw : undefined;
    } catch {
      return undefined;
    }
  }

  function routes(ctx: Pick<BridgeContext<SlackRef>, "router" | "log">): Record<string, Route> {
    const { router } = ctx;
    const me = () => {
      if (!self) throw new Error("learnSelf has not run");
      return self;
    };

    async function handleEvent(env: EventEnvelope): Promise<void> {
      const r = readEvent(env, me());
      if (r.kind !== "message" || !r.text) return;
      const id = identify(r.fields, policy, await displayName(r.fields));
      // The bot's own and other bots' messages never reach here (readEvent);
      // a refusal without a notice is dropped quietly.
      if (!id.ok && !id.notice) return;
      await router.onMessage(id, refOf(r.fields), r.text);
    }

    async function handleAction(payload: BlockActionsPayload): Promise<void> {
      const a = readAction(payload);
      if (!a) return;
      const id = identify(a.fields, policy, await displayName(a.fields));
      const result = await router.onCardAction(id, refOf(a.fields), { buttonId: a.buttonId });
      if (!result) return;
      const url = usableResponseUrl(a.responseUrl);
      if (!url) {
        ctx.log.warn("card action has no usable response_url", { channel: a.fields.channel });
        return;
      }
      if ("card" in result) {
        const c = renderCard(result.card, a.fields.channelType);
        await api.respond(url, { replace_original: true, text: c.text, blocks: c.blocks, ...NO_UNFURL });
      } else {
        await api.respond(url, { response_type: "ephemeral", replace_original: false, text: escapeMrkdwn(result.message), ...NO_UNFURL });
      }
    }

    const fail = (what: string) => (err: unknown) =>
      ctx.log.error(`${what} failed`, { error: slackErrorCode(err) ?? (err as Error)?.name });

    return {
      "POST /slack/events": async (req) => {
        if (!authentic(req)) return unauthorized;
        let env: EventEnvelope;
        try {
          env = JSON.parse(req.body.toString("utf8")) as EventEnvelope;
        } catch {
          return badRequest;
        }
        // Answered only after the signature check, so the endpoint proves
        // nothing to anyone but Slack.
        if (env?.type === "url_verification") {
          return typeof env.challenge === "string"
            ? { status: 200, headers: { "content-type": "text/plain" }, body: env.challenge }
            : badRequest;
        }
        // A retry (X-Slack-Retry-Num) carries the same event_id.
        if (typeof env?.event_id === "string" && !seen.add(env.event_id)) return ok;
        void handleEvent(env).catch(fail("event"));
        return ok;
      },
      "POST /slack/actions": async (req) => {
        if (!authentic(req)) return unauthorized;
        let payload: BlockActionsPayload;
        try {
          const raw = new URLSearchParams(req.body.toString("utf8")).get("payload");
          if (!raw) return badRequest;
          payload = JSON.parse(raw) as BlockActionsPayload;
        } catch {
          return badRequest;
        }
        void handleAction(payload).catch(fail("card action"));
        return ok;
      },
    };
  }

  async function learnSelf(): Promise<SelfIds> {
    const r = await api.call("auth.test", {});
    if (typeof r.user_id !== "string" || !r.user_id) throw new Error("auth.test returned no bot user id");
    self = { userId: r.user_id };
    const team = typeof r.team_id === "string" ? r.team_id : undefined;
    if (team && !cfg.allowedTeams.has(team.toUpperCase())) {
      log.warn("the bot's own workspace is not in SLACK_BRIDGE_ALLOWED_TEAMS; nobody there can use it", { team });
    }
    return self;
  }

  return {
    platform: { name: "slack", label: "Slack" },
    capabilities: { streaming: cfg.streaming ? "personal-only" : "never", maxMessageChars: MAX_MESSAGE_CHARS },
    port,
    learnSelf,
    routes,
    async start(ctx) {
      await learnSelf();
      await ctx.serve(routes(ctx));
    },
  };
}

/** createSlackBridge assembles a ready-to-start bridge. */
export function createSlackBridge(cfg: SlackConfig, opts: SlackBridgeOptions = {}): Bridge<SlackRef> {
  return createBridge(cfg.shared, createSlackAdapter(cfg, opts), opts);
}
