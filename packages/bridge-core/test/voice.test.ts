import { beforeEach, describe, expect, it } from "vitest";

import { EnvReader, loadVoiceConfig } from "../src/config.js";
import { UNTRUSTED_KIND } from "../src/conversation.js";
import { sharedSubject, type Identified, type Scope, type Trust } from "../src/identity.js";
import { ConversationRouter } from "../src/router.js";
import { MemoryStore } from "../src/store.js";
import { GateClient, keyFromSecret } from "../src/gate.js";
import { voiceSurfaces } from "../src/start.js";
import { HELP_TEXT, parseCommand, VOICE_HELP_TEXT } from "../src/text.js";
import { TokenBroker } from "../src/tokens.js";
import {
  DEFAULT_VOICE_CONFIG,
  echoLine,
  languageHint,
  pickFormat,
  spokenPart,
  VOICE_PREFIX,
  VOICE_TEXT,
  VoiceInfoCache,
  VoiceRefusedError,
  type VoiceConfig,
  type VoiceIn,
} from "../src/voice.js";
import {
  fakeSpeech,
  FakeLoomcycle,
  FakePlatform,
  FakeVoicePort,
  httpError,
  settle,
  silentLogger,
  waitFor,
  type FakeRef,
  type StartedRun,
} from "./fakes.js";

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
const note = (over: Partial<VoiceIn> = {}): VoiceIn => ({ kind: "voice", ref: "file-1", mime: "audio/ogg", seconds: 5, bytes: 4000, ...over });

