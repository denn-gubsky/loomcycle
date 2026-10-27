// Run by run.sh: the built bridge CLI against a real loomcycle, with a fake
// Bot API on loopback. Posts webhook updates as Telegram would and checks
// what the bridge sends back. Exits non-zero on the first failed check.

import { spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import http from "node:http";
import { fileURLToPath } from "node:url";

const LOOMCYCLE = process.env.TELEGRAM_BRIDGE_IT_BASE_URL;
const ADMIN = process.env.TELEGRAM_BRIDGE_IT_ADMIN;
const GATE_KEY = process.env.TELEGRAM_BRIDGE_IT_GATE_KEY;
if (!LOOMCYCLE || !ADMIN || !GATE_KEY) throw new Error("run me through run.sh");

const BOT_TOKEN = `4242:${randomBytes(12).toString("hex")}`;
const SECRET = `it_${randomBytes(12).toString("hex")}`;
const BRIDGE_PORT = 18972;
const API_PORT = 18973;
const BOT = { id: 4242, is_bot: true, first_name: "Loom", username: "LoomItBot" };

// The fake Bot API: records every call, answers ok.
const calls = [];
let messageId = 1;
const api = http.createServer((req, res) => {
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", () => {
    const parts = req.url.split("/");
    const method = parts.pop();
    const tokenOk = parts.pop() === `bot${BOT_TOKEN}`;
    calls.push({ method, tokenOk, params: body ? JSON.parse(body) : {} });
    const result = method === "getMe" ? BOT : method === "sendMessage" ? { message_id: messageId++ } : true;
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ ok: true, result }));
  });
});
await new Promise((r) => api.listen(API_PORT, "127.0.0.1", r));

const bridgeLog = [];
const bridge = spawn(process.execPath, [fileURLToPath(new URL("../../dist/cli.js", import.meta.url))], {
  // A clean environment: only what the bridge is meant to read.
  env: {
    PATH: process.env.PATH,
    HOME: process.env.TELEGRAM_BRIDGE_IT_HOME,
    LOOMCYCLE_BASE_URL: LOOMCYCLE,
    // A gate bridge: no loomcycle token, only its own signing key.
    BRIDGE_GATE: "telegram",
    BRIDGE_GATE_AUDIENCE: LOOMCYCLE,
    BRIDGE_GATE_KEY: GATE_KEY,
    BRIDGE_AGENT: "helper",
    PORT: String(BRIDGE_PORT),
    TELEGRAM_BOT_TOKEN: BOT_TOKEN,
    TELEGRAM_BRIDGE_WEBHOOK_SECRET: SECRET,
    TELEGRAM_BRIDGE_ALLOWED_USERS: "42",
    TELEGRAM_BRIDGE_ALLOWED_CHATS: "-100777",
    TELEGRAM_BRIDGE_API_BASE_URL: `http://127.0.0.1:${API_PORT}`,
    TELEGRAM_BRIDGE_STREAMING: "1",
  },
  stdio: ["ignore", "pipe", "pipe"],
});
bridge.stdout.on("data", (d) => bridgeLog.push(String(d)));
bridge.stderr.on("data", (d) => bridgeLog.push(String(d)));

let failed = false;
function check(ok, what) {
  console.log(`${ok ? "ok  " : "FAIL"} ${what}`);
  if (!ok) failed = true;
}

async function until(cond, what, ms = 20_000) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    if (cond()) return true;
    await new Promise((r) => setTimeout(r, 50));
  }
  check(false, `timed out waiting for ${what}`);
  return false;
}

let update = 1;
const post = (body, secret = SECRET) =>
  fetch(`http://127.0.0.1:${BRIDGE_PORT}/telegram/webhook`, {
    method: "POST",
    headers: { "content-type": "application/json", ...(secret ? { "x-telegram-bot-api-secret-token": secret } : {}) },
    body: JSON.stringify({ update_id: update++, ...body }),
  });
