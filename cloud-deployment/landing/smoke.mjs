// Smoke test for the landing server: spins up a stub loomcycle upstream, spawns
// server.js in dev-unverified mode, and asserts the proxy + mint + whoami + the
// /v1/_ admin-plane block + static serving all behave.
//
//   node smoke.mjs        (run from cloud-deployment/landing/)
import { spawn } from "node:child_process";
import http from "node:http";
import path from "node:path";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const WEB_ROOT = path.resolve(HERE, "../../cloud-web"); // serve the real landing static
let failures = 0;
const ok = (c, m) => { console.log((c ? "PASS " : "FAIL ") + m); if (!c) failures++; };

// 1) stub loomcycle upstream — records what /v1/_operatortokendef received.
let mintSaw = null;
const upstream = http.createServer((req, res) => {
  if (req.method === "GET" && req.url === "/healthz") {
    res.setHeader("content-type", "application/json");
    return res.end(JSON.stringify({ ok: true, version: "test" }));
  }
  if (req.method === "GET" && req.url === "/v1/config") {
    res.setHeader("content-type", "application/json");
    return res.end(JSON.stringify({ version: "test", features: { sqlmem: true } }));
  }
  if (req.method === "POST" && req.url === "/v1/_operatortokendef") {
    let b = ""; req.on("data", (d) => (b += d)); req.on("end", () => {
      const body = JSON.parse(b || "{}");
      mintSaw = { auth: req.headers.authorization || "", body };
      res.setHeader("content-type", "application/json");
      res.end(JSON.stringify({
        name: body.name, tenant_id: body.tenant_id, subject: body.subject,
        allowed_scopes: body.scopes, token: "lc_TESTTOKEN0001", token_suffix: "0001",
      }));
    });
    return;
  }
  res.statusCode = 404; res.end("nope");
});
await new Promise((r) => upstream.listen(0, r));
const upPort = upstream.address().port;

// 2) spawn the landing server (dev-unverified so no CF Access is needed).
const child = spawn(process.execPath, ["server.js"], {
  cwd: HERE,
  env: {
    ...process.env, PORT: "8099", LOOMCYCLE_URL: `http://127.0.0.1:${upPort}`,
    LOOMCYCLE_ADMIN_TOKEN: "ADMINSECRET", MINT_DEV_ALLOW_UNVERIFIED: "1", WEB_ROOT,
  },
  stdio: "inherit",
});
const base = "http://127.0.0.1:8099";
async function waitUp() {
  for (let i = 0; i < 50; i++) {
    try { const r = await fetch(base + "/healthz"); if (r.ok) return; } catch {}
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error("landing did not come up");
}

try {
  await waitUp();

  // proxy: /healthz + /v1/config
  const h = await (await fetch(base + "/healthz")).json();
  ok(h.ok === true && h.version === "test", "/healthz proxied to runtime");
  const c = await (await fetch(base + "/v1/config")).json();
  ok(c.version === "test", "/v1/config proxied to runtime");

  // admin plane blocked on the apex
  const admin = await fetch(base + "/v1/_operatortokendef", { method: "POST", headers: { "content-type": "application/json" }, body: "{}" });
  ok(admin.status === 404, "/v1/_operatortokendef refused on the apex (404)");

  // whoami (dev identity)
  const who = await (await fetch(base + "/api/whoami")).json();
  ok(who.authenticated === true && who.email === "dev@example.com" && who.tenant.startsWith("t_"), "/api/whoami returns derived identity");

  // mint → upstream with admin bearer + forced op/scopes; tenant/subject from identity
  const m = await (await fetch(base + "/api/mint", { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ name: "laptop" }) })).json();
  ok(m.token === "lc_TESTTOKEN0001", "/api/mint returns the once-shown token");
  ok(mintSaw && mintSaw.auth === "Bearer ADMINSECRET", "mint used the server-side admin bearer");
  ok(mintSaw && mintSaw.body.op === "create", "mint forced op=create");
  ok(mintSaw && JSON.stringify(mintSaw.body.scopes) === JSON.stringify(["substrate:tenant", "runs:create"]), "mint forced safe scopes");
  ok(mintSaw && mintSaw.body.tenant_id === who.tenant && mintSaw.body.subject === who.email, "mint tenant/subject derived from identity (client cannot choose)");

  // client-chosen scopes/tenant are IGNORED (attack: try to mint admin)
  mintSaw = null;
  await fetch(base + "/api/mint", { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ name: "x", op: "rotate", scopes: ["substrate:admin"], tenant_id: "t_evil" }) });
  ok(mintSaw && mintSaw.body.op === "create" && !mintSaw.body.scopes.includes("substrate:admin") && mintSaw.body.tenant_id !== "t_evil", "client cannot smuggle op/scopes/tenant into the mint");

  // static site
  const idx = await fetch(base + "/");
  const html = await idx.text();
  ok(idx.ok && html.includes("<html"), "static landing served at /");
} catch (e) {
  console.error("smoke error:", e); failures++;
} finally {
  child.kill("SIGKILL"); upstream.close();
}
console.log(failures ? `\n${failures} FAILURE(S)` : "\nALL SMOKE CHECKS PASSED");
process.exit(failures ? 1 : 0);
