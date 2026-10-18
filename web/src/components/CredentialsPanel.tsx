import { FormEvent, useCallback, useEffect, useState } from "react";
import {
  CredentialMeta,
  CredentialScope,
  createCredential,
  deleteCredential,
  deleteCryptoKeyVersion,
  generateCryptoKey,
  importCryptoKey,
  listCredentials,
  rotateCryptoKey,
  setCryptoKeyMinVersion,
} from "../api";
import {
  CRYPTO_KEY_ENCODINGS,
  CRYPTO_KEY_TYPES,
  CryptoKeyType,
  cryptoKeyActions,
  expiryToRFC3339,
  isCryptoKey,
  minVersionBounds,
  parseMinVersion,
  refusalText,
  retireWarning,
  versionChips,
  versionSummary,
} from "../lib/cryptoKeys";

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

function rowKey(r: CredentialRow): string {
  return `${r._scope}/${r.name}`;
}

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
  // Optional URL restriction, as typed: comma- or space-separated https URLs,
  // or the word "none" for a value that is sent nowhere.
  const [allowedURLs, setAllowedURLs] = useState("");
  // Optional user name sent with the value (basic auth for a git source).
  const [username, setUsername] = useState("");
  const [scope, setScope] = useState<CredentialScope>(
    userOnly ? "user" : "tenant",
  );
  const [busy, setBusy] = useState(false);
  // Crypto-key form (RFC EP). Generate is the default: the server makes a key
  // nobody ever sees. Import (existing material) is the secondary path; its
  // `keyValue` is write-only exactly like `value` above.
  const [keyName, setKeyName] = useState("");
  const [keyType, setKeyType] = useState<CryptoKeyType>("aes-256-gcm");
  const [keyExpiry, setKeyExpiry] = useState("");
  const [keyScope, setKeyScope] = useState<CredentialScope>(
    userOnly ? "user" : "tenant",
  );
  const [keyImport, setKeyImport] = useState(false);
  const [keyValue, setKeyValue] = useState("");
  const [keyEncoding, setKeyEncoding] = useState("");
  const [keyBusy, setKeyBusy] = useState(false);
  // The crypto-key row whose version controls are open, and the typed
  // "retire versions below" number for it.
  const [openKey, setOpenKey] = useState<string | null>(null);
  const [minText, setMinText] = useState("");
  // The row a key action is running on (disables that row's buttons).
  const [rowBusy, setRowBusy] = useState<string | null>(null);

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
      await createCredential({
        scope: useScope,
        name: created,
        value,
        ...parseAllowedURLs(allowedURLs),
        ...(username.trim() ? { username: username.trim() } : {}),
      });
      setValue(""); // clear the secret immediately
      setName("");
      setAllowedURLs("");
      setUsername("");
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
    const question = isCryptoKey(r)
      ? `Delete the ${r._scope} crypto key "${r.name}"? Its material is destroyed — anything still encrypted or signed with it can never be decrypted or verified again. (The server refuses while any version is unretired.)`
      : `Delete the ${r._scope} credential "${r.name}"? Anything referencing $cred:${r.name} will stop resolving.`;
    if (!confirm(question)) {
      return;
    }
    setErr(null);
    setFlash(null);
    try {
      await deleteCredential({ scope: r._scope, name: r.name });
      if (openKey === rowKey(r)) setOpenKey(null);
      await refresh();
    } catch (e) {
      // A refused crypto-key delete explains itself (which version is still
      // unretired); show the server's own text.
      setErr(refusalText(e instanceof Error ? e.message : String(e)));
    }
  };

  const onCreateKey = async (e: FormEvent) => {
    e.preventDefault();
    if (keyBusy || !keyName.trim() || (keyImport && !keyValue)) return;
    const expires = expiryToRFC3339(keyExpiry);
    if (expires === null) {
      setKeyValue(""); // never retained, even on a refusal
      setErr("expiry is not a valid date and time");
      return;
    }
    setKeyBusy(true);
    setErr(null);
    setFlash(null);
    const created = keyName.trim();
    const useScope: CredentialScope = userOnly ? "user" : keyScope;
    const base = {
      scope: useScope,
      name: created,
      key_type: keyType,
      ...(expires ? { expires_at: expires } : {}),
    };
    try {
      if (keyImport) {
        await importCryptoKey({
          ...base,
          value: keyValue,
          ...(keyEncoding ? { key_encoding: keyEncoding } : {}),
        });
      } else {
        await generateCryptoKey(base);
      }
      setKeyValue(""); // clear the key material immediately
      setKeyName("");
      setKeyExpiry("");
      setFlash(
        `${keyImport ? "imported" : "generated"} ${useScope} ${keyType} crypto key "${created}"`,
      );
      setDisabled(false);
      await refresh();
    } catch (e2) {
      setKeyValue(""); // clear the key material even on failure — never retained
      const msg = e2 instanceof Error ? e2.message : String(e2);
      if (isCredStoreDisabled(msg)) setDisabled(true);
      else setErr(refusalText(msg));
    } finally {
      setKeyBusy(false);
    }
  };

  // runKeyOp runs one crypto-key change on a row, then reloads the table.
  const runKeyOp = async (
    r: CredentialRow,
    done: string,
    op: () => Promise<unknown>,
  ) => {
    setRowBusy(rowKey(r));
    setErr(null);
    setFlash(null);
    try {
      await op();
      setFlash(done);
      await refresh();
    } catch (e) {
      setErr(refusalText(e instanceof Error ? e.message : String(e)));
    } finally {
      setRowBusy(null);
    }
  };

  const onRotate = (r: CredentialRow) =>
    runKeyOp(r, `rotated "${r.name}" — a new current version`, () =>
      rotateCryptoKey({ scope: r._scope, name: r.name }),
    );

  const onRetire = (r: CredentialRow) => {
    const n = parseMinVersion(r, minText);
    if (n === null) return;
    if (!confirm(retireWarning(r, n))) return;
    void runKeyOp(r, `"${r.name}": versions below v${n} retired`, async () => {
      await setCryptoKeyMinVersion({ scope: r._scope, name: r.name, min_version: n });
      setMinText("");
    });
  };

  const onDeleteVersion = (r: CredentialRow, version: number) => {
    if (
      !confirm(
        `Delete version ${version} of the crypto key "${r.name}"? Its material is destroyed — anything still encrypted or signed with it can never be decrypted or verified again.`,
      )
    ) {
      return;
    }
    void runKeyOp(r, `"${r.name}": version ${version} deleted`, () =>
      deleteCryptoKeyVersion({ scope: r._scope, name: r.name, version }),
    );
  };

  const toggleOpen = (r: CredentialRow) => {
    const k = rowKey(r);
    if (openKey === k) {
      setOpenKey(null);
      return;
    }
    setOpenKey(k);
    const b = minVersionBounds(r);
    setMinText(b ? String(b.lo) : "");
  };

  const cols = userOnly ? 3 : 4;

  // renderVersions is a crypto key's expanded row: retire versions below…, and
  // delete for each retired version.
  const renderVersions = (r: CredentialRow, rb: boolean, k: string) => {
    const acts = cryptoKeyActions(r);
    const bounds = minVersionBounds(r);
    const newMin = parseMinVersion(r, minText);
    return (
      <tr key={`${k}#versions`}>
        <td colSpan={cols}>
          <h4 className="settings-subhead">Retire versions below…</h4>
          {acts.retire && bounds ? (
            <form
              className="cred-create"
              onSubmit={(e) => {
                e.preventDefault();
                onRetire(r);
              }}
            >
              <input
                type="number"
                min={bounds.lo}
                max={bounds.hi}
                step={1}
                value={minText}
                onChange={(e) => setMinText(e.target.value)}
                aria-label={`new min_version (${bounds.lo}–${bounds.hi})`}
                title={`new min_version, ${bounds.lo}–${bounds.hi}: every version below it is retired`}
              />
              <button
                type="submit"
                className="ghost-btn danger"
                disabled={rb || newMin === null}
              >
                retire
              </button>
            </form>
          ) : (
            <p className="settings-muted">
              Nothing to retire: only the current version is in use. Rotate first
              to add a newer version.
            </p>
          )}
          <p className="settings-help">
            <strong>Warning:</strong>{" "}
            {newMin !== null
              ? retireWarning(r, newMin)
              : "Anything encrypted or signed with a retired version stops decrypting or verifying unless it is rewrapped to a newer version first. min_version can only be raised."}
          </p>
          <h4 className="settings-subhead">Retired versions</h4>
          {acts.deletableVersions.length === 0 ? (
            <p className="settings-muted">no retired versions.</p>
          ) : (
            <div>
              {acts.deletableVersions.map((v) => (
                <span key={v}>
                  <button
                    type="button"
                    className="ghost-btn danger"
                    onClick={() => onDeleteVersion(r, v)}
                    disabled={rb}
                    title="destroy this retired version's material"
                  >
                    delete v{v}
                  </button>{" "}
                </span>
              ))}
            </div>
          )}
        </td>
      </tr>
    );
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
        <input
          type="text"
          placeholder="allowed URLs (optional)"
          title={
            "https URLs this value may be sent to, separated by commas. " +
            "Type none for a value that is never sent anywhere. " +
            "Left empty: no restriction, or the stored one is kept on a rotation. " +
            "A restricted credential is used only where the destination is covered " +
            "(an MCP server's or a hook's header, a remote peer, a git source), never as a provider key."
          }
          value={allowedURLs}
          onChange={(e) => setAllowedURLs(e.target.value)}
          autoComplete="off"
          spellCheck={false}
        />
        <input
          type="text"
          placeholder="username (optional)"
          title={
            "The user name sent with the value where a destination takes one " +
            "(HTTP basic auth for a git source). Left empty: none, or the stored one is kept on a rotation."
          }
          value={username}
          onChange={(e) => setUsername(e.target.value)}
          autoComplete="off"
          spellCheck={false}
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

      <h3 className="settings-subhead">Crypto keys</h3>
      <p className="settings-help">
        A crypto key is used by agents through the Crypto tool to encrypt, sign or
        make one-time codes — it is never sent anywhere and its material is never
        shown, not even once. <strong>Generate</strong> makes a key on the server
        that nobody ever sees; import existing material only when you must match a
        key that already exists elsewhere.
      </p>
      <form className="cred-create" onSubmit={onCreateKey}>
        <input
          type="text"
          placeholder="key name (e.g. invoice-signing)"
          value={keyName}
          onChange={(e) => setKeyName(e.target.value)}
          autoComplete="off"
          spellCheck={false}
        />
        <select
          value={keyType}
          onChange={(e) => setKeyType(e.target.value as CryptoKeyType)}
          title="key type — fixed for the key's lifetime"
        >
          {CRYPTO_KEY_TYPES.map((t) => (
            <option key={t} value={t}>
              {t}
            </option>
          ))}
        </select>
        <input
          type="datetime-local"
          value={keyExpiry}
          onChange={(e) => setKeyExpiry(e.target.value)}
          title="expiry (optional) — left empty: the key does not expire"
          aria-label="expiry (optional)"
        />
        {keyImport && (
          <>
            <input
              type="password"
              placeholder="key material (secret — write-only)"
              value={keyValue}
              onChange={(e) => setKeyValue(e.target.value)}
              autoComplete="new-password"
            />
            <select
              value={keyEncoding}
              onChange={(e) => setKeyEncoding(e.target.value)}
              title="how the material is written; default: base64 for symmetric keys, base32 for totp, pem for ed25519 / ecdsa-p256"
            >
              <option value="">encoding: default</option>
              {CRYPTO_KEY_ENCODINGS.map((enc) => (
                <option key={enc} value={enc}>
                  {enc}
                </option>
              ))}
            </select>
          </>
        )}
        {!userOnly && (
          <select
            value={keyScope}
            onChange={(e) => setKeyScope(e.target.value as CredentialScope)}
            title="tenant = shared; user = your own subject"
          >
            <option value="tenant">tenant</option>
            <option value="user">user</option>
          </select>
        )}
        <button
          type="submit"
          className="primary-btn"
          disabled={keyBusy || !keyName.trim() || (keyImport && !keyValue)}
        >
          {keyBusy
            ? keyImport
              ? "importing…"
              : "generating…"
            : keyImport
              ? "import key"
              : "generate crypto key"}
        </button>
        <button
          type="button"
          className="ghost-btn"
          onClick={() => {
            setKeyImport(!keyImport);
            setKeyValue(""); // switching away never keeps typed material
          }}
          disabled={keyBusy}
        >
          {keyImport ? "generate instead" : "import existing key…"}
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
              <td colSpan={cols} className="settings-muted">
                no credentials stored.
              </td>
            </tr>
          ) : (
            rows.flatMap((r) => {
              const crypto = isCryptoKey(r);
              const k = rowKey(r);
              const open = crypto && openKey === k;
              const rb = rowBusy === k;
              const main = (
                <tr key={k}>
                  <td>
                    <code>{r.name}</code>
                    {crypto && <CryptoKeyDetails r={r} />}
                    {r.allowed_urls && (
                      <div className="settings-muted">
                        {r.allowed_urls.length === 0
                          ? "sent nowhere"
                          : `only to ${r.allowed_urls.join(", ")}`}
                      </div>
                    )}
                    {r.username && (
                      <div className="settings-muted">user {r.username}</div>
                    )}
                  </td>
                  {!userOnly && <td>{r._scope}</td>}
                  <td>
                    {r.updated_at ? new Date(r.updated_at).toLocaleString() : "—"}
                  </td>
                  <td>
                    {crypto && cryptoKeyActions(r).rotate && (
                      <>
                        <button
                          type="button"
                          className="ghost-btn"
                          onClick={() => void onRotate(r)}
                          disabled={rb}
                          title="add a new current version, generated on the server; older versions keep decrypting until retired"
                        >
                          rotate
                        </button>{" "}
                        <button
                          type="button"
                          className="ghost-btn"
                          onClick={() => toggleOpen(r)}
                          aria-expanded={open}
                        >
                          {open ? "hide versions" : "versions…"}
                        </button>{" "}
                      </>
                    )}
                    <button
                      type="button"
                      className="ghost-btn danger"
                      onClick={() => void onDelete(r)}
                      disabled={rb}
                    >
                      delete
                    </button>
                  </td>
                </tr>
              );
              if (!open) return [main];
              return [main, renderVersions(r, rb, k)];
            })
          )}
        </tbody>
      </table>
    </div>
  );
}

