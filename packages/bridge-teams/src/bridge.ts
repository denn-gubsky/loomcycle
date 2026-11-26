// The Teams side: the SDK app, its two routes, and the port the core sends
// through. Everything between (tokens, conversations, turns, cards' logic) is
// @loomcycle/bridge-core's.

import {
  consoleLogger,
  createBridge,
  type Bridge,
  type BridgeAdapter,
  type BridgeOptions,
  type CardResult,
  type ConversationRouter,
  type Logger,
  type OutboundActivity,
  type StreamLike,
} from "@loomcycle/bridge-core";
import {
  stripMentionsText,
  type AdaptiveCardActionResponse,
  type CloudEnvironment,
  type ConversationReference,
  type TokenProvider,
} from "@microsoft/teams.api";
import { App, ExpressAdapter } from "@microsoft/teams.apps";

import { parseCardAction, renderCard } from "./cards.js";
import type { TeamsConfig } from "./config.js";
import {
  ATTACH_AGAIN_TEXT,
  isSignOutCommand,
  PendingSignIns,
  pendingKey,
  SIGN_IN_BUTTON,
  SIGN_IN_TEXT,
  SIGN_OUT_FAILED_TEXT,
  SIGNED_OUT_TEXT,
  userTokenStatus,
  type Deliver,
  type PendingOptions,
} from "./delegated.js";
import { attachmentsSummary, dumpFields } from "./dump.js";
import { chooseFileToken, SECRET_ONLY_WARNING, type FileTokenSource } from "./filetoken.js";
import { abortable, DEFAULT_LOOKUP_TIMEOUT_MS, GRAPH_PUBLIC_BASE, GraphFiles, withGraphFiles, type GraphFilesOptions } from "./graph.js";
import { groupsOf, identify, personSub, uninstalledSub } from "./identity.js";
import { hasFileDownloadInfo, looksLikeFile, noFileText, routeMessage, seenOf } from "./inbound.js";
import { filesOf, teamsFilePort, teamsVoicePort } from "./voice.js";

const ADAPTIVE_CARD = "application/vnd.microsoft.card.adaptive";

/** A Teams conversation reference, stored as plain JSON. */
export type ConversationRef = {
  conversation: { id: string; conversationType?: string; tenantId?: string };
  serviceUrl: string;
  [key: string]: unknown;
};

/** TeamsBridgeApp exposes the SDK's activity sender so the bridge can send
 *  and stream to a stored conversation reference outside a handler: a
 *  loomcycle turn outlives the request that started it. */
export class TeamsBridgeApp extends App {
  createStreamFor(ref: ConversationRef): StreamLike {
    return this.activitySender.createStream(ref as unknown as ConversationReference);
  }

  /** graphToken is the app-only Graph token for a tenant (RFC HF §3.6), as
   *  the raw bearer, or undefined. Never logged. */
  async graphToken(tenantId?: string): Promise<string | undefined> {
    const t = await this.getAppGraphToken(tenantId);
    return t ? t.toString() : undefined;
  }

  /** userGraphToken is a person's delegated Graph token on an OAuth
   *  connection (RFC HF §3.5), asked of the Bot Framework token service by
   *  their Bot Framework id, or undefined when they have none (404). The
   *  bridge keeps no copy. Never logged. */
  async userGraphToken(botUserId: string, connectionName: string): Promise<string | undefined> {
    try {
      const t = await this.api.users.getToken({ channelId: "msteams", userId: botUserId, connectionName });
      return t.token || undefined;
    } catch (err) {
      if (userTokenStatus(err) === 404) return undefined;
      throw err;
    }
  }

  async sendTo(ref: ConversationRef, activity: OutboundActivity): Promise<void> {
    const params =
      activity.type === "typing"
        ? { type: "typing" }
        : "card" in activity
          ? { type: "message", attachments: [{ contentType: ADAPTIVE_CARD, content: renderCard(activity.card) }] }
          : { type: "message", text: activity.text, textFormat: "markdown" };
    await this.activitySender.send(params as never, ref as unknown as ConversationReference);
  }
}

/** What of the SDK app a deployment (a sovereign cloud) or a test sets: its
 *  credentials and cloud. Never its request authentication, which BRIDGE_DEV
 *  alone decides. */
