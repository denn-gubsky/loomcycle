import { useEffect, useState } from "react";
import { forgetHash } from "../lib/passwordGate";
import { type DeviceRequest, decideDevice, deviceInfo, deviceRefusalText, userCodeFromHash } from "../lib/deviceLogin";

// DeviceView is /ui/device (RFC FA P5, RFC 8628): where a person approves a
// device login a CLI or the MCP thin client started. It needs a Web UI session
// from any gate; a signed-out visitor is sent to the login page and brought
// back. Typing the code shows what is asking — the client's name, the scopes,
// the address the request came from — and nothing is approved until the
// person presses Approve. The token is issued to the person signed in here.

const pendingCodeKey = "lc_device_code";

function rememberCode(code: string) {
  try {
    if (code) window.sessionStorage.setItem(pendingCodeKey, code);
  } catch {
    /* storage may be unavailable; the person retypes the code */
  }
}

function takeRememberedCode(): string {
  try {
    const c = window.sessionStorage.getItem(pendingCodeKey) ?? "";
    window.sessionStorage.removeItem(pendingCodeKey);
    return c;
  } catch {
    return "";
  }
}

export default function DeviceView() {
  const [code, setCode] = useState("");
  const [signedIn, setSignedIn] = useState<boolean | null>(null);
  const [req, setReq] = useState<DeviceRequest | null>(null);
  const [refusal, setRefusal] = useState("");
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState<"approved" | "denied" | null>(null);

  useEffect(() => {
    const fromLink = userCodeFromHash(window.location.hash);
    forgetHash();
    const initial = fromLink || takeRememberedCode();
    setCode(initial);
    fetch("/v1/_me", { credentials: "same-origin" })
      .then((r) => {
        if (r.status === 401) {
          rememberCode(initial);
          setSignedIn(false);
        } else {
          setSignedIn(r.ok);
        }
      })
      .catch(() => setSignedIn(false));
  }, []);

  const lookup = async (e: React.FormEvent) => {
    e.preventDefault();
    if (code.trim() === "") return;
    setBusy(true);
    setRefusal("");
    const r = await deviceInfo(code);
    setBusy(false);
    if (r.ok) setReq(r.data);
    else setRefusal(r.code);
  };

  const decide = async (approve: boolean) => {
    setBusy(true);
    setRefusal("");
    const r = await decideDevice(code, approve);
    setBusy(false);
    if (r.ok) setDone(approve ? "approved" : "denied");
    else setRefusal(r.code);
  };

  let body: React.ReactNode;
  if (signedIn === null) {
    body = <p className="login-sub">Checking your session…</p>;
  } else if (!signedIn) {
    body = (
      <>
        <p className="login-sub">Sign in first. You will come back here to approve the device.</p>
        <a className="login-btn" href="/ui/login?next=/ui/device">
          Sign in
        </a>
      </>
    );
  } else if (done) {
    body = (
      <p className="login-sub">
        {done === "approved"
          ? "Approved. You can return to your terminal; it signs in within a few seconds."
          : "Denied. The device was not signed in."}
      </p>
    );
  } else if (req) {
    body = (
      <>
        <p className="login-sub">
          <strong>{req.client_id}</strong> is asking to sign in as <strong>{req.subject}</strong>
          {req.tenant_id ? ` (tenant ${req.tenant_id})` : ""}.
        </p>
        <dl className="login-details">
          <dt>Requested from</dt>
          <dd>{req.source_addr || "unknown address"}</dd>
          <dt>Started</dt>
          <dd>{new Date(req.created_at).toLocaleString()}</dd>
          <dt>Access</dt>
          <dd>{req.scopes.length ? req.scopes.join(", ") : "your own"}</dd>
        </dl>
        <p className="login-hint">Approve only if you started this login yourself, just now.</p>
        <button className="login-btn" type="button" disabled={busy} onClick={() => decide(true)}>
          Approve
        </button>
        <button className="login-btn secondary" type="button" disabled={busy} onClick={() => decide(false)}>
          Deny
        </button>
      </>
    );
  } else {
    body = (
      <form onSubmit={lookup}>
        <p className="login-sub">Enter the code your device shows.</p>
        <input
          className="login-input"
          value={code}
          onChange={(e) => setCode(e.target.value)}
          placeholder="BCDF-GHJK"
          autoComplete="off"
          autoCapitalize="characters"
          spellCheck={false}
          maxLength={16}
          aria-label="Device code"
        />
        <button className="login-btn" type="submit" disabled={busy || code.trim() === ""}>
          Continue
        </button>
      </form>
    );
  }

  return (
    <div className="login-page">
      <div className="login-card">
        <div className="login-brand">loomcycle</div>
        <h1 className="login-title">Sign in a device</h1>
        {refusal && (
          <p className="login-error" role="alert">
            {deviceRefusalText(refusal)}
          </p>
        )}
        {body}
      </div>
    </div>
  );
}