// CryptoKeyDetails is a crypto key's name-cell detail: the badge with its type,
// current/min version, expiry, and the compact version list (retired marked).
function CryptoKeyDetails({ r }: { r: CredentialMeta }) {
  return (
    <>
      {" "}
      <span
        className="status-pill status-ok"
        title="crypto key — its material is never shown"
      >
        crypto key · {r.key_type ?? "?"}
      </span>
      <div className="settings-muted">
        {versionSummary(r)}
        {r.expires_at &&
          ` · ${r.expired ? "expired" : "expires"} ${new Date(r.expires_at).toLocaleString()}`}
      </div>
      <div className="settings-muted">
        {versionChips(r).map((c) => (
          <span key={c.label}>
            <span
              className={`status-pill${c.retired ? " status-retired" : c.current ? " status-ok" : ""}`}
              title={
                c.retired
                  ? "retired — no longer decrypts or verifies"
                  : c.current
                    ? "current — used for new data"
                    : "still decrypts and verifies"
              }
            >
              {c.label}
              {c.retired ? " retired" : c.current ? " current" : ""}
            </span>{" "}
          </span>
        ))}
      </div>
    </>
  );
}

// parseAllowedURLs turns the form's text into the request field. Empty text
// sends no field at all, which the server reads as "no restriction" for a new
// credential and "keep what it has" for an existing one. "none" sends an
// empty list: the value is sent nowhere.
function parseAllowedURLs(text: string): { allowed_urls?: string[] } {
  const t = text.trim();
  if (!t) return {};
  if (t.toLowerCase() === "none") return { allowed_urls: [] };
  return { allowed_urls: t.split(/[\s,]+/).filter(Boolean) };
}
