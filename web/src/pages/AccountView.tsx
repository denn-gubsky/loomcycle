import { useEffect, useState } from "react";
import { NewPasswordFields, RecoveryCodes, TotpEnrolBlock, passwordReady } from "../components/PasswordGateParts";
import LinkedSignIns from "../components/LinkedSignIns";
import PasskeySection from "../components/PasskeySection";
import { type PasskeyAnswer, withPasskeyStepUp } from "../lib/passkeys";
import {
  type AccountInfo,
  type Refusal,
  type TotpEnrolment,
  accountInfo,
  changePassword,
  refusalText,
  totpBegin,
  totpConfirm,
  totpRemove,
} from "../lib/passwordGate";

// AccountView is a password-gate user's own account: change the password,
// add or replace the authenticator app, remove it where the sign-in allows,
// and add or remove passkeys. Every change asks for the current password
// again (and the second factor a sign-in would ask for: a code, or the
// passkey the browser is prompted for). Changing the password signs out
// every other browser of the account. A session from an identity provider or
// a passkey has no password: it manages its passkeys and linked sign-ins.
export default function AccountView() {
  const [info, setInfo] = useState<AccountInfo | null>(null);
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [notice, setNotice] = useState("");
  const [current, setCurrent] = useState("");
  const [otp, setOtp] = useState("");
  const [pw, setPw] = useState("");
  const [pw2, setPw2] = useState("");
  const [enrol, setEnrol] = useState<TotpEnrolment | null>(null);
  const [enrolCode, setEnrolCode] = useState("");
  const [codes, setCodes] = useState<string[] | null>(null);
  const [busy, setBusy] = useState(false);

  const load = () =>
    accountInfo().then((r) => {
      if (r.ok) setInfo(r.data);
      else setRefusal(r.refusal);
    });
  useEffect(() => {
    load();
  }, []);

  if (codes) {
    return (
      <div className="page">
        <RecoveryCodes
          codes={codes}
          onDone={() => {
            setCodes(null);
            load();
          }}
        />
      </div>
    );
  }
  if (!info) {
    // A session from an identity provider has no password to manage; its
    // sign-in methods are still the person's own.
    return (
      <div className="page">
        <h1>Your account</h1>
        <LinkedSignIns />
        {refusal && <PasskeySection stepUp={() => ({})} />}
        {refusal ? null : <p>Loading…</p>}
      </div>
    );
  }

  const min = info.has_totp ? 8 : 15;
  const run = async (f: (webauthn?: PasskeyAnswer) => Promise<{ ok: boolean; refusal?: Refusal }>, ok: string) => {
    setBusy(true);
    setRefusal(null);
    setNotice("");
    const r = (await withPasskeyStepUp(f as never)) as { ok: true } | { ok: false; refusal: Refusal };
    setBusy(false);
    if (r.ok) {
      setNotice(ok);
      setCurrent("");
      setOtp("");
      setPw("");
      setPw2("");
      load();
    } else {
      setRefusal(r.refusal);
    }
  };

  return (
    <div className="page">
      <h1>Your account</h1>
      <p>
        Signed in as <strong>{info.login}</strong> through <code>{info.gate}</code>.
      </p>
      {refusal && (
        <p className="login-error" role="alert">
          {refusalText(refusal)}
        </p>
      )}
      {notice && <p role="status">{notice}</p>}

      <fieldset className="login-fieldset">
        <legend>Confirm it is you</legend>
        <input
          className="login-input"
          type="password"
          autoComplete="current-password"
          aria-label="Current password"
          placeholder="Current password"
          value={current}
          onChange={(e) => setCurrent(e.target.value)}
        />
        {info.has_totp && (
          <input
            className="login-input"
            autoComplete="one-time-code"
            aria-label="Authenticator or recovery code"
            placeholder="Code from your authenticator app"
            value={otp}
            onChange={(e) => setOtp(e.target.value)}
          />
        )}
      </fieldset>

      <section>
        <h2>Password</h2>
        <NewPasswordFields value={pw} confirm={pw2} onValue={setPw} onConfirm={setPw2} min={min} disabled={busy} />
        <button
          className="login-btn"
          type="button"
          disabled={busy || current === "" || !passwordReady(pw, pw2, min)}
          onClick={() =>
            run((webauthn) => changePassword(current, pw, otp, fetch, webauthn), "Password changed. Your other browsers are signed out.")
          }
        >
          Change password
        </button>
      </section>

      <section>
        <h2>Authenticator app</h2>
        <p>
          {info.has_totp
            ? `Set up. ${info.recovery_codes_left} recovery code(s) left.`
            : "Not set up. An authenticator protects the account if the password leaks."}
        </p>
        {!enrol && (
          <button
            className="login-btn"
            type="button"
            disabled={busy}
            onClick={async () => {
              const r = await totpBegin();
              if (r.ok) setEnrol(r.data);
              else setRefusal(r.refusal);
            }}
          >
            {info.has_totp ? "Replace authenticator" : "Set up authenticator"}
          </button>
        )}
        {enrol && (
          <>
            <TotpEnrolBlock totp={enrol} code={enrolCode} onCode={setEnrolCode} disabled={busy} />
            <button
              className="login-btn"
              type="button"
              disabled={busy || current === "" || enrolCode.length !== 6}
              onClick={async () => {
                setBusy(true);
                setRefusal(null);
                const r = await withPasskeyStepUp((webauthn) =>
                  totpConfirm({ current_password: current, otp, enrolment: enrol.enrolment, code: enrolCode, webauthn }),
                );
                setBusy(false);
                if (r.ok) {
                  setEnrol(null);
                  setEnrolCode("");
                  setCurrent("");
                  setOtp("");
                  setCodes(r.data.recovery_codes);
                } else {
                  setRefusal(r.refusal);
                }
              }}
            >
              Confirm authenticator
            </button>
          </>
        )}
        {info.has_totp && !info.require_mfa && (
          <button
            className="login-btn"
            type="button"
            disabled={busy || current === ""}
            onClick={() => run((webauthn) => totpRemove(current, otp, fetch, webauthn), "Authenticator removed.")}
          >
            Remove authenticator
          </button>
        )}
      </section>

      <PasskeySection stepUp={() => ({ current_password: current, otp })} onChanged={load} />
    </div>
  );
}
