// The Graph fallback for files Teams does not send (RFC HF P1, app-only):
// the message is read through Graph, its `reference` attachments become
// files, and each is downloaded through the shares endpoint with the Graph
// token, which never reaches SharePoint.

import http from "node:http";
import net from "node:net";

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { FILE_TEXT, MemoryStore, type FileIn, type FilePort } from "@loomcycle/bridge-core";

import { createTeamsBridge, type TeamsBridge, type TeamsBridgeOptions } from "../src/bridge.js";
import { loadConfig } from "../src/config.js";
import { GraphFileRefusal, GraphFiles, shareId, trimTrailingSlashes, withGraphFiles, type GraphFileRef, type GraphFilesOptions } from "../src/graph.js";
import { htmlIsEmpty, looksLikeFile, NO_FILE_TEXT, noFileText } from "../src/inbound.js";
import { FILE_DOWNLOAD_INFO, TeamsFileError, type TeamsAttachment } from "../src/voice.js";
import { FakeLoomcycle, RecordingLogger, silentLogger, waitFor } from "./fakes.js";
import { ALICE, APP_ID, CHAT, CONTENT_URL, fakeGraph, GRAPH, HOSTS, MALLORY, onGraph, PDF, reference, SP_LINK, TENANT, TOKEN, world, type World } from "./graphfake.js";

const lookup = (messageId = "1700000000000") => ({ conversationId: "a:alice", tenantId: TENANT, userId: ALICE, messageId });

function graphFiles(w: World, over: Partial<GraphFilesOptions> = {}) {
  const g = fakeGraph(w);
  const log = new RecordingLogger();
  const files = new GraphFiles({
    base: GRAPH,
    appId: APP_ID,
    token: async () => TOKEN,
    hosts: HOSTS,
    log,
    fetch: g.fetchFn,
    retryDelaysMs: [1, 1, 1],
    ...over,
  });
  return { files, log, requests: g.requests };
}

const failure = (log: RecordingLogger) => log.lines.find((l) => l.msg === "teams graph lookup failed")?.attrs;

