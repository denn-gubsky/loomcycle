// Unit suite for the landing's account flow (/api/signup, /api/team/*) against a
// FAKE loomcycle runtime started in this process. The fake keeps just enough of
// loomcycle's password-gate rules (bootstrap-once, the floor, opaque 404s) to
// exercise every route, and records every request it receives so the tests can
// assert what the landing sent upstream — and what it did not.
//
//   npm test        (node --test; no extra dependencies)
//
// Identity comes from MINT_DEV_ALLOW_UNVERIFIED=1's header path, ONLY here: each
// test signs in as its own e-mail, so each gets its own derived tenant.
import { test, before, after, describe } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createHash, randomBytes } from "node:crypto";
import http from "node:http";
import net from "node:net";
import path from "node:path";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const LANDING_DIR = path.resolve(HERE, "..");
const MANAGE = "manage-" + randomBytes(12).toString("hex");
const ADMIN = "admin-" + randomBytes(12).toString("hex");
const GATE = "cloud";
const ORIGIN = "https://landing.test";
const RETURN_TO = "https://landing.test/?welcome=1";

const tenantOf = (email) => "t_" + createHash("sha256").update(email.trim().toLowerCase()).digest("hex").slice(0, 12);
const rnd = (n = 10) => randomBytes(n).toString("hex");
const b32 = () => [...randomBytes(16)].map((b) => "abcdefghijklmnopqrstuvwxyz234567"[b % 32]).join("");

// ───────────────────────────── the fake runtime ─────────────────────────────
function fakeRuntime() {
  const f = {
    port: 0, recover: true, log: [], codes: [], tenants: new Map(), logins: new Map(),
    // inject: (req) => {status, body} | undefined — a one-off upstream answer.
    inject: null, linkOrigin: null, delayMs: 0,
  };
  const T = (id) => {
    if (!f.tenants.has(id)) f.tenants.set(id, { booted: false, users: new Map(), ids: new Map(), invites: new Map() });
    return f.tenants.get(id);
  };
  f.T = T;
  const pub = () => f.linkOrigin || `http://127.0.0.1:${f.port}`;
  const live = (inv) => !inv.redeemed && !inv.revoked && Date.parse(inv.expires_at) > Date.now();
  f.addUser = (tenant, subject, access_mode = "isolated", login = null) => {
    const t = T(tenant);
    t.booted = true;
    t.users.set(subject, { user_id: subject, display_name: "", access_mode, status: "active", registered: true });
    if (login) { t.ids.set(subject, { gate: GATE, login, has_password: true }); f.logins.set(login, { tenant, subject }); }
  };
  f.addInvite = (tenant, extra = {}) => {
    const id = "inv_" + b32();
    const code = "lci_" + rnd(16);
    T(tenant).invites.set(id, { invite_id: id, tenant_id: tenant, gate: GATE, groups: [], state: "live",
      created_at: new Date().toISOString(), expires_at: new Date(Date.now() + 7 * 864e5).toISOString(), code, ...extra });
    f.codes.push(code);
    return id;
  };
  // redeem plays loomcycle's POST /v1/_auth/invite: the invitee takes a login.
  f.redeem = (url, login) => {
    const code = url.slice(url.indexOf("#") + 1);
    for (const [tenant, t] of f.tenants) for (const inv of t.invites.values()) {
      if (inv.code !== code || !live(inv)) continue;
      const subject = inv.subject || "cloud:" + b32();
      const lg = (inv.login || login || inv.email).toLowerCase();
      if (!inv.subject) t.users.set(subject, { user_id: subject, display_name: "", access_mode: inv.access_mode || "isolated", status: "active", registered: true });
      t.ids.set(subject, { gate: GATE, login: lg, has_password: true });
      f.logins.set(lg, { tenant, subject });
      t.booted = true; inv.redeemed = true;
      return subject;
    }
    throw new Error("redeem: no live invite for that code");
  };
  const aboveFloor = (t, s) => t.users.get(s)?.access_mode === "tenant";
  const send = (res, status, body) => {
    res.statusCode = status;
    if (body === undefined) return res.end();
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify(body));
  };
  const err = (res, status, code) => send(res, status, { code, error: code + " (message with a secret-ish https://x/ui/invite#lci_LEAKME)" });

  f.server = http.createServer((req, res) => {
    // delayMs holds every answer back, so concurrent landing requests interleave
    // their awaits the way they would against a busy runtime.
    let raw = ""; req.on("data", (d) => (raw += d)); req.on("end", () => setTimeout(() => {
      const u = new URL(req.url, "http://x");
      const segs = u.pathname.split("/").slice(1).map(decodeURIComponent);
      let body = null; try { body = raw ? JSON.parse(raw) : null; } catch { body = "<invalid>"; }
      const rec = { method: req.method, rawPath: req.url, path: u.pathname, segs, query: Object.fromEntries(u.searchParams), body, auth: req.headers.authorization || "" };
      f.log.push(rec);
      if (u.pathname === "/v1/_operatortokendef" && req.method === "POST") {
        if (rec.auth !== `Bearer ${ADMIN}`) return err(res, 401, "unauthorized");
        return send(res, 200, { name: body.name, tenant_id: body.tenant_id, subject: body.subject, allowed_scopes: body.scopes, token: "lc_TEST0001", token_suffix: "0001" });
      }
      if (rec.auth !== `Bearer ${MANAGE}`) return err(res, 401, "unauthorized");
      if (f.inject) {
        const once = f.inject(rec);
        if (once) {
          f.inject = null;
          if (once.drop) return req.socket.destroy(); // the runtime vanishes mid-request
          return send(res, once.status, once.body);
        }
      }
      const tenant = rec.query.tenant ?? body?.tenant;
      if (!tenant) return err(res, 400, "tenant_required");
      const t = T(tenant);
      const route = `${req.method} ${segs.slice(0, 2).join("/")}${segs[2] !== undefined ? "/" + (segs[1] === "_authgate" ? segs[3] : "{s}") : ""}`;
      if (segs[1] === "_authgate" && segs[2] !== GATE) return err(res, 404, "gate_not_found");
      switch (route) {
        case "GET v1/_users":
          return send(res, 200, { users: [...t.users.values()] });
        case "PATCH v1/_users/{s}": case "DELETE v1/_users/{s}": {
          const s = segs[2];
          if (!t.users.has(s)) return err(res, 404, "user_not_found");
          if (aboveFloor(t, s)) return err(res, 403, "target_beyond_floor");
          if (req.method === "DELETE") { t.users.delete(s); t.ids.delete(s); return send(res, 204); }
          t.users.get(s).status = body.status;
          return send(res, 200, { tenant_id: tenant, subject: s, ...t.users.get(s) });
        }
        case "GET v1/_authgate/tenant-status":
          return send(res, 200, { tenant_id: tenant, has_users: t.users.size > 0, bootstrapped: t.booted,
            bootstrap_invite_live: [...t.invites.values()].some((i) => i.bootstrap && live(i)) });
        case "GET v1/_authgate/identities": {
          const s = rec.query.subject;
          if (!t.users.has(s)) return err(res, 404, "user_not_found");
          const id = t.ids.get(s);
          return send(res, 200, { tenant_id: tenant, subject: s, identities: id ? [id] : [] });
        }
        case "GET v1/_authgate/invites":
          return send(res, 200, { invites: [...t.invites.values()].map(({ code, ...i }) => ({ ...i, state: live(i) ? "live" : i.redeemed ? "redeemed" : "revoked" })) });
        case "DELETE v1/_authgate/invites": {
          const inv = t.invites.get(segs[4]);
          if (!inv) return err(res, 404, "invite_not_found");
          if (!live(inv)) return err(res, 409, "invite_not_live");
          inv.revoked = true;
          return send(res, 204);
        }
        case "POST v1/_authgate/invites": {
          const known = new Set(["tenant", "subject", "access_mode", "groups", "login", "email", "ttl", "return_to"]);
          if (Object.keys(body).some((k) => !known.has(k))) return err(res, 400, "invalid_body");
          if (body.login && f.logins.has(body.login)) return err(res, 409, "login_taken");
          const extra = { login: body.login || "", email: body.email || "", return_to: body.return_to || "" };
          if (body.subject) {
            if (!t.users.has(body.subject)) return err(res, 404, "user_not_found");
            if (t.ids.has(body.subject)) return err(res, 409, "credential_exists");
            if (aboveFloor(t, body.subject) && !f.recover) return err(res, 403, "target_beyond_floor");
            extra.subject = body.subject;
          } else if (body.access_mode === "tenant") {
            if (t.booted) return err(res, 403, "tenant_not_empty");
            if ([...t.invites.values()].some((i) => i.bootstrap && live(i))) return err(res, 409, "bootstrap_invite_live");
            extra.bootstrap = true; extra.access_mode = "tenant";
          } else if (body.access_mode || body.groups?.length) {
            return err(res, 403, "beyond_floor");
          }
          const id = f.addInvite(tenant, extra);
          const inv = t.invites.get(id);
          return send(res, 201, { invite_id: id, url: `${pub()}/ui/invite#${inv.code}`, expires_at: inv.expires_at });
        }
        case "POST v1/_authgate/resets": {
          if (!t.users.has(body.subject) || !t.ids.has(body.subject)) return err(res, 404, "login_not_found");
          if (aboveFloor(t, body.subject) && !f.recover) return err(res, 403, "target_beyond_floor");
          const code = "lcr_" + rnd(16); f.codes.push(code);
          return send(res, 201, { reset_id: "rst_" + b32(), url: `${pub()}/ui/reset#${code}`, expires_at: new Date(Date.now() + 36e5).toISOString() });
        }
      }
      return err(res, 404, "no_route");
    }, f.delayMs));
  });
  return f;
}

