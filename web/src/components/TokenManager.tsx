import { useEffect, useState } from "react";
import {
  OperatorTokenCreateResult,
  OperatorTokenNameSummary,
  TOKEN_SCOPES,
  listOperatorTokens,
  mintOperatorToken,
  retireOperatorToken,
  rotateOperatorToken,
} from "../api";

// TokenManager is the Settings → Tokens panel (RFC L token minting, web-reachable
// for no-shell deployments — RFC AR/TrueNAS). It mirrors the `loomcycle
// operator-token` CLI: generate a tenant/operator token (shown ONCE), list the
// existing names, rotate, and retire. Admin-only — the backend gates
// POST /v1/_operatortokendef at substrate:admin and SettingsView only renders
// this for an is_admin principal, so this is the redundant client-side half.
const DEFAULT_SCOPES = ["substrate:tenant"];

export default function TokenManager() {
  const [names, setNames] = useState<OperatorTokenNameSummary[]>([]);
  const [listErr, setListErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  // Mint form.
  const [name, setName] = useState("");
  const [tenantId, setTenantId] = useState("");
  const [subject, setSubject] = useState("");
  const [scopes, setScopes] = useState<string[]>(DEFAULT_SCOPES);
  const [busy, setBusy] = useState(false);
  const [formErr, setFormErr] = useState<string | null>(null);

  // The show-once secret (from create OR rotate). Cleared by the operator.
  const [minted, setMinted] = useState<OperatorTokenCreateResult | null>(null);
  const [copied, setCopied] = useState(false);

  const refresh = async () => {
    setLoading(true);
    try {
      const resp = await listOperatorTokens();
      setNames(resp.names ?? []);
      setListErr(null);
    } catch (e) {
      setListErr(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    refresh();
  }, []);

  const toggleScope = (s: string) => {
    setScopes((cur) => (cur.includes(s) ? cur.filter((x) => x !== s) : [...cur, s]));
  };

  const doMint = async (e: React.FormEvent) => {
    e.preventDefault();
    if (busy) return;
    setFormErr(null);
    if (!name.trim() || !tenantId.trim()) {
      setFormErr("name and tenant are required");
      return;
    }
    if (scopes.length === 0) {
      setFormErr("select at least one scope");
      return;
    }
    setBusy(true);
    try {
      const res = await mintOperatorToken({
        name: name.trim(),
        tenant_id: tenantId.trim(),
        subject: subject.trim() || undefined,
        scopes,
      });
      setMinted(res);
      setCopied(false);
      // Clear the form for the next mint; keep the tenant (operators often mint
      // several tokens for one tenant).
      setName("");
      setSubject("");
      setScopes(DEFAULT_SCOPES);
      await refresh();
    } catch (e) {
      setFormErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const doRotate = async (n: string) => {
    if (busy) return;
    if (!confirm(`Rotate token "${n}"? The current secret keeps working during the grace window, then stops. A fresh secret is shown once.`)) {
      return;
    }
    setBusy(true);
    setFormErr(null);
    try {
      const res = await rotateOperatorToken(n);
      setMinted(res);
      setCopied(false);
      await refresh();
    } catch (e) {
      setFormErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const doRetire = async (n: string) => {
    if (busy) return;
    if (!confirm(`Retire token "${n}"? Its secret stops authenticating immediately. This cannot be undone (mint a new token to replace it).`)) {
      return;
    }
    setBusy(true);
    setFormErr(null);
    try {
      await retireOperatorToken(n);
      await refresh();
    } catch (e) {
      setFormErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const copyToken = async () => {
    if (!minted) return;
    try {
      await navigator.clipboard.writeText(minted.token);
      setCopied(true);
    } catch {
      // Clipboard may be unavailable (insecure context); the token is visible
      // for manual copy regardless.
    }
  };

  return (
    <div className="settings-panel">
      <h2>Tokens</h2>
      <p className="settings-help">
        Mint per-principal bearer tokens (RFC L). Each binds an authoritative{" "}
        <code>tenant</code> + <code>subject</code> + scopes. A{" "}
        <code>substrate:tenant</code> token gives a downstream service full power
        within its own tenant; <code>substrate:admin</code> is full operator
        power. The secret is shown <strong>once</strong> and never retrievable
        again.
      </p>

      {minted && (
        <div className="token-reveal">
          <div className="token-reveal-head">
            <strong>New token for “{minted.name}”</strong>
            <span className="token-reveal-warn">{minted.warning}</span>
          </div>
          <div className="token-reveal-row">
            <code className="token-secret">{minted.token}</code>
            <button type="button" onClick={copyToken} className="primary-btn">
              {copied ? "copied ✓" : "copy"}
            </button>
            <button type="button" onClick={() => setMinted(null)} className="ghost-btn">
              dismiss
            </button>
          </div>
          <div className="token-reveal-meta">
            tenant <code>{minted.tenant_id}</code> · subject{" "}
            <code>{minted.subject}</code> · scopes{" "}
            <code>{minted.allowed_scopes.join(", ")}</code>
          </div>
        </div>
      )}

      <form className="token-form" onSubmit={doMint}>
        <div className="token-form-grid">
          <label>
            name
            <input
              type="text"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="jobember-prod"
              pattern="[a-zA-Z0-9_-]{1,64}"
              title="letters, digits, _ and - (1–64 chars)"
            />
          </label>
          <label>
            tenant
            <input
              type="text"
              value={tenantId}
              onChange={(e) => setTenantId(e.target.value)}
              placeholder="jobember"
              pattern="[a-zA-Z0-9_-]{1,64}"
              title="letters, digits, _ and - (1–64 chars)"
            />
          </label>
          <label>
            subject <span className="optional">(optional)</span>
            <input
              type="text"
              value={subject}
              onChange={(e) => setSubject(e.target.value)}
              placeholder={name ? `tok-${name}` : "tok-<name>"}
              pattern="[a-zA-Z0-9_-]{1,64}"
              title="letters, digits, _ and - (1–64 chars)"
            />
          </label>
        </div>
        <fieldset className="token-scopes">
          <legend>scopes</legend>
          {TOKEN_SCOPES.map((s) => (
            <label key={s} className="scope-check">
              <input
                type="checkbox"
                checked={scopes.includes(s)}
                onChange={() => toggleScope(s)}
              />
              <code>{s}</code>
            </label>
          ))}
        </fieldset>
        {formErr && <div className="settings-error">{formErr}</div>}
        <button type="submit" className="primary-btn" disabled={busy}>
          {busy ? "working…" : "Generate token"}
        </button>
      </form>

      <h3 className="settings-subhead">Existing tokens</h3>
      {listErr && <div className="settings-error">{listErr}</div>}
      {loading ? (
        <div className="settings-muted">loading…</div>
      ) : names.length === 0 ? (
        <div className="settings-muted">No tokens minted yet.</div>
      ) : (
        <table className="settings-table">
          <thead>
            <tr>
              <th>name</th>
              <th>tenant</th>
              <th>subject</th>
              <th>status</th>
              <th>updated</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {names.map((t) => (
              <tr key={t.name}>
                <td>
                  <code>{t.name}</code>
                </td>
                <td>
                  <code>{t.tenant_id}</code>
                </td>
                <td>
                  <code>{t.subject}</code>
                </td>
                <td>
                  {t.has_current ? (
                    <span className="status-pill status-ok">active</span>
                  ) : (
                    <span className="status-pill status-retired">retired</span>
                  )}
                  {t.token_count > 1 && (
                    <span className="settings-muted"> · {t.token_count} versions</span>
                  )}
                </td>
                <td className="settings-muted">
                  {new Date(t.last_updated).toLocaleString()}
                </td>
                <td className="settings-row-actions">
                  <button
                    type="button"
                    className="ghost-btn"
                    disabled={busy || !t.has_current}
                    onClick={() => doRotate(t.name)}
                  >
                    rotate
                  </button>
                  <button
                    type="button"
                    className="ghost-btn danger"
                    disabled={busy || !t.has_current}
                    onClick={() => doRetire(t.name)}
                  >
                    retire
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
