import { useState } from "react";
import { type TotpEnrolment } from "../lib/passwordGate";

// Pieces the password gate's pages share: the authenticator enrolment block
// and the one-time display of recovery codes.

/** The secret to add to an authenticator app, and the field for its first
 *  code. The otpauth link opens an authenticator on a phone; on a computer
 *  the person types the secret into the app. */
export function TotpEnrolBlock({
  totp,
  code,
  onCode,
  disabled,
}: {
  totp: TotpEnrolment;
  code: string;
  onCode: (v: string) => void;
  disabled?: boolean;
}) {
  return (
    <fieldset className="login-fieldset">
      <legend>Authenticator app</legend>
      <p className="login-sub">
        Add this account to your authenticator app (any TOTP app): open <a href={totp.otpauth_uri}>this link</a> on
        your phone, or enter the key below by hand. Then type the 6-digit code it shows.
      </p>
      <code className="login-secret" aria-label="Authenticator key">
        {totp.secret.replace(/(.{4})/g, "$1 ").trim()}
      </code>
      <input
        className="login-input"
        inputMode="numeric"
        autoComplete="one-time-code"
        aria-label="Authenticator code"
        placeholder="123456"
        value={code}
        maxLength={6}
        onChange={(e) => onCode(e.target.value.replace(/\D/g, ""))}
        disabled={disabled}
      />
    </fieldset>
  );
}

/** Recovery codes, shown once: each signs in once in place of an
 *  authenticator code. */
export function RecoveryCodes({ codes, onDone }: { codes: string[]; onDone: () => void }) {
  const [saved, setSaved] = useState(false);
  return (
    <div>
      <h2 className="login-title">Save your recovery codes</h2>
      <p className="login-sub">
        Each code signs you in once if you lose your authenticator. They are shown only now: store them somewhere safe.
      </p>
      <ul className="login-codes">
        {codes.map((c) => (
          <li key={c}>
            <code>{c}</code>
          </li>
        ))}
      </ul>
      <label className="login-check">
        <input type="checkbox" checked={saved} onChange={(e) => setSaved(e.target.checked)} /> I have saved these codes
      </label>
      <button className="login-btn" type="button" disabled={!saved} onClick={onDone}>
        Continue
      </button>
    </div>
  );
}

/** A password field with a confirmation, checked against the policy's length
 *  before the server sees it. */
export function NewPasswordFields({
  value,
  confirm,
  onValue,
  onConfirm,
  min,
  disabled,
}: {
  value: string;
  confirm: string;
  onValue: (v: string) => void;
  onConfirm: (v: string) => void;
  min: number;
  disabled?: boolean;
}) {
  return (
    <>
      <input
        className="login-input"
        type="password"
        autoComplete="new-password"
        aria-label="New password"
        placeholder={`New password (at least ${min} characters)`}
        value={value}
        onChange={(e) => onValue(e.target.value)}
        disabled={disabled}
      />
      <input
        className="login-input"
        type="password"
        autoComplete="new-password"
        aria-label="Repeat the new password"
        placeholder="Repeat the new password"
        value={confirm}
        onChange={(e) => onConfirm(e.target.value)}
        disabled={disabled}
      />
      <p className="login-hint">
        Long is strong: a few unrelated words make a good password. Pasting from a password manager is fine.
      </p>
    </>
  );
}

/** Whether a new password is ready to send. */
export function passwordReady(value: string, confirm: string, min: number): boolean {
  return [...value].length >= min && value === confirm;
}