// ───────────────────────────── the landing under test ────────────────────────
async function freePort() {
  return new Promise((r) => { const s = net.createServer().listen(0, () => { const p = s.address().port; s.close(() => r(p)); }); });
}
async function startLanding(env) {
  const port = await freePort();
  const child = spawn(process.execPath, ["server.js"], {
    cwd: LANDING_DIR,
    env: { PATH: process.env.PATH, PORT: String(port), WEB_ROOT: path.resolve(LANDING_DIR, "../../cloud-web"), ...env },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let logs = "";
  child.stdout.on("data", (d) => (logs += d)); child.stderr.on("data", (d) => (logs += d));
  const base = `http://127.0.0.1:${port}`;
  for (let i = 0; i < 100; i++) {
    try { if ((await fetch(base + "/_up")).ok) return { base, child, logs: () => logs }; } catch {}
    await new Promise((r) => setTimeout(r, 50));
  }
  child.kill("SIGKILL");
  throw new Error("landing did not come up:\n" + logs);
}

const fake = fakeRuntime();
let on, off, strict;
let ipSeq = 0;
// call makes one request as `email` (a fresh source address each time unless
// given), same-origin and JSON by default.
async function call(inst, method, p, { email, body, origin = ORIGIN, ip, headers = {} } = {}) {
  const h = { accept: "application/json", ...headers };
  if (email) h["Cf-Access-Authenticated-User-Email"] = email;
  h["CF-Connecting-IP"] = ip || `198.51.100.${(ipSeq++ % 250) + 1}`;
  if (method !== "GET" && origin) h.origin = origin;
  if (body !== undefined) { h["content-type"] = "application/json"; }
  const r = await fetch(inst.base + p, { method, headers: h, body: body === undefined ? undefined : JSON.stringify(body) });
  const text = await r.text();
  let json = null; try { json = JSON.parse(text); } catch {}
  return { status: r.status, json, text, cc: r.headers.get("cache-control") };
}
const mark = () => fake.log.length;
const since = (m) => fake.log.slice(m);
const mutations = (m) => since(m).filter((r) => r.method !== "GET");
const newEmail = () => `owner-${rnd(4)}@example.com`;

before(async () => {
  await new Promise((r) => fake.server.listen(0, "127.0.0.1", r));
  fake.port = fake.server.address().port;
  const common = { LOOMCYCLE_URL: `http://127.0.0.1:${fake.port}`, LOOMCYCLE_ADMIN_TOKEN: ADMIN, MINT_DEV_ALLOW_UNVERIFIED: "1", APP_ORIGIN: ORIGIN };
  on = await startLanding({ ...common, LOOMCYCLE_AUTH_MANAGE_TOKEN: MANAGE, AUTH_GATE: GATE,
    APP_URL: `http://127.0.0.1:${fake.port}`, SIGNUP_RETURN_TO: RETURN_TO });
  off = await startLanding(common);
  // Real Access verification on, with a team that cannot be reached: no
  // request can produce an identity.
  strict = await startLanding({ ...common, MINT_DEV_ALLOW_UNVERIFIED: "", CF_ACCESS_TEAM_DOMAIN: "team.invalid", CF_ACCESS_AUD: "aud",
    LOOMCYCLE_AUTH_MANAGE_TOKEN: MANAGE, APP_URL: `http://127.0.0.1:${fake.port}` });
});
after(() => {
  for (const i of [on, off, strict]) i?.child.kill("SIGKILL");
  fake.server.close();
});

// signUp drives a fresh owner through signup + the invite page, returning
// {email, tenant, self}.
async function signUp(email = newEmail()) {
  const r = await call(on, "POST", "/api/signup", { email, body: {} });
  assert.equal(r.status, 200, r.text);
  const self = fake.redeem(r.json.redirect);
  return { email, tenant: tenantOf(email), self };
}

const ROUTES = [
  ["POST", "/api/signup"], ["GET", "/api/team"], ["GET", "/api/team/invites"], ["POST", "/api/team/invites"],
  ["DELETE", "/api/team/invites/inv_abc"], ["POST", "/api/team/cloud:abc/reset"], ["POST", "/api/team/cloud:abc/disable"],
  ["POST", "/api/team/cloud:abc/enable"], ["DELETE", "/api/team/cloud:abc"],
];

// ───────────────────────────── feature flag ──────────────────────────────────
describe("without LOOMCYCLE_AUTH_MANAGE_TOKEN", () => {
  test("every account route answers 503 auth_gate_not_configured, uncached", async () => {
    for (const [m, p] of ROUTES) {
      const r = await call(off, m, p, { email: newEmail(), body: m === "POST" ? {} : undefined });
      assert.equal(r.status, 503, `${m} ${p}`);
      assert.deepEqual(r.json, { error: "auth_gate_not_configured" });
      assert.equal(r.cc, "no-store");
    }
  });
  test("whoami reports auth_gate false and no signed_up", async () => {
    const r = await call(off, "GET", "/api/whoami", { email: "a@example.com" });
    assert.equal(r.json.auth_gate, false);
    assert.equal("signed_up" in r.json, false);
  });
  test("the mint flow works as before, with the admin token", async () => {
    const m = mark();
    const r = await call(off, "POST", "/api/mint", { email: "a@example.com", body: { name: "laptop" } });
    assert.equal(r.json.token, "lc_TEST0001");
    assert.equal(since(m)[0].auth, `Bearer ${ADMIN}`);
  });
});

// ───────────────────────────── whoami ────────────────────────────────────────
describe("whoami with the account flow on", () => {
  test("reports signed_up false for a fresh tenant and true once the invite is redeemed", async () => {
    const email = newEmail();
    let r = await call(on, "GET", "/api/whoami", { email });
    assert.equal(r.json.auth_gate, true);
    assert.equal(r.json.signed_up, false);
    assert.equal(r.json.app, `http://127.0.0.1:${fake.port}/ui/login`);
    await signUp(email);
    r = await call(on, "GET", "/api/whoami", { email });
    assert.equal(r.json.signed_up, true);
  });
  test("reports signed_up null when the runtime cannot say, and still answers 200", async () => {
    fake.inject = (rec) => rec.path.endsWith("/tenant-status") ? { status: 500, body: { code: "internal" } } : undefined;
    const r = await call(on, "GET", "/api/whoami", { email: newEmail() });
    assert.equal(r.status, 200);
    assert.equal(r.json.signed_up, null);
  });
});

// ───────────────────────────── signup ────────────────────────────────────────
describe("POST /api/signup", () => {
  test("bootstraps an empty tenant with an owner invite built only from the verified identity", async () => {
    const email = newEmail();
    const m = mark();
    // Every field a client could try to steer with is ignored.
    const r = await call(on, "POST", "/api/signup", { email, body: { tenant: "t_evil", subject: "root", access_mode: "tenant", groups: ["admins"], login: "x", return_to: "https://evil.test/" } });
    assert.equal(r.status, 200, r.text);
    assert.ok(r.json.redirect.startsWith(`http://127.0.0.1:${fake.port}/ui/invite#lci_`));
    const create = since(m).find((x) => x.method === "POST");
    assert.deepEqual(create.body, { tenant: tenantOf(email), access_mode: "tenant", login: email, return_to: RETURN_TO });
    assert.equal(create.path, `/v1/_authgate/${GATE}/invites`);
    for (const x of since(m)) assert.equal(x.auth, `Bearer ${MANAGE}`, "only the manage token goes upstream");
    for (const x of since(m)) assert.notEqual(x.query.tenant ?? x.body?.tenant, "t_evil");
  });
  test("replaces a stale live owner invite with a fresh one", async () => {
    const email = newEmail();
    const first = await call(on, "POST", "/api/signup", { email, body: {} });
    const second = await call(on, "POST", "/api/signup", { email, body: {} });
    assert.equal(second.status, 200, second.text);
    assert.notEqual(second.json.redirect, first.json.redirect);
    const invites = [...fake.T(tenantOf(email)).invites.values()];
    assert.equal(invites.filter((i) => i.revoked).length, 1);
    assert.throws(() => fake.redeem(first.json.redirect), /no live invite/);
    fake.redeem(second.json.redirect);
  });
  test("sends a signed-up owner to the app with 409", async () => {
    const { email } = await signUp();
    const r = await call(on, "POST", "/api/signup", { email, body: {} });
    assert.equal(r.status, 409);
    assert.equal(r.json.signed_up, true);
    assert.equal(r.json.app, `http://127.0.0.1:${fake.port}/ui/login`);
  });
  test("attaches a password to a legacy owner's e-mail subject, revoking its stale attach invite", async () => {
    const email = newEmail(), tenant = tenantOf(email);
    fake.addUser(tenant, email, "tenant");
    const stale = fake.addInvite(tenant, { subject: email });
    const m = mark();
    const r = await call(on, "POST", "/api/signup", { email, body: {} });
    assert.equal(r.status, 200, r.text);
    const create = since(m).find((x) => x.method === "POST");
    assert.deepEqual(create.body, { tenant, subject: email, login: email, return_to: RETURN_TO });
    assert.equal(fake.T(tenant).invites.get(stale).revoked, true);
    assert.equal(fake.redeem(r.json.redirect), email, "the owner keeps the legacy subject");
  });
  test("refuses with 409 when the tenant has users but none is this visitor's account", async () => {
    const email = newEmail();
    fake.addUser(tenantOf(email), "cloud:" + b32(), "isolated", "member-" + rnd(3) + "@example.com");
    const m = mark();
    const r = await call(on, "POST", "/api/signup", { email, body: {} });
    assert.equal(r.status, 409);
    assert.equal(mutations(m).length, 0);
  });
  test("an e-mail already a login elsewhere gets a generic refusal that spends the slot, never login_taken", async () => {
    const email = newEmail(), ip = "203.0.113.30";
    fake.logins.set(email, { tenant: "t_other", subject: "cloud:x" });
    for (let i = 0; i < 10; i++) {
      const r = await call(on, "POST", "/api/signup", { email, ip, body: {} });
      assert.equal(r.status, 409);
      assert.equal(r.json.error, "cannot_sign_up");
      assert.equal(typeof r.json.message, "string");
      assert.doesNotMatch(JSON.stringify(r.json), /login_taken/);
    }
    assert.equal((await call(on, "POST", "/api/signup", { email, ip, body: {} })).status, 429);
  });
  test("a signup that revoked a stale invite keeps its slot spent even when the create then fails", async () => {
    const email = newEmail(), ip = "203.0.113.31";
    assert.equal((await call(on, "POST", "/api/signup", { email, ip, body: {} })).status, 200); // slot 1, a live invite
    fake.inject = (rec) => rec.method === "POST" ? { status: 500, body: { code: "internal" } } : undefined;
    assert.equal((await call(on, "POST", "/api/signup", { email, ip, body: {} })).status, 502); // revoked, then failed: slot 2
    for (let i = 0; i < 8; i++) assert.equal((await call(on, "POST", "/api/signup", { email, ip, body: {} })).status, 200, `signup ${i}`);
    assert.equal((await call(on, "POST", "/api/signup", { email, ip, body: {} })).status, 429);
  });
  test("limits signups to 10 an hour per source address", async () => {
    const ip = "203.0.113.7";
    for (let i = 0; i < 10; i++) assert.notEqual((await call(on, "POST", "/api/signup", { email: newEmail(), ip, body: {} })).status, 429);
    const r = await call(on, "POST", "/api/signup", { email: newEmail(), ip, body: {} });
    assert.equal(r.status, 429);
    assert.equal((await call(on, "POST", "/api/signup", { email: newEmail(), ip: "203.0.113.8", body: {} })).status, 200);
  });
  test("limits signups to 10 an hour per verified e-mail whatever source address it claims", async () => {
    const email = newEmail();
    for (let i = 0; i < 10; i++) assert.equal((await call(on, "POST", "/api/signup", { email, body: {} })).status, 200);
    assert.equal((await call(on, "POST", "/api/signup", { email, body: {} })).status, 429);
  });
  test("a signup the runtime failed gives the visitor's slots back", async () => {
    const email = newEmail(), ip = "203.0.113.20";
    fake.inject = (rec) => rec.path.endsWith("/tenant-status") ? { status: 500, body: { code: "internal" } } : undefined;
    assert.equal((await call(on, "POST", "/api/signup", { email, ip, body: {} })).status, 502);
    for (let i = 0; i < 10; i++) assert.equal((await call(on, "POST", "/api/signup", { email, ip, body: {} })).status, 200, `signup ${i}`);
    assert.equal((await call(on, "POST", "/api/signup", { email, ip, body: {} })).status, 429);
  });
  test("concurrent signups for one visitor: exactly 10 pass the limit and one invite stays live", async () => {
    const email = newEmail();
    fake.delayMs = 15;
    try {
      const rs = await Promise.all(Array.from({ length: 14 }, () => call(on, "POST", "/api/signup", { email, body: {} })));
      assert.equal(rs.filter((r) => r.status === 429).length, 4);
      assert.equal(rs.filter((r) => r.status === 200).length, 10, rs.map((r) => r.status).join(","));
    } finally { fake.delayMs = 0; }
    const live = [...fake.T(tenantOf(email)).invites.values()].filter((i) => !i.revoked && !i.redeemed);
    assert.equal(live.length, 1);
  });
  test("refuses a link that does not point at APP_URL, without returning it", async () => {
    fake.linkOrigin = "https://evil.test";
    try {
      const r = await call(on, "POST", "/api/signup", { email: newEmail(), body: {} });
      assert.equal(r.status, 502);
      assert.deepEqual(r.json, { error: "link_mismatch" });
    } finally { fake.linkOrigin = null; }
  });
});

// ───────────────────────────── identity + CSRF + caching ─────────────────────
describe("the shared front of every account route", () => {
  test("an unverified visitor gets 401 and nothing reaches the runtime", async () => {
    for (const [m, p] of ROUTES) {
      const before = mark();
      const r = await call(strict, m, p, { body: m === "POST" ? {} : undefined, headers: { "Cf-Access-Jwt-Assertion": "e30.e30.sig" } });
      assert.equal(r.status, 401, `${m} ${p}`);
      assert.equal(since(before).length, 0);
    }
  });
  test("a cross-origin write is refused with 403 before anything reaches the runtime", async () => {
    for (const [m, p] of ROUTES.filter(([m]) => m !== "GET")) {
      const before = mark();
      const r = await call(on, m, p, { email: newEmail(), origin: "https://evil.test", body: m === "POST" ? {} : undefined });
      assert.equal(r.status, 403, `${m} ${p}`);
      assert.deepEqual(r.json, { error: "cross_origin_refused" });
      assert.equal(since(before).length, 0);
    }
  });
  test("a POST without a JSON body is refused with 415 (no form-post CSRF)", async () => {
    const before = mark();
    const r = await fetch(on.base + "/api/signup", { method: "POST", headers: { "content-type": "application/x-www-form-urlencoded", "Cf-Access-Authenticated-User-Email": newEmail() }, body: "a=1" });
    assert.equal(r.status, 415);
    assert.equal(since(before).length, 0);
  });
  test("every account response is Cache-Control: no-store, success and error alike", async () => {
    const { email } = await signUp();
    for (const [m, p] of [...ROUTES, ["GET", "/api/whoami"]]) {
      const r = await call(on, m, p, { email, body: m === "POST" ? {} : undefined });
      assert.equal(r.cc, "no-store", `${m} ${p} (${r.status})`);
    }
  });
});

// ───────────────────────────── team ──────────────────────────────────────────
describe("GET /api/team", () => {
  test("lists only the owner's tenant, with logins, status and who is the owner", async () => {
    const owner = await signUp();
    const other = await signUp();
    const memberSubject = "cloud:" + b32();
    fake.addUser(owner.tenant, memberSubject, "isolated", "m-" + rnd(3) + "@example.com");
    fake.addUser(other.tenant, "cloud:" + b32(), "isolated", "x-" + rnd(3) + "@example.com");
    const r = await call(on, "GET", "/api/team", { email: owner.email });
    assert.equal(r.status, 200);
    assert.equal(r.json.self, owner.self);
    assert.deepEqual(r.json.members.map((x) => x.subject).sort(), [owner.self, memberSubject].sort());
    const me = r.json.members.find((x) => x.you);
    assert.equal(me.login, owner.email);
    assert.equal(me.access_mode, "tenant");
  });
});

describe("invites", () => {
  test("a member invite carries only the tenant and the e-mail, pinned as the login, and returns the link with its expiry", async () => {
    const { email, tenant } = await signUp();
    const m = mark();
    const r = await call(on, "POST", "/api/team/invites", { email, body: { email: " New@Example.com ", login: "someone-else@example.com", access_mode: "tenant", groups: ["admins"], tenant: "t_evil", subject: "root" } });
    assert.equal(r.status, 201, r.text);
    assert.ok(r.json.url.includes("/ui/invite#lci_"));
    assert.ok(r.json.expires_at);
    // login fixed to the invited address: whoever opens the link cannot take
    // another login (e.g. someone else's e-mail) at redemption.
    assert.deepEqual(mutations(m)[0].body, { tenant, email: "new@example.com", login: "new@example.com" });
  });
  test("an address with an account in any tenant gets one generic refusal — no cross-tenant oracle — and spends the slot", async () => {
    const { email } = await signUp();
    const answers = new Set();
    for (let i = 0; i < 20; i++) {
      const taken = `taken-${i}-${rnd(3)}@example.com`;
      fake.logins.set(taken, { tenant: "t_other", subject: "cloud:x" });
      const r = await call(on, "POST", "/api/team/invites", { email, body: { email: taken } });
      assert.equal(r.status, 409);
      answers.add(JSON.stringify(r.json));
    }
    // credential_exists reads exactly the same
    fake.inject = (rec) => rec.method === "POST" ? { status: 409, body: { code: "credential_exists", error: "x" } } : undefined;
    const ce = await call(on, "POST", "/api/team/invites", { email, body: { email: "ce@example.com" } });
    assert.equal(ce.status, 429, "20 refused invites spent the day's slots");
    fake.inject = null;
    assert.equal(answers.size, 1);
    const only = JSON.parse([...answers][0]);
    assert.equal(only.error, "cannot_invite_address");
    assert.match(only.message, /can't be invited/);
    assert.doesNotMatch([...answers][0], /login_taken|credential_exists/);
  });
  test("credential_exists maps to the same refusal as login_taken", async () => {
    const { email } = await signUp();
    fake.inject = (rec) => rec.method === "POST" ? { status: 409, body: { code: "credential_exists", error: "x" } } : undefined;
    const r = await call(on, "POST", "/api/team/invites", { email, body: { email: "ce2@example.com" } });
    assert.equal(r.status, 409);
    assert.equal(r.json.error, "cannot_invite_address");
  });
  test("a runtime that drops the connection does not spend an invite slot", async () => {
    const { email, tenant } = await signUp();
    fake.inject = (rec) => rec.method === "POST" ? { drop: true } : undefined;
    const dropped = await call(on, "POST", "/api/team/invites", { email, body: { email: "d@example.com" } });
    assert.equal(dropped.status, 502);
    assert.deepEqual(dropped.json, { error: "runtime_unreachable" });
    for (let i = 0; i < 20; i++) {
      const r = await call(on, "POST", "/api/team/invites", { email, body: { email: `k${i}@example.com` } });
      assert.equal(r.status, 201, `invite ${i}: ${r.text}`);
      fake.T(tenant).invites.get(r.json.invite_id).revoked = true;
    }
    assert.equal((await call(on, "POST", "/api/team/invites", { email, body: { email: "l@example.com" } })).status, 429);
  });
  test("an invalid e-mail is a 400 with no upstream write", async () => {
    const { email } = await signUp();
    for (const bad of ["", "no-at-sign", "a@b", "a b@example.com", 7, "x@" + "a".repeat(260) + ".com"]) {
      const m = mark();
      const r = await call(on, "POST", "/api/team/invites", { email, body: { email: bad } });
      assert.equal(r.status, 400, String(bad));
      assert.equal(mutations(m).length, 0);
    }
  });
  test("pending invites list only live ones and never a code or link", async () => {
    const { email, tenant } = await signUp();
    const a = await call(on, "POST", "/api/team/invites", { email, body: { email: "a@example.com" } });
    await call(on, "POST", "/api/team/invites", { email, body: { email: "b@example.com" } });
    fake.T(tenant).invites.get(a.json.invite_id).revoked = true;
    const r = await call(on, "GET", "/api/team/invites", { email });
    assert.equal(r.json.invites.length, 1);
    assert.equal(r.json.invites[0].email, "b@example.com");
    assert.equal(r.json.invites[0].kind, "member");
    assert.doesNotMatch(r.text, /lci_|\/ui\/invite|"url"/);
  });
  test("revoking another tenant's invite is a 404 and never reaches the revoke route", async () => {
    const owner = await signUp();
    const other = await signUp();
    const theirs = fake.addInvite(other.tenant, { email: "z@example.com" });
    const m = mark();
    const r = await call(on, "DELETE", `/api/team/invites/${theirs}?tenant=${other.tenant}`, { email: owner.email });
    assert.equal(r.status, 404);
    assert.deepEqual(r.json, { error: "not_found" });
    assert.equal(mutations(m).length, 0);
    assert.equal(fake.T(other.tenant).invites.get(theirs).revoked, undefined);
  });
  test("revoking an own invite reaches the runtime with the tenant derived from identity", async () => {
    const { email, tenant } = await signUp();
    const id = (await call(on, "POST", "/api/team/invites", { email, body: { email: "c@example.com" } })).json.invite_id;
    const m = mark();
    const r = await call(on, "DELETE", `/api/team/invites/${id}`, { email });
    assert.equal(r.status, 204);
    const del = mutations(m)[0];
    assert.equal(del.path, `/v1/_authgate/${GATE}/invites/${id}`);
    assert.equal(del.query.tenant, tenant);
  });
  test("a malformed invite id is a 404 with no upstream call", async () => {
    const { email } = await signUp();
    for (const bad of ["..", "inv_..", "inv_ABC", "x%2F..%2F_users"]) {
      const m = mark();
      const r = await call(on, "DELETE", `/api/team/invites/${bad}`, { email });
      assert.equal(r.status, 404, bad);
      assert.equal(since(m).length, 0);
    }
  });
  test("limits a tenant to 20 invites a day", async () => {
    const { email, tenant } = await signUp();
    for (let i = 0; i < 20; i++) {
      const r = await call(on, "POST", "/api/team/invites", { email, body: { email: `p${i}@example.com` } });
      assert.equal(r.status, 201, r.text);
      // keep the pending limit out of the way
      fake.T(tenant).invites.get(r.json.invite_id).revoked = true;
    }
    const r = await call(on, "POST", "/api/team/invites", { email, body: { email: "late@example.com" } });
    assert.equal(r.status, 429);
    assert.deepEqual(r.json, { error: "invite_limit" });
  });
  test("concurrent invites past the daily limit: exactly 20 succeed", async () => {
    const { email } = await signUp();
    fake.delayMs = 20;
    try {
      const rs = await Promise.all(Array.from({ length: 26 }, (_, i) =>
        call(on, "POST", "/api/team/invites", { email, body: { email: `c${i}@example.com` } })));
      assert.equal(rs.filter((r) => r.status === 201).length, 20);
      assert.equal(rs.filter((r) => r.status === 429).length, 6);
    } finally { fake.delayMs = 0; }
  });
  test("a failed upstream invite gives its daily slot back", async () => {
    const { email, tenant } = await signUp();
    fake.inject = (rec) => rec.method === "POST" ? { status: 500, body: { code: "internal" } } : undefined;
    assert.equal((await call(on, "POST", "/api/team/invites", { email, body: { email: "f@example.com" } })).status, 502);
    for (let i = 0; i < 20; i++) {
      const r = await call(on, "POST", "/api/team/invites", { email, body: { email: `g${i}@example.com` } });
      assert.equal(r.status, 201, `invite ${i}: ${r.text}`);
      fake.T(tenant).invites.get(r.json.invite_id).revoked = true;
    }
    assert.equal((await call(on, "POST", "/api/team/invites", { email, body: { email: "h@example.com" } })).status, 429);
  });
  test("concurrent invites cannot overshoot the pending limit", async () => {
    const { email, tenant } = await signUp();
    for (let i = 0; i < 48; i++) fake.addInvite(tenant, { email: `s${i}@example.com` });
    fake.delayMs = 20;
    try {
      const rs = await Promise.all(Array.from({ length: 5 }, (_, i) =>
        call(on, "POST", "/api/team/invites", { email, body: { email: `t${i}@example.com` } })));
      assert.equal(rs.filter((r) => r.status === 201).length, 2);
      assert.ok(rs.filter((r) => r.status !== 201).every((r) => r.json.error === "too_many_pending"));
    } finally { fake.delayMs = 0; }
    // a refusal the tenant's own state causes keeps its slot spent: 5 of 20 used
    const left = [...fake.T(tenant).invites.values()];
    for (const i of left) i.revoked = true;
    for (let i = 0; i < 15; i++) assert.equal((await call(on, "POST", "/api/team/invites", { email, body: { email: `u${i}@example.com` } })).status, 201);
    assert.equal((await call(on, "POST", "/api/team/invites", { email, body: { email: "u-last@example.com" } })).status, 429);
  });
  test("refuses a 51st pending invite", async () => {
    const { email, tenant } = await signUp();
    for (let i = 0; i < 50; i++) fake.addInvite(tenant, { email: `q${i}@example.com` });
    const m = mark();
    const r = await call(on, "POST", "/api/team/invites", { email, body: { email: "one-more@example.com" } });
    assert.equal(r.status, 429);
    assert.deepEqual(r.json, { error: "too_many_pending" });
    assert.equal(mutations(m).length, 0);
  });
});

describe("per-subject routes", () => {
  const PER_SUBJECT = (s) => [["POST", `/api/team/${s}/reset`], ["POST", `/api/team/${s}/disable`], ["POST", `/api/team/${s}/enable`], ["DELETE", `/api/team/${s}`]];

  test("a subject of another tenant reads exactly like an unknown one: 404, nothing changed", async () => {
    const owner = await signUp();
    const other = await signUp();
    const victim = "cloud:" + b32();
    fake.addUser(other.tenant, victim, "isolated", "v-" + rnd(3) + "@example.com");
    for (const s of [encodeURIComponent(victim), encodeURIComponent("cloud:" + b32()), encodeURIComponent(other.self)]) {
      for (const [m, p] of PER_SUBJECT(s)) {
        const before = mark();
        // the request also tries to name the other tenant, in the query and the body
        const r = await call(on, m, `${p}?tenant=${other.tenant}`, { email: owner.email, body: m === "POST" ? { tenant: other.tenant, subject: victim } : undefined });
        assert.equal(r.status, 404, `${m} ${p}`);
        assert.deepEqual(r.json, { error: "not_found" });
        assert.equal(mutations(before).length, 0, `${m} ${p} must not write`);
        for (const x of since(before)) assert.equal(x.query.tenant, owner.tenant);
      }
    }
    assert.equal(fake.T(other.tenant).users.get(victim).status, "active");
  });
  test("every per-subject route checks membership with the fixed users read before acting", async () => {
    const { email, tenant } = await signUp();
    const member = "cloud:" + b32();
    fake.addUser(tenant, member, "isolated", "m-" + rnd(3) + "@example.com");
    for (const [m, p] of PER_SUBJECT(encodeURIComponent(member))) {
      const before = mark();
      const r = await call(on, m, p, { email, body: m === "POST" ? {} : undefined });
      assert.ok(r.status < 300, `${m} ${p}: ${r.status} ${r.text}`);
      const calls = since(before);
      assert.equal(calls[0].method, "GET");
      assert.equal(calls[0].path, "/v1/_users");
      assert.equal(calls[0].query.tenant, tenant);
      assert.ok(calls.slice(1).some((x) => x.method !== "GET"));
      if (m === "DELETE") fake.addUser(tenant, member, "isolated");
    }
  });
  test("subjects are validated and percent-encoded into the upstream path", async () => {
    const { email, tenant } = await signUp();
    const member = "cloud:" + b32();
    fake.addUser(tenant, member, "isolated", "m-" + rnd(3) + "@example.com");
    let m = mark();
    await call(on, "POST", `/api/team/${encodeURIComponent(member)}/disable`, { email, body: {} });
    const patch = mutations(m)[0];
    assert.equal(patch.rawPath, `/v1/_users/${encodeURIComponent(member)}?tenant=${tenant}`);
    assert.deepEqual(patch.body, { status: "disabled" });
    for (const bad of ["..", "%2e%2e", "a%2Fb", "%20x", "x%00", "-x"]) {
      m = mark();
      const r = await call(on, "POST", `/api/team/${bad}/disable`, { email, body: {} });
      assert.equal(r.status, 404, bad);
      assert.equal(since(m).length, 0, `${bad}: no upstream call`);
    }
  });
  test("reset returns a link for a member and for the owner's own subject", async () => {
    const { email, tenant, self } = await signUp();
    const member = "cloud:" + b32();
    fake.addUser(tenant, member, "isolated", "m-" + rnd(3) + "@example.com");
    for (const s of [member, self]) {
      const m = mark();
      const r = await call(on, "POST", `/api/team/${encodeURIComponent(s)}/reset`, { email, body: { subject: "someone-else" } });
      assert.equal(r.status, 201, r.text);
      assert.ok(r.json.url.includes("/ui/reset#lcr_"));
      assert.deepEqual(mutations(m)[0].body, { tenant, subject: s });
    }
  });
  test("resetting the owner surfaces loomcycle's refusal when owner recovery is off", async () => {
    const { email, self } = await signUp();
    fake.recover = false;
    try {
      const r = await call(on, "POST", `/api/team/${encodeURIComponent(self)}/reset`, { email, body: {} });
      assert.equal(r.status, 403);
      assert.deepEqual(r.json, { error: "target_beyond_floor" });
    } finally { fake.recover = true; }
  });
  test("reset of a user with no login yet is 409 no_login", async () => {
    const { email, tenant } = await signUp();
    const s = "cloud:" + b32();
    fake.addUser(tenant, s, "isolated");
    const r = await call(on, "POST", `/api/team/${encodeURIComponent(s)}/reset`, { email, body: {} });
    assert.equal(r.status, 409);
    assert.deepEqual(r.json, { error: "no_login" });
  });
  test("disable, enable and remove change a member; the owner is refused by the floor", async () => {
    const { email, tenant, self } = await signUp();
    const member = "cloud:" + b32();
    fake.addUser(tenant, member, "isolated", "m-" + rnd(3) + "@example.com");
    const enc = encodeURIComponent(member);
    assert.deepEqual((await call(on, "POST", `/api/team/${enc}/disable`, { email, body: {} })).json, { subject: member, status: "disabled" });
    assert.equal(fake.T(tenant).users.get(member).status, "disabled");
    assert.deepEqual((await call(on, "POST", `/api/team/${enc}/enable`, { email, body: {} })).json, { subject: member, status: "active" });
    assert.equal((await call(on, "DELETE", `/api/team/${enc}`, { email })).status, 204);
    assert.equal(fake.T(tenant).users.has(member), false);
    const r = await call(on, "POST", `/api/team/${encodeURIComponent(self)}/disable`, { email, body: {} });
    assert.equal(r.status, 403);
    assert.deepEqual(r.json, { error: "target_beyond_floor" });
  });
});

// ───────────────────────────── upstream errors + logs ────────────────────────
describe("upstream failures", () => {
  test("an unexpected upstream error is an opaque 502 that carries none of its body", async () => {
    const { email } = await signUp();
    fake.inject = (rec) => rec.method === "POST" ? { status: 500, body: { code: "internal", error: "db down at https://x/ui/invite#lci_SECRETLINK" } } : undefined;
    const r = await call(on, "POST", "/api/team/invites", { email, body: { email: "e@example.com" } });
    assert.equal(r.status, 502);
    assert.deepEqual(r.json, { error: "upstream_error" });
  });
  test("an unknown upstream code is not passed through", async () => {
    const { email } = await signUp();
    fake.inject = (rec) => rec.method === "POST" ? { status: 403, body: { code: "insufficient_scope", error: "x" } } : undefined;
    const r = await call(on, "POST", "/api/team/invites", { email, body: { email: "e@example.com" } });
    assert.equal(r.status, 502);
    assert.deepEqual(r.json, { error: "upstream_error" });
  });
  test("an upstream not-found (a member removed mid-request) is the same opaque 404", async () => {
    const { email, tenant } = await signUp();
    const member = "cloud:" + b32();
    fake.addUser(tenant, member, "isolated", "m-" + rnd(3) + "@example.com");
    fake.inject = (rec) => rec.method === "PATCH" ? { status: 404, body: { code: "user_not_found", error: "no such user in this tenant" } } : undefined;
    const r = await call(on, "POST", `/api/team/${encodeURIComponent(member)}/disable`, { email, body: {} });
    assert.equal(r.status, 404);
    assert.deepEqual(r.json, { error: "not_found" });
  });
  test("an unreachable runtime is a 502", async () => {
    const dead = await startLanding({ LOOMCYCLE_URL: "http://127.0.0.1:9", LOOMCYCLE_ADMIN_TOKEN: ADMIN, MINT_DEV_ALLOW_UNVERIFIED: "1",
      LOOMCYCLE_AUTH_MANAGE_TOKEN: MANAGE });
    try {
      const r = await call(dead, "GET", "/api/team", { email: newEmail() });
      assert.equal(r.status, 502);
      assert.deepEqual(r.json, { error: "runtime_unreachable" });
    } finally { dead.child.kill("SIGKILL"); }
  });
});

// Runs last (node:test runs top-level suites in order): everything above has
// issued links and errors, so the landing's whole log is the evidence.
describe("logging", () => {
  test("the landing's logs carry no invite or reset code, no link and no token", async () => {
    await new Promise((r) => setTimeout(r, 100));
    const logs = on.logs() + off.logs() + strict.logs();
    assert.ok(logs.includes("account upstream"), "the suite produced account log lines to inspect");
    assert.ok(fake.codes.length > 20);
    for (const code of fake.codes) assert.equal(logs.includes(code), false, "a link code reached the log");
    for (const s of ["lci_", "lcr_", "/ui/invite#", "/ui/reset#", "LEAKME", "SECRETLINK", MANAGE, ADMIN]) {
      assert.equal(logs.includes(s), false, `log contains ${s.slice(0, 8)}…`);
    }
  });
});
