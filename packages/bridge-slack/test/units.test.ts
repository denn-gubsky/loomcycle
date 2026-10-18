import { ConfigError, sharedSubject, VERB, type CanonicalCard, type RawRequest } from "@loomcycle/bridge-core";
import { describe, expect, it } from "vitest";

import { retryAfterMs, SlackApi, SlackApiError } from "../src/api.js";
import { renderCard, MAX_BUTTON_LABEL } from "../src/cards.js";
import { loadConfig } from "../src/config.js";
import { conversationIdOf, identify, NOT_ALLOWED_NOTICE, refOf, type InboundFields, type TrustPolicy } from "../src/identity.js";
import { CARD_BLOCK_ID, cleanInbound, readAction, readEvent, type EventEnvelope } from "../src/inbound.js";
import { SeenSet } from "../src/seen.js";
import { SlackStream } from "../src/stream.js";
import { escapeMrkdwn, neutralise } from "../src/text.js";
import { slackSignature, verifySlackRequest } from "../src/verify.js";
import { FakeSlack, LIVE_MENTION, MENTION_FORMS, silentLogger, waitFor } from "./fakes.js";

function req(body: string, headers: Record<string, string | string[]>): RawRequest {
  return { method: "POST", path: "/slack/events", query: new URLSearchParams(), headers, body: Buffer.from(body) };
}

describe("request signature", () => {
  // Slack's published example ("Verifying requests from Slack").
  const secret = "8f742231b10e8888abcd99yyyzzz85a5";
  const ts = "1531420618";
  const body =
    "token=xyzz0WbapA4vBCDEFasx0q6G&team_id=T1DC2JH3J&team_domain=testteamnow&channel_id=G8PSS9T3V&channel_name=foobar&user_id=U2CERLKJA&user_name=roadrunner&command=%2Fwebhook-collect&text=&response_url=https%3A%2F%2Fhooks.slack.com%2Fcommands%2FT1DC2JH3J%2F397700885554%2F96rGlfmibIGlgcZRskXaIFfN&trigger_id=398738663015.47445629121.803a0bc887a14d10d2c447fce8b6703c";
  const expected = "v0=a2114d57b48eac39b9ad189dd8316235a7b4a8d21a10bd27519666489c69b503";
  const at = Number(ts) * 1000;

  it("matches Slack's published example", () => {
    expect(slackSignature(secret, ts, body)).toBe(expected);
    expect(verifySlackRequest(req(body, { "x-slack-request-timestamp": ts, "x-slack-signature": expected }), secret, at)).toBe(true);
  });

  it("matches a vector computed independently (openssl)", () => {
    const b = '{"type":"event_callback","team_id":"T1","event_id":"Ev1"}';
    expect(slackSignature("generated-secret-0001", "1700000000", b)).toBe(
      "v0=99d637af492e49ad52c39a8ffe6497764dd49601182ec3dba1039a547941f402",
    );
  });

  it("refuses a wrong, missing, malformed or repeated signature", () => {
    const h = { "x-slack-request-timestamp": ts };
    expect(verifySlackRequest(req(body, { ...h, "x-slack-signature": expected.replace(/.$/, "0") }), secret, at)).toBe(false);
    expect(verifySlackRequest(req(body, h), secret, at)).toBe(false);
    expect(verifySlackRequest(req(body, { "x-slack-signature": expected }), secret, at)).toBe(false);
    expect(verifySlackRequest(req(body, { ...h, "x-slack-signature": [expected, expected] }), secret, at)).toBe(false);
    expect(verifySlackRequest(req(body, { "x-slack-request-timestamp": "15314206l8", "x-slack-signature": expected }), secret, at)).toBe(false);
    expect(verifySlackRequest(req(body, { ...h, "x-slack-signature": expected }), "another-secret", at)).toBe(false);
  });

  it("refuses a body changed by one byte", () => {
    expect(verifySlackRequest(req(`${body} `, { "x-slack-request-timestamp": ts, "x-slack-signature": expected }), secret, at)).toBe(false);
  });

  it("refuses a timestamp more than five minutes from now, either way", () => {
    const h = { "x-slack-request-timestamp": ts, "x-slack-signature": expected };
    expect(verifySlackRequest(req(body, h), secret, at + 300_000)).toBe(true);
    expect(verifySlackRequest(req(body, h), secret, at + 301_000)).toBe(false);
    expect(verifySlackRequest(req(body, h), secret, at - 301_000)).toBe(false);
  });
});