export interface TeamsAppOptions {
  clientId?: string;
  clientSecret?: string;
  tenantId?: string;
  token?: TokenProvider;
  cloud?: CloudEnvironment;
  /** The Bot Framework token service's root (tests; a sovereign cloud gets
   *  its own from `cloud`). */
  apiClientSettings?: { oauthUrl: string };
}

export type TeamsBridgeOptions = BridgeOptions<ConversationRef> & {
  logger?: Logger;
  app?: TeamsAppOptions;
  /** The fetch attached files are downloaded with (tests). */
  fileFetch?: typeof fetch;
  /** The Graph fallback's fetch, token and timings (tests). The token
   *  defaults to the SDK's app Graph token in app mode and the sender's own
   *  from the token service in delegated mode; the host to the SDK cloud's. */
  graph?: Partial<Pick<GraphFilesOptions, "fetch" | "token" | "timeoutMs" | "retryDelaysMs">>;
  /** The bot's file token for a link without tempauth (tests); by default
   *  the certificate or the SDK's non-secret credential (filetoken.ts). */
  fileToken?: FileTokenSource;
  /** How long and how many messages wait for a sign-in in delegated mode,
   *  and the clock (tests). */
  signIn?: PendingOptions;
};

/** LoopbackExpressAdapter is the SDK's Express adapter listening on one
 *  address. The SDK's own listens on every interface; in dev mode, where the
 *  SDK checks no request, the bridge must be reachable from this machine only. */
export class LoopbackExpressAdapter extends ExpressAdapter {
  constructor(
    private readonly host: string,
    onError: (err: Error) => void,
  ) {
    super(undefined, { onError });
  }

  override async start(port: number | string): Promise<void> {
    const server = this.server;
    if (!server) throw new Error("the Teams listener has no server");
    await new Promise<void>((resolve, reject) => {
      server.once("error", reject);
      server.listen(Number(port), this.host, () => {
        server.off("error", reject);
        if (this.onError) server.on("error", this.onError);
        resolve();
      });
    });
  }

  /** address is where the listener is bound, once started. */
  address(): ReturnType<NonNullable<typeof this.server>["address"]> | undefined {
    return this.server?.address();
  }
}

export interface TeamsBridge extends Bridge<ConversationRef> {
  app: TeamsBridgeApp;
  router: ConversationRouter<ConversationRef>;
}

