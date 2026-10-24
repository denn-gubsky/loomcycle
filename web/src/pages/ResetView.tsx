import { useEffect, useState } from "react";
import { NewPasswordFields, RecoveryCodes, TotpEnrolBlock, passwordReady } from "../components/PasswordGateParts";
import {
  type Refusal,
  type ResetInfo,
  codeFromHash,
  completeReset,
  forgetHash,
  minLength,
  refusalText,
  resetInfo,
} from "../lib/passwordGate";

// ResetView is loomcycle's page for a password reset link
// (https://<server>/ui/reset#lcr_…). An account with an authenticator also
// gives a code from it (or a recovery code): the link alone does not pass the
// second factor. Completing it signs this browser in and every other session
// of the account out.
export default function ResetView() {
  const [code] = useState(() => codeFromHash(window.location.hash));
  const [info, setInfo] = useState<ResetInfo | null>(null);
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [pw, setPw] = useState("");
  const [pw2, setPw2] = useState("");
  const [otp, setOtp] = useState("");
  const [addTotp, setAddTotp] = useState(false);
  const [totpCode, setTotpCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState<{ redirect: string; codes: string[] } | null>(null);

  useEffect(() => {
    forgetHash();
    if (!code) {
      setRefusal({ code: "link_invalid" });
      return;
    }
    resetInfo(code).then((r) => {
      if (r.ok) {
        setInfo(r.data);
        setAddTotp(r.data.require_mfa && !r.data.has_totp);
      } else {
        setRefusal(r.refusal);
      }
    });
  }, [code]);

  if (done) {
    return (
      <div className="login-page">
        <div className="login-card">
          <RecoveryCodes codes={done.codes} onDone={() => window.location.assign(done.redirect)} />
        </div>
      </div>
    );
  }

  const enrolling = addTotp && !!info?.totp;
  const withMFA = !!info?.has_totp || enrolling;
  const min = info ? minLength(info, withMFA) : 15;
  const ready =
    !!info &&
    passwordReady(pw, pw2, min) &&
    (!info.has_totp || otp.trim() !== "") &&
    (!enrolling || totpCode.length === 6);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!info || !ready) return;
    setBusy(true);
    setRefusal(null);
    const r = await completeReset({
      code,
      password: pw,
      otp: info.has_totp ? otp : undefined,
      totp_enrolment: enrolling ? info.totp?.enrolment : undefined,
      totp_code: enrolling ? totpCode : undefined,
    });
    setBusy(false);
    if (!r.ok) {
      setRefusal(r.refusal);
      return;
    }
    const codes = r.data.recovery_codes ?? [];
    if (codes.length === 0) window.location.assign(r.data.redirect || "/ui/");
    else setDone({ redirect: r.data.redirect || "/ui/", codes });
  };

  return (
    <div className="login-page">
      <form className="login-card" onSubmit={submit}>
        <div className="login-brand">loomcycle</div>
        <h1 className="login-title">Set a new password</h1>
        {refusal && (
          <p className="login-error" role="alert">
            {refusalText(refusal)}
          </p>
        )}
        {info === null && !refusal && <p className="login-sub">Loading…</p>}
        {info && (
          <>
            {info.login && (
              <p className="login-sub">
                For <strong>{info.login}</strong>
              </p>
            )}
            <NewPasswordFields value={pw} confirm={pw2} onValue={setPw} onConfirm={setPw2} min={min} disabled={busy} />
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
            {!info.has_totp && !info.require_mfa && info.totp && (
              <label className="login-check">
                <input type="checkbox" checked={addTotp} onChange={(e) => setAddTotp(e.target.checked)} /> Also add an
                authenticator app
              </label>
            )}
            {enrolling && info.totp && (
              <TotpEnrolBlock totp={info.totp} code={totpCode} onCode={setTotpCode} disabled={busy} />
            )}
            <button className="login-btn" type="submit" disabled={busy || !ready}>
              Set password and sign in
            </button>
          </>
        )}
      </form>
    </div>
  );
}