describe("voice messages (RFC GA)", () => {
  let lc: FakeLoomcycle;
  let platform: FakePlatform;
  let voicePort: FakeVoicePort;
  let store: MemoryStore<FakeRef>;
  let now: number;
  let router: ConversationRouter<FakeRef>;

  function makeRouter(cfg: Partial<VoiceConfig> = {}, port: FakeVoicePort = voicePort, voiceOn = true) {
    const broker = new TokenBroker(lc.client("admin"), {
      agent: "helper",
      tokenName: "test-bridge",
      ttlSeconds: 43200,
      ratePerMinute: 6,
      rateBurst: 10,
      accessMode: "isolated",
      surfaces: ["speech", "transcription"],
      now: () => now,
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
        log: silentLogger,
        now: () => now,
        message: { typingEveryMs: 60_000 },
        ...(voiceOn ? { voice: { port, config: { ...DEFAULT_VOICE_CONFIG, enabled: true, ...cfg }, info: new VoiceInfoCache(() => now) } } : {}),
      },
    );
  }

  beforeEach(() => {
    lc = new FakeLoomcycle();
    lc.speech = fakeSpeech();
    platform = new FakePlatform();
    voicePort = new FakeVoicePort();
    store = new MemoryStore();
    now = 1_000_000;
    lc.now = () => now;
    router = makeRouter();
  });

  function announce(run: StartedRun, n: number) {
    run.events.push({ type: "session", session_id: "sess-1" });
    run.events.push({ type: "agent", run_id: `run-${n}`, agent_id: `agent-run-${n}`, session_id: "sess-1", parent_agent_id: null });
  }

  async function settled(before: number) {
    await settle();
    if (lc.runs.length > before) announce(lc.runs[lc.runs.length - 1]!, lc.runs.length);
    await settle();
  }

  async function speak(v: VoiceIn = note(), id: Identified = ident("alice", "Alice"), locale?: string) {
    const before = lc.runs.length;
    await router.onVoice(id, refFor(id.ok ? id.conversationId : "x"), v, locale);
    await settled(before);
  }

  async function type(text: string, id: Identified = ident("alice", "Alice")) {
    const before = lc.runs.length;
    await router.onMessage(id, refFor(id.ok ? id.conversationId : "x"), text);
    await settled(before);
  }

  /** reply ends the turn with text, as the agent would. */
  async function reply(text: string, run = lc.runs[lc.runs.length - 1]!) {
    run.events.push({ type: "text", text });
    run.events.push({ type: "awaiting_input" });
    await settle();
    await settle();
  }

  const promptOf = (run: StartedRun) => (run.opts.segments?.[0]?.content?.[0] ?? {}) as { type: string; text?: string; kind?: string };

  it("transcribes a voice note with the speaker's token, gives the agent the marked text, and answers in text and voice", async () => {
    await speak(note(), ident("alice", "Alice"), "en-GB");

    expect(voicePort.downloads).toHaveLength(1);
    const [tr] = lc.callsTo("transcribe");
    expect(tr!.token).toMatch(/^tok-test-alice-/);
    expect(tr!.args[1]).toMatchObject({ contentType: "audio/ogg", language: "en" });
    expect(promptOf(lc.runs[0]!)).toEqual({ type: "trusted-text", text: `${VOICE_PREFIX}\nwhat is the weather` });
    expect(lc.mints[0]!.opts.surfaces).toEqual(["speech", "transcription"]);

    await reply("Sunny and warm.");
    await waitFor(() => voicePort.voices.length === 1, "voice reply");
    expect(platform.messages()).toContain("Sunny and warm.");
    const [sp] = lc.callsTo("speech");
    expect(sp!.args).toEqual(["Sunny and warm.", { format: "ogg", language: "en" }]);
    expect(voicePort.voices[0]!.audio).toMatchObject({ mime: "audio/ogg", seconds: 2.5, label: "AI voice" });
    expect(voicePort.indications).toEqual(["listening", "speaking"]);
  });

  it("refuses a recording over the length or size cap before downloading it", async () => {
    await speak(note({ seconds: 121 }));
    await speak(note({ seconds: undefined, bytes: DEFAULT_VOICE_CONFIG.maxBytes + 1 }));
    expect(voicePort.downloads).toHaveLength(0);
    expect(lc.callsTo("transcribe")).toHaveLength(0);
    expect(platform.messages()).toEqual([VOICE_TEXT.tooLong(120), VOICE_TEXT.tooLong(120)]);
  });

  it("answers in text alone when the token was not granted transcription, and downloads nothing", async () => {
    lc.speech!.surfaces = { speech: false, transcription: false };
    await speak();
    expect(voicePort.downloads).toHaveLength(0);
    expect(platform.messages()).toEqual([VOICE_TEXT.unavailable]);
    expect(lc.runs).toHaveLength(0);
  });

  it("takes an untrusted speaker's voice as an untrusted block, and none at all when the operator turned that off", async () => {
    const stranger = ident("bob", "Bob", "conv-g", "group", "untrusted");
    await speak(note(), stranger);
    const block = promptOf(lc.runs[0]!);
    expect(block).toEqual({ type: "untrusted-block", kind: UNTRUSTED_KIND, text: `[Bob] ${VOICE_PREFIX}\nwhat is the weather` });

    lc = new FakeLoomcycle();
    lc.speech = fakeSpeech();
    voicePort = new FakeVoicePort();
    platform = new FakePlatform();
    router = makeRouter({ untrusted: false });
    await speak(note(), stranger);
    expect(voicePort.downloads).toHaveLength(0);
    expect(lc.runs).toHaveLength(0);
    // A group stays quiet, as it does for a photo.
    expect(platform.messages()).toEqual([]);
  });

  it("never reads a transcript as a command, and never lets typed text pass for voice", async () => {
    lc.speech!.transcript = "/new";
    await speak();
    expect(promptOf(lc.runs[0]!).text).toBe(`${VOICE_PREFIX}\n/new`);
    expect((await store.get("conv-1"))?.sessionId).toBe("sess-1");

    // A typed message that copies the marker is passed on as typed text and
    // gets no spoken reply under mirror.
    await reply("Done.");
    await waitFor(() => voicePort.voices.length === 1, "first reply spoken");
    await type(`${VOICE_PREFIX} please speak`);
    await reply("Typed answer.");
    await settle();
    expect(voicePort.voices).toHaveLength(1);
  });

  it("follows /voice: on speaks a typed turn, off silences a spoken one, and only the owner sets it in a shared chat", async () => {
    await type("/voice on");
    expect((await store.get("conv-1"))?.voice).toBe("always");
    await type("hello");
    await reply("Hi there.");
    await waitFor(() => voicePort.voices.length === 1, "always");

    await type("/voice off");
    await speak();
    await reply("Quiet answer.");
    await settle();
    expect(voicePort.voices).toHaveLength(1);

    const owner = ident("alice", "Alice", "conv-g", "group");
    const other = ident("bob", "Bob", "conv-g", "group");
    await type("hi", owner);
    await type("/voice on", other);
    expect(platform.messages().at(-1)).toMatch(/^Only Alice/);
    expect((await store.get("conv-g"))?.voice).toBeUndefined();
  });

  it("speaks only the first part of a long reply and says so in the label", async () => {
    router = makeRouter({ replyMaxChars: 60 });
    await speak();
    const long = "First sentence is here. Second sentence is also here. Third one runs past the cap for sure.";
    await reply(long);
    await waitFor(() => voicePort.voices.length === 1, "partial");
    expect(lc.callsTo("speech")[0]!.args[0]).toBe("First sentence is here. Second sentence is also here.");
    expect(voicePort.voices[0]!.audio.label).toBe(`AI voice · ${VOICE_TEXT.partial}`);
    expect(platform.messages()).toContain(long);
  });

  it("stops speaking in a conversation whose platform refused a voice message", async () => {
    voicePort.sendError = new VoiceRefusedError("privacy");
    await speak();
    await reply("Answer.");
    await waitFor(() => lc.callsTo("speech").length === 1, "spoken");
    for (let i = 0; i < 20 && (await store.get("conv-1"))?.voice !== "never"; i++) await settle();
    expect((await store.get("conv-1"))?.voice).toBe("never");
    // Another spoken turn is not voiced.
    voicePort.sendError = undefined;
    await speak();
    await reply("Second.");
    await settle();
    expect(lc.callsTo("speech")).toHaveLength(1);
  });

  it("drops a language hint the deployment refuses and transcribes anyway", async () => {
    lc.speech!.transcribeErrors.push(Object.assign(httpError(400), { code: "unsupported_language" }));
    await speak(note(), ident("alice", "Alice"), "xx");
    const calls = lc.callsTo("transcribe");
    expect(calls).toHaveLength(2);
    expect(calls[0]!.args[1]).toMatchObject({ language: "xx" });
    expect(calls[1]!.args[1]).not.toHaveProperty("language");
    expect(lc.runs).toHaveLength(1);
  });

  it("tells the speaker when nothing was heard or the engine failed, and sends the agent nothing", async () => {
    lc.speech!.transcript = "   ";
    await speak();
    lc.speech!.transcribeErrors.push(httpError(502));
    await speak();
    expect(platform.messages()).toEqual([VOICE_TEXT.empty, VOICE_TEXT.failed]);
    expect(lc.runs).toHaveLength(0);
  });

  it("on a platform that takes voice in only, transcribes and answers in text", async () => {
    const inOnly = new FakeVoicePort({ voiceFormats: [], out: "none", maxUploadBytes: 0 }, { canSend: false });
    router = makeRouter({}, inOnly);
    await speak();
    await reply("Text only.");
    await settle();
    expect(lc.callsTo("speech")).toHaveLength(0);
    await type("/voice on");
    expect(platform.messages().at(-1)).toBe("Test takes voice messages here, but replies come as text.");
  });

  it("speaks nothing when the deployment offers no format the platform shows as voice", async () => {
    lc.speech!.info.speech.formats = ["wav"];
    await speak();
    await reply("Answer.");
    await settle();
    expect(lc.callsTo("speech")).toHaveLength(0);
    expect(platform.messages()).toContain("Answer.");
  });

  it("quotes what it heard first when echo is on", async () => {
    router = makeRouter({ echo: true });
    await speak();
    expect(platform.messages()[0]).toBe('🎙 "what is the weather"');
  });

  it("lists /voice in /help only when voice is on", async () => {
    await type("/help");
    expect(platform.messages().at(-1)).toBe(`${HELP_TEXT}\n${VOICE_HELP_TEXT}`);
    router = makeRouter({}, voicePort, false);
    await type("/help", ident("carol", "Carol", "conv-2"));
    expect(platform.messages().at(-1)).toBe(HELP_TEXT);
  });
});

