import { createHash } from "node:crypto";

import { ConfigError, gateSharedSubject, sharedSubject, VERB, type CanonicalCard } from "@loomcycle/bridge-core";
import { describe, expect, it } from "vitest";

import { parseCardAction, questionCard, renderCard } from "../src/cards.js";
import { loadConfig } from "../src/config.js";
import {
  conversationSubject,
  groupsOf,
  identify,
  NOT_AVAILABLE_TEXT,
  personSub,
  subjectFor,
  uninstalledSub,
} from "../src/identity.js";

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

describe("identity as a loomcycle gate", () => {
  const act = (type: string, conv = "19:chan@thread.tacv2;messageid=111") => ({
    from: { aadObjectId: U, name: "Al" },
    conversation: { id: conv, tenantId: T.toUpperCase(), conversationType: type },
  });

  it("asserts a person under their lower-cased Entra object id", () => {
    expect(personSub(U)).toBe("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa");
    expect(personSub(" not-a-guid ")).toBeUndefined();
  });

  it("names a person <gate>:<object id> and a shared conversation <gate>:conv:<hash>", () => {
    expect(identify(act("personal", "a:1"), new Set([T]), "teams")).toMatchObject({
      ok: true,
      scope: "personal",
      speaker: { subject: "teams:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", trust: "trusted" },
      runAs: { subject: "teams:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", name: "Al" },
    });
    const channel = identify(act("channel"), new Set([T]), "teams");
    expect(channel).toMatchObject({
      ok: true,
      scope: "channel",
      speaker: { subject: "teams:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" },
      runAs: { subject: gateSharedSubject("teams", T, "19:chan@thread.tacv2"), name: "Teams channel" },
    });
    // Another thread of the channel is the channel's one identity, as in token mode.
    expect(channel.ok && channel.runAs.subject).toBe(conversationSubject(T, "19:chan@thread.tacv2;messageid=222", "teams"));
  });

  it("without a gate, people and shared chats keep the subjects the bridge always gave them", () => {
    const own = identify(act("personal", "a:1"), new Set([T]));
    expect(own.ok && [own.speaker.subject, own.runAs.subject]).toEqual([subjectFor(U), subjectFor(U)]);
    const shared = identify(act("groupChat", "19:g@thread.v2"), new Set([T]));
    expect(shared.ok && shared.runAs.subject).toBe(sharedSubject("teams", T, "19:g@thread.v2"));
  });

  // loomcycle's identity_map.legacy_subject_prefix links a gate's first login
  // to the user named prefix + platform id with ':' read as '-'. With
  // "teams-", that must be exactly the user the token-mode bridge made, or a
  // switched bridge would give everyone a new, empty account.
  it("maps every gate subject onto its token-mode subject under legacy_subject_prefix teams-", () => {
    const legacy = (gateSubject: string) => `teams-${gateSubject.slice("teams:".length).replaceAll(":", "-")}`;
    const tenants = new Set([T]);
    for (const [type, conv] of [
      ["personal", "a:1"],
      ["groupChat", "19:g@thread.v2"],
      ["channel", "19:chan@thread.tacv2;messageid=111"],
    ] as const) {
      const asGate = identify(act(type, conv), tenants, "teams");
      const asToken = identify(act(type, conv), tenants);
      if (!asGate.ok || !asToken.ok) throw new Error(`${type} not identified`);
      expect(legacy(asGate.speaker.subject)).toBe(asToken.speaker.subject);
      expect(legacy(asGate.runAs.subject)).toBe(asToken.runAs.subject);
    }
  });

  it("puts every run identity in its tenant's group, from the stored reference only", () => {
    expect(groupsOf({ conversation: { tenantId: T.toUpperCase() } })).toEqual([`teams-tenant-${T}`]);
    // A channel's reference holds no team id, so it gets the tenant group only.
    expect(groupsOf({ conversation: { id: "19:chan@thread.tacv2", conversationType: "channel", tenantId: T } } as never)).toEqual([`teams-tenant-${T}`]);
    // No tenant: nothing is asserted, so loomcycle syncs no groups.
    expect(groupsOf({ conversation: {} })).toBeUndefined();
    expect(groupsOf({ conversation: { tenantId: " " } })).toBeUndefined();
  });

  it("takes an uninstall from a person's own chat in an allowed tenant as them leaving", () => {
    const tenants = new Set([T]);
    const remove = (type?: string, tenant = T, action = "remove") => ({
      action,
      from: { aadObjectId: U },
      conversation: { id: "a:1", tenantId: tenant, ...(type ? { conversationType: type } : {}) },
    });
    expect(uninstalledSub(remove("personal"), tenants)).toBe(personSub(U));
    expect(uninstalledSub(remove(), tenants)).toBe(personSub(U));
    expect(uninstalledSub(remove("personal", T, "add"), tenants)).toBeUndefined();
    expect(uninstalledSub(remove("groupChat"), tenants)).toBeUndefined();
    expect(uninstalledSub(remove("channel"), tenants)).toBeUndefined();
    expect(uninstalledSub(remove("personal", "22222222-2222-2222-2222-222222222222"), tenants)).toBeUndefined();
    expect(uninstalledSub({ ...remove("personal"), from: { aadObjectId: "bot" } }, tenants)).toBeUndefined();
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

  it("is a gate only when BRIDGE_GATE is set; otherwise it keeps its loomcycle token", () => {
    const cfg = loadConfig(base);
    expect(cfg.shared.gate).toBeUndefined();
    expect(cfg.shared.loomcycleToken).toBe("t");

    const { LOOMCYCLE_TOKEN: _t, ...noToken } = base;
    const gateEnv = { ...noToken, BRIDGE_GATE: "teams", BRIDGE_GATE_AUDIENCE: "https://lc.example", BRIDGE_GATE_KEY: "{}" };
    const gated = loadConfig(gateEnv);
    expect(gated.shared.gate).toMatchObject({ name: "teams", audience: "https://lc.example" });
    expect(gated.shared.loomcycleToken).toBe("");
    // A leftover operator token beside the gate key is refused.
    expect(() => loadConfig({ ...gateEnv, LOOMCYCLE_TOKEN: "t" })).toThrow(/LOOMCYCLE_TOKEN must not be set/);
    expect(() => loadConfig(noToken)).toThrow(/LOOMCYCLE_TOKEN/);
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
