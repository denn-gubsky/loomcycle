// Delegated Graph mode (RFC HF P2): the lookup runs on the sender's own
// token from the Bot Framework token service; a person with none gets one
// sign-in card and the message waits for the sign-in; /signout signs them
// out. The whole bridge runs against a fake token service, a recording Bot
// Connector, a fake Graph and a fake loomcycle.

import { readFileSync } from "node:fs";
import http from "node:http";
import net from "node:net";

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { MemoryStore } from "@loomcycle/bridge-core";

import { createTeamsBridge, type TeamsBridge, type TeamsBridgeOptions } from "../src/bridge.js";
import { loadConfig } from "../src/config.js";
import {
  ATTACH_AGAIN_TEXT,
  isSignOutCommand,
  PENDING_TTL_MS,
  PendingSignIns,
  pendingKey,
  SIGN_IN_TEXT,
  SIGNED_OUT_TEXT,
  type PendingMessage,
} from "../src/delegated.js";
import { NO_FILE_TEXT } from "../src/inbound.js";
import { FakeLoomcycle, RecordingLogger, waitFor } from "./fakes.js";
import { ALICE, APP_ID, fakeGraph, onGraph, PDF, TENANT, world, type World } from "./graphfake.js";

const CONNECTION = "graph-connection";
const USER_TOKEN = "delegated-user-token-secret";
const SSO_TOKEN = "teams-sso-token-secret";
const CODE = "123456";
const BF_ALICE = "29:alice";

describe("the delegated settings", () => {
  const base = {
    LOOMCYCLE_BASE_URL: "https://loomcycle:8787",
    LOOMCYCLE_TOKEN: "t",
    BRIDGE_AGENT: "helper",
    TEAMS_BRIDGE_ALLOWED_TENANTS: TENANT,
    CLIENT_ID: APP_ID,
    BRIDGE_FILES: "1",
  };

  it("takes delegated mode with the OAuth connection and the manifest id", () => {
    expect(loadConfig({ ...base, TEAMS_BRIDGE_FILES_GRAPH: "Delegated", TEAMS_BRIDGE_GRAPH_CONNECTION: ` ${CONNECTION} ` }).graphFiles).toEqual({
      mode: "delegated",
      appId: APP_ID,
      connection: CONNECTION,
    });
  });

  it("refuses to load delegated mode without the connection", () => {
    expect(() => loadConfig({ ...base, TEAMS_BRIDGE_FILES_GRAPH: "delegated" })).toThrow(/TEAMS_BRIDGE_GRAPH_CONNECTION/);
    expect(() => loadConfig({ ...base, TEAMS_BRIDGE_FILES_GRAPH: "delegated", TEAMS_BRIDGE_GRAPH_CONNECTION: "  " })).toThrow(/TEAMS_BRIDGE_GRAPH_CONNECTION/);
  });

  it("refuses delegated mode with an app id that is not a GUID", () => {
    expect(() =>
      loadConfig({ ...base, TEAMS_BRIDGE_FILES_GRAPH: "delegated", TEAMS_BRIDGE_GRAPH_CONNECTION: CONNECTION, TEAMS_BRIDGE_APP_ID: "x' or 1 eq 1" }),
    ).toThrow(/TEAMS_BRIDGE_APP_ID/);
  });
});

describe("the app package", () => {
  it("names the bot's app and its SSO resource, and lets the sign-in page open", () => {
    const manifest = JSON.parse(readFileSync(new URL("../appPackage/manifest.json", import.meta.url), "utf8")) as {
      webApplicationInfo?: { id?: string; resource?: string };
      validDomains?: string[];
    };
    // The resource is the Application ID URI the OAuth connection's token
    // exchange URL must equal, else SSO fails with resourcematchfailed.
    expect(manifest.webApplicationInfo).toEqual({ id: "${{BOT_ID}}", resource: "api://botid-${{BOT_ID}}" });
    expect(manifest.validDomains).toEqual(["token.botframework.com"]);
  });
});

