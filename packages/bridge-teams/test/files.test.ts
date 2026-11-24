// Attached files for Teams (RFC HE): a document attached in a 1:1 chat is
// downloaded from SharePoint with no credential, checked, and put into the
// run's inbox with the person's own token.

import http from "node:http";
import net from "node:net";

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { ATTACHED_FILE_KIND, FILE_TEXT, MemoryStore, type FileIn, type Identified } from "@loomcycle/bridge-core";

import { createTeamsBridge, type ConversationRef, type TeamsBridge } from "../src/bridge.js";
import { loadConfig } from "../src/config.js";
import { identify } from "../src/identity.js";
import { routeMessage, type MessageRouter } from "../src/inbound.js";
import { FILE_DOWNLOAD_INFO, filesOf, TeamsFileError, teamsFilePort, type TeamsAttachment } from "../src/voice.js";
import { FakeLoomcycle, RecordingLogger, silentLogger, waitFor } from "./fakes.js";

const TENANT = "11111111-1111-1111-1111-111111111111";
const ALICE = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const LINK = "https://contoso-my.sharepoint.com/personal/alice/_layouts/15/download.aspx?UniqueId=1&tempauth=secret-tempauth";
const HOSTS = [".sharepoint.com"];
const PDF = new TextEncoder().encode("%PDF-1.7\n1 0 obj\n");
const EXE = new Uint8Array([0x4d, 0x5a, 0x90, 0x00, 0x03, 0x00]);

function attachment(name: string, fileType: string, url = LINK): TeamsAttachment {
  return { contentType: FILE_DOWNLOAD_INFO, name, content: { downloadUrl: url, uniqueId: "u-1", fileType } };
}

type ConvType = "personal" | "groupChat" | "channel";

function activity(type: ConvType = "personal") {
  return { from: { aadObjectId: ALICE, name: "Alice" }, conversation: { id: `conv-${type}`, tenantId: TENANT, conversationType: type } };
}

const ref = (type: ConvType = "personal"): ConversationRef => ({
  conversation: { id: `conv-${type}`, conversationType: type, tenantId: TENANT },
  serviceUrl: "https://smba.example/",
});

describe("filesOf", () => {
  it("reads every attached file with its link, name and declared type", () => {
    expect(filesOf([attachment("Паспорт.docx", "docx"), attachment("a.pdf", "pdf", "https://x.sharepoint.com/2")], true)).toEqual([
      { ref: { url: LINK, name: "Паспорт.docx" }, name: "Паспорт.docx", type: "docx" },
      { ref: { url: "https://x.sharepoint.com/2", name: "a.pdf" }, name: "a.pdf", type: "pdf" },
    ]);
  });

  it("leaves audio to the voice path with voice on, and takes it as a file with voice off", () => {
    expect(filesOf([attachment("memo.m4a", "m4a")], true)).toEqual([]);
    expect(filesOf([attachment("memo.m4a", "m4a")], false)).toHaveLength(1);
  });

  it("takes no other attachment kind and no file without a link", () => {
    const card = { contentType: "application/vnd.microsoft.card.adaptive", content: {} };
    const inline = { contentType: "text/html", content: "<p>hi</p>" };
    const noLink = { contentType: FILE_DOWNLOAD_INFO, name: "a.pdf", content: { fileType: "pdf" } };
    expect(filesOf([card, inline, noLink], false)).toEqual([]);
  });
});