describe("config", () => {
  const base = {
    LOOMCYCLE_BASE_URL: "http://loomcycle:8787",
    LOOMCYCLE_TOKEN: "t",
    BRIDGE_AGENT: "helper",
    SLACK_BOT_TOKEN: "xoxb-1-2-abc",
    SLACK_SIGNING_SECRET: "s3cret",
    SLACK_BRIDGE_ALLOWED_TEAMS: "t0123abcd, T999",
  };

  it("reads defaults", () => {
    const cfg = loadConfig(base);
    expect(cfg.shared.port).toBe(3000);
    expect([...cfg.allowedTeams]).toEqual(["T0123ABCD", "T999"]);
    expect(cfg).toMatchObject({ sharedTrust: "trusted", streaming: true, apiBaseUrl: "https://slack.com/api" });
  });

  it("reads the Slack settings", () => {
    const cfg = loadConfig({
      ...base,
      SLACK_BRIDGE_SHARED_TRUST: "untrusted",
      SLACK_BRIDGE_STREAMING: "0",
      SLACK_BRIDGE_API_BASE_URL: "http://127.0.0.1:9999/api/",
    });
    expect(cfg).toMatchObject({ sharedTrust: "untrusted", streaming: false, apiBaseUrl: "http://127.0.0.1:9999/api" });
  });

  it("is closed by default: no allow-list, no start", () => {
    const { SLACK_BRIDGE_ALLOWED_TEAMS: _, ...rest } = base;
    expect(() => loadConfig(rest)).toThrow(/SLACK_BRIDGE_ALLOWED_TEAMS is required/);
  });

  it("lists every problem at once and never echoes a secret", () => {
    try {
      loadConfig({
        SLACK_BOT_TOKEN: "xoxp-user-token-value",
        SLACK_BRIDGE_ALLOWED_TEAMS: "not a team",
        SLACK_BRIDGE_STREAMING: "yes",
        SLACK_BRIDGE_SHARED_TRUST: "maybe",
        SLACK_BRIDGE_API_BASE_URL: "http://slack.example.com/api",
      });
      expect.unreachable();
    } catch (err) {
      expect(err).toBeInstanceOf(ConfigError);
      const problems = (err as ConfigError).problems.join("\n");
      for (const want of [
        "LOOMCYCLE_BASE_URL",
        "SLACK_SIGNING_SECRET",
        "SLACK_BOT_TOKEN must be a bot token",
        "SLACK_BRIDGE_ALLOWED_TEAMS must list",
        "SLACK_BRIDGE_STREAMING",
        "SLACK_BRIDGE_SHARED_TRUST",
        "SLACK_BRIDGE_API_BASE_URL must be https",
      ]) {
        expect(problems).toContain(want);
      }
      expect(problems).not.toContain("xoxp-user-token-value");
    }
  });
});

const policy: TrustPolicy = { allowedTeams: new Set(["T1"]), sharedTrust: "trusted" };

function fields(over: Partial<InboundFields> = {}): InboundFields {
  return { team: "T1", otherTeams: ["T1"], user: "UALICE", channel: "D1", channelType: "im", ts: "100.1", ...over };
}

