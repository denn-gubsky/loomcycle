// Runs only under test/integration/run.sh, which boots a real loomcycle on
// the mock provider and exports its URL, the admin token, the bridge's tenant
// token and a fresh gate key. The bridge runs as its built CLI, with a fake
// Discord API on localhost.
//
// Two phases, as a live deployment moves: the bridge first runs with a
// loomcycle token and makes its users ("discord-<id>", a channel's
// "discord-conv-<hash>"), then restarts as the loomcycle gate, whose
// legacy_subject_prefix links each first login to the user they already had.

import { spawn, type ChildProcess } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import http from "node:http";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { afterAll, beforeAll, describe, expect, it } from "vitest";

import { appKey, signedHeaders } from "../discord.js";

const BASE = process.env.DISCORD_BRIDGE_IT_BASE_URL;
const ADMIN = process.env.DISCORD_BRIDGE_IT_ADMIN;
const TOKEN = process.env.DISCORD_BRIDGE_IT_TOKEN;
const GATE_KEY = process.env.DISCORD_BRIDGE_IT_GATE_KEY;
const PKG = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");

const APP = "100000000000000001";
const GUILD = "200000000000000002";
const CHANNEL = "300000000000000003";
const DM = "400000000000000004";
const USER = "500000000000000005";
const ROLE = "600000000000000006";
const BOT_TOKEN = `it-bot-${Math.random().toString(36).slice(2)}`;

interface Seen {
  method: string;
  url: string;
  authorized: boolean;
  body: unknown;
}

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const s = net.createServer();
    s.once("error", reject);
    s.listen(0, "127.0.0.1", () => {
      const port = (s.address() as net.AddressInfo).port;
      s.close(() => resolve(port));
    });
  });
}

async function until(cond: () => boolean, what: string, ms = 20_000): Promise<void> {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    if (cond()) return;
    await new Promise((r) => setTimeout(r, 50));
  }
  throw new Error(`timed out waiting for ${what}`);
}

