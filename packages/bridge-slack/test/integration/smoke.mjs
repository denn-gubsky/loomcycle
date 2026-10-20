// Drives the built Slack bridge CLI against a real loomcycle (run.sh boots
// it) with signed Slack requests, while a fake Slack Web API records what the
// bridge sends back. Prints each step; exits non-zero on the first failure.

import { spawn } from "node:child_process";
import { createHmac, randomBytes } from "node:crypto";
import http from "node:http";
import path from "node:path";
import { fileURLToPath } from "node:url";

const PKG = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const LOOMCYCLE = process.env.SLACK_BRIDGE_IT_BASE_URL;
const LC_TOKEN = process.env.SLACK_BRIDGE_IT_TOKEN;
const SLACK_PORT = 18972;
const BRIDGE_PORT = 18973;
const BRIDGE = `http://127.0.0.1:${BRIDGE_PORT}`;
const SECRET = randomBytes(16).toString("hex");

const calls = [];
let seq = 0;
const slack = http.createServer((req, res) => {
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", () => {
    const method = req.url.replace(/^\/api\//, "");
    calls.push({ method, auth: req.headers.authorization?.startsWith("Bearer xoxb-") ? "bearer" : "none", body: JSON.parse(body || "{}") });
    const reply =
      method === "auth.test"
        ? { ok: true, user_id: "UBOT", team_id: "T1" }
        : method === "users.info"
          ? { ok: true, user: { profile: { display_name: "Ada" } } }
          : { ok: true, channel: "x", ts: `1700000000.${String(++seq).padStart(6, "0")}` };
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify(reply));
  });
});
await new Promise((r) => slack.listen(SLACK_PORT, "127.0.0.1", r));

const bridge = spawn(process.execPath, [path.join(PKG, "dist/cli.js")], {
  env: {
    PATH: process.env.PATH,
    LOOMCYCLE_BASE_URL: LOOMCYCLE,
    LOOMCYCLE_TOKEN: LC_TOKEN,
    BRIDGE_AGENT: "helper",
    SLACK_BOT_TOKEN: "xoxb-smoke-test",
    SLACK_SIGNING_SECRET: SECRET,
    SLACK_BRIDGE_ALLOWED_TEAMS: "T1",
    SLACK_BRIDGE_API_BASE_URL: `http://127.0.0.1:${SLACK_PORT}/api`,
    PORT: String(BRIDGE_PORT),
  },
  stdio: ["ignore", "pipe", "pipe"],
});
let bridgeLog = "";
bridge.stdout.on("data", (d) => (bridgeLog += d));
bridge.stderr.on("data", (d) => (bridgeLog += d));