describe("identity", () => {
  it("names a person by team and user, lower-cased", () => {
    const id = identify(fields(), policy, "Alice");
    expect(id).toMatchObject({
      ok: true,
      scope: "personal",
      conversationId: "slack:T1:D1",
      speaker: { subject: "slack-t1-ualice", name: "Alice", trust: "trusted" },
      runAs: { subject: "slack-t1-ualice" },
    });
  });

  it("makes a DM one conversation, threads or not", () => {
    expect(conversationIdOf(fields({ threadTs: "99.9" }))).toBe("slack:T1:D1");
  });

  it("makes each channel thread its own conversation, with the channel's memory", () => {
    const top = identify(fields({ channel: "C1", channelType: "channel", ts: "200.1" }), policy, "Alice");
    const reply = identify(fields({ channel: "C1", channelType: "channel", ts: "200.5", threadTs: "200.1" }), policy, "Alice");
    const other = identify(fields({ channel: "C1", channelType: "channel", ts: "300.1" }), policy, "Alice");
    if (!top.ok || !reply.ok || !other.ok) throw new Error("refused");
    expect(top.conversationId).toBe("slack:T1:C1:200.1");
    expect(reply.conversationId).toBe(top.conversationId);
    expect(other.conversationId).not.toBe(top.conversationId);
    expect(top.runAs.subject).toBe(sharedSubject("slack", "t1", "c1"));
    expect(other.runAs.subject).toBe(top.runAs.subject);
    expect(top.speaker.subject).toBe("slack-t1-ualice");
    expect(top.scope).toBe("channel");
  });

  it("scopes a group DM as a group", () => {
    expect(identify(fields({ channel: "G1", channelType: "mpim" }), policy, "Alice")).toMatchObject({ ok: true, scope: "group" });
  });

  it("marks shared speakers by the setting, and a DM always trusted", () => {
    const untrusted: TrustPolicy = { ...policy, sharedTrust: "untrusted" };
    expect(identify(fields(), untrusted, "A")).toMatchObject({ speaker: { trust: "trusted" } });
    expect(identify(fields({ channel: "C1", channelType: "channel" }), untrusted, "A")).toMatchObject({ speaker: { trust: "untrusted" } });
    expect(identify(fields({ channel: "G1", channelType: "mpim" }), untrusted, "A")).toMatchObject({ speaker: { trust: "untrusted" } });
    expect(identify(fields({ channel: "C1", channelType: "channel" }), policy, "A")).toMatchObject({ speaker: { trust: "trusted" } });
  });

  it("refuses a workspace not in the allow-list, with a notice", () => {
    expect(identify(fields({ team: "T2", otherTeams: ["T2"] }), policy, "A")).toEqual({
      ok: false,
      reason: "team_not_allowed",
      notice: NOT_ALLOWED_NOTICE,
      conversationId: "slack:T2:D1",
    });
  });

  it("refuses a speaker from another organisation in an allowed workspace", () => {
    expect(identify(fields({ otherTeams: ["T9"] }), policy, "A")).toMatchObject({ ok: false, reason: "team_not_allowed" });
    expect(identify(fields({ otherTeams: ["T1", "T9"] }), policy, "A")).toMatchObject({ ok: false, reason: "team_not_allowed" });
  });

  it("replies in the thread in a channel, and where the person wrote in a DM", () => {
    expect(refOf(fields({ channel: "C1", channelType: "channel", ts: "5.1" }))).toMatchObject({ threadTs: "5.1", messageTs: "5.1" });
    expect(refOf(fields({ channel: "C1", channelType: "channel", ts: "5.2", threadTs: "5.1" }))).toMatchObject({ threadTs: "5.1" });
    expect(refOf(fields({ ts: "6.1" }))).not.toHaveProperty("threadTs");
    expect(refOf(fields({ ts: "6.2", threadTs: "6.1" }))).toMatchObject({ threadTs: "6.1", messageTs: "6.2" });
  });
});

