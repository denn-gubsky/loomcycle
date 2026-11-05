// Voice in for Teams (RFC GA §5.4, D12): audio files attached in a 1:1 chat,
// downloaded from SharePoint with no credential, and text replies only.

import { readFileSync } from "node:fs";
import http from "node:http";
import net from "node:net";

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { DEFAULT_VOICE_CONFIG, MemoryStore, VOICE_PREFIX, voiceSurfaces, type Identified, type VoiceIn } from "@loomcycle/bridge-core";

import { createTeamsBridge, type ConversationRef, type TeamsBridge } from "../src/bridge.js";
import { loadConfig } from "../src/config.js";
import { identify } from "../src/identity.js";
import { routeMessage, type MessageRouter } from "../src/inbound.js";
import { audioFileOf, FILE_DOWNLOAD_INFO, hostAllowed, TeamsFileError, teamsVoicePort, type TeamsAttachment } from "../src/voice.js";
import { FakeLoomcycle, silentLogger, waitFor, type StartedRun } from "./fakes.js";

const TENANT = "11111111-1111-1111-1111-111111111111";
const ALICE = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const LINK = "https://contoso-my.sharepoint.com/personal/alice/_layouts/15/download.aspx?UniqueId=1&tempauth=secret-tempauth";
const HOSTS = [".sharepoint.com"];

function fileAttachment(fileType: string, over: Partial<TeamsAttachment> = {}, url = LINK): TeamsAttachment {
  return {
    contentType: FILE_DOWNLOAD_INFO,
    name: `recording.${fileType}`,
    content: { downloadUrl: url, uniqueId: "u-1", fileType },
    ...over,
  };
}

type ConvType = "personal" | "groupChat" | "channel";

function activity(type: ConvType = "personal") {
  return { from: { aadObjectId: ALICE, name: "Alice" }, conversation: { id: `conv-${type}`, tenantId: TENANT, conversationType: type } };
}

const ref = (type: ConvType = "personal"): ConversationRef => ({
  conversation: { id: `conv-${type}`, conversationType: type, tenantId: TENANT },
  serviceUrl: "https://smba.example/",
});

describe("audioFileOf", () => {
  it("reads an attached m4a as an audio file with its link, name and media type", () => {
    expect(audioFileOf([fileAttachment("m4a")])).toEqual({
      kind: "audio_file",
      ref: { url: LINK, name: "recording.m4a" },
      mime: "audio/mp4",
    });
  });

  it("maps every audio type loomcycle reads whole to its media type", () => {
    const mimes = Object.fromEntries(
      ["m4a", "mp4", "mp3", "wav", "ogg", "webm", "flac", "MP3", ".wav"].map((t) => [t, audioFileOf([fileAttachment(t)])?.mime]),
    );
    expect(mimes).toEqual({
      m4a: "audio/mp4",
      mp4: "audio/mp4",
      mp3: "audio/mpeg",
      wav: "audio/wav",
      ogg: "audio/ogg",
      webm: "audio/webm",
      flac: "audio/flac",
      MP3: "audio/mpeg",
      ".wav": "audio/wav",
    });
  });

  it("takes no document, no format that needs converting, and no other attachment kind", () => {
    expect(audioFileOf([fileAttachment("docx")])).toBeUndefined();
    expect(audioFileOf([fileAttachment("amr")])).toBeUndefined();
    expect(audioFileOf([{ ...fileAttachment("m4a"), contentType: "application/vnd.microsoft.card.adaptive" }])).toBeUndefined();
    expect(audioFileOf([{ contentType: FILE_DOWNLOAD_INFO, content: { fileType: "m4a" } }])).toBeUndefined();
    expect(audioFileOf(undefined)).toBeUndefined();
  });

  it("skips a non-audio attachment to find the audio one", () => {
    expect(audioFileOf([fileAttachment("pdf"), fileAttachment("mp3")])?.mime).toBe("audio/mpeg");
  });
});

