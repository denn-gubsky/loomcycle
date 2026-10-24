// loomcycle.cloud landing server.
//
// Three jobs, one Node process:
//   1. Serve the static marketing site (cloud-web/, mounted at WEB_ROOT) — with
//      HTTP range support so the hero/mascot videos stream.
//   2. Proxy the runtime's PUBLIC reads as same-origin "inner links": GET
//      /healthz and GET /v1/* (the operator-admin plane /v1/_* is refused here —
//      it must never be reachable on the public apex).
//   3. Mint substrate:tenant tokens on behalf of a Cloudflare-Access-authenticated
//      visitor, holding the loomcycle ADMIN token server-side. The browser never
//      sees the admin token and cannot choose its own scopes/tenant — the proxy
//      forces op=create + a fixed safe scope set and derives (tenant, subject)
//      from the verified Access identity.
//
// Security model: this service holds an admin credential, so it is reachable ONLY
// via the Cloudflare tunnel (no host port) and the /api/* routes are protected by
// a Cloudflare Access policy. We still VERIFY the Access JWT (Cf-Access-Jwt-
// Assertion) against the team's JWKS + audience — never trust a bare header — so a
// request that somehow reaches us off-tunnel cannot forge an identity.

//
// 4. (When LOOMCYCLE_AUTH_MANAGE_TOKEN is set) the account flow on loomcycle's
//    password gate: /api/signup creates the visitor's workspace by redirecting
//    them to an owner invite, and /api/team/* lets the owner invite, reset,
//    disable and remove the people in their tenant. It holds an auth:manage
//    token — user records only, in any tenant it names — so the tenant is
//    always derived from the verified Access identity, never the request. See
//    ../AUTH-INTEGRATION.md. Without the token those routes answer 503 and the
//    site keeps its token-vault flow.

import express from "express";
import { createRemoteJWKSet, jwtVerify } from "jose";
import { createHash } from "node:crypto";
import { isIP } from "node:net";

