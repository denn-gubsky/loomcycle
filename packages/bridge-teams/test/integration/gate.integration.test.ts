// A Teams bridge moving from a loomcycle token to being a loomcycle gate (RFC
// FA §3.7d), against a real loomcycle: the Teams SDK app in its local
// (unauthenticated) mode, activities POSTed to /api/messages, and a fake Bot
// Connector for replies. Runs only under test/integration/run.sh, which pins
// the gate's public key in loomcycle and hands the private key here.
//
// Two phases, as a live deployment moves: the bridge first runs with a
// loomcycle token and makes its users ("teams-<object id>",
// "teams-conv-<hash>"), then restarts as the gate, whose
// legacy_subject_prefix links each first login to the user it already had.

import http from "node:http";
import type { AddressInfo } from "node:net";

import { MemoryStore } from "@loomcycle/bridge-core";
import { afterAll, beforeAll, describe, expect, it } from "vitest";

import { createTeamsBridge, type ConversationRef, type TeamsBridge } from "../../src/bridge.js";
import { loadConfig } from "../../src/config.js";
import { conversationSubject, subjectFor } from "../../src/identity.js";
import { silentLogger } from "../fakes.js";

const BASE = process.env.TEAMS_BRIDGE_IT_BASE_URL;
const TOKEN = process.env.TEAMS_BRIDGE_IT_TOKEN;
const ADMIN = process.env.TEAMS_BRIDGE_IT_ADMIN;
const GATE_KEY = process.env.TEAMS_BRIDGE_IT_GATE_KEY;
const TENANT = "11111111-1111-1111-1111-111111111111";
const USER = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee";
const PERSONAL = "a:gate-it";
const GROUP = "19:gate-it@thread.v2";
const PORT = 18972;

interface Received {
  url: string;
  body: Record<string, unknown>;
}

interface UserRow {
  user_id: string;
  registered: boolean;
  status: string;
}

async function until(cond: () => boolean | Promise<boolean>, what: string, ms = 20_000): Promise<void> {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    if (await cond()) return;
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(`timed out waiting for ${what}`);
}

describe.skipIf(!BASE || !TOKEN || !ADMIN || !GATE_KEY)("teams bridge moving to a loomcycle gate", () => {
  const received: Received[] = [];
  let connector: http.Server;
  let connectorUrl: string;
  let bridge: TeamsBridge | undefined;
  let seq = 0;

  beforeAll(async () => {
    connector = http.createServer((req, res) => {
      let data = "";
      req.on("data", (c) => (data += c));
      req.on("end", () => {
        received.push({ url: decodeURIComponent(req.url ?? ""), body: data ? JSON.parse(data) : {} });
        res.writeHead(201, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ id: `act-${++seq}` }));
      });
    });
    await new Promise<void>((r) => connector.listen(0, "127.0.0.1", r));
    connectorUrl = `http://127.0.0.1:${(connector.address() as AddressInfo).port}/`;
  });

  afterAll(async () => {
    await bridge?.stop();
    await new Promise((r) => connector?.close(r));
  });

  async function startBridge(auth: Record<string, string>): Promise<void> {
    const cfg = loadConfig({
      LOOMCYCLE_BASE_URL: BASE,
      ...auth,
      BRIDGE_AGENT: "helper",
      TEAMS_BRIDGE_ALLOWED_TENANTS: TENANT,
      BRIDGE_DEV: "1",
      PORT: String(PORT),
    });
    // A fresh memory store, as a restarted bridge with BRIDGE_STORE=memory.
    bridge = createTeamsBridge(cfg, { store: new MemoryStore<ConversationRef>(), logger: silentLogger });
    await bridge.start();
  }

  async function stopBridge(): Promise<void> {
    await bridge?.stop();
    bridge = undefined;
  }

  const post = (activity: Record<string, unknown>) =>
    fetch(`http://127.0.0.1:${PORT}/api/messages`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        channelId: "msteams",
        serviceUrl: connectorUrl,
        timestamp: new Date().toISOString(),
        id: `in-${++seq}`,
        from: { id: "29:user", aadObjectId: USER, name: "Erin" },
        recipient: { id: "28:bot", name: "Bot" },
        channelData: { tenant: { id: TENANT } },
        ...activity,
      }),
    });

  /** say posts a message to a conversation and waits for the bot's reply there. */
  async function say(conversationId: string, conversationType: string, text: string): Promise<void> {
    const before = received.length;
    const res = await post({ type: "message", text, conversation: { id: conversationId, conversationType, tenantId: TENANT } });
    expect(res.status).toBe(200);
    await until(
      () => received.slice(before).some((r) => r.body.type === "message" && r.url.includes(`/v3/conversations/${conversationId}/activities`)),
      `a reply in ${conversationId}`,
    );
  }

  async function users(): Promise<UserRow[]> {
    const res = await fetch(`${BASE}/v1/_users?tenant=acme`, { headers: { Authorization: `Bearer ${ADMIN}` } });
    expect(res.status).toBe(200);
    const body = (await res.json()) as { users?: UserRow[] };
    return (body.users ?? []).filter((u) => u.registered);
  }

  it("keeps the person's and the group chat's users across the switch, and revokes on uninstall", async () => {
    const person = subjectFor(USER)!;
    const group = conversationSubject(TENANT, GROUP);

    // Phase 1: the bridge as it ran before, with a loomcycle token.
    await startBridge({ LOOMCYCLE_TOKEN: TOKEN! });
    await say(PERSONAL, "personal", "Hello before the gate");
    await say(GROUP, "groupChat", "Hello group before the gate");
    await stopBridge();
    const before = (await users()).map((u) => u.user_id);
    expect(before).toEqual(expect.arrayContaining([person, group]));

    // Phase 2: the same bridge as the loomcycle gate: no loomcycle token,
    // only its own signing key.
    await startBridge({ BRIDGE_GATE: "teams", BRIDGE_GATE_AUDIENCE: BASE!, BRIDGE_GATE_KEY: GATE_KEY! });
    await say(PERSONAL, "personal", "Hello through the gate");
    await say(GROUP, "groupChat", "Hello group through the gate");

    // The gate linked each first login to the user the bridge made before, so
    // the person and the group keep their history: no teams:* user appears.
    const after = await users();
    expect(after.map((u) => u.user_id)).toEqual(expect.arrayContaining([person, group]));
    expect(after.filter((u) => u.user_id.startsWith("teams:"))).toEqual([]);
    expect(after.find((u) => u.user_id === person)?.status).toBe("active");

    // Uninstalling the app from the 1:1 chat is the person leaving: loomcycle
    // disables the user the gate is linked to. The group chat is untouched.
    const res = await post({ type: "installationUpdate", action: "remove", conversation: { id: PERSONAL, conversationType: "personal", tenantId: TENANT } });
    expect(res.status).toBe(200);
    await until(async () => (await users()).find((u) => u.user_id === person)?.status === "disabled", "the person disabled");
    expect((await users()).find((u) => u.user_id === group)?.status).toBe("active");
  }, 90_000);
});
