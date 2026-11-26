// What the bridge logs about inbound activities: the redacted dump behind
// TEAMS_BRIDGE_DUMP_ACTIVITIES=1, and the always-on line for a message whose
// attachments held no file.

import http from "node:http";
import net from "node:net";

import { afterEach, describe, expect, it } from "vitest";

import { MemoryStore } from "@loomcycle/bridge-core";

import { createTeamsBridge, type TeamsBridge } from "../src/bridge.js";
import { loadConfig } from "../src/config.js";
import { attachmentsSummary, DUMP_MAX_STRING, redactActivity } from "../src/dump.js";
import { FILE_DOWNLOAD_INFO } from "../src/voice.js";
import { FakeLoomcycle, RecordingLogger, waitFor } from "./fakes.js";

const TENANT = "11111111-1111-1111-1111-111111111111";
const ALICE = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const LINK = "https://contoso-my.sharepoint.com/personal/alice/_layouts/15/download.aspx?UniqueId=1&tempauth=secret-tempauth";

describe("redactActivity", () => {
  it("keeps a URL's host and path and drops its query and fragment", () => {
    expect(redactActivity({ downloadUrl: LINK, other: "https://a.example/x#frag", plain: "https://a.example/y" })).toEqual({
      downloadUrl: "https://contoso-my.sharepoint.com/personal/alice/_layouts/15/download.aspx?[redacted]",
      other: "https://a.example/x#[redacted]",
      plain: "https://a.example/y",
    });
  });

  it("replaces a field named like a credential, at any depth", () => {
    const out = JSON.stringify(redactActivity({ a: [{ b: { accessToken: "t-1", clientSecret: "s-1", Authorization: "Bearer x", tempauth: "q" } }] }));
    expect(out).not.toMatch(/t-1|s-1|Bearer x|"q"/);
    expect(out).toContain("[redacted]");
  });

  it("keeps everything else as Teams sent it, and cuts a long string", () => {
    const long = "x".repeat(DUMP_MAX_STRING + 5);
    expect(redactActivity({ text: "перевір", n: 3, ok: true, none: null, long })).toEqual({
      text: "перевір",
      n: 3,
      ok: true,
      none: null,
      long: `${"x".repeat(DUMP_MAX_STRING)}…(+5 chars)`,
    });
  });
});

describe("attachmentsSummary", () => {
  it("counts the attachments and lists their content types once, nothing else", () => {
    const s = attachmentsSummary([
      { contentType: "text/html", content: "" },
      { contentType: "text/html", content: "<p>a</p>" },
      { contentType: FILE_DOWNLOAD_INFO, name: "secret-name.docx", content: { downloadUrl: LINK } },
      { name: "x" },
    ]);
    expect(s).toEqual({ attachments: 4, contentTypes: ["text/html", FILE_DOWNLOAD_INFO, "(none)"] });
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

// The whole bridge, the Teams SDK app in local mode, with activities POSTed
// to /api/messages as the Bot Connector would.
describe("the Teams bridge's inbound logs", () => {
  let bridge: TeamsBridge | undefined;
  let connector: http.Server | undefined;
  let log: RecordingLogger;
  let lc: FakeLoomcycle;
  let port: number;
  let connectorUrl: string;
  let said: string[];
  let seq = 0;

  async function start(env: Record<string, string>): Promise<void> {
    lc = new FakeLoomcycle();
    log = new RecordingLogger();
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
    await new Promise<void>((r) => connector!.listen(0, "127.0.0.1", r));
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
        PORT: String(port),
        ...env,
      }),
      { clients: lc.client, logger: log, store: new MemoryStore(), stream: { stopPollMs: 5 }, fileFetch: (async () => new Response("no", { status: 404 })) as typeof fetch },
    );
    await bridge.start();
  }

  afterEach(async () => {
    for (const run of lc?.runs ?? []) run.events.end();
    await bridge?.stop();
    bridge = undefined;
    if (connector) await new Promise((r) => connector!.close(r));
    connector = undefined;
  });

  const post = (body: Record<string, unknown>, conv = "a:personal") =>
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
        conversation: { id: conv, conversationType: "personal", tenantId: TENANT },
        channelData: { tenant: { id: TENANT }, app: { version: "1.1.0" } },
        ...body,
      }),
    });

  const lines = (msg: string) => log.lines.filter((l) => l.msg === msg);

  it("dumps every inbound activity, whatever its type, with no download link's query", async () => {
    await start({ TEAMS_BRIDGE_DUMP_ACTIVITIES: "1" });
    const file = { contentType: FILE_DOWNLOAD_INFO, name: "Паспорт.docx", content: { downloadUrl: LINK, uniqueId: "u-1", fileType: "docx" } };
    expect((await post({ text: "", attachments: [{ contentType: "text/html", content: "" }, file] })).status).toBe(200);
    expect((await post({ type: "messageUpdate", channelData: { eventType: "editMessage", tenant: { id: TENANT } } })).status).toBe(200);
    await waitFor(() => lines("teams activity").length >= 2, "two dumps");

    const [msg, upd] = lines("teams activity");
    expect(msg!.attrs).toMatchObject({ type: "message", conversationType: "personal", attachments: 2, contentTypes: ["text/html", FILE_DOWNLOAD_INFO] });
    const dumped = JSON.stringify(msg!.attrs);
    expect(dumped).toContain("Паспорт.docx");
    expect(dumped).toContain("contoso-my.sharepoint.com/personal/alice/_layouts/15/download.aspx?[redacted]");
    expect(dumped).not.toContain("tempauth");
    expect(upd!.attrs).toMatchObject({ type: "messageUpdate" });
    expect(lines("TEAMS_BRIDGE_DUMP_ACTIVITIES is on: every inbound activity is logged, what people write included")).toHaveLength(1);
    // The dump only watches: the file still goes to the file path.
    await waitFor(() => said.length > 0, "the file path's answer");
    expect(said[0]).toMatch(/file/i);
  });

  it("dumps nothing with the switch off", async () => {
    await start({});
    expect((await post({ text: "hi" })).status).toBe(200);
    await waitFor(() => lc.runs.length > 0, "a run");
    expect(lines("teams activity")).toEqual([]);
  });

  it("logs a message whose attachments held no file by count and content type, and not a plain message", async () => {
    await start({});
    // What a live tenant sent for an attached .docx: an empty text/html, no text.
    expect((await post({ attachments: [{ contentType: "text/html", content: "" }] })).status).toBe(200);
    // A formatted message: text with its text/html body. Not a lost file.
    expect((await post({ text: "hello", attachments: [{ contentType: "text/html", content: "<p>hello</p>" }] }, "a:two")).status).toBe(200);
    // Text beside an attachment the bot cannot read: maybe a lost file.
    expect((await post({ text: "see this", attachments: [{ contentType: "reference", name: "x.docx", contentUrl: LINK }] }, "a:three")).status).toBe(200);
    await waitFor(() => lines("teams message carried no file").length >= 2, "two lines");
    await waitFor(() => lc.runs.length >= 2, "the two texts' runs");

    const got = lines("teams message carried no file").map((l) => l.attrs);
    expect(got).toEqual([
      { conversation: "a:personal", conversationType: "personal", attachments: 1, contentTypes: ["text/html"] },
      { conversation: "a:three", conversationType: "personal", attachments: 1, contentTypes: ["reference"] },
    ]);
    expect(JSON.stringify(got)).not.toMatch(/x\.docx|sharepoint/);
  });
});