const PORT = parseInt(process.env.PORT || "8080", 10);
const RUNTIME_URL = (process.env.LOOMCYCLE_URL || "http://tailscale:8787").replace(/\/+$/, "");
const ADMIN_TOKEN = process.env.LOOMCYCLE_ADMIN_TOKEN || "";
const TEAM_DOMAIN = (process.env.CF_ACCESS_TEAM_DOMAIN || "").replace(/^https?:\/\//, "").replace(/\/+$/, "");
const ACCESS_AUD = process.env.CF_ACCESS_AUD || "";
const MINT_SCOPES = (process.env.MINT_SCOPES || "substrate:tenant,runs:create")
  .split(",").map((s) => s.trim()).filter(Boolean);
const TENANT_PREFIX = process.env.MINT_TENANT_PREFIX || "t_";
const WEB_ROOT = process.env.WEB_ROOT || "/srv";
// LOCAL DEV ONLY: skip JWT verification and trust the email header (or a stub).
const DEV_ALLOW_UNVERIFIED = process.env.MINT_DEV_ALLOW_UNVERIFIED === "1";
const APP_ORIGIN = process.env.APP_ORIGIN || ""; // e.g. https://loomcycle.cloud — CSRF origin check
// The account flow (job 4). Empty token = the flow is off and its routes 503.
const MANAGE_TOKEN = process.env.LOOMCYCLE_AUTH_MANAGE_TOKEN || "";
const AUTH_GATE = process.env.AUTH_GATE || "cloud";
const APP_URL = (process.env.APP_URL || "https://app.loomcycle.cloud").replace(/\/+$/, "");
const SIGNUP_RETURN_TO = process.env.SIGNUP_RETURN_TO || "https://loomcycle.cloud/?welcome=1";

// Fail fast on misconfiguration.
if (!ADMIN_TOKEN) {
  console.error("FATAL: LOOMCYCLE_ADMIN_TOKEN is required");
  process.exit(1);
}
// The gate name goes into every upstream path, so it gets loomcycle's own gate
// name rule rather than being trusted to encode cleanly.
if (!/^[a-z0-9][a-z0-9_-]{0,31}$/.test(AUTH_GATE)) {
  console.error("FATAL: AUTH_GATE must be a loomcycle gate name (lowercase letters, digits, - and _)");
  process.exit(1);
}
if (!/^https?:\/\/[^/?#]+$/.test(APP_URL)) {
  console.error("FATAL: APP_URL must be an origin such as https://app.loomcycle.cloud");
  process.exit(1);
}
if (!DEV_ALLOW_UNVERIFIED && (!TEAM_DOMAIN || !ACCESS_AUD)) {
  console.error("FATAL: CF_ACCESS_TEAM_DOMAIN + CF_ACCESS_AUD are required (or set MINT_DEV_ALLOW_UNVERIFIED=1 for local dev)");
  process.exit(1);
}

const JWKS = TEAM_DOMAIN
  ? createRemoteJWKSet(new URL(`https://${TEAM_DOMAIN}/cdn-cgi/access/certs`))
  : null;

// accessIdentity verifies the Cloudflare Access assertion and returns { email }
// or null. In dev-unverified mode it trusts the email header (or a stub).
async function accessIdentity(req) {
  const assertion =
    req.get("Cf-Access-Jwt-Assertion") || cookie(req, "CF_Authorization");
  if (!assertion) {
    if (DEV_ALLOW_UNVERIFIED) {
      return { email: req.get("Cf-Access-Authenticated-User-Email") || "dev@example.com" };
    }
    return null;
  }
  if (DEV_ALLOW_UNVERIFIED) {
    // decode without verifying (dev only)
    try {
      const p = JSON.parse(Buffer.from(assertion.split(".")[1], "base64url").toString());
      return { email: p.email || "dev@example.com" };
    } catch { return { email: "dev@example.com" }; }
  }
  try {
    const { payload } = await jwtVerify(assertion, JWKS, {
      issuer: `https://${TEAM_DOMAIN}`,
      audience: ACCESS_AUD,
    });
    const email = payload.email || payload.sub;
    return email ? { email: String(email) } : null;
  } catch (e) {
    console.warn("access verify failed:", e && e.message);
    return null;
  }
}

function cookie(req, name) {
  const raw = req.get("cookie") || "";
  for (const part of raw.split(";")) {
    const i = part.indexOf("=");
    if (i > 0 && part.slice(0, i).trim() === name) return part.slice(i + 1).trim();
  }
  return null;
}

// Deterministic (tenant, subject) from the verified email, so a visitor always
// lands in — and re-mints into — the same isolated tenant. Hash-based so two
// different emails can never collide onto one tenant.
function principalFor(email) {
  const e = email.trim().toLowerCase();
  return { tenant: TENANT_PREFIX + createHash("sha256").update(e).digest("hex").slice(0, 12), subject: e };
}

const app = express();
app.disable("x-powered-by");
app.set("trust proxy", true);
app.use(express.json({ limit: "16kb" }));

// Liveness of the landing process itself (does NOT proxy the runtime).
app.get("/_up", (_req, res) => res.json({ ok: true }));

// Nothing under /api is ever cacheable: identities, invite and reset links.
app.use("/api", (_req, res, next) => { res.set("Cache-Control", "no-store"); next(); });

// GET /api/whoami — the sign-in probe. As an XHR it returns the identity as JSON
// (401 when not signed in); as a full-page navigation (the "Sign in" button, which
// triggers the Cloudflare Access login) it redirects back to the landing.
app.get("/api/whoami", async (req, res) => {
  const wantsHTML = (req.get("accept") || "").includes("text/html");
  const id = await accessIdentity(req);
  res.set("Cache-Control", "no-store");
  if (!id) {
    if (wantsHTML) return res.redirect("/");
    return res.status(401).json({ authenticated: false });
  }
  const p = principalFor(id.email);
  if (wantsHTML) return res.redirect("/?signedin=1");
  const out = { authenticated: true, email: id.email, tenant: p.tenant, subject: p.subject, auth_gate: !!MANAGE_TOKEN };
  if (MANAGE_TOKEN) {
    out.app = APP_URL + "/ui/login";
    // null = the runtime could not say; the page then offers both actions and
    // /api/signup answers for itself.
    try {
      const st = await signupState(p);
      out.signed_up = st.kind === "signed_up" || st.kind === "exists";
    } catch (e) {
      logWarn("whoami: signup state unavailable:", e && e.message);
      out.signed_up = null;
    }
  }
  res.json(out);
});

// POST /api/mint {name} — mint a substrate:tenant token for the authenticated
// visitor's derived tenant. The admin token + the scope/tenant are enforced HERE;
// the client only supplies a display name.
app.post("/api/mint", async (req, res) => {
  res.set("Cache-Control", "no-store");
  // CSRF: reject a cross-origin POST if an Origin is present and doesn't match.
  const origin = req.get("origin");
  if (APP_ORIGIN && origin && origin !== APP_ORIGIN) {
    return res.status(403).json({ error: "cross-origin request refused" });
  }
  const id = await accessIdentity(req);
  if (!id) return res.status(401).json({ error: "not signed in" });
  const p = principalFor(id.email);
  const name = String((req.body && req.body.name) || "token").trim().slice(0, 64) || "token";
  try {
    const up = await fetch(`${RUNTIME_URL}/v1/_operatortokendef`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json",
        Authorization: `Bearer ${ADMIN_TOKEN}`,
      },
      body: JSON.stringify({
        op: "create",
        name,
        tenant_id: p.tenant,
        subject: p.subject,
        scopes: MINT_SCOPES,
      }),
    });
    const text = await up.text();
    if (!up.ok) {
      console.warn("mint upstream", up.status, text.slice(0, 200));
      let msg = `upstream ${up.status}`;
      try { const j = JSON.parse(text); if (j.error) msg = j.error; } catch {}
      return res.status(502).json({ error: msg });
    }
    const out = JSON.parse(text);
    // Return only what the browser needs; the plaintext token is shown once.
    res.json({
      name: out.name,
      tenant_id: out.tenant_id,
      subject: out.subject,
      allowed_scopes: out.allowed_scopes,
      token: out.token,
      token_suffix: out.token_suffix,
    });
  } catch (e) {
    console.error("mint error:", e && e.message);
    res.status(500).json({ error: "internal" });
  }
});