describe("the messages waiting for a sign-in", () => {
  const msg = (text: string): PendingMessage<string> => ({
    id: { ok: false, reason: "test" },
    ref: "ref",
    text,
    activity: {},
    lookup: { conversationId: "a:1", userId: ALICE, messageId: "m" },
  });

  it("asks for a card for the first message only, and keeps the latest", () => {
    const p = new PendingSignIns<string>();
    expect(p.hold("k", msg("one"))).toBe(true);
    expect(p.hold("k", msg("two"))).toBe(false);
    expect(p.take("k")?.text).toBe("two");
    expect(p.take("k")).toBeUndefined();
  });

  it("forgets a message that waited too long, counted from its card", () => {
    let now = 0;
    const p = new PendingSignIns<string>({ now: () => now });
    p.hold("k", msg("one"));
    now = PENDING_TTL_MS - 1;
    expect(p.hold("k", msg("two"))).toBe(false);
    now = PENDING_TTL_MS;
    expect(p.take("k")).toBeUndefined();
    // After it expired, the next message is a first one again.
    expect(p.hold("k", msg("three"))).toBe(true);
  });

  it("holds at most max messages, dropping the oldest", () => {
    const p = new PendingSignIns<string>({ max: 2 });
    p.hold("a", msg("a"));
    p.hold("b", msg("b"));
    p.hold("c", msg("c"));
    expect(p.size).toBe(2);
    expect(p.take("a")).toBeUndefined();
    expect(p.take("c")?.text).toBe("c");
  });

  it("keys a message by conversation and person", () => {
    expect(pendingKey("a:1", "29:x")).not.toBe(pendingKey("a:1", "29:y"));
    expect(pendingKey("a:1", "29:x")).not.toBe(pendingKey("a:2", "29:x"));
  });

  it("knows /signout as the whole message only", () => {
    expect(isSignOutCommand(" /SignOut ")).toBe(true);
    expect(isSignOutCommand("/signout now")).toBe(false);
    expect(isSignOutCommand("please /signout")).toBe(false);
  });
});

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const s = net.createServer();
    s.once("error", reject);
    s.listen(0, "127.0.0.1", () => {
      const p = (s.address() as net.AddressInfo).port;
      s.close(() => resolve(p));
    });
  });
}

/** A stand-in for the Bot Framework token service: who is signed in, and
 *  every call made to it. */
class FakeTokenService {
  readonly signedIn = new Set<string>();
  /** Set to answer every GetToken with a 500. */
  broken = false;
  readonly calls: Array<{ method: string; path: string; userId: string | null; code: string | null }> = [];
  private server?: http.Server;
  url = "";

  async start(): Promise<void> {
    this.server = http.createServer((req, res) => {
      let body = "";
      req.on("data", (c) => (body += c));
      req.on("end", () => {
        const u = new URL(req.url ?? "/", "http://token.test");
        const userId = u.searchParams.get("userId");
        const code = u.searchParams.get("code");
        this.calls.push({ method: req.method ?? "", path: u.pathname, userId, code });
        const json = (status: number, v: unknown) => {
          res.writeHead(status, { "content-type": "application/json" });
          res.end(JSON.stringify(v));
        };
        const token = { connectionName: CONNECTION, token: USER_TOKEN, expiration: "2099-01-01T00:00:00Z" };
        switch (u.pathname) {
          case "/api/usertoken/GetToken":
            if (this.broken) return json(500, { error: USER_TOKEN });
            if (code !== null) {
              if (code !== CODE || !userId) return json(404, {});
              this.signedIn.add(userId);
              return json(200, token);
            }
            return userId && this.signedIn.has(userId) ? json(200, token) : json(404, {});
          case "/api/botsignin/GetSignInResource":
            return json(200, {
              signInLink: "https://token.test/sign-in",
              tokenExchangeResource: { id: "tx-1", uri: `api://botid-${APP_ID}` },
              tokenPostResource: { sasUrl: "https://token.test/post" },
            });
          case "/api/usertoken/exchange": {
            const sent = (JSON.parse(body || "{}") as { token?: string }).token;
            if (sent !== SSO_TOKEN || !userId) return json(412, {});
            this.signedIn.add(userId);
            return json(200, token);
          }
          case "/api/usertoken/SignOut":
            if (userId) this.signedIn.delete(userId);
            return json(200, {});
          default:
            return json(404, {});
        }
      });
    });
    await new Promise<void>((r) => this.server!.listen(0, "127.0.0.1", r));
    this.url = `http://127.0.0.1:${(this.server.address() as net.AddressInfo).port}`;
  }

