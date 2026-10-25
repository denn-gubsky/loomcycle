import { useEffect, useState } from "react";
import { type LoginConfig, type LoginGate, parseLoginConfig } from "../lib/uiSession";
import { refusalText as passwordRefusalText, signInWithPassword } from "../lib/passwordGate";
import { REDIRECT_GATE_KINDS, redirectLoginError, redirectLoginHref } from "../lib/oidcGate";
import { signedInDestination } from "../lib/returnPath";

// LoginView is the sign-in page. It renders from /ui/login-config.json: the
// deployment's mode and its gates (name, kind, label) — nothing else about the
// server is known before sign-in. Each gate kind has its own form: `token`, the
// paste box (BaseAuthGate), and `password`, a login and password with an
// authenticator code when the account has one (NaiveAuthGate), and `oidc`, a
// button that starts a sign-in at the identity provider (a top-level
// navigation; the provider sends the browser back to the server). A gate kind
// this page does not know is listed but not offered.
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
      window.location.href = signedInDestination(window.location.search);
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

// PasswordGateForm signs in with a login and a password. The login names the
// account on its own (it is unique per gate), so no tenant is asked. When the
// account has an authenticator the server asks for its code after the
// password is accepted, and the form shows that field.
function PasswordGateForm({ gate, disabled }: { gate: LoginGate; disabled: boolean }) {
  const [login, setLogin] = useState("");
  const [pw, setPw] = useState("");
  const [otp, setOtp] = useState("");
  const [needOtp, setNeedOtp] = useState(false);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (login.trim() === "" || pw === "") return;
    setBusy(true);
    setError("");
    const r = await signInWithPassword(gate.name, login, pw, otp);
    setBusy(false);
    if (r.ok) {
      window.location.href = signedInDestination(window.location.search);
      return;
    }
    if (r.refusal.code === "otp_required") {
      setNeedOtp(true);
      if (otp === "") return;
    }
    setError(passwordRefusalText(r.refusal));
  };
  return (
    <form onSubmit={submit} aria-label={gate.label}>
      <h2 className="login-gate">{gate.label}</h2>
      <input
        className="login-input"
        aria-label="Login"
        placeholder="Login"
        autoComplete="username"
        value={login}
        onChange={(e) => setLogin(e.target.value)}
        disabled={disabled || busy}
      />
      <input
        className="login-input"
        type="password"
        aria-label="Password"
        placeholder="Password"
        autoComplete="current-password"
        value={pw}
        onChange={(e) => setPw(e.target.value)}
        disabled={disabled || busy}
      />
      {needOtp && (
        <input
          className="login-input"
          aria-label="Authenticator or recovery code"
          placeholder="Code from your authenticator app, or a recovery code"
          autoComplete="one-time-code"
          value={otp}
          onChange={(e) => setOtp(e.target.value)}
          autoFocus
          disabled={disabled || busy}
        />
      )}
      {error && (
        <p className="login-error" role="alert">
          {error}
        </p>
      )}
      <button className="login-btn" type="submit" disabled={disabled || busy || login.trim() === "" || pw === ""}>
        Sign in
      </button>
      <p className="login-hint">Forgot your password? Ask your administrator for a reset link.</p>
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
        window.location.href = signedInDestination(window.location.search);
      }
    };
    window.addEventListener("message", onMessage);
    return () => window.removeEventListener("message", onMessage);
  }, [config]);

  const tokenGate = config?.gates.find((g) => g.kind === "token");
  const passwordGates = config?.gates.filter((g) => g.kind === "password") ?? [];
  const redirectGates = config?.gates.filter((g) => REDIRECT_GATE_KINDS.includes(g.kind)) ?? [];
  const blocked = config?.sessionRefused ?? "";
  const returned = redirectLoginError(new URLSearchParams(window.location.search).get("error") ?? "");

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
        {returned && !blocked && (
          <p className="login-error" role="alert">
            {returned}
          </p>
        )}
        {redirectGates.map((g) =>
          blocked ? (
            <button key={g.name} className="login-btn" type="button" disabled>
              {g.label}
            </button>
          ) : (
            <a key={g.name} className="login-btn" href={redirectLoginHref(g.name)}>
              {g.label}
            </a>
          ),
        )}
        {passwordGates.map((g) => (
          <PasswordGateForm key={g.name} gate={g} disabled={blocked !== ""} />
        ))}
        {tokenGate && <TokenGateForm gate={tokenGate} disabled={blocked !== ""} />}
        {config !== null && !tokenGate && passwordGates.length === 0 && redirectGates.length === 0 && (
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