describe("routeMessage", () => {
  type Call = { kind: "message"; text: string } | { kind: "voice"; voice: VoiceIn; locale?: string; ref: ConversationRef };
  let calls: Call[];
  let router: MessageRouter<ConversationRef>;
  const TENANTS = new Set([TENANT]);

  beforeEach(() => {
    calls = [];
    router = {
      onMessage: async (_id: Identified, _ref: ConversationRef, text: string) => {
        calls.push({ kind: "message", text });
      },
      onVoice: async (_id: Identified, r: ConversationRef, voice: VoiceIn, locale?: string) => {
        calls.push({ kind: "voice", voice, locale, ref: r });
      },
    };
  });

  const route = (type: ConvType, attachments: TeamsAttachment[], text = "", voiceOn = true, tenants = TENANTS) =>
    routeMessage(router, identify(activity(type), tenants), ref(type), text, { attachments, locale: "en-GB" }, voiceOn);

  it("routes an audio file in a 1:1 chat to the voice path with the person's locale", async () => {
    await route("personal", [fileAttachment("m4a")]);
    expect(calls).toEqual([
      { kind: "voice", voice: { kind: "audio_file", ref: { url: LINK, name: "recording.m4a" }, mime: "audio/mp4" }, locale: "en-GB", ref: ref() },
    ]);
  });

  it("sends text typed with the file after the voice message", async () => {
    await route("personal", [fileAttachment("m4a")], "answer in French");
    expect(calls.map((c) => c.kind)).toEqual(["voice", "message"]);
    expect(calls[1]).toEqual({ kind: "message", text: "answer in French" });
  });

  it("keeps a group chat's and a channel's files on the text path", async () => {
    await route("groupChat", [fileAttachment("m4a")], "hi");
    await route("channel", [fileAttachment("m4a")], "hi");
    expect(calls).toEqual([
      { kind: "message", text: "hi" },
      { kind: "message", text: "hi" },
    ]);
  });

  it("keeps a non-audio file on the text path", async () => {
    await route("personal", [fileAttachment("pdf")], "");
    expect(calls).toEqual([{ kind: "message", text: "" }]);
  });

  it("with voice off, sends everything the text path as before", async () => {
    await route("personal", [fileAttachment("m4a")], "", false);
    expect(calls).toEqual([{ kind: "message", text: "" }]);
  });

  it("sends an activity the tenant fence refused down the text path, which refuses it", async () => {
    await route("personal", [fileAttachment("m4a")], "", true, new Set(["22222222-2222-2222-2222-222222222222"]));
    expect(calls).toEqual([{ kind: "message", text: "" }]);
  });
});

/** A scripted file host: each URL answers with a status, headers and body
 *  chunks; every request is kept with its options. */
class FakeFileHost {
  readonly requests: Array<{ url: string; init: RequestInit | undefined }> = [];
  readonly routes = new Map<string, { status?: number; headers?: Record<string, string>; chunks?: Uint8Array[] }>();
  /** How many chunks a body was asked for: the cap must stop reading. */
  pulled = 0;

  fetch = (async (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    this.requests.push({ url, init });
    const r = this.routes.get(url);
    if (!r) return new Response("not found", { status: 404 });
    const chunks = [...(r.chunks ?? [])];
    const body =
      r.status && r.status >= 300 && r.status < 400
        ? null
        : new ReadableStream<Uint8Array>({
            pull: (c) => {
              const next = chunks.shift();
              if (!next) return c.close();
              this.pulled++;
              c.enqueue(next);
            },
          });
    return new Response(body, { status: r.status ?? 200, headers: r.headers ?? { "content-type": "audio/mp4" } });
  }) as typeof fetch;
}