describe("voice helpers", () => {
  it("spokenPart keeps a short reply whole and cuts a long one at a paragraph, a sentence or a word", () => {
    expect(spokenPart("Short.", 100)).toEqual({ text: "Short.", partial: false });
    const para = `${"a".repeat(85)}.\n\n${"b".repeat(40)}`;
    expect(spokenPart(para, 100)).toEqual({ text: `${"a".repeat(85)}.`, partial: true });
    expect(spokenPart("One. Two three four five six", 15)).toEqual({ text: "One.", partial: true });
    expect(spokenPart("alpha beta gamma delta", 12)).toEqual({ text: "alpha beta", partial: true });
  });

  it("spokenPart never stops inside a code block", () => {
    const text = "Here is how.\n\n```\nlet x = 1;\nlet y = 2;\n```\nThat is all.";
    const got = spokenPart(text, 30);
    expect(got.text).toBe("Here is how.");
    expect(got.text).not.toContain("```");
  });

  it("languageHint reads a platform locale as a two-letter code", () => {
    expect(languageHint("en")).toBe("en");
    expect(languageHint("pt-BR")).toBe("pt");
    expect(languageHint("uk_UA")).toBe("uk");
    expect(languageHint("english")).toBeUndefined();
    expect(languageHint(undefined)).toBeUndefined();
  });

  it("echoLine quotes at most 300 characters", () => {
    expect(echoLine("  hello   there ")).toBe('🎙 "hello there"');
    const quoted = /^🎙 "(.*)"$/.exec(echoLine("x".repeat(400)))![1]!;
    expect(quoted.length).toBe(300);
    expect(quoted.endsWith("…")).toBe(true);
  });

  it("pickFormat takes the platform's first preference the deployment returns", () => {
    const caps = { voiceFormats: ["ogg", "mp3"] as Array<"ogg" | "mp3" | "wav">, out: "native" as const, maxUploadBytes: 1 };
    const info = (formats: string[]) => ({ speech: { enabled: true, formats }, transcription: { enabled: true } });
    expect(pickFormat(caps, info(["wav", "mp3"]))).toBe("mp3");
    expect(pickFormat(caps, info(["wav", "ogg"]))).toBe("ogg");
    expect(pickFormat(caps, info(["wav"]))).toBeUndefined();
  });

  it("parseCommand reads /voice and its three settings", () => {
    expect(parseCommand("/voice")).toBe("voice");
    expect(parseCommand(" /Voice   ON ")).toBe("voice on");
    expect(parseCommand("/voice off")).toBe("voice off");
    expect(parseCommand("/voice auto")).toBe("voice auto");
    expect(parseCommand("/voice loud")).toBeUndefined();
  });

  it("loadVoiceConfig has RFC GA's defaults and reports a bad value", () => {
    const ok = new EnvReader({});
    expect(loadVoiceConfig(ok)).toEqual(DEFAULT_VOICE_CONFIG);
    expect(ok.problems).toEqual([]);
    const set = new EnvReader({ BRIDGE_VOICE: "1", BRIDGE_VOICE_UNTRUSTED: "0", BRIDGE_VOICE_ECHO: "1", BRIDGE_VOICE_REPLY: "always", BRIDGE_VOICE_LANGUAGE: "uk-UA" });
    expect(loadVoiceConfig(set)).toMatchObject({ enabled: true, untrusted: false, echo: true, reply: "always", language: "uk" });
    const bad = new EnvReader({ BRIDGE_VOICE: "on", BRIDGE_VOICE_REPLY: "loud", BRIDGE_VOICE_LANGUAGE: "english", BRIDGE_VOICE_MAX_SECONDS: "0" });
    loadVoiceConfig(bad);
    expect(bad.problems).toHaveLength(4);
  });

  it("VoiceInfoCache asks loomcycle once per identity within its lifetime", async () => {
    const lc = new FakeLoomcycle();
    lc.speech = fakeSpeech();
    let now = 0;
    const cache = new VoiceInfoCache(() => now);
    const client = lc.client("t") as Required<ReturnType<FakeLoomcycle["client"]>>;
    await cache.surfacesOf("u", client);
    await cache.surfacesOf("u", client);
    expect(lc.callsTo("whoami")).toHaveLength(1);
    now += 11 * 60 * 1000;
    await cache.surfacesOf("u", client);
    expect(lc.callsTo("whoami")).toHaveLength(2);
    cache.forget("u");
    await cache.surfacesOf("u", client);
    expect(lc.callsTo("whoami")).toHaveLength(3);
  });
});