let failed = false;
function check(what, cond, detail = "") {
  console.log(`${cond ? "ok  " : "FAIL"} ${what}${detail ? `: ${detail}` : ""}`);
  if (!cond) failed = true;
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function until(cond, ms = 15_000) {
  for (let t = 0; t < ms; t += 100) {
    if (cond()) return true;
    await sleep(100);
  }
  return false;
}

function post(route, body, { sign = true, headers = {} } = {}) {
  const ts = String(Math.floor(Date.now() / 1000));
  const sig = `v0=${createHmac("sha256", SECRET).update(`v0:${ts}:${body}`).digest("hex")}`;
  return fetch(`${BRIDGE}${route}`, {
    method: "POST",
    headers: { "x-slack-request-timestamp": ts, "x-slack-signature": sign ? sig : "v0=00", ...headers },
    body,
  });
}

const to = (m) => calls.filter((c) => c.method === m);
let ev = 0;
const event = (event, team = "T1") => JSON.stringify({ type: "event_callback", team_id: team, event_id: `EvSmoke${++ev}`, event });

try {
  check("bridge is up", await until(() => /slack-bridge listening/.test(bridgeLog)), bridgeLog.trim().split("\n")[0]);
  check("learned itself with auth.test", to("auth.test").length === 1);

  const challenge = JSON.stringify({ type: "url_verification", challenge: "chal-123" });
  check("unsigned url_verification refused", (await post("/slack/events", challenge, { sign: false })).status === 401);
  const v = await post("/slack/events", challenge);
  check("signed url_verification answered", v.status === 200 && (await v.text()) === "chal-123");

  // A DM: streamed into the thread of the message it answers.
  const started = Date.now();
  const dm = await post("/slack/events", event({ type: "message", channel_type: "im", channel: "D1", user: "UALICE", text: "hello there", ts: "1700000100.000100", team: "T1" }));
  check("DM acked", dm.status === 200, `${Date.now() - started} ms`);
  check("DM answer streamed and stopped", await until(() => to("chat.stopStream").length === 1));
  const s = to("chat.startStream")[0];
  const streamed = [...to("chat.startStream"), ...to("chat.appendStream")].map((c) => c.body.markdown_text).join("");
  check("stream threads under the DM", s?.body.channel === "D1" && s?.body.thread_ts === "1700000100.000100");
  console.log(`     streamed text: ${JSON.stringify(streamed)}`);
  check("streamed some text", streamed.length > 0);

  // The same event again, as Slack's retry: taken once.
  const runsBefore = to("chat.startStream").length;
  const again = await post(
    "/slack/events",
    event({ type: "message", channel_type: "im", channel: "D1", user: "UALICE", text: "second", ts: "1700000100.000200", team: "T1" }),
  );
  check("second DM acked", again.status === 200);
  await until(() => to("chat.stopStream").length === 2);
  const retryBody = JSON.stringify({
    type: "event_callback",
    team_id: "T1",
    event_id: `EvSmoke${ev}`,
    event: { type: "message", channel_type: "im", channel: "D1", user: "UALICE", text: "second", ts: "1700000100.000200", team: "T1" },
  });
  await post("/slack/events", retryBody, { headers: { "x-slack-retry-num": "1", "x-slack-retry-reason": "http_timeout" } });
  await sleep(1500);
  check("retry not answered twice", to("chat.startStream").length === runsBefore + 1, `${to("chat.startStream").length} streams`);

  // A channel mention: one message in the thread.
  await post("/slack/events", event({ type: "app_mention", channel: "C1", user: "UALICE", text: "<@UBOT> status?", ts: "1700000200.000100", team: "T1" }));
  check("channel answer posted", await until(() => to("chat.postMessage").some((c) => c.body.channel === "C1")));
  const ch = to("chat.postMessage").find((c) => c.body.channel === "C1");
  check("in the thread, unfurling off", ch?.body.thread_ts === "1700000200.000100" && ch?.body.unfurl_links === false && ch?.body.unfurl_media === false);
  console.log(`     channel reply: ${JSON.stringify(ch?.body.markdown_text)}`);

  // Bot messages and a foreign workspace.
  const before = calls.length;
  await post("/slack/events", event({ type: "message", channel_type: "im", channel: "D1", user: "UBOT", text: "echo", ts: "1700000300.000100" }));
  await post("/slack/events", event({ type: "message", channel_type: "im", channel: "D1", user: "UX", bot_id: "B2", text: "beep", ts: "1700000300.000200" }));
  await sleep(800);
  check("bot messages dropped", calls.length === before);
  await post("/slack/events", event({ type: "message", channel_type: "im", channel: "D9", user: "UEVE", text: "hi", ts: "1700000400.000100", team: "T2" }, "T2"));
  check("foreign workspace told once", await until(() => to("chat.postMessage").some((c) => c.body.channel === "D9")));

  check("bad signature refused", (await post("/slack/events", event({ type: "message" }), { sign: false })).status === 401);
  check("every call carried the bot token", calls.every((c) => c.auth === "bearer"));

  const health = await (await fetch(`${BRIDGE}/healthz`)).json();
  console.log(`     /healthz: ${JSON.stringify(health)}`);
  check("bridge log carries no token or text", !bridgeLog.includes("xoxb-smoke-test") && !bridgeLog.includes(LC_TOKEN) && !bridgeLog.includes("hello there"));
} finally {
  bridge.kill("SIGTERM");
  slack.close();
}
console.log(failed ? "SMOKE FAILED" : "SMOKE PASSED");
process.exit(failed ? 1 : 0);
