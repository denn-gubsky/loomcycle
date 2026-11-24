import { beforeEach, describe, expect, it } from "vitest";

import { EnvReader, loadFilesConfig, loadSharedConfig } from "../src/config.js";
import { putName } from "../src/conversation.js";
import { PinnedDownloadError } from "../src/download.js";
import {
  ATTACHED_FILE_LABEL,
  ATTACHED_FILES_HEADER,
  announceLine,
  declaredType,
  DEFAULT_FILES_CONFIG,
  FILE_TEXT,
  sniff,
  zipNames,
  type FileIn,
  type FilesConfig,
} from "../src/files.js";
import { sharedSubject, type Identified, type Scope, type Trust } from "../src/identity.js";
import type { Logger } from "../src/ports.js";
import { ConversationRouter } from "../src/router.js";
import { createBridge, type BridgeAdapter } from "../src/start.js";
import { MemoryStore } from "../src/store.js";
import { TokenBroker } from "../src/tokens.js";
import { fakeInbox, FakeFilePort, FakeLoomcycle, FakePlatform, httpError, settle, silentLogger, waitFor, type FakeRef, type StartedRun } from "./fakes.js";

// ---- fixtures ----

const enc = new TextEncoder();

/** zip builds a stored (uncompressed) ZIP with the given members, in order:
 *  enough of the format for the bridge's member-name check. */
function zip(members: Array<{ name: string; data?: string }>): Uint8Array {
  const parts: number[] = [];
  const central: number[] = [];
  const u16 = (a: number[], v: number) => a.push(v & 0xff, (v >>> 8) & 0xff);
  const u32 = (a: number[], v: number) => a.push(v & 0xff, (v >>> 8) & 0xff, (v >>> 16) & 0xff, (v >>> 24) & 0xff);
  for (const m of members) {
    const name = [...enc.encode(m.name)];
    const data = [...enc.encode(m.data ?? "x")];
    const offset = parts.length;
    u32(parts, 0x04034b50);
    u16(parts, 20);
    u16(parts, 0);
    u16(parts, 0);
    u16(parts, 0);
    u16(parts, 0);
    u32(parts, 0);
    u32(parts, data.length);
    u32(parts, data.length);
    u16(parts, name.length);
    u16(parts, 0);
    parts.push(...name, ...data);
    u32(central, 0x02014b50);
    u16(central, 20);
    u16(central, 20);
    u16(central, 0);
    u16(central, 0);
    u16(central, 0);
    u16(central, 0);
    u32(central, 0);
    u32(central, data.length);
    u32(central, data.length);
    u16(central, name.length);
    u16(central, 0);
    u16(central, 0);
    u16(central, 0);
    u16(central, 0);
    u32(central, 0);
    u32(central, offset);
    central.push(...name);
  }
  const cdOffset = parts.length;
  const end: number[] = [];
  u32(end, 0x06054b50);
  u16(end, 0);
  u16(end, 0);
  u16(end, members.length);
  u16(end, members.length);
  u32(end, central.length);
  u32(end, cdOffset);
  u16(end, 0);
  return new Uint8Array([...parts, ...central, ...end]);
}

const DOCX = zip([{ name: "[Content_Types].xml" }, { name: "_rels/.rels" }, { name: "word/document.xml", data: "<w:document/>" }]);
const XLSX = zip([{ name: "[Content_Types].xml" }, { name: "xl/workbook.xml" }]);
const ODT = zip([{ name: "mimetype", data: "application/vnd.oasis.opendocument.text" }, { name: "content.xml" }]);
const PDF = enc.encode("%PDF-1.7\n1 0 obj\n");
const DOC = new Uint8Array([0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1, 0, 0]);
/** A Windows executable's first bytes. */
const EXE = new Uint8Array([0x4d, 0x5a, 0x90, 0x00, 0x03, 0x00, 0x00, 0x00]);

const SECRET_URL = "https://contoso-my.sharepoint.com/personal/x/_layouts/download.aspx?tempauth=SECRETTOKEN";
const FILE_NAME = "Паспорт рішення.docx";

