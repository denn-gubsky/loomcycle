// Runs only under test/integration/run.sh, which boots a real loomcycle on
// the mock provider and exports its URL and the bridge's tenant token. The
// bridge runs as its built CLI, with a fake Discord API on localhost.

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
const TOKEN = process.env.DISCORD_BRIDGE_IT_TOKEN;
const PKG = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");

const APP = "100000000000000001";
const GUILD = "200000000000000002";
const CHANNEL = "300000000000000003";
const DM = "400000000000000004";
const USER = "500000000000000005";
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

describe.skipIf(!BASE || !TOKEN)("discord bridge CLI against a real loomcycle", () => {
  const key = appKey();
  const seen: Seen[] = [];
  let fake: http.Server;
  let bridge: ChildProcess;
  let bridgeUrl = "";
  let output = "";
  let home = "";
  let seq = 0;

  beforeAll(async () => {
    fake = http.createServer((req, res) => {
      let raw = "";
      req.on("data", (c) => (raw += c));
      req.on("end", () => {
        seen.push({ method: req.method ?? "", url: req.url ?? "", authorized: req.headers.authorization === `Bot ${BOT_TOKEN}`, body: raw ? JSON.parse(raw) : undefined });
        res.writeHead(req.url?.endsWith("/typing") ? 204 : 200, { "content-type": "application/json" });
        res.end(req.url?.endsWith("/typing") ? "" : "{}");
      });
    });
    const fakePort = await new Promise<number>((r) => fake.listen(0, "127.0.0.1", () => r((fake.address() as net.AddressInfo).port)));
    const port = await freePort();
    bridgeUrl = `http://127.0.0.1:${port}`;
    home = mkdtempSync(path.join(os.tmpdir(), "discord-bridge-home-"));
    // Only what the bridge needs: nothing from the developer's environment.
    bridge = spawn(process.execPath, [path.join(PKG, "dist/cli.js")], {
      env: {
        PATH: process.env.PATH,
        HOME: home,
        PORT: String(port),
        LOOMCYCLE_BASE_URL: BASE,
        LOOMCYCLE_TOKEN: TOKEN,
        BRIDGE_AGENT: "helper",
        DISCORD_APPLICATION_ID: APP,
        DISCORD_PUBLIC_KEY: key.publicHex,
        DISCORD_BOT_TOKEN: BOT_TOKEN,
        DISCORD_BRIDGE_ALLOWED_GUILDS: GUILD,
        DISCORD_BRIDGE_ALLOW_DMS: "1",
        DISCORD_BRIDGE_REGISTER_COMMANDS: "1",
        DISCORD_BRIDGE_API_BASE_URL: `http://127.0.0.1:${fakePort}/api/v10`,
      },
      stdio: ["ignore", "pipe", "pipe"],
    });
    bridge.stdout!.on("data", (c) => (output += c));
    bridge.stderr!.on("data", (c) => (output += c));
    await until(() => output.includes("discord-bridge listening"), "the bridge to start");
  }, 30_000);

  afterAll(async () => {
    bridge?.kill("SIGTERM");
    await new Promise((r) => bridge?.once("exit", r));
    await new Promise((r) => fake?.close(r));
    if (home) rmSync(home, { recursive: true, force: true });
    console.log(`bridge output:\n${output}`);
  });

  async function post(ix: object, sign = true) {
    const body = JSON.stringify(ix);
    const headers = sign ? signedHeaders(key.privateKey, body, String(Math.floor(Date.now() / 1000))) : { "content-type": "application/json" };
    const res = await fetch(`${bridgeUrl}/discord/interactions`, { method: "POST", headers, body });
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

  it("runs /ask in a DM through loomcycle and edits the deferred response with the answer, twice", async () => {
    const first = command("hello from discord", "dm");
    expect(await post(first)).toMatchObject({ status: 200, json: { type: 5, data: { flags: 4, allowed_mentions: { parse: [] } } } });
    await until(() => edits(first.token).length === 1, "the first answer");
    const a1 = (edits(first.token)[0].body as { content: string }).content;
    console.log(`first answer: ${JSON.stringify(a1)}`);
    expect(a1.length).toBeGreaterThan(0);
    expect(edits(first.token)[0].authorized).toBe(false);

    const second = command("and again", "dm");
    expect(await post(second)).toMatchObject({ status: 200, json: { type: 5, data: { flags: 4, allowed_mentions: { parse: [] } } } });
    await until(() => edits(second.token).length === 1, "the second answer");
    console.log(`second answer: ${JSON.stringify((edits(second.token)[0].body as { content: string }).content)}`);
  }, 30_000);

  it("runs /ask in a server channel as the channel", async () => {
    const ix = command("hello channel", "guild");
    expect(await post(ix)).toMatchObject({ status: 200, json: { type: 5, data: { flags: 4, allowed_mentions: { parse: [] } } } });
    await until(() => edits(ix.token).length === 1, "the channel answer");
    console.log(`channel answer: ${JSON.stringify((edits(ix.token)[0].body as { content: string }).content)}`);
  }, 30_000);

  it("never logs the bot token, an interaction token or message text", () => {
    expect(output).not.toContain(BOT_TOKEN);
    expect(output).not.toContain(TOKEN!);
    expect(output).not.toContain("it-itok-");
    expect(output).not.toContain("hello from discord");
  });
});