describe("the Teams voice port", () => {
  let host: FakeFileHost;
  const port = () => teamsVoicePort<ConversationRef>({ hosts: HOSTS, fetch: host.fetch });
  const voice = (url = LINK): VoiceIn => ({ kind: "audio_file", ref: { url, name: "memo.m4a" }, mime: "audio/mp4" });
  const signal = () => AbortSignal.timeout(5_000);

  beforeEach(() => {
    host = new FakeFileHost();
  });

  it("downloads the pre-authorised link with no credential and no header of its own", async () => {
    host.routes.set(LINK, { chunks: [new Uint8Array([1, 2]), new Uint8Array([3])] });
    const got = await port().download(voice(), 1000, signal());
    expect(got).toEqual({ bytes: new Uint8Array([1, 2, 3]), mime: "audio/mp4", name: "memo.m4a" });
    expect(host.requests).toHaveLength(1);
    const init = host.requests[0]!.init!;
    expect(init.headers).toBeUndefined();
    expect(new Headers(init.headers).has("authorization")).toBe(false);
    expect(init.redirect).toBe("manual");
  });

  it("refuses a link off the SharePoint hosts without fetching it", async () => {
    for (const url of [
      "https://example.com/file.m4a",
      "https://evilsharepoint.com/file.m4a",
      "https://contoso.sharepoint.com.example.com/file.m4a",
      "http://contoso.sharepoint.com/file.m4a",
      "https://user:pw@contoso.sharepoint.com/file.m4a",
      "not a url",
    ]) {
      await expect(port().download(voice(url), 1000, signal())).rejects.toMatchObject({ name: "TeamsFileError", reason: "host" });
    }
    expect(host.requests).toEqual([]);
  });

  it("follows a redirect to another allowed host, never one off them", async () => {
    const next = "https://contoso.sharepoint.com/files/memo.m4a";
    host.routes.set(LINK, { status: 302, headers: { location: next } });
    host.routes.set(next, { chunks: [new Uint8Array([9])] });
    expect((await port().download(voice(), 1000, signal())).bytes).toEqual(new Uint8Array([9]));

    host = new FakeFileHost();
    host.routes.set(LINK, { status: 302, headers: { location: "https://attacker.example/steal" } });
    await expect(port().download(voice(), 1000, signal())).rejects.toMatchObject({ reason: "redirect" });
    expect(host.requests.map((r) => r.url)).toEqual([LINK]);
  });

  it("stops reading once the file passes the byte cap", async () => {
    const chunk = new Uint8Array(400);
    host.routes.set(LINK, { chunks: [chunk, chunk, chunk, chunk, chunk, chunk] });
    await expect(port().download(voice(), 1000, signal())).rejects.toMatchObject({ reason: "size" });
    // Three chunks pass 1000 bytes; the rest are never asked for.
    expect(host.pulled).toBeLessThanOrEqual(4);

    // A declared length over the cap is refused before reading: the body
    // here is small, so only the header can refuse it.
    host.routes.set(LINK, { headers: { "content-type": "audio/mp4", "content-length": "5000" }, chunks: [new Uint8Array(10)] });
    await expect(port().download(voice(), 1000, signal())).rejects.toMatchObject({ reason: "size" });
  });

  it("refuses a sign-in page or an error status, and never names the link", async () => {
    host.routes.set(LINK, { headers: { "content-type": "text/html; charset=utf-8" }, chunks: [new Uint8Array([60])] });
    const err = await port()
      .download(voice(), 1000, signal())
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(TeamsFileError);
    expect((err as TeamsFileError).reason).toBe("type");
    expect(String((err as Error).message)).not.toContain("tempauth");
    expect(String((err as Error).message)).not.toContain("sharepoint");

    host.routes.set(LINK, { status: 403 });
    await expect(port().download(voice(), 1000, signal())).rejects.toMatchObject({ reason: "status" });
  });

  it("takes voice in only: no sendVoice, no formats, out none, and asks for transcription alone", () => {
    const p = port();
    expect(p.sendVoice).toBeUndefined();
    expect(p.capabilities).toEqual({ voiceFormats: [], out: "none", maxUploadBytes: 0 });
    expect(voiceSurfaces({ ...DEFAULT_VOICE_CONFIG, enabled: true }, p)).toEqual(["transcription"]);
  });

  it("matches hosts by a dotted suffix only", () => {
    expect(hostAllowed(new URL("https://contoso-my.sharepoint.com/x"), HOSTS)).toBe(true);
    expect(hostAllowed(new URL("https://CONTOSO.SharePoint.com/x"), HOSTS)).toBe(true);
    expect(hostAllowed(new URL("https://sharepoint.com/x"), HOSTS)).toBe(false);
    expect(hostAllowed(new URL("https://contoso.sharepoint.us/x"), HOSTS)).toBe(false);
    expect(hostAllowed(new URL("https://contoso.sharepoint.us/x"), [".sharepoint.us"])).toBe(true);
  });
});