// ---- the byte checks ----

describe("file type checks (RFC HE)", () => {
  it("accepts each type whose bytes match it", () => {
    expect(sniff("docx", DOCX)).toBe(true);
    expect(sniff("xlsx", XLSX)).toBe(true);
    expect(sniff("odt", ODT)).toBe(true);
    expect(sniff("pdf", PDF)).toBe(true);
    expect(sniff("doc", DOC)).toBe(true);
    expect(sniff("txt", enc.encode("hello, світ"))).toBe(true);
    expect(sniff("md", enc.encode("# Title\n"))).toBe(true);
  });

  it("refuses an executable renamed to each document type", () => {
    for (const t of ["docx", "xlsx", "odt", "pdf", "doc", "txt", "md"]) expect(sniff(t, EXE), t).toBe(false);
  });

  it("refuses a spreadsheet renamed to a document and a ZIP with no content types", () => {
    expect(sniff("docx", XLSX)).toBe(false);
    expect(sniff("docx", zip([{ name: "word/document.xml" }]))).toBe(false);
  });

  it("refuses an ODF file whose mimetype names another kind", () => {
    expect(sniff("odt", zip([{ name: "mimetype", data: "application/vnd.oasis.opendocument.spreadsheet" }]))).toBe(false);
    // mimetype present but not the first member, as ODF requires.
    expect(sniff("odt", zip([{ name: "content.xml" }, { name: "mimetype", data: "application/vnd.oasis.opendocument.text" }]))).toBe(false);
  });

  it("refuses text that is not UTF-8 or carries a NUL byte", () => {
    expect(sniff("txt", new Uint8Array([0x68, 0xff, 0xfe, 0x69]))).toBe(false);
    expect(sniff("txt", new Uint8Array([0x68, 0x00, 0x69]))).toBe(false);
  });

  it("never takes a type it has no check for", () => {
    expect(sniff("exe", EXE)).toBe(false);
    expect(sniff("__proto__", EXE)).toBe(false);
  });

  it("reads member names from a truncated or foreign ZIP as nothing", () => {
    expect(zipNames(DOCX)).toEqual(["[Content_Types].xml", "_rels/.rels", "word/document.xml"]);
    expect(zipNames(DOCX.subarray(0, DOCX.length - 10))).toBeUndefined();
    expect(zipNames(EXE)).toBeUndefined();
  });

  it("refuses a declared type that disagrees with the name's extension", () => {
    expect(declaredType({ ref: 1, name: "setup.exe", type: "docx" })).toBeUndefined();
    expect(declaredType({ ref: 1, name: "Report.DOCX", type: ".docx" })).toBe("docx");
    expect(declaredType({ ref: 1, name: "notes.md" })).toBe("md");
    expect(declaredType({ ref: 1 })).toBeUndefined();
  });

  it("puts a file under its base name with the checked type as extension", () => {
    expect(putName("../../etc/Report.docx", "docx")).toBe("Report.docx");
    expect(putName("Report", "pdf")).toBe("Report.pdf");
    expect(putName(undefined, "txt")).toBe("file.txt");
    expect(putName(`${"a".repeat(300)}.pdf`, "pdf")).toHaveLength(200);
  });
});

