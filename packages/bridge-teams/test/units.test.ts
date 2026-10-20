import { createHash } from "node:crypto";

import { ConfigError, VERB, type CanonicalCard } from "@loomcycle/bridge-core";
import { describe, expect, it } from "vitest";

import { parseCardAction, questionCard, renderCard } from "../src/cards.js";
import { loadConfig } from "../src/config.js";
import { conversationSubject, identify, NOT_AVAILABLE_TEXT, subjectFor } from "../src/identity.js";

const T = "11111111-1111-1111-1111-111111111111";
const U = "AAAAAAAA-aaaa-aaaa-aaaa-aaaaaaaaaaaa";

describe("identity", () => {
  it("maps the Entra object id to a stable, lower-cased subject", () => {
    expect(subjectFor(U)).toBe("teams-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa");
    expect(subjectFor("not-a-guid")).toBeUndefined();
  });

  it("reads the tenant from the conversation or, failing that, channelData", () => {
    const fromChannelData = identify(
      { from: { aadObjectId: U }, conversation: { id: "c" }, channelData: { tenant: { id: T.toUpperCase() } } },
      new Set([T]),
    );
    expect(fromChannelData).toMatchObject({ ok: true, scope: "personal", speaker: { trust: "trusted" } });
  });

  it("refuses an activity whose tenant ids disagree, even when one is allowed", () => {
    const other = "22222222-2222-2222-2222-222222222222";
    const refused = { ok: false, reason: "tenant_not_allowed", notice: NOT_AVAILABLE_TEXT, conversationId: "c" };
    expect(
      identify({ from: { aadObjectId: U }, conversation: { id: "c", tenantId: T }, channelData: { tenant: { id: other } } }, new Set([T])),
    ).toEqual(refused);
    expect(
      identify({ from: { aadObjectId: U }, conversation: { id: "c", tenantId: other }, channelData: { tenant: { id: T } } }, new Set([T])),
    ).toEqual(refused);
  });

  it("refuses a missing user or a tenant outside the allowlist", () => {
    expect(identify({ conversation: { id: "c", tenantId: T } }, new Set([T]))).toEqual({ ok: false, reason: "no_user" });
    expect(identify({ from: { aadObjectId: U }, conversation: { id: "c", tenantId: "x" } }, new Set([T]))).toMatchObject({
      ok: false,
      reason: "tenant_not_allowed",
    });
  });

  it("maps Teams' conversation types to the core's scopes and run identities", () => {
    const act = (type: string) => ({ from: { aadObjectId: U, name: "Al" }, conversation: { id: "19:x", tenantId: T, conversationType: type } });
    expect(identify(act("groupChat"), new Set([T]))).toMatchObject({ scope: "group", runAs: { name: "Teams group chat" } });
    expect(identify(act("channel"), new Set([T]))).toMatchObject({ scope: "channel", runAs: { name: "Teams channel" } });
    expect(identify(act("personal"), new Set([T]))).toMatchObject({ scope: "personal", runAs: { subject: subjectFor(U), name: "Al" } });
  });

  it("keeps the shared-conversation subject the Teams bridge has always used", () => {
    // Before the core existed: sha256("<tenant>\n<conversation without thread>")[:32].
    const conv = "19:chan@thread.tacv2;messageid=111";
    const before = `teams-conv-${createHash("sha256").update(`${T}\n19:chan@thread.tacv2`).digest("hex").slice(0, 32)}`;
    expect(conversationSubject(T.toUpperCase(), conv)).toBe(before);
  });

  it("gives every thread of a channel the channel's one identity", () => {
    const a = conversationSubject(T, "19:chan@thread.tacv2;messageid=111");
    const b = conversationSubject(T, "19:chan@thread.tacv2;messageid=222");
    expect(a).toBe(b);
    expect(a).not.toBe(conversationSubject(T, "19:other@thread.tacv2;messageid=111"));
  });
});

describe("config", () => {
  const base = {
    LOOMCYCLE_BASE_URL: "http://loomcycle:8787/",
    LOOMCYCLE_TOKEN: "t",
    BRIDGE_AGENT: "helper",
    TEAMS_BRIDGE_ALLOWED_TENANTS: `${T.toUpperCase()}, `,
    CLIENT_ID: "bot",
  };

  it("reads defaults and normalises tenants", () => {
    const cfg = loadConfig(base);
    expect(cfg.shared.loomcycleBaseUrl).toBe("http://loomcycle:8787");
    expect([...cfg.allowedTenants]).toEqual([T]);
    expect(cfg.shared).toMatchObject({ tokenTtlSeconds: 43200, userAccessMode: "isolated", idleMinutes: 1440, store: "memory", dev: false, port: 3978 });
  });

  it("refuses to start without bot credentials unless in dev mode", () => {
    const { CLIENT_ID: _drop, ...noBot } = base;
    expect(() => loadConfig(noBot)).toThrow(ConfigError);
    expect(loadConfig({ ...noBot, BRIDGE_DEV: "1" }).shared.dev).toBe(true);
  });

  it("lists every problem at once", () => {
    try {
      loadConfig({ BRIDGE_USER_ACCESS_MODE: "admin" });
      expect.unreachable();
    } catch (err) {
      expect((err as ConfigError).problems).toEqual(
        expect.arrayContaining(["TEAMS_BRIDGE_ALLOWED_TENANTS is required", "BRIDGE_AGENT is required", expect.stringContaining("CLIENT_ID")]),
      );
    }
  });
});