describe.skipIf(!BASE || !ADMIN || !TOKEN || !GATE_KEY)("discord bridge CLI against a real loomcycle", () => {
  const key = appKey();
  const seen: Seen[] = [];
  let fake: http.Server;
  let fakePort = 0;
  let bridge: ChildProcess | undefined;
  let bridgeUrl = "";
  let port = 0;
  let output = "";
  let home = "";
  let seq = 0;
  let legacyChannel = "";

  /** startBridge runs the built CLI with only what the bridge needs: nothing
   *  from the developer's environment. auth is its loomcycle credential. */
  async function startBridge(auth: Record<string, string>): Promise<void> {
    output = "";
    bridge = spawn(process.execPath, [path.join(PKG, "dist/cli.js")], {
      env: {
        PATH: process.env.PATH,
        HOME: home,
        PORT: String(port),
        LOOMCYCLE_BASE_URL: BASE,
        ...auth,
        BRIDGE_AGENT: "helper",
        DISCORD_APPLICATION_ID: APP,
        DISCORD_PUBLIC_KEY: key.publicHex,
        DISCORD_BOT_TOKEN: BOT_TOKEN,
        DISCORD_BRIDGE_ALLOWED_GUILDS: GUILD,
        DISCORD_BRIDGE_TRUSTED_GUILDS: GUILD,
        DISCORD_BRIDGE_ALLOW_DMS: "1",
        DISCORD_BRIDGE_REGISTER_COMMANDS: "1",
        DISCORD_BRIDGE_API_BASE_URL: `http://127.0.0.1:${fakePort}/api/v10`,
      },
      stdio: ["ignore", "pipe", "pipe"],
    });
    bridge.stdout!.on("data", (c) => (output += c));
    bridge.stderr!.on("data", (c) => (output += c));
    await until(() => output.includes("discord-bridge listening"), "the bridge to start");
  }

  async function stopBridge(): Promise<void> {
    if (!bridge || bridge.exitCode !== null) return;
    const b = bridge;
    b.kill("SIGTERM");
    await new Promise((r) => b.once("exit", r));
  }

  async function users(): Promise<Array<{ user_id: string; registered: boolean; status: string }>> {
    const res = await fetch(`${BASE}/v1/_users?tenant=acme`, { headers: { authorization: `Bearer ${ADMIN}` } });
    const body = (await res.json()) as { users?: Array<{ user_id: string; registered: boolean; status: string }> };
    return (body.users ?? []).filter((u) => u.registered);
  }

  beforeAll(async () => {
    fake = http.createServer((req, res) => {
      let raw = "";
      req.on("data", (c) => (raw += c));
      req.on("end", () => {
        seen.push({ method: req.method ?? "", url: req.url ?? "", authorized: req.headers.authorization === `Bot ${BOT_TOKEN}`, body: raw ? JSON.parse(raw) : undefined });
        // The person is a member of the trusted server, with one role.
        const member = req.method === "GET" && req.url === `/api/v10/guilds/${GUILD}/members/${USER}`;
        res.writeHead(req.url?.endsWith("/typing") ? 204 : 200, { "content-type": "application/json" });
        res.end(req.url?.endsWith("/typing") ? "" : member ? JSON.stringify({ user: { id: USER }, roles: [ROLE] }) : "{}");
      });
    });
    fakePort = await new Promise<number>((r) => fake.listen(0, "127.0.0.1", () => r((fake.address() as net.AddressInfo).port)));
    port = await freePort();
    bridgeUrl = `http://127.0.0.1:${port}`;
    home = mkdtempSync(path.join(os.tmpdir(), "discord-bridge-home-"));
    // Phase 1: the bridge as it ran before, with a loomcycle token.
    await startBridge({ LOOMCYCLE_TOKEN: TOKEN! });
  }, 30_000);

  afterAll(async () => {
    await stopBridge();
    await new Promise((r) => fake?.close(r));
    if (home) rmSync(home, { recursive: true, force: true });
    console.log(`bridge output:\n${output}`);
  });

  async function post(ix: object, sign = true, route = "/discord/interactions") {
    const body = JSON.stringify(ix);
    const headers = sign ? signedHeaders(key.privateKey, body, String(Math.floor(Date.now() / 1000))) : { "content-type": "application/json" };
    const res = await fetch(`${bridgeUrl}${route}`, { method: "POST", headers, body });
    const text = await res.text();
    return { status: res.status, json: text.startsWith("{") ? JSON.parse(text) : text };
  }

  function command(prompt: string, where: "dm" | "guild") {
    const id = `90000000000000${String(++seq).padStart(4, "0")}`;
    const place =
      where === "dm"
        ? { context: 1, channel: { id: DM, type: 1 }, user: { id: USER, username: "carol", global_name: "Carol" } }
        : { context: 0, guild_id: GUILD, channel: { id: CHANNEL, type: 0 }, member: { user: { id: USER, username: "carol", global_name: "Carol" } } };
    return { id, application_id: APP, type: 2, token: `it-itok-${id}-${Math.random().toString(36).slice(2)}`, ...place, data: { name: "ask", type: 1, options: [{ name: "prompt", type: 3, value: prompt }] } };
  }

  const edits = (token: string) => seen.filter((s) => s.method === "PATCH" && s.url === `/api/v10/webhooks/${APP}/${token}/messages/@original`);

  async function askAndAnswer(prompt: string, where: "dm" | "guild"): Promise<string> {
    const ix = command(prompt, where);
    expect(await post(ix)).toMatchObject({ status: 200, json: { type: 5, data: { flags: 4, allowed_mentions: { parse: [] } } } });
    await until(() => edits(ix.token).length === 1, `the answer to ${JSON.stringify(prompt)}`);
    expect(edits(ix.token)[0].authorized).toBe(false);
    return (edits(ix.token)[0].body as { content: string }).content;
  }

  describe("phase 1: with a loomcycle token", () => {
    it("registers its commands in the allowed server and the bot's DMs", () => {
      const puts = seen.filter((s) => s.method === "PUT");
      expect(puts.map((p) => [p.url, p.authorized])).toEqual([
        [`/api/v10/applications/${APP}/guilds/${GUILD}/commands`, true],
        [`/api/v10/applications/${APP}/commands`, true],
      ]);
    });

    it("answers PING and refuses an unsigned request", async () => {
      expect(await post({ type: 1 })).toEqual({ status: 200, json: { type: 1 } });
      expect((await post({ type: 1 }, false)).status).toBe(401);
    });

    it("serves no Webhook Events route", async () => {
      expect((await post({ version: 1, application_id: APP, type: 0 }, true, "/discord/events")).status).toBe(404);
    });

    it("runs /ask in a DM through loomcycle and edits the deferred response with the answer, twice", async () => {
      const a1 = await askAndAnswer("hello from discord", "dm");
      console.log(`first answer: ${JSON.stringify(a1)}`);
      expect(a1.length).toBeGreaterThan(0);
      console.log(`second answer: ${JSON.stringify(await askAndAnswer("and again", "dm"))}`);
    }, 30_000);

    it("runs /ask in a server channel as the channel", async () => {
      console.log(`channel answer: ${JSON.stringify(await askAndAnswer("hello channel", "guild"))}`);
    }, 30_000);

    it("made the person's and the channel's users", async () => {
      const subjects = (await users()).map((u) => u.user_id);
      console.log(`users before the gate: ${JSON.stringify(subjects)}`);
      legacyChannel = subjects.find((s) => s.startsWith("discord-conv-")) ?? "";
      expect(subjects).toContain(`discord-${USER}`);
      expect(legacyChannel).not.toBe("");
    });

    it("never logs the bot token, an interaction token or message text", () => {
      expect(output).not.toContain(BOT_TOKEN);
      expect(output).not.toContain(TOKEN!);
      expect(output).not.toContain("it-itok-");
      expect(output).not.toContain("hello from discord");
    });
  });

  describe("phase 2: the same bridge as the loomcycle gate", () => {
    beforeAll(async () => {
      await stopBridge();
      // No loomcycle token: only its own signing key.
      await startBridge({ BRIDGE_GATE: "discord", BRIDGE_GATE_AUDIENCE: BASE!, BRIDGE_GATE_KEY: GATE_KEY! });
    }, 30_000);

    it("answers in a DM and in a server channel", async () => {
      console.log(`gate DM answer: ${JSON.stringify(await askAndAnswer("hello through the gate", "dm"))}`);
      console.log(`gate channel answer: ${JSON.stringify(await askAndAnswer("hello channel through the gate", "guild"))}`);
      // The person's trusted-server membership was asked with the bot token.
      const lookups = seen.filter((s) => s.method === "GET" && s.url === `/api/v10/guilds/${GUILD}/members/${USER}`);
      expect(lookups.length).toBeGreaterThan(0);
      expect(lookups.every((l) => l.authorized)).toBe(true);
    }, 30_000);

    it("kept the person's and the channel's users, and made no discord:* user", async () => {
      const subjects = (await users()).map((u) => u.user_id);
      console.log(`users after the gate: ${JSON.stringify(subjects)}`);
      expect(subjects).toContain(`discord-${USER}`);
      expect(subjects).toContain(legacyChannel);
      expect(subjects.some((s) => s.startsWith("discord:"))).toBe(false);
    });

    it("revokes a person who removed the app: loomcycle disables their user", async () => {
      expect((await post({ version: 1, application_id: APP, type: 0 }, false, "/discord/events")).status).toBe(401);
      expect((await post({ version: 1, application_id: APP, type: 0 }, true, "/discord/events")).status).toBe(204);
      const event = { version: 1, application_id: APP, type: 1, event: { type: "APPLICATION_DEAUTHORIZED", timestamp: new Date().toISOString(), data: { user: { id: USER } } } };
      // 204 only once loomcycle took the revoke.
      expect((await post(event, true, "/discord/events")).status).toBe(204);
      const person = (await users()).find((u) => u.user_id === `discord-${USER}`);
      console.log(`after the revoke: ${JSON.stringify(person)}`);
      expect(person?.status).not.toBe("active");
      // The channel is not a person, and stays.
      expect((await users()).find((u) => u.user_id === legacyChannel)?.status).toBe("active");
    });

    it("never logs the gate key, an issued token or message text", () => {
      expect(output).not.toContain(BOT_TOKEN);
      expect(output).not.toContain(JSON.parse(GATE_KEY!).d);
      expect(output).not.toContain("lct_");
      expect(output).not.toContain("it-itok-");
      expect(output).not.toContain("hello through the gate");
    });
  });
});
