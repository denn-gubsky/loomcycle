import { FormEvent, useCallback, useEffect, useState } from "react";
import {
  CredentialMeta,
  CredentialScope,
  createCredential,
  deleteCredential,
  listCredentials,
} from "../api";

// CredentialsPanel is the shared RFC AR credential UI, used in two places:
//   - the operator Settings hub (full: tenant + user scopes, a scope selector);
//   - the standalone "My Credentials" page (userOnly: the caller's own scope=user
//     tokens, no scope selector), which RFC CN opens to every user login incl.
//     isolated substrate:user users.
// Keeping it one component means the operator and user surfaces cannot drift.

// isCredStoreDisabled detects the fail-closed error surfaced when the operator
// hasn't set LOOMCYCLE_SECRET_KEY (the inline backend is off). The server returns
// the tool's error text verbatim inside the 422 envelope, so we match its markers.
function isCredStoreDisabled(msg: string): boolean {
  return (
    msg.includes("LOOMCYCLE_SECRET_KEY") ||
    msg.includes("no credential engine") ||
    msg.includes("disabled")
  );
}

// CredentialRow is a metadata row tagged with the scope it was listed under.
type CredentialRow = CredentialMeta & { _scope: CredentialScope };

// well-known provider/tool key env-var names (mirror docs/CREDENTIALS.md);
// free-form custom names (e.g. $cred: labels) are still allowed.
const KNOWN_KEY_NAMES = [
  "ANTHROPIC_API_KEY",
  "OPENAI_API_KEY",
  "DEEPSEEK_API_KEY",
  "GEMINI_API_KEY",
  "OLLAMA_API_KEY",
  // Web-search providers (RFC BB).
  "BRAVE_API_KEY",
  "SERPER_API_KEY",
  "EXA_API_KEY",
  "TAVILY_API_KEY",
];