describe("the Graph lookup", () => {
  it("finds the chat through the installed app, reads the message and turns its references into files", async () => {
    const { files, requests } = graphFiles(world());
    expect(await files.lookup(lookup("1700000000001"))).toEqual([
      { ref: { graph: "reference", contentUrl: CONTENT_URL, name: "a.pdf", tenantId: TENANT, userId: ALICE }, name: "a.pdf", type: "pdf" },
    ]);
    const urls = requests.map((r) => decodeURIComponent(r.url));
    expect(urls).toEqual([
      `${GRAPH}/v1.0/users/${ALICE}/teamwork/installedApps?$expand=teamsApp&$filter=teamsApp/externalId eq '${APP_ID}'`,
      `${GRAPH}/v1.0/users/${ALICE}/teamwork/installedApps/inst-1/chat`,
      `${GRAPH}/v1.0/chats/${CHAT}/messages/1700000000001`,
    ]);
    expect(requests.every((r) => r.auth === `Bearer ${TOKEN}` && r.redirect === "error")).toBe(true);
  });

  it("resolves a conversation's chat once and reads only the message after that", async () => {
    const { files, requests } = graphFiles(world());
    await files.lookup(lookup("1"));
    await files.lookup(lookup("2"));
    expect(requests.filter((r) => r.url.includes("/installedApps?")).length).toBe(1);
    expect(requests.filter((r) => r.url.endsWith("/chat")).length).toBe(1);
    expect(requests.filter((r) => r.url.includes("/chats/")).length).toBe(2);
  });

  it("re-reads a message Graph does not have yet", async () => {
    const { files, requests } = graphFiles(world({ misses: 2 }));
    expect(await files.lookup(lookup())).toHaveLength(1);
    expect(requests.filter((r) => r.url.includes("/chats/")).length).toBe(3);
  });

  it("refuses a message from anyone but the activity's sender", async () => {
    const { files, log } = graphFiles(world({ message: { from: { user: { id: MALLORY } }, attachments: [reference()] } }));
    expect(await files.lookup(lookup())).toEqual([]);
    expect(failure(log)).toMatchObject({ reason: "sender" });
  });

  it("refuses a message with no user sender", async () => {
    const { files, log } = graphFiles(world({ message: { from: null, attachments: [reference()] } }));
    expect(await files.lookup(lookup())).toEqual([]);
    expect(failure(log)).toMatchObject({ reason: "sender" });
  });

  it("refuses a chat that is not one-on-one, and does not keep it", async () => {
    const { files, log, requests } = graphFiles(world({ chat: { id: CHAT, chatType: "group" } }));
    expect(await files.lookup(lookup())).toEqual([]);
    expect(failure(log)).toMatchObject({ reason: "chat" });
    expect(requests.some((r) => r.url.includes("/chats/"))).toBe(false);
    await files.lookup(lookup());
    expect(requests.filter((r) => r.url.endsWith("/chat")).length).toBe(2);
  });

  it("finds no chat when the app is not installed for the person", async () => {
    const { files, log } = graphFiles(world({ installs: [] }));
    expect(await files.lookup(lookup())).toEqual([]);
    expect(failure(log)).toMatchObject({ reason: "chat" });
  });

  it("takes only reference attachments with a link and a name", async () => {
    const attachments = [
      { contentType: "text/html", content: "<p></p>" },
      { contentType: "reference", name: "no-link.pdf" },
      { contentType: "reference", contentUrl: CONTENT_URL },
      reference("Звіт.docx"),
    ];
    const { files } = graphFiles(world({ message: { from: { user: { id: ALICE } }, attachments } }));
    expect(await files.lookup(lookup())).toEqual([expect.objectContaining({ name: "Звіт.docx", type: "docx" })]);
  });

  it("routes the text alone when Graph does not answer in time", async () => {
    const { files, log } = graphFiles(world({ hang: true }), { timeoutMs: 30 });
    const started = Date.now();
    expect(await files.lookup(lookup())).toEqual([]);
    expect(Date.now() - started).toBeLessThan(2_000);
    expect(failure(log)).toMatchObject({ reason: "timeout" });
  });

  it("asks Graph nothing without a token", async () => {
    const { files, log, requests } = graphFiles(world(), { token: async () => undefined });
    expect(await files.lookup(lookup())).toEqual([]);
    expect(failure(log)).toMatchObject({ reason: "token" });
    expect(requests).toEqual([]);
  });

  it("logs a Graph refusal by status, never a URL or a token", async () => {
    const { files, log } = graphFiles(world({ misses: 10 }));
    expect(await files.lookup(lookup())).toEqual([]);
    expect(failure(log)).toEqual({ conversation: "a:alice", reason: "status", status: 404 });
    const logged = JSON.stringify(log.lines);
    expect(logged).not.toContain(TOKEN);
    expect(logged).not.toContain("graph.microsoft.com");
    expect(logged).not.toContain(CHAT);
  });
});