describe("events", () => {
  const self = { userId: "UBOT" };
  const env = (event: Record<string, unknown>): EventEnvelope => ({ type: "event_callback", team_id: "T1", event_id: "Ev1", event });

  it("takes a DM", () => {
    const r = readEvent(env({ type: "message", channel_type: "im", user: "UALICE", text: "hi &amp; bye", ts: "1.1", channel: "D1", team: "T1" }), self);
    expect(r).toMatchObject({ kind: "message", text: "hi & bye", fields: { channelType: "im", user: "UALICE", otherTeams: ["T1"] } });
  });

  it("takes a channel mention, without the mention", () => {
    const r = readEvent(env({ type: "app_mention", user: "UALICE", text: "<@UBOT> what time is it?", ts: "1.1", channel: "C1" }), self);
    expect(r).toMatchObject({ kind: "message", text: "what time is it?", fields: { channelType: "channel", channel: "C1" } });
  });

  it("ignores a channel's plain message, so a mention is answered once", () => {
    expect(readEvent(env({ type: "message", channel_type: "channel", user: "UALICE", text: "<@UBOT> hi", ts: "1", channel: "C1" }), self)).toEqual({
      kind: "ignore",
    });
  });

  it("drops edits, joins, bots and the bot itself", () => {
    const dm = { type: "message", channel_type: "im", user: "UALICE", text: "x", ts: "1", channel: "D1" };
    for (const e of [
      { ...dm, subtype: "message_changed" },
      { ...dm, subtype: "bot_message" },
      { ...dm, bot_id: "B2" },
      { ...dm, user: "UBOT" },
      { type: "app_mention", user: "UOTHERBOT", bot_id: "B3", text: "<@UBOT> hi", ts: "1", channel: "C1" },
      { type: "app_mention", user: "UBOT", text: "<@UBOT> hi", ts: "1", channel: "C1" },
    ]) {
      expect(readEvent(env(e), self)).toEqual({ kind: "self_or_bot" });
    }
  });

  it("carries a Slack Connect speaker's own team for the fence", () => {
    const r = readEvent(env({ type: "app_mention", user: "UX", user_team: "T9", text: "hi", ts: "1", channel: "C1" }), self);
    expect(r).toMatchObject({ fields: { team: "T1", otherTeams: ["T9"] } });
  });

  it("unwraps links and decodes entities", () => {
    expect(cleanInbound("see <https://a.example/x?y=1&amp;z=2|the doc> and <https://b.example> &lt;3", "UBOT")).toBe(
      "see the doc (https://a.example/x?y=1&z=2) and https://b.example <3",
    );
    expect(cleanInbound("<@UBOT|bot> hello <@UALICE>", "UBOT")).toBe("hello <@UALICE>");
  });
});

