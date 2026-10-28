import { useEffect, useState } from "react";
import { NewPasswordFields, RecoveryCodes, TotpEnrolBlock, passwordReady } from "../components/PasswordGateParts";
import {
  type InviteInfo,
  type Refusal,
  codeFromHash,
  forgetHash,
  inviteInfo,
  minLength,
  redeemInvite,
  refusalText,
} from "../lib/passwordGate";
import { withPasskeyStepUp } from "../lib/passkeys";

// InviteView is loomcycle's own page for an invitation link
// (https://<server>/ui/invite#lci_…): the person chooses a login (or takes the
// one the invitation names), sets a password and, when the sign-in requires
// it, adds an authenticator. The code is read from the fragment and sent in a
// POST body; the address bar forgets it straight away.
export default function InviteView() {
  const [code] = useState(() => codeFromHash(window.location.hash));
  const [info, setInfo] = useState<InviteInfo | null>(null);
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [login, setLogin] = useState("");
  const [pw, setPw] = useState("");
  const [pw2, setPw2] = useState("");
  const [addTotp, setAddTotp] = useState(false);
  const [totpCode, setTotpCode] = useState("");
  const [otp, setOtp] = useState("");
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState<{ redirect: string; codes: string[] } | null>(null);

  useEffect(() => {
    forgetHash();
    if (!code) {
      setRefusal({ code: "link_invalid" });
      return;
    }
    inviteInfo(code).then((r) => {
      if (r.ok) {
        setInfo(r.data);
        setLogin(r.data.login || r.data.email || "");
        setAddTotp(r.data.require_mfa);
      } else {
        setRefusal(r.refusal);
      }
    });
  }, [code]);

  if (done) {
    const go = () => window.location.assign(done.redirect);
    return (
      <div className="login-page">
        <div className="login-card">
          {done.codes.length > 0 ? <RecoveryCodes codes={done.codes} onDone={go} /> : null}
          {done.codes.length === 0 && (
            <>
              <h1 className="login-title">You are signed in</h1>
              <button className="login-btn" type="button" onClick={go}>
                Continue
              </button>
            </>
          )}
        </div>
      </div>
    );
  }

  const withMFA = addTotp && !!info?.totp;
  const min = info ? minLength(info, withMFA) : 15;
  const ready = !!info && login.trim() !== "" && passwordReady(pw, pw2, min) && (!withMFA || totpCode.length === 6);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!info || !ready) return;
    setBusy(true);
    setRefusal(null);
    // An invite to an existing passkey account confirms that passkey.
    const r = await withPasskeyStepUp((webauthn) =>
      redeemInvite({
        code,
        login,
        password: pw,
        webauthn,
        otp: info.has_totp && !webauthn ? otp : undefined,
        totp_enrolment: withMFA ? info.totp?.enrolment : undefined,
        totp_code: withMFA ? totpCode : undefined,
      }),
    );
    setBusy(false);
    if (r.ok) setDone({ redirect: r.data.redirect || "/ui/", codes: r.data.recovery_codes ?? [] });
    else setRefusal(r.refusal);
  };

  return (
    <div className="login-page">
      <form className="login-card" onSubmit={submit}>
        <div className="login-brand">loomcycle</div>
        <h1 className="login-title">Create your account</h1>
        {refusal && (
          <p className="login-error" role="alert">
            {refusalText(refusal)}
          </p>
        )}
        {info === null && !refusal && <p className="login-sub">Loading…</p>}
        {info && (
          <>
            <input
              className="login-input"
              aria-label="Login"
              placeholder="Login (your e-mail, or a name)"
              autoComplete="username"
              value={login}
              readOnly={!!info.login}
              onChange={(e) => setLogin(e.target.value)}
              disabled={busy}
            />
            <NewPasswordFields value={pw} confirm={pw2} onValue={setPw} onConfirm={setPw2} min={min} disabled={busy} />
            {info.has_passkey && <p className="login-sub">You will confirm with your passkey.</p>}
            {info.has_totp && (
              <input
                className="login-input"
                autoComplete="one-time-code"
                aria-label="Authenticator or recovery code"
                placeholder="Code from your authenticator app, or a recovery code"
                value={otp}
                onChange={(e) => setOtp(e.target.value)}
                disabled={busy}
              />
            )}
            {!info.require_mfa && info.totp && (
              <label className="login-check">
                <input type="checkbox" checked={addTotp} onChange={(e) => setAddTotp(e.target.checked)} /> Also add an
                authenticator app (a shorter password is then allowed)
              </label>
            )}
            {withMFA && info.totp && <TotpEnrolBlock totp={info.totp} code={totpCode} onCode={setTotpCode} disabled={busy} />}
            <button className="login-btn" type="submit" disabled={busy || !ready}>
              Create account and sign in
            </button>
          </>
        )}
      </form>
    </div>
  );
}