const msg = (text, chat, from, extra = {}) => ({ message: { message_id: update, date: 1, chat, from, text, ...extra } });
const alice = { id: 42, is_bot: false, first_name: "Alice" };
const sent = (chatId) => calls.filter((c) => (c.method === "sendMessage" || c.method === "editMessageText") && c.params.chat_id === chatId);

try {
  const healthy = await until(
    () => bridgeLog.join("").includes("telegram-bridge listening"),
    "the bridge to start",
  );
  if (!healthy) throw new Error("bridge did not start");
  check(calls[0]?.method === "getMe" && calls[0].tokenOk, "getMe at start, token in the path");

  const bad = await post(msg("hi", { id: 42, type: "private" }, alice), "wrong");
  check(bad.status === 401, `wrong secret refused (${bad.status})`);

  const t0 = Date.now();
  const ok = await post(msg("Hello from Telegram", { id: 42, type: "private" }, alice));
  check(ok.status === 200 && Date.now() - t0 < 1000, `private message acked in ${Date.now() - t0} ms`);
  await until(() => sent(42).length > 0, "a reply in the private chat");
  // Let the stream finish its last edit.
  await new Promise((r) => setTimeout(r, 1500));
  const reply = sent(42).at(-1);
  console.log("     private reply:", JSON.stringify({ method: reply?.method, text: reply?.params.text, parse_mode: reply?.params.parse_mode }));
  check(!!reply?.params.text, "the agent's answer reached the chat");
  check(sent(42).every((c) => c.params.link_preview_options?.is_disabled === true), "link previews disabled on every message and edit");

  const groupChat = { id: -100777, type: "supergroup", title: "IT" };
  const g = await post(msg("@LoomItBot hello group", groupChat, alice, { entities: [{ type: "mention", offset: 0, length: 10 }] }));
  check(g.status === 200, "group mention acked");
  await until(() => sent(-100777).length > 0, "a reply in the group");
  console.log("     group reply:", JSON.stringify(sent(-100777).at(-1)?.params.text));

  await post(msg("chatter not for the bot", groupChat, { id: 43, first_name: "Bob" }));
  await post(msg("I am a bot", { id: 42, type: "private" }, { id: 77, is_bot: true, first_name: "Other" }));
  await post(msg("let me in", { id: 66, type: "private" }, { id: 66, first_name: "Stranger" }));
  await until(() => sent(66).length === 1, "the refusal notice");
  console.log("     refusal:", JSON.stringify(sent(66)[0]?.params.text));
  await new Promise((r) => setTimeout(r, 1000));
  check(sent(-100777).length === 1, "group chatter and a bot's message got no answer");

  // loomcycle provisioned the person and the group under the gate's
  // subjects, created by the gate (RFC FA §3.5).
  const users = await (await fetch(`${LOOMCYCLE}/v1/_users?tenant=acme`, { headers: { authorization: `Bearer ${ADMIN}` } })).json();
  const subjects = (users.users ?? []).filter((u) => u.registered).map((u) => u.user_id);
  console.log("     users:", JSON.stringify(subjects));
  check(subjects.includes("telegram:42"), "the private chat's person is the gate-provisioned user telegram:42");
  check(subjects.some((s) => s.startsWith("telegram:conv:")), "the group runs as its own gate-provisioned user");

  const logs = bridgeLog.join("");
  check(!logs.includes(BOT_TOKEN.split(":")[1]) && !logs.includes(SECRET) && !logs.includes("Hello from Telegram") && !logs.includes(JSON.parse(GATE_KEY).d) && !logs.includes("lct_"),
    "no token, secret, gate key, issued token or message text in the bridge's logs");
  check(calls.every((c) => c.tokenOk), "every Bot API call carried the token in its path");
} finally {
  bridge.kill("SIGTERM");
  await new Promise((r) => bridge.once("exit", r));
  api.close();
  console.log("--- bridge log ---");
  console.log(bridgeLog.join("").trim());
}
process.exit(failed ? 1 : 0);