export function CredentialsPanel({ userOnly = false }: { userOnly?: boolean }) {
  const [rows, setRows] = useState<CredentialRow[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [disabled, setDisabled] = useState(false);
  const [flash, setFlash] = useState<string | null>(null);
  // Create form. `value` is write-only — cleared on submit (success OR failure)
  // and never re-displayed. userOnly locks the scope to the caller's own subject.
  const [name, setName] = useState("");
  const [value, setValue] = useState("");
  const [scope, setScope] = useState<CredentialScope>(
    userOnly ? "user" : "tenant",
  );
  const [busy, setBusy] = useState(false);

  const refresh = useCallback(async () => {
    setErr(null);
    try {
      if (userOnly) {
        // Only the caller's own subject — a user login can only address scope=user
        // (the server confines an isolated caller to it anyway).
        const u = await listCredentials("user");
        setRows(
          (u.credentials ?? []).map((c) => ({ ...c, _scope: "user" as const })),
        );
      } else {
        // List BOTH scopes so the operator table shows tenant-shared + own-user
        // credentials together. list returns metadata only, never a value.
        const [t, u] = await Promise.all([
          listCredentials("tenant"),
          listCredentials("user"),
        ]);
        setRows([
          ...(t.credentials ?? []).map((c) => ({
            ...c,
            _scope: "tenant" as const,
          })),
          ...(u.credentials ?? []).map((c) => ({
            ...c,
            _scope: "user" as const,
          })),
        ]);
      }
      setDisabled(false);
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      if (isCredStoreDisabled(msg)) setDisabled(true);
      else setErr(msg);
    }
  }, [userOnly]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const onCreate = async (e: FormEvent) => {
    e.preventDefault();
    if (busy || !name.trim() || !value) return;
    setBusy(true);
    setErr(null);
    setFlash(null);
    const created = name.trim();
    const useScope: CredentialScope = userOnly ? "user" : scope;
    try {
      await createCredential({ scope: useScope, name: created, value });
      setValue(""); // clear the secret immediately
      setName("");
      setFlash(`stored ${useScope} credential "${created}"`);
      setDisabled(false);
      await refresh();
    } catch (e2) {
      setValue(""); // clear the secret even on failure — never retained
      const msg = e2 instanceof Error ? e2.message : String(e2);
      if (isCredStoreDisabled(msg)) setDisabled(true);
      else setErr(msg);
    } finally {
      setBusy(false);
    }
  };

  const onDelete = async (r: CredentialRow) => {
    if (
      !confirm(
        `Delete the ${r._scope} credential "${r.name}"? Anything referencing $cred:${r.name} will stop resolving.`,
      )
    ) {
      return;
    }
    try {
      await deleteCredential({ scope: r._scope, name: r.name });
      await refresh();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  };

  return (
    <div className="settings-panel">
      <h2>{userOnly ? "My Credentials" : "Credentials"}</h2>
      <p className="settings-help">
        {userOnly ? (
          <>
            Your own encrypted API tokens (RFC AR). A stored secret is referenced
            elsewhere as <code>$cred:&lt;name&gt;</code> — e.g. a per-user Slack or
            Telegram bot token an agent uses to publish to <em>your</em> channel —
            and the runtime binds it server-side, so the value is never shown again
            after you save it. These are private to your own subject; no one else,
            not even a tenant operator, can read them.
          </>
        ) : (
          <>
            Encrypted API credentials for this tenant (RFC AR). A stored secret is
            referenced elsewhere as <code>$cred:&lt;name&gt;</code> in an MCP
            server&apos;s env or headers, and the runtime binds it server-side — the
            value is never shown again after you save it. Naming one after a provider
            key env-var (e.g. <code>ANTHROPIC_API_KEY</code>,{" "}
            <code>BRAVE_API_KEY</code>) overrides the operator&apos;s key for this
            tenant&apos;s runs (RFC AR / AX). <strong>tenant</strong> scope is shared
            across the tenant; <strong>user</strong> scope is private to your own
            subject (per-user tokens, e.g. a personal Telegram bot token).
          </>
        )}
      </p>

      {disabled && (
        <div className="settings-error">
          The credential store is disabled. The operator must set{" "}
          <code>LOOMCYCLE_SECRET_KEY</code> to enable encrypted credential storage.
        </div>
      )}
      {err && <div className="settings-error">{err}</div>}
      {flash && <div className="settings-flash">{flash}</div>}

      <form className="cred-create" onSubmit={onCreate}>
        <input
          type="text"
          list="cred-key-names"
          placeholder={
            userOnly ? "name (e.g. TELEGRAM_BOT_TOKEN)" : "name (e.g. ANTHROPIC_API_KEY)"
          }
          value={name}
          onChange={(e) => setName(e.target.value)}
          autoComplete="off"
          spellCheck={false}
        />
        <datalist id="cred-key-names">
          {KNOWN_KEY_NAMES.map((n) => (
            <option key={n} value={n} />
          ))}
        </datalist>
        <input
          type="password"
          placeholder="value (secret — write-only)"
          value={value}
          onChange={(e) => setValue(e.target.value)}
          autoComplete="new-password"
        />
        {!userOnly && (
          <select
            value={scope}
            onChange={(e) => setScope(e.target.value as CredentialScope)}
            title="tenant = shared; user = your own subject"
          >
            <option value="tenant">tenant</option>
            <option value="user">user</option>
          </select>
        )}
        <button
          type="submit"
          className="primary-btn"
          disabled={busy || !name.trim() || !value}
        >
          {busy ? "storing…" : "store"}
        </button>
      </form>

      <table className="settings-table cred-table">
        <thead>
          <tr>
            <th>name</th>
            {!userOnly && <th>scope</th>}
            <th>updated</th>
            <th aria-label="actions" />
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 ? (
            <tr>
              <td colSpan={userOnly ? 3 : 4} className="settings-muted">
                no credentials stored.
              </td>
            </tr>
          ) : (
            rows.map((r) => (
              <tr key={`${r._scope}/${r.name}`}>
                <td>
                  <code>{r.name}</code>
                </td>
                {!userOnly && <td>{r._scope}</td>}
                <td>
                  {r.updated_at ? new Date(r.updated_at).toLocaleString() : "—"}
                </td>
                <td>
                  <button
                    type="button"
                    className="ghost-btn danger"
                    onClick={() => void onDelete(r)}
                  >
                    delete
                  </button>
                </td>
              </tr>
            ))
          )}
        </tbody>
      </table>
    </div>
  );
}
