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

import express from "express";
import { createRemoteJWKSet, jwtVerify } from "jose";
import { createHash } from "node:crypto";

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

// Fail fast on misconfiguration.
if (!ADMIN_TOKEN) {
  console.error("FATAL: LOOMCYCLE_ADMIN_TOKEN is required");
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
  res.json({ authenticated: true, email: id.email, tenant: p.tenant, subject: p.subject });
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

app.listen(PORT, () => {
  console.log(`landing listening on :${PORT} → runtime ${RUNTIME_URL}` +
    (DEV_ALLOW_UNVERIFIED ? " [DEV: Access verification DISABLED]" : ` [CF Access aud=${ACCESS_AUD ? "set" : "unset"}]`));
});
