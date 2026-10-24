import { useEffect, useState } from "react";
import { type LoginConfig, type LoginGate, parseLoginConfig } from "../lib/uiSession";

// LoginView is the sign-in page. It renders from /ui/login-config.json: the
// deployment's mode and its gates (name, kind, label) — nothing else about the
// server is known before sign-in. Each gate kind has its own form; P0 has one,
// `token`, the paste box (BaseAuthGate). A gate kind this page does not know is
// listed but not offered.
//
// Signing in creates a server-side session: the browser gets an HttpOnly
// session cookie and never keeps the token. The token travels once, in an
// Authorization header to POST /ui/session — never in a URL, history, Referer
// or an access log.
//
// The postMessage handoff: a configured first-party origin
// (LOOMCYCLE_UI_LOGIN_ORIGINS — e.g. the loomcycle.cloud landing that mints
// tenant tokens) may hand a bearer over an origin-pinned message; the receiver
// exchanges it for a session the same way. The server lists origins only in
// token mode, so in strict mode the receiver stays off.

// signInWithToken exchanges a bearer for a session. Returns "" on success, or
// the server's refusal code.
async function signInWithToken(token: string): Promise<string> {
  const r = await fetch("/ui/session", {
    method: "POST",
    credentials: "same-origin",
    headers: { Authorization: "Bearer " + token },
  });
  if (r.status === 204) return "";
  try {
    const body = (await r.json()) as { code?: string };
    return body.code || "refused";
  } catch {
    return "refused";
  }
}

const refusalText: Record<string, string> = {
  invalid_credentials: "That token was not accepted — check it and try again.",
  insecure_transport:
    "This server only signs you in over HTTPS. Open it through its https:// address, or ask the operator to enable sign-in over plain HTTP for this network.",
  gate_unavailable: "This server does not accept a pasted token.",
  cross_site_login_refused: "Sign-in must start from this page.",
};

function refusalMessage(code: string): string {
  return refusalText[code] ?? "Sign-in failed — try again.";
}

function TokenGateForm({ gate, disabled }: { gate: LoginGate; disabled: boolean }) {
  const [token, setToken] = useState("");
  const [error, setError] = useState("");
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    const t = token.trim();
    if (!t) return;
    setError("");
    const refused = await signInWithToken(t);
    if (refused === "") {
      window.location.href = "/ui";
    } else {
      setError(refusalMessage(refused));
    }
  };
  return (
    <form onSubmit={submit}>
      <p className="login-sub">
        Paste your access token. A <code>substrate:admin</code> token signs you in as super-admin (all tenants); any
        other token opens just its own tenant&rsquo;s workspace.
      </p>
      <input
        className="login-input"
        type="password"
        aria-label={gate.label}
        value={token}
        onChange={(e) => setToken(e.target.value)}
        placeholder="lct_…  (or the legacy LOOMCYCLE_AUTH_TOKEN)"
        autoFocus
        spellCheck={false}
        autoComplete="off"
        disabled={disabled}
      />
      {error && (
        <p className="login-error" role="alert">
          {error}
        </p>
      )}
      <button className="login-btn" type="submit" disabled={disabled || token.trim() === ""}>
        Sign in
      </button>
      <p className="login-hint">
        Tokens are minted with <code>loomcycle operator-token create</code>. Lost tokens are rotated, not recovered.
      </p>
    </form>
  );
}

export default function LoginView() {
  const [config, setConfig] = useState<LoginConfig | null>(null);

  useEffect(() => {
    let live = true;
    fetch("/ui/login-config.json", { credentials: "same-origin" })
      .then((r) => (r.ok ? r.json() : null))
      .then((raw) => {
        if (live) setConfig(parseLoginConfig(raw));
      })
      .catch(() => {
        if (live) setConfig(parseLoginConfig(null));
      });
    return () => {
      live = false;
    };
  }, []);

  // postMessage login handoff: accept a bearer from an ALLOWED first-party
  // origin and exchange it for a session. The origin pin is the login-CSRF
  // guard — an unlisted page's message is ignored, so no site can force this
  // browser to adopt an attacker's token.
  useEffect(() => {
    const allowed = config?.origins ?? [];
    if (allowed.length === 0) return;
    const onMessage = async (e: MessageEvent) => {
      if (!allowed.includes(e.origin)) return;
      const d = e.data as { type?: string; token?: unknown } | null;
      if (!d || d.type !== "loomcycle.login" || typeof d.token !== "string") return;
      if ((await signInWithToken(d.token)) === "") {
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
  }, [config]);

  const tokenGate = config?.gates.find((g) => g.kind === "token");
  const blocked = config?.sessionRefused ?? "";

  return (
    <div className="login-page">
      <div className="login-card">
        <div className="login-brand">loomcycle</div>
        <h1 className="login-title">Sign in</h1>
        {blocked && (
          <p className="login-error" role="alert">
            {refusalMessage(blocked)}
          </p>
        )}
        {config === null && <p className="login-sub">Loading…</p>}
        {tokenGate && <TokenGateForm gate={tokenGate} disabled={blocked !== ""} />}
        {config !== null && !tokenGate && (
          <p className="login-sub">
            {config.gates.length === 0
              ? "This server offers no way to sign in to the Web UI."
              : "This page cannot offer this server's sign-in methods: " +
                config.gates.map((g) => g.label).join(", ") +
                "."}
          </p>
        )}
      </div>
    </div>
  );
}
