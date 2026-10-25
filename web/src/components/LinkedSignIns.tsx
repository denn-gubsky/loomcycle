import { useEffect, useState } from "react";
import { type LoginGate, parseLoginConfig } from "../lib/uiSession";
import { REDIRECT_GATE_KINDS, redirectLoginHref } from "../lib/oidcGate";
import { type UserIdentity, linkErrorMessage, listIdentities, startLink, unlink } from "../lib/identityLinks";

interface Me {
  subject: string;
  gate: string;
}

// LinkedSignIns is the account page's "Sign-in methods" panel (RFC FA D14):
// the identities the signed-in user can sign in with, a way to link another
// identity provider account, and to unlink one. Linking needs a recent
// sign-in; when the server says so the panel offers "sign in again" through
// the gate in use, then the person clicks Link once more.
export default function LinkedSignIns() {
  const [me, setMe] = useState<Me | null>(null);
  const [gates, setGates] = useState<LoginGate[]>([]);
  const [ids, setIds] = useState<UserIdentity[]>([]);
  const [error, setError] = useState("");
  const [needReauth, setNeedReauth] = useState(false);
  const params = new URLSearchParams(window.location.search);
  const linked = params.get("linked") ?? "";
  const linkError = params.get("link_error") ?? "";

  const load = async (subject: string) => {
    const r = await listIdentities(subject);
    if (r.ok) setIds(r.data.identities ?? []);
  };
  useEffect(() => {
    fetch("/v1/_me", { credentials: "same-origin" })
      .then((r) => (r.ok ? r.json() : null))
      .then((p: { subject?: string; auth?: { gate?: string } } | null) => {
        if (!p?.subject) return;
        setMe({ subject: p.subject, gate: p.auth?.gate ?? "" });
        void load(p.subject);
      })
      .catch(() => {});
    fetch("/ui/login-config.json", { credentials: "same-origin" })
      .then((r) => (r.ok ? r.json() : null))
      .then((raw) => setGates(parseLoginConfig(raw).gates))
      .catch(() => {});
  }, []);

  if (!me) return null;
  const linkable = gates.filter(
    (g) => REDIRECT_GATE_KINDS.includes(g.kind) && g.name !== me.gate && !ids.some((i) => i.gate === g.name),
  );
  const current = gates.find((g) => g.name === me.gate);
  const label = (name: string) => gates.find((g) => g.name === name)?.label ?? name;

  const link = async (gate: string) => {
    setError("");
    setNeedReauth(false);
    const r = await startLink(me.subject, gate);
    if (r.ok) {
      window.location.assign(r.data.redirect);
      return;
    }
    if (r.code === "reauth_required") setNeedReauth(true);
    setError(linkErrorMessage(r.code));
  };
  const remove = async (gate: string) => {
    if (!confirm(`Unlink ${label(gate)}? You will no longer be able to sign in with it.`)) return;
    const r = await unlink(me.subject, gate);
    if (!r.ok) {
      if (r.code === "reauth_required") setNeedReauth(true);
      setError(r.code === "reauth_required" ? linkErrorMessage(r.code) : r.message);
      return;
    }
    if (gate === me.gate) {
      window.location.assign("/ui/login");
      return;
    }
    void load(me.subject);
  };

  return (
    <section className="settings-section">
      <h2>Sign-in methods</h2>
      {linked && <p className="settings-muted">Linked {label(linked)}.</p>}
      {linkError && (
        <p className="login-error" role="alert">
          {linkErrorMessage(linkError)}
        </p>
      )}
      {error && (
        <p className="login-error" role="alert">
          {error}
        </p>
      )}
      {needReauth && current && REDIRECT_GATE_KINDS.includes(current.kind) && (
        <p>
          <a className="ghost-btn" href={redirectLoginHref(current.name, "/ui/account", true)}>
            Sign in again with {current.label}
          </a>
        </p>
      )}
      {needReauth && (!current || !REDIRECT_GATE_KINDS.includes(current.kind)) && (
        <p className="settings-muted">Sign out and sign in again, then link the account within a few minutes.</p>
      )}
      <table className="settings-table">
        <tbody>
          {ids.map((i) => (
            <tr key={i.gate}>
              <td>{label(i.gate)}</td>
              <td className="settings-muted">{i.email || i.external_subject}</td>
              <td className="settings-row-actions">
                {gates.find((g) => g.name === i.gate)?.kind !== "password" && (
                  <button type="button" className="ghost-btn danger" onClick={() => void remove(i.gate)}>
                    unlink
                  </button>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {linkable.map((g) => (
        <button key={g.name} type="button" className="ghost-btn" onClick={() => void link(g.name)}>
          Link {g.label}
        </button>
      ))}
    </section>
  );
}