describe("the Graph download", () => {
  const signal = () => AbortSignal.timeout(5_000);
  const ref = (contentUrl = CONTENT_URL): GraphFileRef => ({ graph: "reference", contentUrl, name: "a.pdf", tenantId: TENANT, userId: ALICE });

  it("checks the file is in the sender's OneDrive, fetches it by item with the token and follows the redirect with none", async () => {
    const { files, requests } = graphFiles(world());
    expect(await files.download(ref(), 1000, signal())).toEqual(PDF);
    expect(requests.map((r) => decodeURIComponent(r.url))).toEqual([
      `${GRAPH}/v1.0/shares/${shareId(CONTENT_URL)}/driveItem?$select=id,name,parentReference`,
      `${GRAPH}/v1.0/users/${ALICE}/drive?$select=id`,
      `${GRAPH}/v1.0/drives/drive-${ALICE}/items/item-1/content`,
      SP_LINK,
    ]);
    expect(requests.slice(0, 3).every((r) => r.auth === `Bearer ${TOKEN}`)).toBe(true);
    expect(requests[3]!.auth).toBeNull();
  });

  it("refuses a file in another user's drive without fetching any content", async () => {
    const { files, requests, log } = graphFiles(world({ itemDrive: `drive-${MALLORY}` }));
    const port = withGraphFiles({ download: async () => ({ bytes: PDF }) }, files, log);
    await expect(port.download({ ref: ref(), name: "a.pdf", type: "pdf" }, 1000, signal())).rejects.toBeInstanceOf(GraphFileRefusal);
    expect(requests.some((r) => r.url.includes("/content") || !onGraph(r))).toBe(false);
    expect(log.lines).toEqual([{ level: "warn", msg: "teams file download refused", attrs: { reason: "not_sender_drive", via: "graph" } }]);
  });

  it("refuses a Graph file with no sender without asking Graph", async () => {
    const { files, requests } = graphFiles(world());
    await expect(files.download({ ...ref(), userId: undefined }, 1000, signal())).rejects.toMatchObject({ reason: "not_sender_drive" });
    expect(requests).toEqual([]);
  });

  it("asks for the sender's drive once", async () => {
    const { files, requests } = graphFiles(world());
    await files.download(ref(), 1000, signal());
    await files.download(ref(), 1000, signal());
    expect(requests.filter((r) => r.url.includes("/drive?")).length).toBe(1);
  });

  it("encodes a sharing URL as unpadded base64url", () => {
    // Standard base64: aHR0cHM6Ly9hLnNoYXJlcG9pbnQuY29tL3g/eT0x and ...tLz8+Pg==
    expect(shareId("https://a.sharepoint.com/x?y=1")).toBe("u!aHR0cHM6Ly9hLnNoYXJlcG9pbnQuY29tL3g_eT0x");
    expect(shareId("https://a.sharepoint.com/?>>")).toBe("u!aHR0cHM6Ly9hLnNoYXJlcG9pbnQuY29tLz8-Pg");
  });

  it("refuses a contentUrl off the allowed hosts without asking Graph", async () => {
    const { files, requests } = graphFiles(world());
    const err = await files.download(ref("https://evil.example/a.pdf"), 1000, signal()).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(TeamsFileError);
    expect((err as TeamsFileError).reason).toBe("host");
    expect(requests).toEqual([]);
  });

  it("refuses a redirect off the allowed hosts without fetching it", async () => {
    const { files, requests } = graphFiles(world({ redirect: "https://evil.example/steal" }));
    await expect(files.download(ref(), 1000, signal())).rejects.toMatchObject({ reason: "redirect", detail: { host: "evil.example" } });
    expect(requests.some((r) => r.url.includes("evil.example"))).toBe(false);
  });

  it("refuses a sign-in page and a body over the cap behind the redirect", async () => {
    const w = world({ files: new Map([[SP_LINK, { body: new TextEncoder().encode("<html/>"), headers: { "content-type": "text/html" } }]]) });
    await expect(graphFiles(w).files.download(ref(), 1000, signal())).rejects.toMatchObject({ reason: "type" });
    const big = world({ files: new Map([[SP_LINK, { body: new Uint8Array(4096) }]]) });
    await expect(graphFiles(big).files.download(ref(), 1000, signal())).rejects.toMatchObject({ reason: "size" });
  });

  it("caps a body Graph serves itself", async () => {
    const g = graphFiles(world({ redirect: undefined, direct: new Uint8Array(4096) }));
    await expect(g.files.download(ref(), 1000, signal())).rejects.toMatchObject({ reason: "size" });
  });

  it("sends a file Teams attached itself through the attachment port", async () => {
    const seen: FileIn[] = [];
    const base: FilePort = { download: async (f) => (seen.push(f), { bytes: PDF }) };
    const { files } = graphFiles(world());
    const port = withGraphFiles(base, files, silentLogger);
    const attached: FileIn = { ref: { url: SP_LINK, name: "a.pdf" }, name: "a.pdf", type: "pdf" };
    expect(await port.download(attached, 1000, signal())).toEqual({ bytes: PDF });
    expect(seen).toEqual([attached]);
    expect(await port.download({ ref: ref(), name: "a.pdf", type: "pdf" }, 1000, signal())).toEqual({ bytes: PDF });
    expect(seen).toHaveLength(1);
  });
});

describe("looksLikeFile", () => {
  const html = (content: unknown): TeamsAttachment => ({ contentType: "text/html", content });

  it("is a message with no text and only empty text/html", () => {
    expect(looksLikeFile("", [html("")])).toBe(true);
    expect(looksLikeFile(" ", [html("<p>&nbsp;</p>")])).toBe(true);
    expect(looksLikeFile("", [html(undefined)])).toBe(true);
  });

  it("is not a captioned message, a plain one, or one with any other attachment", () => {
    expect(looksLikeFile("check this", [html("<p>check this</p>")])).toBe(false);
    expect(looksLikeFile("", [])).toBe(false);
    expect(looksLikeFile("", [html("<p>hi</p>")])).toBe(false);
    expect(looksLikeFile("", [html(""), { contentType: FILE_DOWNLOAD_INFO, content: {} }])).toBe(false);
  });

  it("reads an HTML body as empty only when it shows no text", () => {
    expect(htmlIsEmpty("<div><p> </p>&NBSP;&#160;<br/></div>\n")).toBe(true);
    expect(htmlIsEmpty("<p>a</p>")).toBe(false);
    expect(htmlIsEmpty("<p")).toBe(false);
    expect(htmlIsEmpty("&amp;")).toBe(false);
    expect(htmlIsEmpty("<<<<".repeat(50_000))).toBe(false);
    expect(htmlIsEmpty({})).toBe(false);
  });

  it("trims a base URL's trailing slashes", () => {
    expect(trimTrailingSlashes("https://graph.microsoft.us///")).toBe("https://graph.microsoft.us");
    expect(trimTrailingSlashes("https://graph.microsoft.com")).toBe("https://graph.microsoft.com");
    expect(trimTrailingSlashes("/".repeat(100_000))).toBe("");
  });

  it("adds the operator's hint to the reply", () => {
    expect(noFileText()).toBe(NO_FILE_TEXT);
    expect(noFileText("Email it to the help desk.")).toBe(`${NO_FILE_TEXT} Email it to the help desk.`);
  });
});