  async stop(): Promise<void> {
    if (this.server) await new Promise((r) => this.server!.close(r));
  }

  to(path: string) {
    return this.calls.filter((c) => c.path === path);
  }
}

describe("teams bridge in delegated Graph mode", () => {
  let lc: FakeLoomcycle;
  let w: World;
  let graph: ReturnType<typeof fakeGraph>;
  let tokens: FakeTokenService;
  let connector: http.Server;
  let connectorUrl: string;
  let said: string[];
  let cards: string[];
  let log: RecordingLogger;
  let bridge: TeamsBridge | undefined;
  let port: number;
  let now: number;
  let seq = 0;

  beforeEach(async () => {
    lc = new FakeLoomcycle();
    lc.inbox = [];
    w = world();
    graph = fakeGraph(w);
    tokens = new FakeTokenService();
    await tokens.start();
    said = [];
    cards = [];
    log = new RecordingLogger();
    now = 1_000_000;
    connector = http.createServer((req, res) => {
      let body = "";
      req.on("data", (c) => (body += c));
      req.on("end", () => {
        try {
          const a = JSON.parse(body) as { type?: string; text?: string; attachments?: Array<{ contentType?: string; content?: { text?: string } }> };
          if (a.type === "message" && a.text) said.push(a.text);
          for (const att of a.attachments ?? []) if (att.contentType === "application/vnd.microsoft.card.oauth") cards.push(att.content?.text ?? "");
        } catch {
          // not an activity
        }
        res.writeHead(201, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ id: `act-${++seq}` }));
      });
    });
    await new Promise<void>((r) => connector.listen(0, "127.0.0.1", r));
    connectorUrl = `http://127.0.0.1:${(connector.address() as net.AddressInfo).port}/`;
    port = await freePort();
  });

  afterEach(async () => {
    for (const run of lc.runs) run.events.end();
    await bridge?.stop();
    bridge = undefined;
    await new Promise((r) => connector.close(r));
    await tokens.stop();
  });

  async function start(opts: Partial<TeamsBridgeOptions> = {}) {
    bridge = createTeamsBridge(
      loadConfig({
        LOOMCYCLE_BASE_URL: "https://loomcycle.test",
        LOOMCYCLE_TOKEN: "admin-token",
        BRIDGE_AGENT: "helper",
        TEAMS_BRIDGE_ALLOWED_TENANTS: TENANT,
        BRIDGE_DEV: "1",
        BRIDGE_FILES: "1",
        BRIDGE_FILES_MAX_BYTES: "2048",
        PORT: String(port),
        TEAMS_BRIDGE_FILES_GRAPH: "delegated",
        TEAMS_BRIDGE_GRAPH_CONNECTION: CONNECTION,
        TEAMS_BRIDGE_APP_ID: APP_ID,
      }),
      {
        clients: lc.client,
        logger: log,
        store: new MemoryStore(),
        stream: { stopPollMs: 5 },
        app: { apiClientSettings: { oauthUrl: tokens.url } },
        // No token here: in delegated mode it comes from the token service.
        graph: { fetch: graph.fetchFn, retryDelaysMs: [1] },
        signIn: { now: () => now },
        ...opts,
      },
    );
    await bridge.start();
  }

  const activity = (over: Record<string, unknown>) => ({
    channelId: "msteams",
    serviceUrl: connectorUrl,
    timestamp: new Date().toISOString(),
    id: `in-${++seq}`,
    from: { id: BF_ALICE, aadObjectId: ALICE, name: "Alice" },
    recipient: { id: "28:bot", name: "Bot" },
    conversation: { id: "a:personal", conversationType: "personal", tenantId: TENANT },
    channelData: { tenant: { id: TENANT } },
    ...over,
  });

  const send = (body: unknown) =>
    fetch(`http://127.0.0.1:${port}/api/messages`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });

  const post = (text: string, html = text ? `<p>${text}</p>` : "", conversation?: Record<string, unknown>) =>
    send(activity({ type: "message", text, attachments: [{ contentType: "text/html", content: html }], ...(conversation ? { conversation } : {}) }));

  const exchange = () => send(activity({ type: "invoke", name: "signin/tokenExchange", value: { id: "ex-1", connectionName: CONNECTION, token: SSO_TOKEN } }));
  const verify = () => send(activity({ type: "invoke", name: "signin/verifyState", value: { state: CODE } }));

  const openRun = async () => {
    await waitFor(() => lc.runs.length > 0, "a run");
    const run = lc.runs[0]!;
    run.events.push({ type: "session", session_id: "s-1" });
    run.events.push({ type: "agent", run_id: "run-1", agent_id: "ag-1", session_id: "s-1", parent_agent_id: null });
  };

  /** The tokens appear nowhere the bridge logs or hands loomcycle. */
  const tokensKept = () => {
    for (const t of [USER_TOKEN, SSO_TOKEN]) {
      expect(JSON.stringify(log.lines)).not.toContain(t);
      expect(JSON.stringify(lc.calls)).not.toContain(t);
      expect(JSON.stringify(lc.runs.map((r) => r.opts))).not.toContain(t);
    }
  };

  it("looks a signed-in person's message up with their own token, through the app-mode path", async () => {
    tokens.signedIn.add(BF_ALICE);
    await start();
    expect((await post("", "")).status).toBe(200);
    await openRun();
    await waitFor(() => lc.callsTo("sendRunInput").length > 0, "the file block");
    expect(lc.inbox![0]!.bytes).toEqual(PDF);
    const toGraph = graph.requests.filter(onGraph);
    expect(toGraph.length).toBeGreaterThan(3);
    expect(toGraph.every((r) => r.auth === `Bearer ${USER_TOKEN}`)).toBe(true);
    // The SharePoint hop carries no credential.
    expect(graph.requests.filter((r) => !onGraph(r)).every((r) => r.auth === null)).toBe(true);
    // The token service is asked by the sender's Bot Framework id, on the
    // connection, for the lookup and again for the download.
    expect(tokens.to("/api/usertoken/GetToken").every((c) => c.userId === BF_ALICE)).toBe(true);
    expect(tokens.to("/api/usertoken/GetToken").length).toBeGreaterThanOrEqual(2);
    expect(cards).toEqual([]);
    expect(said).toEqual([]);
    tokensKept();
  });

  it("sends one sign-in card to a person with no token and holds the message", async () => {
    await start();
    await post("", "");
    await waitFor(() => cards.length > 0, "the sign-in card");
    expect(cards).toEqual([SIGN_IN_TEXT]);
    expect(graph.requests).toEqual([]);
    expect(lc.runs).toEqual([]);
    // Not the no-file reply: the file may well be there.
    expect(said).toEqual([]);
  });

  it("replaces the waiting message with a later one, with no second card, and processes it after SSO", async () => {
    await start();
    await post("", "");
    await waitFor(() => cards.length > 0, "the sign-in card");
    expect((await post("check this")).status).toBe(200);
    expect(cards).toHaveLength(1);
    expect(lc.runs).toEqual([]);
    expect((await exchange()).status).toBe(200);
    await openRun();
    await waitFor(() => lc.callsTo("sendRunInput").length > 0, "the file block");
    expect(lc.runs).toHaveLength(1);
    expect(JSON.stringify(lc.runs[0]!.opts)).toContain("check this");
    expect(lc.callsTo("putInboxFile")).toHaveLength(1);
    // The message looked up is the second one, by its own activity id.
    const reads = graph.requests.filter((r) => r.url.includes("/messages/"));
    expect(reads).toHaveLength(1);
    expect(reads[0]!.url).toContain(`/messages/in-`);
    expect(graph.requests.filter(onGraph).every((r) => r.auth === `Bearer ${USER_TOKEN}`)).toBe(true);
    expect(cards).toHaveLength(1);
    tokensKept();
  });

  it("processes the waiting message after a sign-in with the card's code", async () => {
    await start();
    await post("", "");
    await waitFor(() => cards.length > 0, "the sign-in card");
    expect((await verify()).status).toBe(200);
    await openRun();
    await waitFor(() => lc.callsTo("sendRunInput").length > 0, "the file block");
    expect(lc.inbox![0]!.bytes).toEqual(PDF);
    expect(tokens.to("/api/usertoken/GetToken").some((c) => c.code === CODE)).toBe(true);
    tokensKept();
  });

  it("asks for the file again when the sign-in comes after the message expired", async () => {
    await start();
    await post("", "");
    await waitFor(() => cards.length > 0, "the sign-in card");
    now += PENDING_TTL_MS;
    await exchange();
    await waitFor(() => said.length > 0, "the reply");
    expect(said).toEqual([ATTACH_AGAIN_TEXT]);
    expect(lc.runs).toEqual([]);
    expect(graph.requests).toEqual([]);
  });

  it("says no file came when the signed-in lookup finds none", async () => {
    tokens.signedIn.add(BF_ALICE);
    w.message.attachments = [];
    await start();
    await post("", "");
    await waitFor(() => said.length > 0, "the no-file reply");
    expect(said).toEqual([NO_FILE_TEXT]);
  });

  it("signs the person out on /signout, in one line, and asks them to sign in again after", async () => {
    tokens.signedIn.add(BF_ALICE);
    await start();
    expect((await post("/signout")).status).toBe(200);
    await waitFor(() => said.length > 0, "the sign-out reply");
    expect(said).toEqual([SIGNED_OUT_TEXT]);
    expect(tokens.to("/api/usertoken/SignOut").map((c) => c.userId)).toEqual([BF_ALICE]);
    expect(lc.runs).toEqual([]);
    expect(graph.requests).toEqual([]);
    await post("", "");
    await waitFor(() => cards.length > 0, "the sign-in card");
    expect(cards).toEqual([SIGN_IN_TEXT]);
  });

  it("drops a waiting message on /signout", async () => {
    await start();
    await post("", "");
    await waitFor(() => cards.length > 0, "the sign-in card");
    await post("/signout");
    await waitFor(() => said.length > 0, "the sign-out reply");
    await exchange();
    await waitFor(() => said.length > 1, "the reply");
    expect(said).toEqual([SIGNED_OUT_TEXT, ATTACH_AGAIN_TEXT]);
    expect(lc.runs).toEqual([]);
  });

  it("routes the text alone, with no card, when the token service fails, and logs its status only", async () => {
    tokens.broken = true;
    await start();
    await post("hello");
    await waitFor(() => lc.runs.length > 0, "a run");
    expect(JSON.stringify(lc.runs[0]!.opts)).toContain("hello");
    expect(cards).toEqual([]);
    expect(graph.requests).toEqual([]);
    expect(log.lines.find((l) => l.msg === "teams graph lookup failed")?.attrs).toEqual({ conversation: "a:personal", reason: "token", status: 500 });
    tokensKept();
  });

  it("never holds a group chat message, asks the token service nothing for it and routes it as before", async () => {
    await start();
    await post("hello group", undefined, { id: "19:group@thread.v2", conversationType: "groupChat", tenantId: TENANT });
    await waitFor(() => lc.runs.length > 0, "a run");
    expect(JSON.stringify(lc.runs[0]!.opts)).toContain("hello group");
    expect(cards).toEqual([]);
    expect(tokens.calls).toEqual([]);
    expect(graph.requests).toEqual([]);
  });

  it("leaves /signout in a group chat to the agent", async () => {
    await start();
    await post("/signout", undefined, { id: "19:group@thread.v2", conversationType: "groupChat", tenantId: TENANT });
    await waitFor(() => lc.runs.length > 0, "a run");
    expect(tokens.to("/api/usertoken/SignOut")).toEqual([]);
  });
});