describe("files configuration", () => {
  it("is off by default with the RFC's defaults", () => {
    const r = new EnvReader({});
    expect(loadFilesConfig(r)).toEqual(DEFAULT_FILES_CONFIG);
    expect(r.problems).toEqual([]);
    expect(DEFAULT_FILES_CONFIG).toMatchObject({ enabled: false, maxBytes: 20 * 1024 * 1024, maxCount: 5, types: ["docx", "pdf", "odt", "doc", "xlsx", "md", "txt"] });
  });

  it("reads every BRIDGE_FILES_* setting", () => {
    const r = new EnvReader({ BRIDGE_FILES: "1", BRIDGE_FILES_MAX_BYTES: "4096", BRIDGE_FILES_TYPES: ".PDF, md,pdf", BRIDGE_FILES_MAX_COUNT: "2" });
    expect(loadFilesConfig(r)).toEqual({ enabled: true, maxBytes: 4096, types: ["pdf", "md"], maxCount: 2 });
    expect(r.problems).toEqual([]);
  });

  it("refuses a type it cannot check and a cap past loomcycle's", () => {
    const r = new EnvReader({ BRIDGE_FILES_TYPES: "pdf,exe", BRIDGE_FILES_MAX_BYTES: String(65 * 1024 * 1024), BRIDGE_FILES_MAX_COUNT: "0" });
    loadFilesConfig(r);
    expect(r.problems.join("\n")).toMatch(/"exe" is not a type the bridge can check/);
    expect(r.problems.join("\n")).toMatch(/BRIDGE_FILES_MAX_BYTES/);
    expect(r.problems.join("\n")).toMatch(/BRIDGE_FILES_MAX_COUNT/);
  });

  it("is part of the shared configuration", () => {
    const r = new EnvReader({ LOOMCYCLE_BASE_URL: "https://lc.example", LOOMCYCLE_TOKEN: "lct_x", BRIDGE_AGENT: "chat", BRIDGE_FILES: "1" });
    expect(loadSharedConfig(r, { port: 0 }).files?.enabled).toBe(true);
  });
});

// ---- the conversation ----

function ident(user: string, name: string, conv = "conv-1", scope: Scope = "personal", trust: Trust = "trusted"): Identified {
  const subject = `test-${user}`;
  return {
    ok: true,
    speaker: { subject, name, trust },
    scope,
    conversationId: conv,
    runAs: scope === "personal" ? { subject, name } : { subject: sharedSubject("test", "org", conv), name: "Test group" },
  };
}
const refFor = (conv = "conv-1"): FakeRef => ({ conversation: conv });

/** A logger that keeps every line with its fields, as JSON. */
function recordingLogger(): { logger: Logger; text: () => string; lines: Array<{ msg: string; fields?: Record<string, unknown> }> } {
  const lines: Array<{ msg: string; fields?: Record<string, unknown> }> = [];
  const at = (msg: string, fields?: Record<string, unknown>) => void lines.push({ msg, ...(fields ? { fields } : {}) });
  return { logger: { info: at, warn: at, error: at }, text: () => JSON.stringify(lines), lines };
}