describe("voice grants", () => {
  it("grants a token only the surfaces its platform uses, and none with voice off", () => {
    const on = { ...DEFAULT_VOICE_CONFIG, enabled: true };
    expect(voiceSurfaces(on, new FakeVoicePort())).toEqual(["speech", "transcription"]);
    expect(voiceSurfaces(on, new FakeVoicePort({ voiceFormats: [], out: "none", maxUploadBytes: 0 }, { canSend: false }))).toEqual(["transcription"]);
    expect(voiceSurfaces(DEFAULT_VOICE_CONFIG, new FakeVoicePort())).toEqual([]);
    expect(voiceSurfaces(on, undefined)).toEqual([]);
  });

  it("asks a gate for the surfaces in the assertion's token claim", async () => {
    const posted: Array<Record<string, unknown>> = [];
    const fetchFn = (async (_url: string, init: RequestInit) => {
      posted.push(JSON.parse(String(init.body)));
      return new Response(JSON.stringify({ token: "lct_x", expires_at: new Date(Date.now() + 3600_000).toISOString() }), { status: 200 });
    }) as unknown as typeof fetch;
    const gc = new GateClient({
      baseUrl: "http://lc",
      gate: "tg",
      audience: "https://lc.example",
      key: keyFromSecret("s".repeat(40)),
      fetch: fetchFn,
      token: { agents: ["chat"], surfaces: ["speech", "transcription"] },
    });
    await gc.tokenFor({ sub: "1" });
    const payload = JSON.parse(Buffer.from(String(posted[0]!.assertion).split(".")[1]!, "base64url").toString());
    expect(payload.token).toEqual({ agents: ["chat"], surfaces: ["speech", "transcription"] });
  });
});
