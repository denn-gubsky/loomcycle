// Smoke test for the landing server: spins up a stub loomcycle upstream, spawns
// server.js in dev-unverified mode, and asserts the proxy + mint + whoami + the
// /v1/_ admin-plane block + static serving all behave.
//
//   node smoke.mjs        (run from cloud-deployment/landing/)
//
// With a REAL loomcycle running a `kind: password` gate, it also drives the
// account flow end to end (signup → redeem → team invite → member redeem →
// reset → disable/enable → remove, plus the refusals). Point it there with:
//
//   SMOKE_RUNTIME_URL=http://127.0.0.1:8787   the runtime (its LOOMCYCLE_PUBLIC_URL)
//   SMOKE_MANAGE_TOKEN=…                      an auth:manage operator token
//   SMOKE_AUTH_GATE=cloud                     the gate's name (default cloud)
//   SMOKE_RETURN_TO=/ui/                      a return_to the gate accepts (default /ui/)
//
// Without the first two that section is skipped. Links, codes and the token
// are never printed.
import { spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import http from "node:http";
import path from "node:path";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const WEB_ROOT = path.resolve(HERE, "../../cloud-web"); // serve the real landing static
let failures = 0;
const ok = (c, m) => { console.log((c ? "PASS " : "FAIL ") + m); if (!c) failures++; };

// 1) stub loomcycle upstream — records what /v1/_operatortokendef received. It
// decodes + normalizes the path the way Go's net/http does, so a `..`/`%5f`
// bypass forwarded by the landing WOULD reach the admin plane here (→ a "leak"
// the regression catches).
let mintSaw = null;
const upstream = http.createServer((req, res) => {
  let p;
  try { p = path.posix.normalize(decodeURIComponent(new URL(req.url, "http://x").pathname)); }
  catch { p = req.url; }
  if (req.method === "GET" && p === "/healthz") {
    res.setHeader("content-type", "application/json");
    return res.end(JSON.stringify({ ok: true, version: "test" }));
  }
  if (req.method === "GET" && p === "/v1/config") {
    res.setHeader("content-type", "application/json");
    return res.end(JSON.stringify({ version: "test", features: { sqlmem: true } }));
  }
  if (p === "/v1/_operatortokendef") {
    if (req.method === "POST") {
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
    // reaching the admin plane by any other method means the landing let a bypass
    // through — a detectable leak.
    res.statusCode = 200; res.setHeader("content-type", "application/json");
    return res.end(JSON.stringify({ leaked: true }));
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

  // the /v1 proxy is an ALLOWLIST (not a bypassable /v1/_ denylist): raw requests
  // that skip fetch's URL normalization — traversal + percent-encoding — must NOT
  // reach the admin plane or any non-allowlisted endpoint.
  const rawGet = (pathRaw) => new Promise((resolve) => {
    const r = http.request({ host: "127.0.0.1", port: 8099, method: "GET", path: pathRaw }, (res) => { res.resume(); resolve(res.statusCode); });
    r.on("error", () => resolve(0)); r.end();
  });
  for (const p of ["/v1/config/../_operatortokendef", "/v1/config/..%2f_operatortokendef", "/v1/%5foperatortokendef", "/v1/runs"]) {
    const st = await rawGet(p);
    ok(st === 404, `apex refuses ${p} (allowlist, not a bypassable denylist)`);
  }

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

  // without LOOMCYCLE_AUTH_MANAGE_TOKEN the account flow is off
  ok(who.auth_gate === false, "/api/whoami reports auth_gate:false without the manage token");
  const off = await fetch(base + "/api/signup", { method: "POST", headers: { "content-type": "application/json" }, body: "{}" });
  ok(off.status === 503 && (await off.json()).error === "auth_gate_not_configured", "/api/signup answers 503 without the manage token");
} catch (e) {
  console.error("smoke error:", e); failures++;
} finally {
  child.kill("SIGKILL"); upstream.close();
}

// 3) the account flow against a real runtime (optional).
const RT = (process.env.SMOKE_RUNTIME_URL || "").replace(/\/+$/, "");
const MANAGE = process.env.SMOKE_MANAGE_TOKEN || "";
if (!RT || !MANAGE) {
  console.log("SKIP account flow against a real runtime (set SMOKE_RUNTIME_URL + SMOKE_MANAGE_TOKEN)");
} else {
  await liveAccountFlow().catch((e) => { console.error("live smoke error:", e && e.message); failures++; });
}

async function liveAccountFlow() {
  const ORIGIN = "http://smoke.test";
  const lport = 8098;
  const landing = spawn(process.execPath, ["server.js"], {
    cwd: HERE,
    env: {
      ...process.env, PORT: String(lport), LOOMCYCLE_URL: RT, MINT_DEV_ALLOW_UNVERIFIED: "1", WEB_ROOT,
      LOOMCYCLE_ADMIN_TOKEN: "unused-by-the-account-flow", LOOMCYCLE_AUTH_MANAGE_TOKEN: MANAGE,
      AUTH_GATE: process.env.SMOKE_AUTH_GATE || "cloud", APP_URL: RT,
      SIGNUP_RETURN_TO: process.env.SMOKE_RETURN_TO || "/ui/", APP_ORIGIN: ORIGIN,
    },
    stdio: "inherit",
  });
  const lb = `http://127.0.0.1:${lport}`;
  const as = (email) => async (method, p, body, origin = ORIGIN) => {
    const headers = { accept: "application/json", "Cf-Access-Authenticated-User-Email": email };
    if (method !== "GET") headers.origin = origin;
    if (body !== undefined) headers["content-type"] = "application/json";
    const r = await fetch(lb + p, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
    let json = null; try { json = await r.json(); } catch {}
    return { status: r.status, json };
  };
  // redeem plays the person on loomcycle's invite page: the code from the
  // link's fragment, a password, same-origin with the UI header.
  const redeem = async (url) => {
    const code = url.slice(url.indexOf("#") + 1);
    const r = await fetch(RT + "/v1/_auth/invite", {
      method: "POST",
      headers: { "content-type": "application/json", "X-Loomcycle-UI": "1", Origin: RT },
      body: JSON.stringify({ code, password: "pw-" + randomBytes(16).toString("hex") }),
    });
    return r.status;
  };
  try {
    for (let i = 0; i < 50; i++) {
      try { if ((await fetch(lb + "/_up")).ok) break; } catch {}
      await new Promise((r) => setTimeout(r, 100));
    }
    const run = Date.now().toString(36);
    const ownerEmail = `smoke-owner-${run}@example.com`;
    const owner = as(ownerEmail);

    let r = await fetch(lb + "/api/whoami", { headers: { accept: "application/json", "Cf-Access-Authenticated-User-Email": ownerEmail } }).then((x) => x.json());
    ok(r.auth_gate === true && r.signed_up === false, "live: whoami → auth_gate:true, signed_up:false for a fresh visitor");

    r = await owner("POST", "/api/signup", {});
    ok(r.status === 200 && r.json.redirect.startsWith(RT + "/ui/invite#"), "live: signup → redirect to the app's invite page");
    ok((await redeem(r.json.redirect)) === 201, "live: the owner invite redeems with a password");

    r = await fetch(lb + "/api/whoami", { headers: { accept: "application/json", "Cf-Access-Authenticated-User-Email": ownerEmail } }).then((x) => x.json());
    ok(r.signed_up === true, "live: whoami → signed_up:true after redemption");
    r = await owner("POST", "/api/signup", {});
    ok(r.status === 409 && r.json.signed_up === true, "live: a second signup → 409 signed_up");

    r = await owner("GET", "/api/team");
    const me = r.json.members.find((m) => m.you);
    ok(r.status === 200 && r.json.members.length === 1 && me && me.login === ownerEmail && me.access_mode === "tenant", "live: team lists the owner (you, tenant, login = e-mail)");

    const memberEmail = `smoke-member-${run}@example.com`;
    r = await owner("POST", "/api/team/invites", { email: memberEmail });
    ok(r.status === 201 && r.json.url.startsWith(RT + "/ui/invite#") && r.json.expires_at, "live: member invite → link + expiry");
    const memberLink = r.json.url;
    r = await owner("GET", "/api/team/invites");
    ok(r.json.invites.some((i) => i.email === memberEmail) && !JSON.stringify(r.json).includes("#"), "live: pending invites list it, with no link");
    ok((await redeem(memberLink)) === 201, "live: the member redeems the invite");

    r = await owner("GET", "/api/team");
    const member = r.json.members.find((m) => m.login === memberEmail);
    ok(member && member.access_mode === "isolated" && member.status === "active", "live: the member is in the team at the floor (isolated)");
    const enc = encodeURIComponent(member.subject);

    r = await owner("POST", `/api/team/${enc}/reset`, {});
    ok(r.status === 201 && r.json.url.startsWith(RT + "/ui/reset#"), "live: member reset → link");
    r = await owner("POST", `/api/team/${enc}/disable`, {});
    ok(r.status === 200 && r.json.status === "disabled", "live: disable the member");
    r = await owner("POST", `/api/team/${enc}/enable`, {});
    ok(r.status === 200 && r.json.status === "active", "live: re-enable the member");

    // refusals
    const intruder = as(`smoke-intruder-${run}@example.com`);
    r = await intruder("POST", `/api/team/${enc}/disable`, {});
    ok(r.status === 404, "live: another visitor's request on that member → 404");
    r = await owner("GET", "/api/team");
    ok(r.json.members.find((m) => m.subject === member.subject).status === "active", "live: … and the member is unchanged");
    r = await owner("POST", `/api/team/${enc}/disable`, {}, "https://evil.test");
    ok(r.status === 403, "live: a cross-origin POST → 403");
    r = await owner("POST", `/api/team/${encodeURIComponent(me.subject)}/disable`, {});
    ok(r.status === 403 && r.json.error === "target_beyond_floor", "live: the owner cannot disable themselves through the website (the floor)");

    r = await owner("POST", `/api/team/${encodeURIComponent(me.subject)}/reset`, {});
    ok(r.status === 201 || (r.status === 403 && r.json.error === "target_beyond_floor"),
      `live: reset my password → ${r.status === 201 ? "link (manager_may_recover_owners on)" : "refused (manager_may_recover_owners off)"}`);

    r = await owner("POST", "/api/team/invites", { email: `smoke-revoke-${run}@example.com` });
    const rid = r.json.invite_id;
    r = await owner("DELETE", `/api/team/invites/${rid}`);
    ok(r.status === 204, "live: revoke a pending invite");
    r = await owner("GET", "/api/team/invites");
    ok(!r.json.invites.some((i) => i.invite_id === rid), "live: … and it is no longer pending");

    r = await owner("DELETE", `/api/team/${enc}`);
    ok(r.status === 204, "live: remove the member");
    r = await owner("GET", "/api/team");
    ok(r.json.members.length === 1, "live: the team is the owner again");
  } finally {
    landing.kill("SIGKILL");
  }
}
console.log(failures ? `\n${failures} FAILURE(S)` : "\nALL SMOKE CHECKS PASSED");
process.exit(failures ? 1 : 0);