describe("the Graph settings", () => {
  const base = {
    LOOMCYCLE_BASE_URL: "https://loomcycle:8787",
    LOOMCYCLE_TOKEN: "t",
    BRIDGE_AGENT: "helper",
    TEAMS_BRIDGE_ALLOWED_TENANTS: TENANT,
    CLIENT_ID: APP_ID,
    BRIDGE_FILES: "1",
  };

  it("is off by default, and not read with files off", () => {
    expect(loadConfig(base).graphFiles).toEqual({ mode: "off" });
    expect(loadConfig({ ...base, BRIDGE_FILES: "0", TEAMS_BRIDGE_FILES_GRAPH: "bogus", TEAMS_BRIDGE_NO_FILE_HINT: "x" }).graphFiles).toEqual({ mode: "off" });
  });

  it("takes app mode with the manifest id, defaulting to CLIENT_ID", () => {
    expect(loadConfig({ ...base, TEAMS_BRIDGE_FILES_GRAPH: "app" }).graphFiles).toEqual({ mode: "app", appId: APP_ID });
    const other = "dddddddd-dddd-dddd-dddd-dddddddddddd";
    expect(loadConfig({ ...base, TEAMS_BRIDGE_FILES_GRAPH: "App", TEAMS_BRIDGE_APP_ID: other.toUpperCase() }).graphFiles).toEqual({ mode: "app", appId: other });
  });

  it("refuses delegated mode until it is built, an unknown mode and an app id that is not a GUID", () => {
    expect(() => loadConfig({ ...base, TEAMS_BRIDGE_FILES_GRAPH: "delegated" })).toThrow(/not built yet \(RFC HF P2\)/);
    expect(() => loadConfig({ ...base, TEAMS_BRIDGE_FILES_GRAPH: "yes" })).toThrow(/TEAMS_BRIDGE_FILES_GRAPH/);
    expect(() => loadConfig({ ...base, TEAMS_BRIDGE_FILES_GRAPH: "app", TEAMS_BRIDGE_APP_ID: "x' or 1 eq 1" })).toThrow(/TEAMS_BRIDGE_APP_ID/);
  });

  it("reads the no-file hint with Graph on or off", () => {
    expect(loadConfig({ ...base, TEAMS_BRIDGE_NO_FILE_HINT: " Email it. " }).graphFiles).toEqual({ mode: "off", noFileHint: "Email it." });
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

// The whole bridge: the Teams SDK app in local mode, the message Teams sent
// for an attached file (an empty text/html attachment) POSTed to
// /api/messages, a recording Bot Connector, a fake loomcycle with the inbox
// put, and a fake Graph and SharePoint.
describe("teams bridge with the Graph fallback", () => {
  let lc: FakeLoomcycle;
  let w: World;
  let graph: ReturnType<typeof fakeGraph>;
  let connector: http.Server;
  let connectorUrl: string;
  let said: string[];
  let bridge: TeamsBridge | undefined;
  let port: number;
  let seq = 0;

  beforeEach(async () => {
    lc = new FakeLoomcycle();
    lc.inbox = [];
    w = world();
    graph = fakeGraph(w);
    said = [];
    connector = http.createServer((req, res) => {
      let body = "";
      req.on("data", (c) => (body += c));
      req.on("end", () => {
        try {
          const a = JSON.parse(body) as { type?: string; text?: string };
          if (a.type === "message" && a.text) said.push(a.text);
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
  });

  async function start(env: Record<string, string>, opts: Partial<TeamsBridgeOptions> = {}) {
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
        ...env,
      }),
      {
        clients: lc.client,
        logger: silentLogger,
        store: new MemoryStore(),
        stream: { stopPollMs: 5 },
        graph: { fetch: graph.fetchFn, token: async () => TOKEN, retryDelaysMs: [1] },
        ...opts,
      },
    );
    await bridge.start();
  }

  const graphOn = { TEAMS_BRIDGE_FILES_GRAPH: "app", TEAMS_BRIDGE_APP_ID: APP_ID };

  const post = (text: string, html: string) =>
    fetch(`http://127.0.0.1:${port}/api/messages`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        type: "message",
        channelId: "msteams",
        serviceUrl: connectorUrl,
        timestamp: new Date().toISOString(),
        id: `in-${++seq}`,
        from: { id: "29:user", aadObjectId: ALICE, name: "Alice" },
        recipient: { id: "28:bot", name: "Bot" },
        conversation: { id: "a:personal", conversationType: "personal", tenantId: TENANT },
        channelData: { tenant: { id: TENANT } },
        text,
        attachments: [{ contentType: "text/html", content: html }],
      }),
    });

  const openRun = async () => {
    await waitFor(() => lc.runs.length > 0, "a run");
    const run = lc.runs[0]!;
    run.events.push({ type: "session", session_id: "s-1" });
    run.events.push({ type: "agent", run_id: "run-1", agent_id: "ag-1", session_id: "s-1", parent_agent_id: null });
  };

  it("puts the file Graph found for an empty message into the inbox, with no token on the SharePoint hop", async () => {
    await start(graphOn);
    expect((await post("", "")).status).toBe(200);
    await openRun();
    await waitFor(() => lc.callsTo("sendRunInput").length > 0, "the file block");
    expect(lc.callsTo("putInboxFile")[0]!.args).toEqual(["run-1", "a.pdf", "application/pdf"]);
    expect(lc.inbox![0]!.bytes).toEqual(PDF);
    expect(graph.requests.find((r) => r.url.includes("/messages/"))!.url).toContain(`/messages/in-`);
    const sp = graph.requests.filter((r) => !onGraph(r));
    expect(sp.map((r) => r.url)).toEqual([SP_LINK]);
    expect(sp[0]!.auth).toBeNull();
    expect(JSON.stringify(lc.calls)).not.toContain(TOKEN);
    expect(JSON.stringify(lc.calls)).not.toContain("sharepoint");
    expect(said).toEqual([]);
  });

  it("gives the agent a captioned file and its caption as one message", async () => {
    await start(graphOn);
    await post("check this", "<p>check this</p>");
    await openRun();
    await waitFor(() => lc.callsTo("sendRunInput").length > 0, "the file block");
    expect(lc.runs).toHaveLength(1);
    expect(JSON.stringify(lc.runs[0]!.opts)).toContain("check this");
    expect(lc.callsTo("putInboxFile")).toHaveLength(1);
    expect(said).toEqual([]);
  });

  it("refuses, in words, a file the message points at outside the sender's own OneDrive", async () => {
    w.itemDrive = `drive-${MALLORY}`;
    await start(graphOn);
    await post("", "");
    await waitFor(() => said.length > 0, "the download refusal");
    expect(said).toEqual([FILE_TEXT.download(1)]);
    expect(lc.callsTo("putInboxFile")).toEqual([]);
    expect(graph.requests.some((r) => r.url.includes("/content") || !onGraph(r))).toBe(false);
  });

  it("tells the person no file came when Graph is off, with the operator's hint", async () => {
    await start({ TEAMS_BRIDGE_NO_FILE_HINT: "Email it to the help desk." });
    await post("", "");
    await waitFor(() => said.length > 0, "the no-file reply");
    expect(said).toEqual([noFileText("Email it to the help desk.")]);
    expect(graph.requests).toEqual([]);
    expect(lc.runs).toEqual([]);
  });

  it("tells the person no file came when Graph found none", async () => {
    w.message.attachments = [];
    await start(graphOn);
    await post("", "");
    await waitFor(() => said.length > 0, "the no-file reply");
    expect(said).toEqual([NO_FILE_TEXT]);
    expect(lc.runs).toEqual([]);
  });

  it("routes a captioned message as text when Graph finds no file, without the reply", async () => {
    w.message.attachments = [];
    await start(graphOn);
    await post("hello", "<p>hello</p>");
    await waitFor(() => lc.runs.length > 0, "a run");
    expect(JSON.stringify(lc.runs[0]!.opts)).toContain("hello");
    expect(lc.callsTo("putInboxFile")).toEqual([]);
    expect(said).toEqual([]);
  });
});