describe("cards", () => {
  it("offers a text field when the question has no options", () => {
    const card = questionCard({ runId: "r", interruptId: "q", question: "Why?", options: [] }) as { body: Array<{ type: string }> };
    expect(card.body.some((b) => b.type === "Input.Text")).toBe(true);
  });

  it("ignores card actions the bridge did not issue", () => {
    expect(parseCardAction({ action: { verb: "other", data: { runId: "r" } } })).toBeUndefined();
    expect(parseCardAction({ action: { verb: VERB.approve, data: {} } })).toBeUndefined();
    expect(parseCardAction({ action: { verb: VERB.answer, data: { runId: "r", interruptId: "q", answer: " yes " } } })).toEqual({
      verb: VERB.answer,
      runId: "r",
      interruptId: "q",
      answer: "yes",
      feedback: undefined,
    });
  });

  // The Adaptive Cards the bridge sent before it moved onto the core, byte for
  // byte: moving must not change what a Teams user sees or clicks.
  it("renders the core's cards exactly as the Teams bridge always has", () => {
    const question: CanonicalCard = {
      kind: "question",
      runId: "run-1",
      interruptId: "q1",
      question: "Which floor?",
      context: "Only Alice can answer.",
      options: ["2", "3"],
      buttons: [],
    };
    expect(JSON.stringify(renderCard(question))).toBe(
      '{"type":"AdaptiveCard","$schema":"http://adaptivecards.io/schemas/adaptive-card.json","version":"1.5","body":[{"type":"TextBlock","text":"Which floor?","wrap":true,"weight":"Bolder"},{"type":"TextBlock","text":"Only Alice can answer.","wrap":true,"isSubtle":true,"size":"Small"}],"actions":[{"type":"Action.Execute","title":"2","verb":"lc.answer","data":{"runId":"run-1","interruptId":"q1","answer":"2"}},{"type":"Action.Execute","title":"3","verb":"lc.answer","data":{"runId":"run-1","interruptId":"q1","answer":"3"}},{"type":"Action.Execute","title":"Decline","verb":"lc.decline","data":{"runId":"run-1","interruptId":"q1"}}]}',
    );
    expect(JSON.stringify(renderCard({ ...question, context: undefined, options: [] }))).toBe(
      '{"type":"AdaptiveCard","$schema":"http://adaptivecards.io/schemas/adaptive-card.json","version":"1.5","body":[{"type":"TextBlock","text":"Which floor?","wrap":true,"weight":"Bolder"},{"type":"Input.Text","id":"answer","isMultiline":true,"placeholder":"Your answer"}],"actions":[{"type":"Action.Execute","title":"Send","verb":"lc.answer","data":{"runId":"run-1","interruptId":"q1"}},{"type":"Action.Execute","title":"Decline","verb":"lc.decline","data":{"runId":"run-1","interruptId":"q1"}}]}',
    );
    expect(JSON.stringify(renderCard({ kind: "review", runId: "run-1", round: 2, buttons: [] }))).toBe(
      '{"type":"AdaptiveCard","$schema":"http://adaptivecards.io/schemas/adaptive-card.json","version":"1.5","body":[{"type":"TextBlock","text":"The revised answer (round 2) is ready for review.","wrap":true,"weight":"Bolder"},{"type":"Input.Text","id":"feedback","isMultiline":true,"placeholder":"Feedback (to send it back for a revision)"}],"actions":[{"type":"Action.Execute","title":"Approve","verb":"lc.approve","data":{"runId":"run-1"}},{"type":"Action.Execute","title":"Send back","verb":"lc.reject","data":{"runId":"run-1"}}]}',
    );
    expect(JSON.stringify(renderCard({ kind: "result", message: "Approved." }))).toBe(
      '{"type":"AdaptiveCard","$schema":"http://adaptivecards.io/schemas/adaptive-card.json","version":"1.5","body":[{"type":"TextBlock","text":"Approved.","wrap":true,"isSubtle":true}]}',
    );
  });
});