export function createTeamsBridge(cfg: TeamsConfig, opts: TeamsBridgeOptions = {}): TeamsBridge {
  const { shared } = cfg;
  if (!shared.dev && process.env.DANGEROUSLY_ALLOW_UNAUTHENTICATED_REQUESTS) {
    (opts.logger ?? console).warn("DANGEROUSLY_ALLOW_UNAUTHENTICATED_REQUESTS is ignored: set BRIDGE_DEV=1 for local testing");
  }
  const log = opts.logger ?? console;
  // Delegated Graph mode signs people in through one OAuth connection (RFC
  // HF §3.5). The flow is registered with the app, so the SDK answers the
  // sign-in invokes (signin/tokenExchange, signin/verifyState) itself.
  // `state: false` keeps the SDK's per-turn state off, as it is without a
  // flow; the flow then tracks its sign-ins in bounded process memory.
  const connection = cfg.graphFiles.mode === "delegated" ? cfg.graphFiles.connection : undefined;
  const app = new TeamsBridgeApp({
    ...opts.app,
    ...(connection ? { oauthFlows: [connection], state: false } : {}),
    // Always explicit: the SDK would otherwise read the env var itself, and
    // a stray one must not turn off request validation in production.
    dangerouslyAllowUnauthenticatedRequests: shared.dev,
    // Dev mode checks no request, so it listens on loopback only (D6): the
    // core's BRIDGE_BIND, which from bridge-core 0.4 it holds to loopback in
    // dev mode, else 127.0.0.1 (an older core has no bind address).
    ...(shared.dev
      ? {
          httpServerAdapter: new LoopbackExpressAdapter((shared as typeof shared & { bindHost?: string }).bindHost ?? "127.0.0.1", (err) =>
            log.error("teams listener error", { error: err.name }),
          ),
        }
      : {}),
  });
  // The bridge's gate name in loomcycle; unset for a bridge with a token.
  const gate = shared.gate?.name;
  // The bot's token for a file link without tempauth (RFC HF §3.7).
  const fileToken = opts.fileToken
    ? { kind: "token" as const, source: opts.fileToken }
    : chooseFileToken({
        certificate: cfg.fileCertificate && { ...cfg.fileCertificate, loginEndpoint: app.cloud.loginEndpoint },
        credentials: app.credentials,
        app: app.tokenProvider,
      });
  const fileLog = opts.logger ?? consoleLogger;
  // The Graph fallback (RFC HF). Graph's host comes from the SDK's cloud, so
  // a sovereign cloud's token goes to its own Graph. In app mode the token is
  // the app's; in delegated mode the sender's own, from the token service.
  const { appId, noFileHint } = cfg.graphFiles;
  const graph =
    (cfg.graphFiles.mode === "app" || connection) && appId
      ? new GraphFiles({
          base: app.graphBaseUrl ?? GRAPH_PUBLIC_BASE,
          appId,
          token: connection
            ? async (_tenantId, botUserId) => (botUserId ? app.userGraphToken(botUserId, connection) : undefined)
            : (tenantId) => app.graphToken(tenantId),
          hosts: cfg.fileHosts,
          log: fileLog,
          ...opts.graph,
        })
      : undefined;
  const filePort = teamsFilePort({ hosts: cfg.fileHosts, fetch: opts.fileFetch, token: fileToken.source, log: fileLog });
  const flow = connection ? app.getOAuthFlow(connection) : undefined;
  const pending = new PendingSignIns<ConversationRef>(opts.signIn);

  const adapter: BridgeAdapter<ConversationRef> = {
    platform: { name: "teams", label: "Teams" },
    // Teams streams in 1:1 chats only; group chats and channels get one
    // message per turn. Teams refuses messages over about 28 KB.
    capabilities: { streaming: "personal-only", maxMessageChars: 20000 },
    port: {
      send: (ref, activity) => app.sendTo(ref, activity),
      createStream: (ref) => app.createStreamFor(ref),
    },
    /** groupsFor is what loomcycle is told about a run identity's groups
     *  (RFC FA §3.7d): the Entra tenant the conversation is in, from its
     *  stored reference. No lookups: Teams says nothing more without Graph. */
    async groupsFor(_subject, ref) {
      return groupsOf(ref);
    },
    // Voice in only (RFC GA D12): audio files attached in a 1:1 chat. The
    // core uses it only with BRIDGE_VOICE=1, and grants the user tokens
    // transcription alone, since the port cannot send voice.
    voice: teamsVoicePort<ConversationRef>({
      hosts: cfg.fileHosts,
      fetch: opts.fileFetch,
      token: fileToken.source,
      typing: (ref) => app.sendTo(ref, { type: "typing" }),
      log: opts.logger ?? consoleLogger,
    }),
    // Attached files (RFC HE), from the same hosts as recordings, and the
    // files the Graph fallback found (RFC HF). The core uses it only with
    // BRIDGE_FILES=1.
    files: graph ? withGraphFiles(filePort, graph, fileLog) : filePort,
    async start(ctx) {
      if ((ctx.config.files?.enabled || ctx.config.voice?.enabled) && fileToken.kind === "secret") ctx.log.warn(SECRET_ONLY_WARNING);
      if (cfg.dumpActivities) {
        // Registered first, so it sees every activity that passed the SDK's
        // request check, whatever its type, before any handler runs.
        app.use(async ({ activity, next }) => {
          ctx.log.info("teams activity", dumpFields(activity as never));
          return next();
        });
        ctx.log.warn("TEAMS_BRIDGE_DUMP_ACTIVITIES is on: every inbound activity is logged, what people write included");
      }
      const filesOn = ctx.config.files?.enabled === true;
      const voiceOn = ctx.config.voice?.enabled === true;
      const say = (ref: ConversationRef, text: string) =>
        app.sendTo(ref, { type: "message", text }).catch((err: unknown) => ctx.log.warn("teams reply failed", { error: (err as Error)?.name }));
      // deliver hands a message to the router once its Graph files are known
      // (none when Graph is off): at once, or after a sign-in it waited for.
      const deliver: Deliver<ConversationRef> = async (id, cref, text, fields, found) => {
        // A message shaped like a file that brought none is answered, so the
        // person knows the file never arrived (RFC HF D6). It still goes on to
        // the router, which drops an empty message but notes the person wrote.
        if (filesOn && id.ok && id.scope === "personal" && found.length === 0 && looksLikeFile(text, fields.attachments)) {
          await app
            .sendTo(cref, { type: "message", text: noFileText(noFileHint) })
            .catch((err: unknown) => ctx.log.warn("teams no-file reply failed", { error: (err as Error)?.name }));
        }
        await routeMessage(ctx.router, id, cref, text, fields, voiceOn, filesOn, found);
      };
      app.on("message", async (mctx) => {
        const { activity, ref } = mctx;
        // Card submits from older clients arrive as messages with no text.
        const text = stripMentionsText(activity) ?? "";
        // A message whose attachments held no file a bot can download (only
        // file.download.info is one) is logged, by count and content type,
        // when it looks like a file the bot never got: no text beside it, or
        // an attachment other than the text/html body Teams adds to a
        // formatted message. A plain message is not logged.
        const atts = activity.attachments ?? [];
        const unread = atts.some((a) => !(a.contentType ?? "").toLowerCase().startsWith("text/html"));
        if (atts.length > 0 && filesOf(atts, false).length === 0 && (unread || !text.trim())) {
          ctx.log.info("teams message carried no file", {
            conversation: activity.conversation?.id,
            conversationType: activity.conversation?.conversationType,
            ...attachmentsSummary(activity.attachments),
          });
        }
        const id = identify(activity, cfg.allowedTenants, gate);
        const cref = ref as unknown as ConversationRef;
        const fields = { attachments: activity.attachments, timestamp: activity.timestamp };
        const personal = filesOn && id.ok && id.scope === "personal";
        const botUserId = activity.from?.id;
        const conversationId = activity.conversation?.id;
        // /signout (delegated mode, 1:1 chats only) is the bridge's own
        // command: it never reaches the agent.
        if (flow && personal && botUserId && conversationId && isSignOutCommand(text)) {
          pending.drop(pendingKey(conversationId, botUserId));
          let reply = SIGNED_OUT_TEXT;
          try {
            await flow.signOut(mctx);
            ctx.log.info("teams user signed out of Graph", { conversation: conversationId });
          } catch (err) {
            ctx.log.warn("teams sign-out failed", { status: userTokenStatus(err) });
            reply = SIGN_OUT_FAILED_TEXT;
          }
          await say(cref, reply);
          return;
        }
        // With Graph on, every 1:1 message Teams sent no file for is looked
        // up (RFC HF D3): a caption leaves no trace of the file, so a message
        // with text may carry one too. The lookup is bounded, and the
        // message waits for it, so its text and files reach the agent as one.
        const sender = personSub(activity.from?.aadObjectId ?? "");
        const q =
          graph && personal && sender && conversationId && activity.id && !hasFileDownloadInfo(atts)
            ? { conversationId, tenantId: activity.conversation?.tenantId, userId: sender, messageId: activity.id, ...(botUserId ? { botUserId } : {}) }
            : undefined;
        if (!graph || !q) return deliver(id, cref, text, fields, []);
        if (!flow) return deliver(id, cref, text, fields, await graph.lookup(q));
        // Delegated mode: the lookup runs on the sender's own token. A person
        // who has none is sent one sign-in card, and the message waits for the
        // sign-in (D4); one who writes again meanwhile replaces it, with no
        // second card. Only a 1:1 message gets here, so nothing from a group
        // chat or a channel is ever held.
        if (!botUserId) return deliver(id, cref, text, fields, []);
        const key = pendingKey(conversationId!, botUserId);
        let token: string | undefined;
        // The token service is held to the lookup's bound too, so a message
        // never waits on it longer than on Graph.
        const bound = AbortSignal.timeout(opts.graph?.timeoutMs ?? DEFAULT_LOOKUP_TIMEOUT_MS);
        try {
          token = await abortable(flow.getToken(mctx), bound);
          if (!token) {
            if (!pending.hold(key, { id, ref: cref, text, activity: fields, lookup: q })) {
              ctx.log.info("teams message waits for sign-in", { conversation: conversationId, replaced: true });
              return;
            }
            token = await abortable(flow.signIn(mctx, { oauthCardText: SIGN_IN_TEXT, signInButtonText: SIGN_IN_BUTTON }), bound);
            if (!token) {
              ctx.log.info("teams message waits for sign-in", { conversation: conversationId, replaced: false });
              return;
            }
            // Signed in after all (between the two calls): nothing waits.
            pending.drop(key);
          }
        } catch (err) {
          // The token service failed: the text goes on alone, as a failed
          // lookup does. Its status only; the error may carry the request.
          pending.drop(key);
          ctx.log.warn("teams graph lookup failed", { conversation: conversationId, reason: "token", status: userTokenStatus(err) });
          return deliver(id, cref, text, fields, []);
        }
        return deliver(id, cref, text, fields, await graph.lookup(q, token));
      });
      if (flow && graph) {
        // A sign-in completed, by SSO (signin/tokenExchange) or with the card's
        // code (signin/verifyState): the message that waited for it is looked
        // up with the token just issued and goes on as any other. The waiting
        // message is found by who sent the invoke, so only that same person,
        // in that same conversation, ever releases it.
        flow.onSignInComplete(async (sctx, token) => {
          const a = sctx.activity;
          const conversationId = a.conversation?.id;
          const botUserId = a.from?.id;
          if (!conversationId || !botUserId) return;
          const held = pending.take(pendingKey(conversationId, botUserId));
          if (!held) {
            // The message waited too long, or the bridge restarted. Told only
            // to someone the tenant fence lets in.
            const sid = identify(a, cfg.allowedTenants, gate);
            if (sid.ok && sid.scope === "personal") await say(sctx.ref as unknown as ConversationRef, ATTACH_AGAIN_TEXT);
            return;
          }
          ctx.log.info("teams sign-in completed; the waiting message goes on", { conversation: conversationId });
          await deliver(held.id, held.ref, held.text, held.activity, await graph.lookup(held.lookup, token.token || undefined));
        });
      }
      app.on("card.action", async ({ activity, ref }) => {
        const res = await ctx.router.onCardAction(
          identify(activity, cfg.allowedTenants, gate),
          ref as unknown as ConversationRef,
          parseCardAction(activity.value),
          seenOf(activity),
        );
        return toInvokeBody(res ?? { message: "This card is no longer active." });
      });
      const g = ctx.gate;
      if (g) {
        // A person who uninstalls the app from their 1:1 chat has left the
        // bot, and the bridge, as the gate for Teams, tells loomcycle so (RFC
        // FA §3.7c): the user is disabled at once and removed per the gate's
        // policy. The activity arrives on the same Bot Connector-authenticated
        // route as a message, and passes the same tenant fence.
        //
        // The revoke is awaited before Teams is answered, and a failed one
        // fails the turn: the SDK answers a handler that throws with a 500,
        // so the Bot Connector redelivers the activity instead of the person
        // staying active in loomcycle (fail closed, not open). The error
        // thrown carries a fixed message: the SDK logs it.
        app.on("install.remove", async ({ activity }) => {
          const sub = uninstalledSub(activity, cfg.allowedTenants);
          if (!sub) return;
          try {
            await g.revoke(g.subjectOf(sub));
          } catch (err) {
            ctx.log.warn("teams revoke failed", { status: (err as { status?: number })?.status });
            throw new Error("teams-bridge: the revoke did not reach loomcycle; Teams will redeliver");
          }
          ctx.log.info("teams user uninstalled the app; revoked in loomcycle", { conversation: activity.conversation?.id });
        });
      }
      await app.start(shared.port);
    },
    async stop() {
      await app.stop();
    },
  };

  const bridge = createBridge(shared, adapter, opts);
  return { ...bridge, app };
}

function toInvokeBody(res: CardResult): AdaptiveCardActionResponse {
  // Our cards are plain JSON built in cards.ts; the SDK types them as IAdaptiveCard.
  return "card" in res
    ? { statusCode: 200, type: ADAPTIVE_CARD, value: renderCard(res.card) as never }
    : { statusCode: 200, type: "application/vnd.microsoft.activity.message", value: res.message };
}