describe("cards", () => {
  const question = (options: string[]): CanonicalCard => ({
    kind: "question",
    runId: "r1",
    interruptId: "q1",
    question: "Which **floor**?",
    options,
    buttons: [
      ...(options.length > 0
        ? options.map((o, i) => ({ id: `b${i}`, label: o, action: { verb: VERB.answer, runId: "r1", interruptId: "q1", answer: o } }))
        : [{ id: "bs", label: "Send", action: { verb: VERB.answer, runId: "r1", interruptId: "q1" } }]),
      { id: "bd", label: "Decline", action: { verb: VERB.decline, runId: "r1", interruptId: "q1" } },
    ],
  });

  const actionsOf = (blocks: Array<Record<string, unknown>>) => blocks.find((b) => b.type === "actions") as { block_id: string; elements: Array<Record<string, unknown>> };

  it("renders one button per option and decline, each carrying only its id", () => {
    const c = renderCard(question(["2", "3"]), "channel");
    expect(c.blocks[0]).toEqual({ type: "markdown", text: "Which **floor**?" });
    const a = actionsOf(c.blocks);
    expect(a.block_id).toBe(CARD_BLOCK_ID.channel);
    expect(a.elements.map((e) => e.value)).toEqual(["b0", "b1", "bd"]);
    const ids = a.elements.map((e) => e.action_id);
    expect(new Set(ids).size).toBe(ids.length);
  });

  it("cuts a long option's button label and shows the option whole", () => {
    const long = "x".repeat(150);
    const c = renderCard(question([long]), "im");
    const label = (actionsOf(c.blocks).elements[0].text as { text: string }).text;
    expect(label.length).toBe(MAX_BUTTON_LABEL);
    expect(c.blocks.some((b) => b.type === "markdown" && String(b.text).includes(long))).toBe(true);
  });

  it("asks for a reply instead of a send button on a free-text question", () => {
    const c = renderCard(question([]), "channel");
    expect(actionsOf(c.blocks).elements.map((e) => e.value)).toEqual(["bd"]);
    expect(JSON.stringify(c.blocks)).toContain("Reply in this thread with your answer");
  });

  it("renders a review with approve and send back", () => {
    const c = renderCard(
      {
        kind: "review",
        runId: "r1",
        round: 2,
        buttons: [
          { id: "ba", label: "Approve", action: { verb: VERB.approve, runId: "r1" } },
          { id: "br", label: "Send back", action: { verb: VERB.reject, runId: "r1" } },
        ],
      },
      "im",
    );
    expect(c.text).toContain("round 2");
    expect(actionsOf(c.blocks).elements).toMatchObject([{ value: "ba", style: "primary" }, { value: "br" }]);
  });

  it("round-trips: a press of a rendered button reads back as that button", () => {
    const c = renderCard(question(["2"]), "mpim");
    const a = actionsOf(c.blocks);
    const el = a.elements[0];
    const read = readAction({
      type: "block_actions",
      team: { id: "T1" },
      user: { id: "UALICE", team_id: "T1" },
      channel: { id: "G1" },
      message: { ts: "9.2", thread_ts: "9.1" },
      response_url: "https://hooks.slack.com/actions/x",
      actions: [{ type: "button", action_id: String(el.action_id), block_id: a.block_id, value: String(el.value) }],
    });
    expect(read).toMatchObject({ buttonId: "b0", fields: { channelType: "mpim", threadTs: "9.1", channel: "G1" } });
    expect(Buffer.byteLength(String(el.value))).toBeLessThanOrEqual(2000);
  });

  it("ignores a press that is not one of the bridge's buttons", () => {
    const p = {
      type: "block_actions",
      team: { id: "T1" },
      user: { id: "U1" },
      channel: { id: "C1" },
      message: { ts: "1" },
      actions: [{ type: "button", action_id: "lc.x", block_id: CARD_BLOCK_ID.channel, value: "x" }],
    };
    expect(readAction(p)).toBeDefined();
    expect(readAction({ ...p, actions: [{ ...p.actions[0], action_id: "theirs" }] })).toBeUndefined();
    expect(readAction({ ...p, actions: [{ ...p.actions[0], block_id: "theirs" }] })).toBeUndefined();
    expect(readAction({ ...p, actions: [{ ...p.actions[0], value: "x".repeat(33) }] })).toBeUndefined();
    expect(readAction({ ...p, type: "view_submission" })).toBeUndefined();
  });

  it("defuses a broadcast mention in the agent's text", () => {
    const c = renderCard({ kind: "result", message: "hey <!channel> and <!here>" }, "channel");
    expect(JSON.stringify(c.blocks)).not.toContain("<!");
    expect(neutralise(neutralise("<!here>"))).toBe(neutralise("<!here>"));
  });

  it("carries no live mention from a question, its context or its options", () => {
    const all = MENTION_FORMS.join(" ");
    for (const form of MENTION_FORMS) {
      const card: CanonicalCard = {
        kind: "question",
        runId: "r1",
        interruptId: "q1",
        question: `Ping ${form}?`,
        context: `context ${form}`,
        options: [`yes ${form}`, all],
        buttons: [
          { id: "b0", label: `yes ${form}`, action: { verb: VERB.answer, runId: "r1", interruptId: "q1", answer: `yes ${form}` } },
          { id: "b1", label: all, action: { verb: VERB.answer, runId: "r1", interruptId: "q1", answer: all } },
        ],
      };
      expect(JSON.stringify(renderCard(card, "channel"))).not.toMatch(LIVE_MENTION);
      expect(JSON.stringify(renderCard({ kind: "result", message: `Answered: ${form}` }, "channel"))).not.toMatch(LIVE_MENTION);
    }
  });
});

describe("outbound text", () => {
  it("defuses every mention form in Markdown, and stays readable", () => {
    for (const form of MENTION_FORMS) {
      const out = neutralise(`x ${form} y`);
      expect(out).not.toMatch(LIVE_MENTION);
      expect(out.replace(/\u2060/g, "")).toBe(`x ${form} y`);
      expect(neutralise(out)).toBe(out);
    }
  });

  it("escapes mrkdwn's control characters and defuses bare mentions", () => {
    expect(escapeMrkdwn("a & <b> <!here> @channel")).toBe("a &amp; &lt;b&gt; &lt;!here&gt; @\u2060channel");
    for (const form of MENTION_FORMS) expect(escapeMrkdwn(form)).not.toMatch(LIVE_MENTION);
  });
});