// ─── The account flow: /api/signup + /api/team/* (job 4) ─────────────────────
//
// Every route below follows the same rules (AUTH-INTEGRATION.md):
//   - the tenant comes ONLY from the verified Access identity (principalFor);
//     no request field names a tenant, and a subject outside the owner's tenant
//     is a 404, the same answer as an unknown one;
//   - a per-subject route first finds the subject in the owner's tenant
//     (GET /v1/_users?tenant=, the only per-tenant user read auth:manage has);
//   - upstream paths are fixed strings plus validated, encodeURIComponent-ed
//     segments, and upstream bodies are built field by field;
//   - an upstream error reaches the client only as a whitelisted code;
//   - nothing logs an invite or reset link or code, or the manage token — until
//     it is redeemed a link is a credential.

const UPSTREAM_TIMEOUT_MS = 10_000;
// A subject is a loomcycle-made `<gate>:<base32>` or a legacy owner's e-mail.
// The first character is alphanumeric, so `.` and `..` can never be a segment.
const SUBJECT_RE = /^[A-Za-z0-9][A-Za-z0-9._+@:'-]{0,253}$/;
const INVITE_ID_RE = /^inv_[a-z2-7]{1,64}$/;
const EMAIL_RE = /^[A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]{1,64}@[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)+$/;
// Upstream error codes the client may see, and the status it sees them with.
// A "not found" of any kind collapses into one 404 so another tenant's ids
// read exactly like unknown ones. Anything else is an opaque 502.
const PASS_CODES = new Map([
  ["login_taken", 409], ["bootstrap_invite_live", 409], ["credential_exists", 409],
  ["invite_not_live", 409], ["tenant_not_empty", 409], ["target_beyond_floor", 403],
  ["invalid_login", 400], ["user_not_found", 404], ["login_not_found", 404],
  ["invite_not_found", 404],
  // Deployment faults: the operator needs the code, the visitor cannot fix them.
  ["invalid_return_to", 502], ["gate_not_found", 502],
]);

// redact strips what must never reach a log line: link codes, URL fragments
// (where the codes travel), bearer values and the manage token itself.
function redact(s) {
  let out = String(s)
    .replace(/lc[a-z]_[A-Za-z0-9_-]+/g, "lc?_[redacted]")
    .replace(/#[^\s"'<>]*/g, "#[redacted]")
    .replace(/Bearer\s+\S+/gi, "Bearer [redacted]");
  if (MANAGE_TOKEN) out = out.split(MANAGE_TOKEN).join("[redacted]");
  return out;
}
function logWarn(...parts) { console.warn(parts.map(redact).join(" ")); }

// A sliding-window counter per key, in memory: there is one landing process
// (move it to Redis if the landing ever scales out). Keys are a tenant id, a
// verified e-mail or a source address; old hits are pruned on use and hourly.
function limiter(max, windowMs) {
  const hits = new Map();
  const live = (key, now) => (hits.get(key) || []).filter((t) => now - t < windowMs);
  setInterval(() => {
    const now = Date.now();
    for (const k of hits.keys()) {
      const l = live(k, now);
      if (l.length) hits.set(k, l); else hits.delete(k);
    }
  }, 60 * 60 * 1000).unref();
  return {
    allowed(key, now = Date.now()) { return live(key, now).length < max; },
    take(key, now = Date.now()) {
      const l = live(key, now);
      if (l.length >= max) return false;
      l.push(now); hits.set(key, l);
      return true;
    },
  };
}
const HOUR = 60 * 60 * 1000;
const signupLimit = limiter(10, HOUR);          // per source address AND per verified e-mail
const inviteLimit = limiter(20, 24 * HOUR);     // invites issued per tenant per day
const MAX_PENDING_INVITES = 50;                 // live invites per tenant (counted upstream)

// clientSource is the address the per-source signup limit counts. The landing
// already trusts its proxy (`trust proxy`), and Cloudflare's edge sets
// CF-Connecting-IP, overwriting a client's own. But any container on the
// compose network can reach the landing and forge that header too, so it is
// only ever read AFTER the Access JWT verified (the limiter runs behind
// accessIdentity), and the signup limit is keyed on the verified e-mail as
// well — the one key nobody can forge.
function clientSource(req) {
  const cf = (req.get("cf-connecting-ip") || "").trim();
  if (cf && isIP(cf)) return cf;
  return req.ip || req.socket.remoteAddress || "unknown";
}

// The login a loomcycle password gate stores for an e-mail: it compares logins
// after NFKC and case folding (lowercase is the fold for e-mail addresses).
const loginOf = (s) => String(s).trim().normalize("NFKC").toLowerCase();

class UpstreamFailure extends Error {
  constructor(label, up) { super(`${label}: ${up.status}`); this.label = label; this.up = up; }
}

const gatePath = (...segs) =>
  `/v1/_authgate/${encodeURIComponent(AUTH_GATE)}` + segs.map((s) => "/" + encodeURIComponent(s)).join("");
const userPath = (subject) => "/v1/_users/" + encodeURIComponent(subject);

// manage calls loomcycle with the auth:manage token. `path` is always built
// from fixed strings and gatePath/userPath; it never follows a redirect, so the
// bearer cannot be bounced elsewhere. status 0 = the runtime is unreachable.
async function manage(method, path, { query, body } = {}) {
  const url = new URL(RUNTIME_URL + path);
  for (const [k, v] of Object.entries(query || {})) url.searchParams.set(k, v);
  const headers = { Accept: "application/json", Authorization: `Bearer ${MANAGE_TOKEN}` };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  let r;
  try {
    r = await fetch(url, {
      method, headers, redirect: "manual",
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(UPSTREAM_TIMEOUT_MS),
    });
  } catch {
    return { status: 0, json: null };
  }
  const text = await r.text().catch(() => "");
  let json = null;
  try { json = text ? JSON.parse(text) : null; } catch {}
  return { status: r.status, json };
}

// sendUpstreamError answers a failed upstream call with a whitelisted code, or
// an opaque 502. Only the status and the code are logged, never the body.
function sendUpstreamError(res, label, up) {
  const raw = up.json && typeof up.json.code === "string" ? up.json.code : "";
  const code = /^[a-z_]{1,48}$/.test(raw) ? raw : "";
  logWarn(`account upstream ${label}: ${up.status || "unreachable"} ${code || "-"}`);
  if (up.status === 0) return res.status(502).json({ error: "runtime_unreachable" });
  const st = PASS_CODES.get(code);
  if (st === 404) return notFound(res);
  if (st) return res.status(st).json({ error: code });
  return res.status(502).json({ error: "upstream_error" });
}
const notFound = (res) => res.status(404).json({ error: "not_found" });

// checkedLink accepts an invite/reset link only when it points at the app's own
// page, so a misconfigured runtime (a wrong LOOMCYCLE_PUBLIC_URL) can't turn the
// signup redirect into an open redirect.
function checkedLink(json, page) {
  if (!json || typeof json.url !== "string" || json.url.length > 2048) return null;
  if (!json.url.startsWith(APP_URL + page)) return null;
  return { url: json.url, expires_at: typeof json.expires_at === "string" ? json.expires_at : null };
}

// tenantUsers lists the registered users of a tenant (no run activity: an
// auth:manage token sees user records only).
async function tenantUsers(tenant) {
  const up = await manage("GET", "/v1/_users", { query: { tenant } });
  if (up.status !== 200 || !up.json || !Array.isArray(up.json.users)) throw new UpstreamFailure("users", up);
  return up.json.users.filter((u) => u && typeof u.user_id === "string");
}

// gateIdentity is the subject's login at AUTH_GATE, or null when it has none.
async function gateIdentity(tenant, subject) {
  const up = await manage("GET", gatePath("identities"), { query: { tenant, subject } });
  if (up.status === 404) return null;
  if (up.status !== 200 || !up.json || !Array.isArray(up.json.identities)) throw new UpstreamFailure("identities", up);
  const id = up.json.identities.find((i) => i && i.gate === AUTH_GATE);
  return id ? { login: String(id.login || ""), has_password: !!id.has_password, last_login_at: id.last_login_at || null } : null;
}

async function tenantInvites(tenant) {
  const up = await manage("GET", gatePath("invites"), { query: { tenant } });
  if (up.status !== 200 || !up.json || !Array.isArray(up.json.invites)) throw new UpstreamFailure("invites", up);
  return up.json.invites.filter((i) => i && typeof i.invite_id === "string");
}

// findSelf finds the signed-in owner among the tenant's users: the user whose
// login at the gate is their e-mail (an owner made by a signup invite has a
// loomcycle-made subject), else the legacy owner whose subject IS the e-mail
// (made by the old /api/mint path). Only access_mode tenant users and the
// e-mail subject are looked at, so a large team costs no extra calls.
// `ids` caches identity lookups for the caller.
async function findSelf(p, users, ids = new Map()) {
  const login = loginOf(p.subject);
  const idOf = async (s) => {
    if (!ids.has(s)) ids.set(s, await gateIdentity(p.tenant, s));
    return ids.get(s);
  };
  const candidates = users.filter((u) => u.access_mode === "tenant" || u.user_id === p.subject).slice(0, 20);
  for (const u of candidates) {
    const id = await idOf(u.user_id);
    if (id && loginOf(id.login) === login) return { subject: u.user_id, hasLogin: true };
  }
  const legacy = users.find((u) => u.user_id === p.subject);
  if (legacy) return { subject: legacy.user_id, hasLogin: !!(await idOf(legacy.user_id)) };
  return null;
}

// signupState decides what /api/signup does for this visitor:
//   new       — the tenant never had a user: a bootstrap owner invite;
//   legacy    — the e-mail is a user without a login at the gate: an invite
//               that attaches a password to that subject (phase 4);
//   signed_up — the visitor already has a login: go to the app;
//   exists    — the tenant has (or had) users but none is this visitor's
//               account: nothing the website may do (go to the app).
async function signupState(p) {
  const up = await manage("GET", gatePath("tenant-status"), { query: { tenant: p.tenant } });
  if (up.status !== 200 || !up.json) throw new UpstreamFailure("tenant-status", up);
  if (!up.json.bootstrapped) return { kind: "new", inviteLive: !!up.json.bootstrap_invite_live };
  const self = await findSelf(p, await tenantUsers(p.tenant));
  if (!self) return { kind: "exists" };
  return { kind: self.hasLogin ? "signed_up" : "legacy", subject: self.subject };
}

// revokeLive revokes the tenant's live invites that match pred. A stale one is
// the visitor's own: only this owner's verified identity can ask the landing
// for an invite into this tenant, and its link was shown only in their browser.
async function revokeLive(tenant, pred) {
  for (const inv of await tenantInvites(tenant)) {
    if (inv.state !== "live" || !pred(inv) || !INVITE_ID_RE.test(inv.invite_id)) continue;
    const up = await manage("DELETE", gatePath("invites", inv.invite_id), { query: { tenant } });
    if (up.status !== 204 && !(up.json && up.json.code === "invite_not_live")) throw new UpstreamFailure("revoke", up);
  }
}

// accountRoute wraps a handler with the shared front: the flow is configured,
// a write is same-origin JSON, and the visitor is verified. The handler gets
// the principal derived from that identity — and nothing else to name a tenant.
function accountRoute(handler) {
  return async (req, res) => {
    if (!MANAGE_TOKEN) return res.status(503).json({ error: "auth_gate_not_configured" });
    if (req.method !== "GET") {
      // CSRF: the same Origin check as /api/mint …
      const origin = req.get("origin");
      if (APP_ORIGIN && origin && origin !== APP_ORIGIN) return res.status(403).json({ error: "cross_origin_refused" });
      // … and a JSON body, which a cross-site <form> cannot send without a
      // CORS preflight the landing never answers.
      if (req.method === "POST" && !(req.get("content-type") || "").toLowerCase().startsWith("application/json")) {
        return res.status(415).json({ error: "json_required" });
      }
    }
    const id = await accessIdentity(req);
    if (!id) return res.status(401).json({ error: "not signed in" });
    try {
      await handler(req, res, principalFor(id.email));
    } catch (e) {
      if (e instanceof UpstreamFailure) return sendUpstreamError(res, e.label, e.up);
      logWarn("account route error:", e && e.message);
      if (!res.headersSent) res.status(500).json({ error: "internal" });
    }
  };
}

// memberOrNull finds :subject in the owner's tenant; null = answer 404.
async function memberOrNull(p, subject) {
  if (typeof subject !== "string" || !SUBJECT_RE.test(subject)) return null;
  return (await tenantUsers(p.tenant)).find((u) => u.user_id === subject) || null;
}

// POST /api/signup — create the visitor's workspace, or re-enter it. The answer
// is {redirect}: the page location.replace()s it (the code rides the fragment,
// so it reaches no server log, and replace() keeps it out of this site's history).
app.post("/api/signup", accountRoute(async (req, res, p) => {
  if (!signupLimit.allowed("ip:" + clientSource(req)) || !signupLimit.allowed("id:" + p.subject)) {
    return res.status(429).json({ error: "rate_limited" });
  }
  signupLimit.take("ip:" + clientSource(req));
  signupLimit.take("id:" + p.subject);
  const st = await signupState(p);
  let body;
  if (st.kind === "new") {
    // A live owner invite (P2 allows one) is the visitor's own earlier attempt
    // whose link they no longer have: revoke it and issue a fresh one rather
    // than telling them to find a link that was only ever in their address bar.
    if (st.inviteLive) await revokeLive(p.tenant, (inv) => inv.bootstrap === true);
    body = { tenant: p.tenant, access_mode: "tenant", login: p.subject, return_to: SIGNUP_RETURN_TO };
  } else if (st.kind === "legacy") {
    await revokeLive(p.tenant, (inv) => inv.subject === st.subject);
    body = { tenant: p.tenant, subject: st.subject, login: p.subject, return_to: SIGNUP_RETURN_TO };
  } else {
    return res.status(409).json({ error: "already_signed_up", signed_up: true, app: APP_URL + "/ui/login" });
  }
  const up = await manage("POST", gatePath("invites"), { body });
  if (up.status !== 201) return sendUpstreamError(res, "signup", up);
  const link = checkedLink(up.json, "/ui/invite#");
  if (!link) { logWarn("account upstream signup: link outside APP_URL"); return res.status(502).json({ error: "link_mismatch" }); }
  res.json({ redirect: link.url, expires_at: link.expires_at });
}));

// GET /api/team — the owner's tenant: who is in it, their login and status.
app.get("/api/team", accountRoute(async (_req, res, p) => {
  const users = (await tenantUsers(p.tenant)).slice(0, 200);
  const ids = new Map();
  const self = await findSelf(p, users, ids);
  // Fetch the remaining logins a few at a time.
  const todo = users.filter((u) => !ids.has(u.user_id));
  for (let i = 0; i < todo.length; i += 6) {
    await Promise.all(todo.slice(i, i + 6).map(async (u) => ids.set(u.user_id, await gateIdentity(p.tenant, u.user_id))));
  }
  res.json({
    self: self ? self.subject : null,
    members: users.map((u) => {
      const id = ids.get(u.user_id);
      return {
        subject: u.user_id, display_name: u.display_name || "", login: id ? id.login : "",
        access_mode: u.access_mode || "", status: u.status || "",
        has_password: !!(id && id.has_password), last_login_at: id ? id.last_login_at : null,
        you: !!self && self.subject === u.user_id,
      };
    }),
  });
}));

// GET /api/team/invites — the tenant's pending invites, never a code or link.
app.get("/api/team/invites", accountRoute(async (_req, res, p) => {
  const live = (await tenantInvites(p.tenant)).filter((i) => i.state === "live");
  res.json({
    invites: live.map((i) => ({
      invite_id: i.invite_id, email: i.email || "", login: i.login || "",
      created_at: i.created_at || null, expires_at: i.expires_at || null,
      kind: i.bootstrap ? "owner" : i.subject ? "attach" : "member",
    })),
  });
}));

// POST /api/team/invites {email} — a member invite. It carries no access_mode
// and no groups: the member gets the gate's floor, the API's own rule.
app.post("/api/team/invites", accountRoute(async (req, res, p) => {
  const email = typeof req.body?.email === "string" ? req.body.email.trim().toLowerCase() : "";
  if (email.length > 254 || !EMAIL_RE.test(email)) return res.status(400).json({ error: "invalid_email" });
  if (!inviteLimit.allowed(p.tenant)) return res.status(429).json({ error: "invite_limit" });
  const pending = (await tenantInvites(p.tenant)).filter((i) => i.state === "live").length;
  if (pending >= MAX_PENDING_INVITES) return res.status(429).json({ error: "too_many_pending" });
  inviteLimit.take(p.tenant);
  const up = await manage("POST", gatePath("invites"), { body: { tenant: p.tenant, email } });
  if (up.status !== 201) return sendUpstreamError(res, "invite", up);
  const link = checkedLink(up.json, "/ui/invite#");
  if (!link) { logWarn("account upstream invite: link outside APP_URL"); return res.status(502).json({ error: "link_mismatch" }); }
  res.status(201).json({ invite_id: up.json.invite_id, url: link.url, expires_at: link.expires_at });
}));

// DELETE /api/team/invites/:id — revoke one of the tenant's own invites.
app.delete("/api/team/invites/:id", accountRoute(async (req, res, p) => {
  const id = req.params.id;
  if (!INVITE_ID_RE.test(id)) return notFound(res);
  if (!(await tenantInvites(p.tenant)).some((i) => i.invite_id === id)) return notFound(res);
  const up = await manage("DELETE", gatePath("invites", id), { query: { tenant: p.tenant } });
  if (up.status !== 204) return sendUpstreamError(res, "revoke", up);
  res.status(204).end();
}));

// POST /api/team/:subject/reset — a reset link for the owner to hand over. On
// the owner's own subject it is "reset my password", which loomcycle allows
// only with the gate's manager_may_recover_owners (else 403 target_beyond_floor).
app.post("/api/team/:subject/reset", accountRoute(async (req, res, p) => {
  const u = await memberOrNull(p, req.params.subject);
  if (!u) return notFound(res);
  const up = await manage("POST", gatePath("resets"), { body: { tenant: p.tenant, subject: u.user_id } });
  if (up.status === 404 && up.json?.code === "login_not_found") return res.status(409).json({ error: "no_login" });
  if (up.status !== 201) return sendUpstreamError(res, "reset", up);
  const link = checkedLink(up.json, "/ui/reset#");
  if (!link) { logWarn("account upstream reset: link outside APP_URL"); return res.status(502).json({ error: "link_mismatch" }); }
  res.status(201).json({ url: link.url, expires_at: link.expires_at });
}));

// POST /api/team/:subject/disable | /enable — loomcycle ends a disabled user's
// sessions and retires its tokens; enabling does not re-issue them.
for (const [action, status] of [["disable", "disabled"], ["enable", "active"]]) {
  app.post(`/api/team/:subject/${action}`, accountRoute(async (req, res, p) => {
    const u = await memberOrNull(p, req.params.subject);
    if (!u) return notFound(res);
    const up = await manage("PATCH", userPath(u.user_id), { query: { tenant: p.tenant }, body: { status } });
    if (up.status !== 200) return sendUpstreamError(res, action, up);
    res.json({ subject: u.user_id, status: up.json?.status === "disabled" ? "disabled" : "active" });
  }));
}

// DELETE /api/team/:subject — remove a member: loomcycle retires its tokens,
// drops its logins and password, then the user row. Its data stays; erasure is
// an operator action.
app.delete("/api/team/:subject", accountRoute(async (req, res, p) => {
  const u = await memberOrNull(p, req.params.subject);
  if (!u) return notFound(res);
  const up = await manage("DELETE", userPath(u.user_id), { query: { tenant: p.tenant } });
  if (up.status !== 204) return sendUpstreamError(res, "remove", up);
  res.status(204).end();
}));

// Same-origin proxy for the runtime's PUBLIC reads, exposed as inner links. This
// is an explicit ALLOWLIST that forwards a HARDCODED upstream path — never a path
// derived from the request — so no percent-encoding (`%5f`) or `..` traversal can
// smuggle a different upstream target (e.g. the operator-admin plane /v1/_*). A
// blanket `/v1/*` proxy + a `/v1/_` denylist would be bypassable, because the
// guard sees one form of the path and the upstream re-normalizes another. The
// app's bearer-gated API stays on app.loomcycle.cloud, not this public apex.
async function proxyGet(req, res, upstreamPath) {
  try {
    const up = await fetch(RUNTIME_URL + upstreamPath, {
      headers: { Accept: req.get("accept") || "application/json" },
    });
    res.status(up.status);
    const ct = up.headers.get("content-type");
    if (ct) res.set("Content-Type", ct);
    res.set("Cache-Control", "no-store");
    res.send(Buffer.from(await up.arrayBuffer()));
  } catch {
    res.status(502).json({ error: "runtime unreachable" });
  }
}
app.get("/healthz", (req, res) => proxyGet(req, res, "/healthz"));
app.get("/v1/config", (req, res) => proxyGet(req, res, "/v1/config"));

// The static marketing site last (index at /, assets, range-served videos).
app.use(express.static(WEB_ROOT, { index: "index.html", extensions: ["html"] }));

// Express 5 hands a listen error (EADDRINUSE, EACCES) to this callback instead
// of throwing it, so without the check the process logs "listening" and exits 0.
app.listen(PORT, (err) => {
  if (err) {
    console.error(`FATAL: cannot listen on :${PORT}: ${err.message}`);
    process.exit(1);
  }
  console.log(`landing listening on :${PORT} → runtime ${RUNTIME_URL}` +
    (DEV_ALLOW_UNVERIFIED ? " [DEV: Access verification DISABLED]" : ` [CF Access aud=${ACCESS_AUD ? "set" : "unset"}]`) +
    (MANAGE_TOKEN ? ` [accounts: gate ${AUTH_GATE}, app ${APP_URL}]` : " [accounts: off — LOOMCYCLE_AUTH_MANAGE_TOKEN unset]"));
});
