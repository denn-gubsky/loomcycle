// The real Teams SDK app, end to end, without Teams: activities are POSTed to
// /api/messages in the SDK's local (unauthenticated) mode, and replies go to a
// fake Bot Connector that records them. Runs only under test/integration/run.sh.

import http from "node:http";
import type { AddressInfo } from "node:net";

import { afterAll, beforeAll, describe, expect, it } from "vitest";

import { createTeamsBridge, type TeamsBridge } from "../../src/bridge.js";
import { loadConfig } from "../../src/config.js";
import { MemoryStore } from "../../src/store.js";
import { silentLogger } from "../fakes.js";

const BASE = process.env.TEAMS_BRIDGE_IT_BASE_URL;
const TOKEN = process.env.TEAMS_BRIDGE_IT_TOKEN;
const TENANT = "11111111-1111-1111-1111-111111111111";
const USER = "dddddddd-dddd-dddd-dddd-dddddddddddd";
const PORT = 18971;

interface Received {
  url: string;
  body: Record<string, unknown>;
}

describe.skipIf(!BASE || !TOKEN)("teams bridge through the Teams SDK", () => {
  const received: Received[] = [];
  let connector: http.Server;
  let connectorUrl: string;
  let bridge: TeamsBridge;
  let seq = 0;

  beforeAll(async () => {
    connector = http.createServer((req, res) => {
      let data = "";
      req.on("data", (c) => (data += c));
      req.on("end", () => {
        received.push({ url: req.url ?? "", body: data ? JSON.parse(data) : {} });
        res.writeHead(201, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ id: `act-${++seq}` }));
      });
    });
    await new Promise<void>((r) => connector.listen(0, "127.0.0.1", r));
    connectorUrl = `http://127.0.0.1:${(connector.address() as AddressInfo).port}/`;

    const cfg = loadConfig({
      LOOMCYCLE_BASE_URL: BASE,
      LOOMCYCLE_TOKEN: TOKEN,
      TEAMS_BRIDGE_AGENT: "helper",
      TEAMS_BRIDGE_ALLOWED_TENANTS: TENANT,
      TEAMS_BRIDGE_DEV: "1",
      PORT: String(PORT),
    });
    bridge = createTeamsBridge(cfg, { store: new MemoryStore(), logger: silentLogger });
    await bridge.start();
  });

  afterAll(async () => {
    await bridge?.stop();
    await new Promise((r) => connector?.close(r));
  });

  const post = (activity: Record<string, unknown>) =>
    fetch(`http://127.0.0.1:${PORT}/api/messages`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        channelId: "msteams",
        serviceUrl: connectorUrl,
        timestamp: new Date().toISOString(),
        from: { id: "29:user", aadObjectId: USER, name: "Dana" },
        recipient: { id: "28:bot", name: "Bot" },
        conversation: { id: "a:sdk-conv", conversationType: "personal", tenantId: TENANT },
        channelData: { tenant: { id: TENANT } },
        ...activity,
      }),
    });

  it("answers a message by streaming to the conversation, after the request has returned", async () => {
    const res = await post({ type: "message", id: "m1", text: "Hello bot" });
    expect(res.status).toBe(200);

    const end = Date.now() + 20_000;
    const final = () =>
      received.find(
        (r) => r.body.type === "message" && Array.isArray(r.body.entities) && (r.body.entities as Array<Record<string, unknown>>).some((e) => e.streamType === "final"),
      );
    while (!final() && Date.now() < end) await new Promise((r) => setTimeout(r, 100));

    const f = final();
    expect(f, "a final streamed message").toBeTruthy();
    expect(decodeURIComponent(f!.url)).toContain("/v3/conversations/a:sdk-conv/activities");
    expect(String(f!.body.text).length).toBeGreaterThan(0);
    // The stream opened with typing activities carrying streaminfo.
    expect(received.some((r) => r.body.type === "typing" && JSON.stringify(r.body.entities ?? []).includes("streamSequence"))).toBe(true);
  }, 30_000);

  it("answers a stale card button with a replacement card", async () => {
    const res = await post({
      type: "invoke",
      id: "i1",
      name: "adaptiveCard/action",
      value: { action: { type: "Action.Execute", verb: "lc.approve", data: { runId: "no-such-run" } } },
    });
    expect(res.status).toBe(200);
    const body = (await res.json()) as { statusCode: number; type: string; value: { body: Array<{ text: string }> } };
    expect(body.statusCode).toBe(200);
    expect(body.type).toBe("application/vnd.microsoft.card.adaptive");
    expect(body.value.body[0].text).toBe("This is no longer open.");
  });
});