describe("web api", () => {
  it("honours Retry-After on a 429, then succeeds", async () => {
    const slack = new FakeSlack();
    slack.reply("chat.postMessage", { status: 429, headers: { "retry-after": "2" } });
    const slept: number[] = [];
    const api = new SlackApi({ baseUrl: slack.base, token: "xoxb-secret", fetch: slack.fetch, sleep: async (ms) => void slept.push(ms) });
    await expect(api.call("chat.postMessage", { channel: "C1" })).resolves.toMatchObject({ ok: true });
    expect(slept).toEqual([2000]);
    expect(slack.to("chat.postMessage")).toHaveLength(2);
    expect(slack.calls[0].auth).toBe("Bearer xoxb-secret");
  });

  it("gives up on a Retry-After it should not wait out", async () => {
    const slack = new FakeSlack();
    slack.reply("chat.postMessage", { status: 429, headers: { "retry-after": "120" } });
    const api = new SlackApi({ baseUrl: slack.base, token: "xoxb-secret", fetch: slack.fetch, sleep: async () => {} });
    await expect(api.call("chat.postMessage", {})).rejects.toMatchObject({ code: "ratelimited", status: 429 });
  });

  it("reports Slack's error code and nothing else", async () => {
    const slack = new FakeSlack();
    slack.reply("chat.postMessage", { json: { ok: false, error: "channel_not_found" } });
    slack.reply("chat.postMessage", { json: { ok: false, error: "xoxb-secret leaked?" } });
    const api = new SlackApi({ baseUrl: slack.base, token: "xoxb-secret", fetch: slack.fetch });
    const e1 = await api.call("chat.postMessage", {}).catch((e: unknown) => e);
    expect(e1).toBeInstanceOf(SlackApiError);
    expect((e1 as SlackApiError).code).toBe("channel_not_found");
    const e2 = (await api.call("chat.postMessage", {}).catch((e: unknown) => e)) as SlackApiError;
    expect(e2.code).toBe("unknown_error");
    expect(e2.message).not.toContain("xoxb");
  });

  it("never sends the bot token to a response_url", async () => {
    const slack = new FakeSlack();
    const api = new SlackApi({ baseUrl: slack.base, token: "xoxb-secret", fetch: slack.fetch });
    await api.respond("https://hooks.slack.com/actions/1/2/3", { text: "x" });
    expect(slack.calls[0]).toMatchObject({ method: "response_url" });
    expect(slack.calls[0].auth).toBeUndefined();
  });

  it("reads Retry-After as seconds", () => {
    expect(retryAfterMs("30")).toBe(30_000);
    expect(retryAfterMs(null)).toBe(1000);
    expect(retryAfterMs("soon")).toBe(1000);
  });
});

describe("seen set", () => {
  it("drops a repeat inside the TTL, and forgets after it", () => {
    let now = 0;
    const s = new SeenSet(1000, 10, () => now);
    expect(s.add("a")).toBe(true);
    expect(s.add("a")).toBe(false);
    now = 1000;
    expect(s.add("a")).toBe(true);
  });

  it("stays bounded", () => {
    const s = new SeenSet(60_000, 3, () => 0);
    for (const id of ["a", "b", "c", "d", "e"]) s.add(id);
    expect(s.size).toBe(3);
    expect(s.add("e")).toBe(false);
    expect(s.add("a")).toBe(true);
  });
});