describe("routeMessage with files on", () => {
  type Call = { kind: "message"; text: string } | { kind: "voice" } | { kind: "file"; files: readonly FileIn[]; text?: string };
  let calls: Call[];
  let router: MessageRouter<ConversationRef>;
  const TENANTS = new Set([TENANT]);

  beforeEach(() => {
    calls = [];
    router = {
      onMessage: async (_id: Identified, _r: ConversationRef, text: string) => void calls.push({ kind: "message", text }),
      onVoice: async () => void calls.push({ kind: "voice" }),
      onFile: async (_id: Identified, _r: ConversationRef, files: readonly FileIn[], opts?: { text?: string }) =>
        void calls.push({ kind: "file", files, text: opts?.text }),
    };
  });

  const route = (type: ConvType, attachments: TeamsAttachment[], text = "", voiceOn = false, filesOn = true, tenants = TENANTS) =>
    routeMessage(router, identify(activity(type), tenants), ref(type), text, { attachments }, voiceOn, filesOn);

  it("hands a document and the text typed beside it to the file path as one message", async () => {
    await route("personal", [attachment("a.pdf", "pdf")], "check this");
    expect(calls).toEqual([{ kind: "file", files: [{ ref: { url: LINK, name: "a.pdf" }, name: "a.pdf", type: "pdf" }], text: "check this" }]);
  });

  it("with voice on, sends the recording the voice path and the document the file path", async () => {
    await route("personal", [attachment("memo.m4a", "m4a"), attachment("a.pdf", "pdf")], "both", true);
    expect(calls.map((c) => c.kind)).toEqual(["voice", "file"]);
    expect(calls[1]).toMatchObject({ kind: "file", text: "both" });
  });

  it("with voice off, takes an audio file as a file (D4)", async () => {
    await route("personal", [attachment("memo.m4a", "m4a")]);
    expect(calls).toEqual([{ kind: "file", files: [expect.objectContaining({ type: "m4a" })], text: "" }]);
  });

  it("hands a group chat's files to the core, which refuses them in words", async () => {
    await route("groupChat", [attachment("a.pdf", "pdf")], "hi");
    expect(calls).toEqual([{ kind: "file", files: [expect.objectContaining({ type: "pdf" })], text: "hi" }]);
  });

  it("with files off, sends a document the text path as before", async () => {
    await route("personal", [attachment("a.pdf", "pdf")], "hi", false, false);
    expect(calls).toEqual([{ kind: "message", text: "hi" }]);
  });

  it("sends an activity the tenant fence refused down the text path", async () => {
    await route("personal", [attachment("a.pdf", "pdf")], "", false, true, new Set(["22222222-2222-2222-2222-222222222222"]));
    expect(calls).toEqual([{ kind: "message", text: "" }]);
  });
});

/** A scripted SharePoint: each URL answers with a status, headers and a body. */
function fakeHost(routes: Map<string, { status?: number; headers?: Record<string, string>; body?: Uint8Array }>) {
  const requests: Array<{ url: string; init?: RequestInit }> = [];
  const fetchFn = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    requests.push({ url, init });
    const r = routes.get(url);
    if (!r) return new Response("not found", { status: 404 });
    const redirect = r.status !== undefined && r.status >= 300 && r.status < 400;
    return new Response(redirect ? null : (r.body ?? new Uint8Array()), { status: r.status ?? 200, headers: r.headers ?? { "content-type": "application/octet-stream" } });
  }) as typeof fetch;
  return { requests, fetchFn };
}