describe("the Teams app package and configuration", () => {
  it("lets people attach files to the bot", () => {
    const manifest = JSON.parse(readFileSync(new URL("../appPackage/manifest.json", import.meta.url), "utf8")) as {
      bots: Array<{ supportsFiles: boolean; scopes: string[] }>;
    };
    expect(manifest.bots[0]!.supportsFiles).toBe(true);
    expect(manifest.bots[0]!.scopes).toContain("personal");
  });

  const base = {
    LOOMCYCLE_BASE_URL: "http://loomcycle:8787",
    LOOMCYCLE_TOKEN: "t",
    BRIDGE_AGENT: "helper",
    TEAMS_BRIDGE_ALLOWED_TENANTS: TENANT,
    CLIENT_ID: "bot",
  };

  it("allows SharePoint hosts by default and reads TEAMS_BRIDGE_FILE_HOSTS as dotted suffixes", () => {
    expect(loadConfig(base).fileHosts).toEqual([".sharepoint.com"]);
    expect(loadConfig({ ...base, TEAMS_BRIDGE_FILE_HOSTS: "sharepoint.us, .SharePoint.de" }).fileHosts).toEqual([".sharepoint.us", ".sharepoint.de"]);
  });

  it("refuses a file-host suffix that would match almost anywhere", () => {
    expect(() => loadConfig({ ...base, TEAMS_BRIDGE_FILE_HOSTS: ".com" })).toThrow(/TEAMS_BRIDGE_FILE_HOSTS/);
    expect(() => loadConfig({ ...base, TEAMS_BRIDGE_FILE_HOSTS: "sharepoint.com/x" })).toThrow(/TEAMS_BRIDGE_FILE_HOSTS/);
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
// attached recording POSTed to /api/messages, a recording Bot Connector, a
// fake loomcycle with the speech calls and a fake SharePoint.
describe("teams bridge with voice on", () => {
  let lc: FakeLoomcycle;
  let files: FakeFileHost;
  let connector: http.Server;
  let connectorUrl: string;
  let bridge: TeamsBridge | undefined;
  let port: number;
  let seq = 0;

  beforeEach(async () => {
    lc = new FakeLoomcycle();
    lc.speech = { transcription: true, transcript: "what is on my calendar" };
    files = new FakeFileHost();
    files.routes.set(LINK, { chunks: [new Uint8Array([1, 2, 3])] });
    connector = http.createServer((req, res) => {
      req.resume();
      req.on("end", () => {
        res.writeHead(201, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ id: `act-${++seq}` }));
      });
    });
    await new Promise<void>((r) => connector.listen(0, "127.0.0.1", r));
    connectorUrl = `http://127.0.0.1:${(connector.address() as net.AddressInfo).port}/`;
    port = await freePort();
    bridge = createTeamsBridge(
      loadConfig({
        LOOMCYCLE_BASE_URL: "http://loomcycle.test",
        LOOMCYCLE_TOKEN: "admin-token",
        BRIDGE_AGENT: "helper",
        TEAMS_BRIDGE_ALLOWED_TENANTS: TENANT,
        BRIDGE_DEV: "1",
        BRIDGE_VOICE: "1",
        PORT: String(port),
      }),
      { clients: lc.client, fileFetch: files.fetch, logger: silentLogger, store: new MemoryStore(), stream: { stopPollMs: 5 } },
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
        locale: "de-DE",
        from: { id: "29:user", aadObjectId: ALICE, name: "Alice" },
        recipient: { id: "28:bot", name: "Bot" },
        conversation: { id: `a:${conversationType}`, conversationType, tenantId: TENANT },
        channelData: { tenant: { id: TENANT } },
        text,
        attachments,
      }),
    });

  const promptOf = (run: StartedRun) => run.opts.segments?.[0]?.content?.[0] as { type: string; text?: string };

  it("transcribes a recording attached in a 1:1 chat with the person's token and gives the agent the text", async () => {
    expect((await post("personal", [fileAttachment("m4a")])).status).toBe(200);
    await waitFor(() => lc.runs.length > 0, "a run");

    // The person's token may transcribe, and nothing more: Teams never speaks.
    expect(lc.mints[0]!.opts.surfaces).toEqual(["transcription"]);
    expect(files.requests).toHaveLength(1);
    expect(files.requests[0]!.init?.headers).toBeUndefined();
    const [tr] = lc.callsTo("transcribe");
    expect(tr!.token).toMatch(new RegExp(`^tok-teams-${ALICE}-`));
    expect(tr!.args[0]).toEqual(new Uint8Array([1, 2, 3]));
    expect(tr!.args[1]).toMatchObject({ contentType: "audio/mp4", filename: "recording.m4a", language: "de" });
    expect(promptOf(lc.runs[0]!)).toEqual({ type: "trusted-text", text: `${VOICE_PREFIX}\nwhat is on my calendar` });
    expect(lc.callsTo("speech")).toEqual([]);
  });

  it("does not take a recording sent in a group chat", async () => {
    expect((await post("groupChat", [fileAttachment("m4a")], "hello all")).status).toBe(200);
    await waitFor(() => lc.runs.length > 0, "a run");
    expect(files.requests).toEqual([]);
    expect(lc.callsTo("transcribe")).toEqual([]);
    expect((promptOf(lc.runs[0]!).text ?? "")).not.toContain(VOICE_PREFIX);
  });
});
