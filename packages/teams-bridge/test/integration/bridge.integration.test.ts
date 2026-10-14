// Runs only under test/integration/run.sh, which boots a real loomcycle on
// the mock provider and exports its URL and the bridge's tenant token.

import { LoomcycleClient } from "@loomcycle/client";
import { describe, expect, it } from "vitest";

import type { ConversationRef } from "../../src/ports.js";
import { ConversationRouter } from "../../src/router.js";
import { MemoryStore } from "../../src/store.js";
import { TokenBroker } from "../../src/tokens.js";
import { FakeTeams, silentLogger } from "../fakes.js";

const BASE = process.env.TEAMS_BRIDGE_IT_BASE_URL;
const TOKEN = process.env.TEAMS_BRIDGE_IT_TOKEN;
const TENANT = "11111111-1111-1111-1111-111111111111";
const USER = "cccccccc-cccc-cccc-cccc-cccccccccccc";
const SUBJECT = `teams-${USER}`;

async function until(cond: () => boolean, what: string, ms = 20_000): Promise<void> {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    if (cond()) return;
    await new Promise((r) => setTimeout(r, 50));
  }
  throw new Error(`timed out waiting for ${what}`);
}

describe.skipIf(!BASE || !TOKEN)("teams bridge against a real loomcycle", () => {
  const clients = (token: string) => new LoomcycleClient({ baseUrl: BASE!, authToken: token });
  const store = new MemoryStore();
  const teams = new FakeTeams();
  const broker = new TokenBroker(clients(TOKEN!), {
    agent: "helper",
    ttlSeconds: 3600,
    ratePerMinute: 30,
    rateBurst: 30,
    accessMode: "isolated",
  });
  const makeRouter = () =>
    new ConversationRouter(
      { allowedTenants: new Set([TENANT]), idleMinutes: 60 },
      { agent: "helper", store, broker, clients, port: teams, log: silentLogger, stream: { stopPollMs: 50 } },
    );
  const activity = { from: { aadObjectId: USER, name: "Carol" }, conversation: { id: "it-conv", tenantId: TENANT, conversationType: "personal" } };
  const ref: ConversationRef = { conversation: { id: "it-conv", conversationType: "personal" }, serviceUrl: "https://smba.example/" };

  it("runs an interactive conversation as the Teams user, turn by turn and across a restart", async () => {
    let router = makeRouter();

    await router.onMessage(activity, ref, "Hello from Teams");
    await until(() => teams.streams.length === 1 && teams.streams[0].closed, "first turn");
    expect(teams.streams[0].text.length).toBeGreaterThan(0);
    const first = await store.get("it-conv");
    expect(first?.runId).toBeTruthy();
    expect(first?.sessionId).toBeTruthy();

    await router.onMessage(activity, ref, "And a follow-up");
    await until(() => teams.streams.length === 2 && teams.streams[1].closed, "second turn");
    expect((await store.get("it-conv"))?.runId).toBe(first?.runId);

    // A new process: same store, no live stream. The next message continues
    // the session in a new run and the old run is ended.
    await router.stop();
    router = makeRouter();
    await router.onMessage(activity, ref, "Still there?");
    await until(() => teams.streams.length === 3 && teams.streams[2].closed, "turn after restart");
    const after = await store.get("it-conv");
    expect(after?.sessionId).toBe(first?.sessionId);
    expect(after?.runId).not.toBe(first?.runId);

    const userClient = clients(await broker.tokenFor(SUBJECT));
    const old = await userClient.getRun(first!.runId!);
    expect(old.status).toBe("cancelled");
    await router.stop();
  }, 60_000);

  it("gives the user a token that can run only the configured agent, interactively", async () => {
    const userClient = clients(await broker.tokenFor(SUBJECT, "Carol"));
    const firstEvent = async (opts: Parameters<LoomcycleClient["runStreaming"]>[0]) => {
      for await (const ev of userClient.runStreaming(opts)) return ev;
      return undefined;
    };
    const segments = [{ role: "user" as const, content: [{ type: "trusted-text" as const, text: "hi" }] }];

    await expect(firstEvent({ agent: "other", segments, interactive: true })).rejects.toMatchObject({ status: 403 });
    await expect(firstEvent({ agent: "helper", segments, interactive: true, maxIterations: 50 })).rejects.toMatchObject({ status: 403 });

    // Off the run plane entirely.
    const res = await fetch(`${BASE}/v1/_library/agents`, { headers: { Authorization: `Bearer ${await broker.tokenFor(SUBJECT)}` } });
    expect(res.status).toBe(403);
  });
});