describe("the Teams file port", () => {
  const signal = () => AbortSignal.timeout(5_000);
  const file = (url = LINK): FileIn => ({ ref: { url, name: "a.pdf" }, name: "a.pdf", type: "pdf" });

  it("downloads the pre-authorised link with no header of its own", async () => {
    const host = fakeHost(new Map([[LINK, { body: PDF }]]));
    expect(await teamsFilePort({ hosts: HOSTS, fetch: host.fetchFn }).download(file(), 1000, signal())).toEqual({ bytes: PDF });
    expect(host.requests[0]!.init?.headers).toBeUndefined();
  });

  it("refuses a redirect to a host off the allowed list without fetching it, and logs no link", async () => {
    const evil = "https://evil.example/steal?tempauth=x";
    const host = fakeHost(new Map([[LINK, { status: 302, headers: { location: evil } }]]));
    const log = new RecordingLogger();
    const err = await teamsFilePort({ hosts: HOSTS, fetch: host.fetchFn, log })
      .download(file(), 1000, signal())
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(TeamsFileError);
    expect((err as TeamsFileError).reason).toBe("redirect");
    expect(host.requests.map((r) => r.url)).toEqual([LINK]);
    expect(JSON.stringify(log.lines)).not.toContain("tempauth");
    expect(JSON.stringify(log.lines)).toContain("evil.example");
  });

  it("refuses a sign-in page and a body over the cap", async () => {
    const big = "https://contoso-my.sharepoint.com/big";
    const host = fakeHost(
      new Map([
        [LINK, { headers: { "content-type": "text/html; charset=utf-8" }, body: new TextEncoder().encode("<html>sign in</html>") }],
        [big, { body: new Uint8Array(4096) }],
      ]),
    );
    const port = teamsFilePort({ hosts: HOSTS, fetch: host.fetchFn });
    await expect(port.download(file(), 1000, signal())).rejects.toMatchObject({ reason: "type" });
    await expect(port.download(file(big), 1000, signal())).rejects.toMatchObject({ reason: "size" });
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

// The whole bridge: the Teams SDK app in local mode, an activity with an
// attached document POSTed to /api/messages, a recording Bot Connector, a
// fake loomcycle with the inbox put and a fake SharePoint.
describe("teams bridge with files on", () => {
  let lc: FakeLoomcycle;
  let host: ReturnType<typeof fakeHost>;
  let routes: Map<string, { status?: number; headers?: Record<string, string>; body?: Uint8Array }>;
  let connector: http.Server;
  let connectorUrl: string;
  let said: string[];
  let bridge: TeamsBridge | undefined;
  let port: number;
  let seq = 0;

  beforeEach(async () => {
    lc = new FakeLoomcycle();
    lc.inbox = [];
    routes = new Map([[LINK, { body: PDF }]]);
    host = fakeHost(routes);
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
      }),
      { clients: lc.client, fileFetch: host.fetchFn, logger: silentLogger, store: new MemoryStore(), stream: { stopPollMs: 5 } },
    );
    await bridge.start();
  });

  afterEach(async () => {
    for (const run of lc.runs) run.events.end();
    await bridge?.stop();
    bridge = undefined;
    await new Promise((r) => connector.close(r));
  });

  const post = (conversationType: ConvType, attachments: TeamsAttachment[], text = "") =>
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
        conversation: { id: `a:${conversationType}`, conversationType, tenantId: TENANT },
        channelData: { tenant: { id: TENANT } },
        text,
        attachments,
      }),
    });

  it("puts a document attached in a 1:1 chat with the person's token and tells the agent its ref", async () => {
    expect((await post("personal", [attachment("Паспорт.pdf", "pdf")], "перевір")).status).toBe(200);
    await waitFor(() => lc.runs.length > 0, "a run");
    const run = lc.runs[0]!;
    run.events.push({ type: "session", session_id: "s-1" });
    run.events.push({ type: "agent", run_id: "run-1", agent_id: "ag-1", session_id: "s-1", parent_agent_id: null });
    await waitFor(() => lc.callsTo("sendRunInput").length > 0, "the file lines");

    expect(lc.mints[0]!.opts.surfaces).toEqual(["inbox_put"]);
    const [put] = lc.callsTo("putInboxFile");
    expect(put!.token).toMatch(new RegExp(`^tok-teams-${ALICE}-`));
    expect(put!.args).toEqual(["run-1", "Паспорт.pdf", "application/pdf"]);
    expect(lc.inbox![0]!.bytes).toEqual(PDF);
    // The run started with the text; the file follows as an untrusted block.
    const blocks = lc.callsTo("sendRunInput")[0]!.args[1];
    expect(blocks).toEqual([
      {
        type: "untrusted-block",
        kind: ATTACHED_FILE_KIND,
        text: `{"name":"_______.pdf","ref":"volume:inbox:documents/_______.pdf","media_type":"application/pdf","bytes":${PDF.byteLength}}`,
      },
    ]);
    const input = JSON.stringify(blocks);
    expect(input).not.toContain("sharepoint");
    expect(input).not.toContain("tempauth");
    expect(said).toEqual([]);
  });

  it("refuses an executable renamed .pdf, a file over the cap and a group chat's file, each in plain words", async () => {
    routes.set(LINK, { body: EXE });
    await post("personal", [attachment("Паспорт.pdf", "pdf")]);
    await waitFor(() => said.length >= 1, "the type refusal");
    routes.set(LINK, { body: new Uint8Array(4096) });
    await post("personal", [attachment("big.pdf", "pdf")]);
    await waitFor(() => said.length >= 2, "the size refusal");
    await post("groupChat", [attachment("a.pdf", "pdf")]);
    await waitFor(() => said.length >= 3, "the group refusal");
    expect(said).toEqual([FILE_TEXT.type(1, ["docx", "pdf", "odt", "doc", "xlsx", "md", "txt"]), FILE_TEXT.tooBig(1, 2048), FILE_TEXT.personalOnly]);
    expect(lc.callsTo("putInboxFile")).toEqual([]);
  });
});