describe("attached files (RFC HE)", () => {
  let lc: FakeLoomcycle;
  let platform: FakePlatform;
  let filePort: FakeFilePort;
  let store: MemoryStore<FakeRef>;
  let logs: ReturnType<typeof recordingLogger>;
  let router: ConversationRouter<FakeRef>;

  function makeRouter(cfg: Partial<FilesConfig> | null = {}) {
    const broker = new TokenBroker(lc.client("admin"), {
      agent: "helper",
      tokenName: "test-bridge",
      ttlSeconds: 43200,
      ratePerMinute: 6,
      rateBurst: 10,
      accessMode: "isolated",
      now: () => 1_000_000,
    });
    return new ConversationRouter<FakeRef>(
      { idleMinutes: 60 },
      {
        platform: { name: "test", label: "Test" },
        capabilities: { streaming: "never", maxMessageChars: 20000 },
        agent: "helper",
        store,
        broker,
        clients: lc.client,
        port: platform,
        log: logs.logger,
        now: () => 1_000_000,
        message: { typingEveryMs: 60_000 },
        ...(cfg ? { files: { port: filePort, config: { ...DEFAULT_FILES_CONFIG, enabled: true, ...cfg } } } : {}),
      },
    );
  }

  beforeEach(() => {
    lc = new FakeLoomcycle();
    lc.inbox = fakeInbox();
    platform = new FakePlatform();
    filePort = new FakeFilePort();
    store = new MemoryStore();
    logs = recordingLogger();
    router = makeRouter();
    filePort.files.set(SECRET_URL, DOCX);
  });

  function announce(run: StartedRun, n: number) {
    run.events.push({ type: "session", session_id: "sess-1" });
    run.events.push({ type: "agent", run_id: `run-${n}`, agent_id: `agent-run-${n}`, session_id: "sess-1", parent_agent_id: null });
  }

  /** settled lets the conversation run, announcing each run it starts. */
  async function settled(before: number) {
    for (let i = 0; i < 4; i++) {
      await settle();
      while (lc.runs.length > before) announce(lc.runs[before]!, ++before);
    }
  }

  async function type(text: string, id: Identified = ident("alice", "Alice")) {
    const before = lc.runs.length;
    await router.onMessage(id, refFor(id.ok ? id.conversationId : "x"), text);
    await settled(before);
  }

  async function attach(files: FileIn[], text = "", id: Identified = ident("alice", "Alice")) {
    const before = lc.runs.length;
    await router.onFile(id, refFor(id.ok ? id.conversationId : "x"), files, { text });
    await settled(before);
  }

  const passport = (over: Partial<FileIn> = {}): FileIn => ({ ref: SECRET_URL, name: FILE_NAME, type: "docx", ...over });

  /** A live run, parked after its first turn. */
  async function parkedRun() {
    await type("hello");
    lc.runs[0]!.events.push({ type: "awaiting_input" });
    await settle();
  }

  function sentInputs(): string[] {
    return lc.callsTo("sendRunInput").map((c) => String(c.args[1]));
  }

  it("puts a file into the live run's inbox and tells the agent its ref, never its bytes or link", async () => {
    await parkedRun();
    await attach([passport()], "перевір");
    expect(lc.inbox!.puts).toEqual([
      { runId: "run-1", name: FILE_NAME, bytes: DOCX.byteLength, contentType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document" },
    ]);
    const [input] = sentInputs();
    expect(input).toBeDefined();
    const lines = input!.split("\n");
    expect(lines[0]).toBe("перевір");
    expect(lines[1]).toBe(ATTACHED_FILES_HEADER);
    expect(lines[2]!.startsWith(`${ATTACHED_FILE_LABEL} `)).toBe(true);
    // Only what loomcycle answered the put with: its sanitised name and ref.
    expect(JSON.parse(lines[2]!.slice(ATTACHED_FILE_LABEL.length + 1))).toEqual({
      name: "_______________.docx",
      ref: "volume:inbox:documents/_______________.docx",
      media_type: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
      bytes: DOCX.byteLength,
    });
    expect(input).not.toContain("sharepoint");
    expect(input).not.toContain("SECRETTOKEN");
    expect(input).not.toContain("word/document.xml");
    // One turn: no new run was started for the file.
    expect(lc.runs).toHaveLength(1);
    expect(platform.messages()).toEqual([]);
  });

  it("starts the run with the text and a line saying files follow, then puts them into it", async () => {
    filePort.files.set("pdf-1", PDF);
    filePort.files.set("bad-1", EXE);
    // The third file is refused before the run starts, so only two are
    // announced; the put of the second fails after, so the agent hears that
    // one announced file did not arrive.
    lc.inbox!.errors.push(undefined, httpError(500));
    await attach([passport(), passport({ ref: "pdf-1", name: "a.pdf", type: "pdf" }), passport({ ref: "bad-1" })], "check these");
    expect(lc.runs).toHaveLength(1);
    const first = lc.runs[0]!.opts.segments![0]!.content[0]!;
    expect(first).toEqual({ type: "trusted-text", text: `check these\n\n${announceLine(2)}` });
    expect(lc.inbox!.puts.map((p) => p.runId)).toEqual(["run-1"]);
    const [input] = sentInputs();
    expect(input!.split("\n")).toEqual([
      ATTACHED_FILES_HEADER,
      expect.stringMatching(/^attached_file: \{"name":"_+\.docx","ref":"volume:inbox:documents\/_+\.docx"/),
      "[One attached file could not be delivered.]",
    ]);
    expect(platform.messages()).toEqual([FILE_TEXT.type(1, DEFAULT_FILES_CONFIG.types), FILE_TEXT.failed(1)]);
  });

  it("refuses an executable renamed .docx and puts nothing", async () => {
    filePort.files.set(SECRET_URL, EXE);
    await attach([passport()], "check this");
    expect(lc.inbox!.puts).toEqual([]);
    expect(platform.messages()[0]).toBe(FILE_TEXT.type(1, DEFAULT_FILES_CONFIG.types));
    // The text still reaches the agent, as a message without the file.
    expect(lc.runs[0]!.opts.segments![0]!.content[0]).toEqual({ type: "trusted-text", text: "check this" });
  });

  it("refuses a type not taken before downloading it", async () => {
    await attach([passport({ name: "setup.exe", type: "exe" })]);
    expect(filePort.downloads).toEqual([]);
    expect(platform.messages()).toEqual([FILE_TEXT.type(1, DEFAULT_FILES_CONFIG.types)]);
  });

  it("refuses a file over the cap, declared or found while reading", async () => {
    router = makeRouter({ maxBytes: 4096 });
    await attach([passport({ bytes: 30 * 1024 * 1024 })]);
    expect(filePort.downloads).toEqual([]);
    filePort.errors.set("big", new PinnedDownloadError("size"));
    await attach([passport({ ref: "big" })]);
    expect(platform.messages()).toEqual([FILE_TEXT.tooBig(1, 4096), FILE_TEXT.tooBig(1, 4096)]);
    expect(lc.inbox!.puts).toEqual([]);
  });

  it("says a download that failed in plain words", async () => {
    filePort.errors.set(SECRET_URL, new PinnedDownloadError("host", { host: "evil.example", redirected: true }));
    await attach([passport()]);
    expect(platform.messages()).toEqual([FILE_TEXT.download(1)]);
    expect(lc.inbox!.puts).toEqual([]);
  });

  it("refuses files in a shared chat with a plain line and downloads nothing", async () => {
    await attach([passport()], "look", ident("alice", "Alice", "grp-1", "group"));
    expect(filePort.downloads).toEqual([]);
    expect(lc.inbox!.puts).toEqual([]);
    expect(platform.messages()[0]).toBe(FILE_TEXT.personalOnly);
  });

  it("refuses an untrusted speaker's files", async () => {
    await attach([passport()], "", ident("guest", "Guest", "conv-1", "personal", "untrusted"));
    expect(filePort.downloads).toEqual([]);
    expect(platform.messages()).toEqual([FILE_TEXT.unavailable]);
  });

  it("takes at most the cap per message and names how many were skipped, once", async () => {
    router = makeRouter({ maxCount: 2 });
    await parkedRun();
    const many = Array.from({ length: 5 }, () => passport());
    await attach(many);
    expect(lc.inbox!.puts).toHaveLength(2);
    expect(filePort.downloads).toHaveLength(2);
    expect(platform.messages()).toEqual([FILE_TEXT.count(3, 2)]);
    expect(sentInputs()[0]!.split("\n").filter((l) => l.startsWith(ATTACHED_FILE_LABEL))).toHaveLength(2);
  });

  it("refuses files in plain words when the loomcycle client has no inbox put", async () => {
    lc.inbox = undefined;
    await attach([passport()], "hi");
    expect(filePort.downloads).toEqual([]);
    expect(platform.messages()[0]).toBe(FILE_TEXT.unavailable);
    expect(logs.text()).toContain("putInboxFile");
    // The text still goes on.
    expect(lc.runs[0]!.opts.segments![0]!.content[0]).toEqual({ type: "trusted-text", text: "hi" });
  });

  it("says a full inbox and a token without inbox_put in plain words", async () => {
    await parkedRun();
    lc.inbox!.errors.push(Object.assign(httpError(413), { code: "inbox_full" }), Object.assign(httpError(403), { code: "restricted_token" }));
    await attach([passport(), passport()]);
    expect(platform.messages()).toEqual([FILE_TEXT.full, FILE_TEXT.unavailable]);
    expect(sentInputs()).toEqual([]);
  });

  it("puts into a new run when the live one ended before the put", async () => {
    await parkedRun();
    lc.inbox!.errors.push(httpError(409));
    await attach([passport()], "here");
    expect(lc.runs).toHaveLength(2);
    expect(lc.runs[1]!.kind).toBe("continue");
    expect(lc.runs[1]!.opts.segments![0]!.content[0]).toEqual({ type: "trusted-text", text: `here\n\n${announceLine(1)}` });
    expect(lc.inbox!.puts.map((p) => p.runId)).toEqual(["run-2"]);
    expect(sentInputs()).toHaveLength(1);
  });

  it("never logs a file's link or name", async () => {
    await parkedRun();
    await attach([passport()], "ok");
    filePort.files.set(SECRET_URL, EXE);
    await attach([passport()]);
    filePort.errors.set(SECRET_URL, new PinnedDownloadError("status", { status: 403 }));
    await attach([passport()]);
    await attach([passport({ name: "Таємне.exe", type: "exe" })]);
    lc.inbox!.errors.push(httpError(500));
    filePort.errors.clear();
    filePort.files.set(SECRET_URL, DOCX);
    await attach([passport()]);
    await waitFor(() => logs.lines.length >= 5, "log lines");
    const all = logs.text();
    expect(all).toContain("files: refused");
    expect(all).toContain("files: delivered");
    for (const leak of ["sharepoint", "SECRETTOKEN", "tempauth", "Паспорт", "Таємне", "setup"]) expect(all).not.toContain(leak);
  });

  it("asks for the question to be answered first when one is open", async () => {
    await type("hello");
    lc.runs[0]!.events.push({ type: "interruption_pending", interruption: { interrupt_id: "i1", question: "Which one?", options: [] } } as never);
    await settle();
    await attach([passport()], "this one");
    expect(lc.inbox!.puts).toEqual([]);
    expect(lc.callsTo("resolveInterrupt")).toEqual([]);
    expect(platform.messages()).toContain(FILE_TEXT.answerFirst);
  });

  it("with files off, ignores attachments and delivers the text as a plain message", async () => {
    router = makeRouter(null);
    await attach([passport()], "just text");
    expect(filePort.downloads).toEqual([]);
    expect(lc.callsTo("putInboxFile")).toEqual([]);
    expect(lc.runs[0]!.opts.segments![0]!.content[0]).toEqual({ type: "trusted-text", text: "just text" });
    expect(platform.messages()).toEqual([]);
  });
});

// ---- wiring ----

describe("files wiring", () => {
  const ENV = { LOOMCYCLE_BASE_URL: "https://lc.example", LOOMCYCLE_TOKEN: "lct_x", BRIDGE_AGENT: "chat" };

  function adapter(files: boolean): BridgeAdapter<FakeRef> {
    return {
      platform: { name: "test", label: "Test" },
      capabilities: { streaming: "never", maxMessageChars: 4000 },
      port: { send: async () => {} },
      async start() {},
      ...(files ? { files: new FakeFilePort() } : {}),
    };
  }

  async function mintedSurfaces(env: Record<string, string>, files: boolean) {
    const lc = new FakeLoomcycle();
    const r = new EnvReader({ ...ENV, ...env });
    const cfg = loadSharedConfig(r, { port: 0 });
    const bridge = createBridge(cfg, adapter(files), { clients: lc.client, logger: silentLogger, store: new MemoryStore() });
    await bridge.router.onMessage(ident("alice", "Alice"), refFor(), "hi");
    await waitFor(() => lc.mints.length > 0, "a token mint");
    return lc.mints[0]!.opts.surfaces;
  }

  it("asks for inbox_put only with files on and a platform that takes them", async () => {
    expect(await mintedSurfaces({ BRIDGE_FILES: "1" }, true)).toEqual(["inbox_put"]);
    expect(await mintedSurfaces({}, true)).toBeUndefined();
    expect(await mintedSurfaces({ BRIDGE_FILES: "1" }, false)).toBeUndefined();
  });
});
