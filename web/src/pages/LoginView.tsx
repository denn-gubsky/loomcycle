import { useState } from "react";

// LoginView is the token-entry auth page (multi-tenant UI authz). loomcycle
// authenticates by bearer token, not username/password — paste an
// operator-token (lct_… or the legacy LOOMCYCLE_AUTH_TOKEN) and the
// resolved principal's scopes decide the experience:
//   - a substrate:admin token → super-admin (sees/edits all tenants);
//   - any other token → that token's tenant workspace only.
//
// Submitting navigates to /ui?token=… — the Go handler (internal/webui)
// sets the HttpOnly loomcycle_session cookie and 302s back to /ui, so the
// token never lives in JS storage. This is also where a 401 bounces to.
export default function LoginView() {
  const [token, setToken] = useState("");

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const t = token.trim();
    if (!t) return;
    // The server's ?token= landing handler sets the cookie + redirects
    // back to /ui (stripping the token from the URL so a refresh doesn't
    // re-set it). Full navigation, not a router push — we want the Go
    // handler to run and the SPA to re-boot with the cookie present.
    window.location.href = "/ui?token=" + encodeURIComponent(t);
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
