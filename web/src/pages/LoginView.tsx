import { useEffect, useState } from "react";

// LoginView is the token-entry auth page (multi-tenant UI authz). loomcycle
// authenticates by bearer token, not username/password — paste an
// operator-token (lct_… or the legacy LOOMCYCLE_AUTH_TOKEN) and the
// resolved principal's scopes decide the experience:
//   - a substrate:admin token → super-admin (sees/edits all tenants);
//   - any other token → that token's tenant workspace only.
//
// Two URL-free ways in, both via POST /ui/session (the bearer travels in an
// Authorization header, never a query string, so it stays out of history,
// Referer, and access logs). The server sets the HttpOnly loomcycle_session
// cookie; we then reload /ui so the SPA re-boots with the cookie present.
//   1. Paste a token into the form below.
//   2. A configured first-party origin (LOOMCYCLE_UI_LOGIN_ORIGINS — e.g. the
//      loomcycle.cloud landing that mints tenant tokens) hands a bearer over an
//      origin-pinned postMessage; the receiver below exchanges it for the cookie.
// GET /ui?token= still works (bookmarks / the CLI hint) but keeps the token in
// the URL, so it is not used here.

// establishSession swaps a bearer for the HttpOnly session cookie without ever
// putting it in a URL. Returns true on success (204).
async function establishSession(token: string): Promise<boolean> {
  const r = await fetch("/ui/session", {
    method: "POST",
    credentials: "same-origin",
    headers: { Authorization: "Bearer " + token },
  });
  return r.status === 204;
}

export default function LoginView() {
  const [token, setToken] = useState("");
  const [error, setError] = useState("");

  // postMessage login handoff: accept a bearer from an ALLOWED first-party
  // origin and exchange it for the session cookie. The origin pin is the
  // login-CSRF guard — an unlisted page's message is ignored, so no site can
  // force this browser to adopt an attacker's token.
  useEffect(() => {
    let allowed: string[] = [];
    fetch("/ui/login-config.json", { credentials: "same-origin" })
      .then((r) => (r.ok ? r.json() : null))
      .then((c) => {
        if (c && Array.isArray(c.login_origins)) allowed = c.login_origins;
      })
      .catch(() => {});
    const onMessage = async (e: MessageEvent) => {
      if (allowed.length === 0 || !allowed.includes(e.origin)) return;
      const d = e.data as { type?: string; token?: unknown } | null;
      if (!d || d.type !== "loomcycle.login" || typeof d.token !== "string") return;
      if (await establishSession(d.token)) {
        // ack the opener so it can stop retrying / avoid the URL fallback.
        try {
          (e.source as Window | null)?.postMessage({ type: "loomcycle.login.ok" }, e.origin);
        } catch {
          /* opener gone — harmless */
        }
        window.location.href = "/ui";
      }
    };
    window.addEventListener("message", onMessage);
    return () => window.removeEventListener("message", onMessage);
  }, []);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    const t = token.trim();
    if (!t) return;
    setError("");
    if (await establishSession(t)) {
      window.location.href = "/ui";
    } else {
      setError("That token was not accepted — check it and try again.");
    }
  };

  return (
    <div className="login-page">
      <form className="login-card" onSubmit={submit}>
        <div className="login-brand">loomcycle</div>
        <h1 className="login-title">Sign in</h1>
        <p className="login-sub">
          Paste your access token. A <code>substrate:admin</code> token signs
          you in as super-admin (all tenants); any other token opens just its
          own tenant&rsquo;s workspace.
        </p>
        <input
          className="login-input"
          type="password"
          value={token}
          onChange={(e) => setToken(e.target.value)}
          placeholder="lct_…  (or the legacy LOOMCYCLE_AUTH_TOKEN)"
          autoFocus
          spellCheck={false}
          autoComplete="off"
        />
        {error && <p className="login-error" role="alert">{error}</p>}
        <button className="login-btn" type="submit" disabled={token.trim() === ""}>
          Sign in
        </button>
        <p className="login-hint">
          Tokens are minted with <code>loomcycle operator-token create</code>.
          Lost tokens are rotated, not recovered.
        </p>
      </form>
    </div>
  );
}
