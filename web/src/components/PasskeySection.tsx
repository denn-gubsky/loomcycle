import { useEffect, useState } from "react";
import { type Refusal, refusalText } from "../lib/passwordGate";
import {
  type PasskeyInfo,
  type StepUp,
  createPasskey,
  listPasskeys,
  passkeysSupported,
  registerBegin,
  registerFinish,
  removePasskey,
  withPasskeyStepUp,
} from "../lib/passkeys";

// PasskeySection lists the signed-in person's passkeys and adds or removes
// one (RFC FA P6). Each change re-authenticates in its own request: on a
// password account the current password (and the code or passkey a sign-in
// would ask for) — the parent's "confirm it is you" fields, handed in as
// stepUp; on a passkey account the browser prompts for one of its passkeys;
// on an identity-provider account the sign-in itself must be recent.
export default function PasskeySection({ stepUp, onChanged }: { stepUp: () => StepUp; onChanged?: () => void }) {
  const [keys, setKeys] = useState<PasskeyInfo[] | null>(null);
  const [available, setAvailable] = useState(true);
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [notice, setNotice] = useState("");

  const load = () =>
    listPasskeys().then((r) => {
      if (r.ok) {
        setKeys(r.data.passkeys);
        setAvailable(r.data.available);
      } else setRefusal(r.refusal);
    });
  useEffect(() => {
    load();
  }, []);

  const add = async () => {
    setBusy(true);
    setRefusal(null);
    setNotice("");
    const begin = await withPasskeyStepUp((webauthn) => registerBegin({ ...stepUp(), webauthn }));
    if (!begin.ok) {
      setBusy(false);
      setRefusal(begin.refusal);
      return;
    }
    let answer = null;
    try {
      answer = await createPasskey(begin.data);
    } catch {
      answer = null;
    }
    if (!answer) {
      setBusy(false);
      setRefusal({ code: "passkey_cancelled" });
      return;
    }
    const r = await registerFinish({ ...answer, name: name.trim() || "Passkey" });
    setBusy(false);
    if (!r.ok) {
      setRefusal(r.refusal);
      return;
    }
    setName("");
    setNotice("Passkey added.");
    load();
    onChanged?.();
  };

  const remove = async (id: string) => {
    setBusy(true);
    setRefusal(null);
    setNotice("");
    const r = await withPasskeyStepUp((webauthn) => removePasskey(id, { ...stepUp(), webauthn }));
    setBusy(false);
    if (r.ok) {
      setNotice("Passkey removed.");
      load();
      onChanged?.();
    } else setRefusal(r.refusal);
  };

  if (!passkeysSupported()) return null;
  return (
    <section>
      <h2>Passkeys</h2>
      {!available && <p>Passkeys are not available on this server.</p>}
      {refusal && (
        <p className="login-error" role="alert">
          {refusalText(refusal)}
        </p>
      )}
      {notice && <p role="status">{notice}</p>}
      {keys && keys.length === 0 && (
        <p>None yet. A passkey is a second factor for your password, and signs you in without one where the server offers a passkey sign-in.</p>
      )}
      {keys && keys.length > 0 && (
        <ul>
          {keys.map((k) => (
            <li key={k.id}>
              <strong>{k.name}</strong> — added {new Date(k.created_at).toLocaleDateString()}
              {k.last_used_at ? `, last used ${new Date(k.last_used_at).toLocaleDateString()}` : ""}
              {k.flagged && " — refused: its signature counter went backwards (a possible copy). Remove it."}{" "}
              <button className="login-btn" type="button" disabled={busy} onClick={() => remove(k.id)}>
                Remove
              </button>
            </li>
          ))}
        </ul>
      )}
      {available && (
        <>
          <input
            className="login-input"
            aria-label="Passkey name"
            placeholder="Name it (e.g. laptop, security key)"
            value={name}
            onChange={(e) => setName(e.target.value)}
            disabled={busy}
          />
          <button className="login-btn" type="button" disabled={busy} onClick={add}>
            Add a passkey
          </button>
        </>
      )}
    </section>
  );
}
