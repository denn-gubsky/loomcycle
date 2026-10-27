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
import { cleanName, identify, personSub, refOf, teamsAllowed, type InboundFields, type SlackRef, type TrustPolicy } from "./identity.js";
import { readAction, readDeletedUser, readEvent, type BlockActionsPayload, type DeletedUser, type EventEnvelope, type SelfIds } from "./inbound.js";
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
  /** How long a gate waits for loomcycle to take a deactivation before it
   *  answers Slack with a failure (Slack wants an answer within 3 s). */
  revokeTimeoutMs?: number;
}

export interface SlackAdapter extends BridgeAdapter<SlackRef> {
  /** Learns the bot's own user id with auth.test; start() calls it. */
  learnSelf(): Promise<SelfIds>;
  /** The two routes, served by start(); exposed for a server of your own and for tests. */
  routes(ctx: Pick<BridgeContext<SlackRef>, "router" | "log" | "gate">): Record<string, Route>;
  groupsFor(subject: string, ref: SlackRef): Promise<string[] | undefined>;
}

const SLACK_HOOKS_ORIGIN = "https://hooks.slack.com";
/** Slack retries an event for a few minutes; ids are kept well past that. */
const SEEN_TTL_MS = 60 * 60_000;
const NAME_TTL_MS = 60 * 60_000;
const MAX_NAMES = 5000;
/** How long a person's looked-up groups are reused, and how many are kept. */
const GROUP_CACHE_MS = 10 * 60 * 1000;
const MAX_GROUP_CACHE = 5000;
const DEFAULT_REVOKE_TIMEOUT_MS = 2000;

/** A link the agent writes is never unfurled: an unfurl fetches the URL from
 *  Slack's side, which a prompt-injected agent could use to send data out
 *  with no click. Every message the bridge posts carries this; Slack's
 *  streaming methods take no such flag, and do not unfurl. */
const NO_UNFURL = { unfurl_links: false, unfurl_media: false } as const;

const ok: RawResponse = { status: 200 };
const unauthorized: RawResponse = { status: 401 };
const badRequest: RawResponse = { status: 400 };
const unavailable: RawResponse = { status: 503 };