describe("stream", () => {
  function make(slack: FakeSlack, over: { maxChars?: number } = {}) {
    const api = new SlackApi({ baseUrl: slack.base, token: "xoxb-t", fetch: slack.fetch, sleep: async () => {} });
    const fallback: string[] = [];
    const s = new SlackStream(api, {
      channel: "D1",
      threadTs: "1.1",
      maxChars: over.maxChars ?? 10_000,
      intervalMs: 20,
      fallback: async (t) => void fallback.push(t),
      log: silentLogger,
      conversation: "c",
    });
    return { s, fallback };
  }

  it("starts in the thread, appends what follows, and stops on close", async () => {
    const slack = new FakeSlack();
    const { s } = make(slack);
    s.emit("Hel");
    await waitFor(() => slack.to("chat.startStream").length === 1, "start");
    s.emit("lo ");
    s.emit("world");
    await s.close();
    expect(slack.to("chat.startStream")[0].body).toMatchObject({ channel: "D1", thread_ts: "1.1", markdown_text: "Hel" });
    const appended = slack.to("chat.appendStream").map((c) => c.body.markdown_text).join("");
    expect(appended).toBe("lo world");
    expect(slack.to("chat.appendStream").every((c) => c.body.ts === slack.to("chat.stopStream")[0].body.ts)).toBe(true);
    expect(slack.to("chat.stopStream")).toHaveLength(1);
  });

  it("batches the deltas of one interval into one append", async () => {
    const slack = new FakeSlack();
    const { s } = make(slack);
    s.emit("a");
    await waitFor(() => slack.to("chat.startStream").length === 1, "start");
    for (const t of ["b", "c", "d", "e"]) s.emit(t);
    await waitFor(() => slack.to("chat.appendStream").length === 1, "append");
    await s.close();
    expect(slack.to("chat.appendStream")).toHaveLength(1);
    expect(slack.to("chat.appendStream")[0].body.markdown_text).toBe("bcde");
  });

  it("sends the turn as messages when the stream cannot start", async () => {
    const slack = new FakeSlack();
    slack.reply("chat.startStream", { json: { ok: false, error: "invalid_thread_ts" } });
    const { s, fallback } = make(slack);
    s.emit("Hello ");
    await waitFor(() => slack.to("chat.startStream").length === 1, "start");
    s.emit("there");
    await s.close();
    expect(fallback).toEqual(["Hello there"]);
    expect(slack.to("chat.appendStream")).toHaveLength(0);
    expect(slack.to("chat.stopStream")).toHaveLength(0);
  });

  it("reports a stop pressed in Slack", async () => {
    const slack = new FakeSlack();
    slack.reply("chat.appendStream", { json: { ok: false, error: "stopped_by_user" } });
    const { s, fallback } = make(slack);
    s.emit("a");
    await waitFor(() => slack.to("chat.startStream").length === 1, "start");
    s.emit("b");
    await waitFor(() => s.canceled, "canceled");
    await s.close();
    expect(fallback).toEqual([]);
  });

  it("continues in a new message past the size limit", async () => {
    const slack = new FakeSlack();
    const { s } = make(slack, { maxChars: 5 });
    s.emit("abcdefghijkl");
    await s.close();
    expect(slack.to("chat.startStream").map((c) => c.body.markdown_text)).toEqual(["abcde", "fghij", "kl"]);
    expect(slack.to("chat.stopStream")).toHaveLength(3);
  });

  it("defuses a broadcast mention split across two deltas", async () => {
    const slack = new FakeSlack();
    const { s } = make(slack);
    s.emit("ping <");
    await waitFor(() => slack.to("chat.startStream").length === 1, "start");
    // A "<" is broken whatever follows it, so it need not wait for the next delta.
    expect(slack.to("chat.startStream")[0].body.markdown_text).toBe("ping <\u2060");
    s.emit("!channel> now");
    await s.close();
    const sent = [...slack.to("chat.startStream"), ...slack.to("chat.appendStream")].map((c) => c.body.markdown_text).join("");
    expect(sent).not.toContain("<!");
    expect(sent.replace(/\u2060/g, "")).toBe("ping <!channel> now");
  });

  it("defuses a bare mention split across two deltas", async () => {
    const slack = new FakeSlack();
    const { s } = make(slack);
    s.emit("hey @he");
    await waitFor(() => slack.to("chat.startStream").length === 1, "start");
    s.emit("re and <@U0123ABC>");
    await s.close();
    const sent = [...slack.to("chat.startStream"), ...slack.to("chat.appendStream")].map((c) => c.body.markdown_text).join("");
    expect(sent).not.toMatch(LIVE_MENTION);
    expect(sent.replace(/\u2060/g, "")).toBe("hey @here and <@U0123ABC>");
  });
});