export function createSlackAdapter(cfg: SlackConfig, opts: SlackBridgeOptions = {}): SlackAdapter {
  const now = opts.now ?? Date.now;
  const log: Logger = opts.logger ?? consoleLogger;
  const api = new SlackApi({ baseUrl: cfg.apiBaseUrl, token: cfg.botToken, fetch: opts.fetch, sleep: opts.sleep });
  const policy: TrustPolicy = {
    allowedTeams: cfg.allowedTeams,
    sharedTrust: cfg.sharedTrust,
    ...(cfg.shared.gate ? { gate: cfg.shared.gate.name } : {}),
  };
  const seen = new SeenSet(SEEN_TTL_MS, 10_000, now);
  const names = new Map<string, { name: string; at: number }>();
  const apiOrigin = new URL(cfg.apiBaseUrl).origin;
  const groupCache = new Map<string, { groups: string[]; until: number }>();
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

  /** groupsFor is what loomcycle is told about a run identity's groups
   *  (RFC FA §3.7d), named from what Slack knows; an operator maps them
   *  (group_map). A shared conversation's own user is in its workspace's
   *  group and its channel's. A person (a DM) is in their own workspace's
   *  group, and its admins group when users.info says they administer or own
   *  it — believed only when the account is that workspace's own: under
   *  Enterprise Grid or Slack Connect the flags may be another team's. A
   *  guest (restricted, ultra-restricted, or a stranger) is in the guests
   *  group only. A deleted account or a bot, and a lookup that fails (no
   *  users:read scope, say), assert nothing, so loomcycle never removes a
   *  group on a guess. Cached per person for a while: a token is asked for
   *  rarely, but a restart asks for all of them at once. */
  async function groupsFor(subject: string, ref: SlackRef): Promise<string[] | undefined> {
    if (ref.channelType !== "im") {
      const team = ref.team.toLowerCase();
      return [`slack-team-${team}`, `slack-channel-${team}-${ref.channel.toLowerCase()}`];
    }
    const gate = cfg.shared.gate?.name;
    if (!gate || !subject.startsWith(`${gate}:`)) return undefined;
    const [team, user, ...rest] = subject.slice(gate.length + 1).split(":");
    if (!team || !user || rest.length > 0) return undefined;
    const at = now();
    const hit = groupCache.get(subject);
    if (hit && hit.until > at) return hit.groups;
    let info: {
      team_id?: unknown;
      deleted?: unknown;
      is_bot?: unknown;
      is_restricted?: unknown;
      is_ultra_restricted?: unknown;
      is_stranger?: unknown;
      is_admin?: unknown;
      is_owner?: unknown;
    };
    try {
      // The subject is lower-cased; Slack's ids are upper case.
      const r = await api.call("users.info", { user: user.toUpperCase() });
      info = (r.user ?? {}) as typeof info;
    } catch (err) {
      log.warn("could not read a Slack user's groups", { error: slackErrorCode(err) ?? (err as Error)?.name });
      return undefined;
    }
    if (info.deleted === true || info.is_bot === true) return undefined;
    let groups: string[];
    if (info.is_restricted === true || info.is_ultra_restricted === true || info.is_stranger === true) {
      groups = [`slack-team-${team}-guests`];
    } else {
      groups = [`slack-team-${team}`];
      const own = typeof info.team_id === "string" && info.team_id.toLowerCase() === team;
      if (own && (info.is_admin === true || info.is_owner === true)) groups.push(`slack-team-${team}-admins`);
    }
    groupCache.delete(subject);
    groupCache.set(subject, { groups, until: at + GROUP_CACHE_MS });
    if (groupCache.size > MAX_GROUP_CACHE) groupCache.delete(groupCache.keys().next().value!);
    return groups;
  }

  /** onUserDeleted: a person deactivated in their workspace has left it, and
   *  the bridge, as the gate for Slack, tells loomcycle so (RFC FA §3.7c):
   *  the user is disabled at once and removed per the gate's policy. Coming
   *  back takes an operator (reactivate: operator). It reports whether
   *  loomcycle took the revoke within the timeout. No local deny-list is
   *  kept: loomcycle disables the revoked user, so a later assert for them
   *  is refused there. */
  async function onUserDeleted(gate: NonNullable<BridgeContext<SlackRef>["gate"]>, u: DeletedUser): Promise<boolean> {
    const subject = gate.subjectOf(personSub(u.team, u.user));
    let timer: ReturnType<typeof setTimeout> | undefined;
    const timeout = new Promise<never>((_, reject) => {
      timer = setTimeout(() => reject(Object.assign(new Error("revoke timed out"), { status: 504 })), opts.revokeTimeoutMs ?? DEFAULT_REVOKE_TIMEOUT_MS);
    });
    try {
      await Promise.race([gate.revoke(subject), timeout]);
      groupCache.delete(subject);
      log.info("slack user deactivated; revoked in loomcycle", { user: u.user });
      return true;
    } catch (err) {
      log.warn("slack revoke failed", { status: (err as { status?: number })?.status });
      return false;
    } finally {
      clearTimeout(timer);
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

  function routes(ctx: Pick<BridgeContext<SlackRef>, "router" | "log" | "gate">): Record<string, Route> {
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
        // A deactivation must not be lost: a gate revokes before it answers,
        // and on a failure answers so Slack retries, with the event id
        // forgotten so the retry is taken. A token-mode bridge ignores it.
        const deleted = ctx.gate ? readDeletedUser(env, cfg.allowedTeams) : undefined;
        if (ctx.gate && deleted) {
          if (await onUserDeleted(ctx.gate, deleted)) return ok;
          if (typeof env.event_id === "string") seen.forget(env.event_id);
          return unavailable;
        }
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
    groupsFor,
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
