import { useCallback, useEffect, useState, type FormEvent } from "react";
import { KeyRound, Pencil, Plus, Trash2 } from "lucide-react";
import {
  CreateUserBody,
  MintedUserToken,
  UpdateUserBody,
  UserSummary,
  UserTokenMeta,
  createUser,
  deleteUser,
  listUserTokens,
  listUsers,
  mintUserToken,
  revokeUserToken,
  updateUser,
} from "../api";
import { useFocusTenant } from "../components/Layout";

// UsersView — RFC BX P2c: the tenant-operator Users console. Manage the
// first-class users table (create / edit / delete) and mint / list / revoke each
// user's bearer tokens (delegated minting). Tenant-scoped by the API: a
// substrate:tenant operator manages only its own tenant's users; an admin sees
// all rows and focuses one tenant via the topbar switcher. The page is
// data-driven — no client-side role branch (the server scopes + gates each op).
//
// A minted token's plaintext is shown ONCE in a reveal box; scopes are DERIVED
// server-side from the user's access_mode (the console never sends scopes), so a
// tenant-mode member gets runs + channels and an isolated member gets
// substrate:user only.

const ACCESS_MODES = ["tenant", "isolated"] as const;
const STATUSES = ["active", "disabled"] as const;

function activityLabel(u: UserSummary): string {
  if (u.running_count > 0) return `${u.running_count} running`;
  return `${u.total_count} run${u.total_count === 1 ? "" : "s"}`;
}

export default function UsersView() {
  const focusTenant = useFocusTenant();
  const [users, setUsers] = useState<UserSummary[] | null>(null);
  const [err, setErr] = useState("");
  const [loading, setLoading] = useState(false);

  const fetchUsers = useCallback(async () => {
    setLoading(true);
    setErr("");
    try {
      const resp = await listUsers(focusTenant || undefined);
      setUsers(resp.users ?? []);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, [focusTenant]);

  useEffect(() => {
    void fetchUsers();
  }, [fetchUsers]);

  return (
    <div className="settings-panel">
      <div className="settings-head-row">
        <div>
          <h1>users</h1>
          <p className="settings-help">
            Tenant-owned member identities and their bearer tokens. Each user's{" "}
            <code>access_mode</code> is the collaboration dial —{" "}
            <code>tenant</code> (full whole-tenant member) or <code>isolated</code>{" "}
            (sandboxed to their own scope). Minting a token derives its scopes from
            that dial; the secret is shown <strong>once</strong>.
          </p>
        </div>
        <button
          type="button"
          className="ghost-btn"
          onClick={() => void fetchUsers()}
          disabled={loading}
        >
          {loading ? "loading…" : "refresh"}
        </button>
      </div>

      {err && <div className="settings-error">{err}</div>}

      <CreateUserForm onCreated={fetchUsers} />

      {users && users.length > 0 && (
        <table className="settings-table">
          <thead>
            <tr>
              <th>subject</th>
              <th>display name</th>
              <th>access mode</th>
              <th>status</th>
              <th>activity</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {users.map((u) => (
              <UserRow key={u.user_id} user={u} onChanged={fetchUsers} />
            ))}
          </tbody>
        </table>
      )}

      {users && users.length === 0 && (
        <div className="settings-muted">no users yet — register one above.</div>
      )}
    </div>
  );
}

// CreateUserForm registers a first-class user in the caller's own tenant (the
// tenant is server-derived; never sent).
function CreateUserForm({ onCreated }: { onCreated: () => Promise<void> }) {
  const [subject, setSubject] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [accessMode, setAccessMode] = useState<(typeof ACCESS_MODES)[number]>("tenant");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (busy) return;
    if (!subject.trim()) {
      setErr("subject is required");
      return;
    }
    setBusy(true);
    setErr("");
    try {
      const body: CreateUserBody = {
        subject: subject.trim(),
        display_name: displayName.trim() || undefined,
        access_mode: accessMode,
      };
      await createUser(body);
      setSubject("");
      setDisplayName("");
      setAccessMode("tenant");
      await onCreated();
    } catch (e2) {
      setErr(e2 instanceof Error ? e2.message : String(e2));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form className="users-add" onSubmit={submit}>
      <span className="users-add-label">
        <Plus size={14} /> add user
      </span>
      <input
        type="text"
        placeholder="subject (user id)"
        value={subject}
        onChange={(e) => setSubject(e.target.value)}
      />
      <input
        type="text"
        placeholder="display name (optional)"
        value={displayName}
        onChange={(e) => setDisplayName(e.target.value)}
      />
      <select value={accessMode} onChange={(e) => setAccessMode(e.target.value as (typeof ACCESS_MODES)[number])}>
        {ACCESS_MODES.map((m) => (
          <option key={m} value={m}>
            {m}
          </option>
        ))}
      </select>
      <button type="submit" className="primary-btn" disabled={busy}>
        {busy ? "…" : "create"}
      </button>
      {err && <span className="settings-error users-add-err">{err}</span>}
    </form>
  );
}

// UserRow renders one user with inline edit (registered users) or a register
// affordance (a subject seen only in runs), plus an expandable token panel.
function UserRow({ user, onChanged }: { user: UserSummary; onChanged: () => Promise<void> }) {
  const registered = user.registered === true;
  const [editing, setEditing] = useState(false);
  const [showTokens, setShowTokens] = useState(false);
  const [displayName, setDisplayName] = useState(user.display_name ?? "");
  const [accessMode, setAccessMode] = useState(user.access_mode || "tenant");
  const [status, setStatus] = useState(user.status || "active");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  // Re-sync drafts when the persisted row changes under us (a refresh).
  useEffect(() => {
    setDisplayName(user.display_name ?? "");
    setAccessMode(user.access_mode || "tenant");
    setStatus(user.status || "active");
  }, [user.display_name, user.access_mode, user.status]);

  const save = async () => {
    setBusy(true);
    setErr("");
    try {
      const body: UpdateUserBody = { display_name: displayName, access_mode: accessMode, status };
      await updateUser(user.user_id, body);
      setEditing(false);
      await onChanged();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const register = async () => {
    setBusy(true);
    setErr("");
    try {
      await createUser({ subject: user.user_id, access_mode: "tenant" });
      await onChanged();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    if (!confirm(`Delete user "${user.user_id}"? This removes the identity record; their runs/memory are left intact. Existing tokens keep working until revoked.`)) {
      return;
    }
    setBusy(true);
    setErr("");
    try {
      await deleteUser(user.user_id);
      await onChanged();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <tr>
        <td>
          <code>{user.user_id}</code>
        </td>
        <td>
          {editing ? (
            <input type="text" value={displayName} onChange={(e) => setDisplayName(e.target.value)} disabled={busy} />
          ) : (
            user.display_name || <span className="settings-muted">—</span>
          )}
        </td>
        <td>
          {editing ? (
            <select value={accessMode} onChange={(e) => setAccessMode(e.target.value)} disabled={busy}>
              {ACCESS_MODES.map((m) => (
                <option key={m} value={m}>
                  {m}
                </option>
              ))}
            </select>
          ) : registered ? (
            <code>{user.access_mode}</code>
          ) : (
            <span className="settings-muted">unregistered</span>
          )}
        </td>
        <td>
          {editing ? (
            <select value={status} onChange={(e) => setStatus(e.target.value)} disabled={busy}>
              {STATUSES.map((st) => (
                <option key={st} value={st}>
                  {st}
                </option>
              ))}
            </select>
          ) : registered ? (
            <span className={"status-pill " + (user.status === "active" ? "status-ok" : "status-retired")}>
              {user.status}
            </span>
          ) : (
            <span className="settings-muted">—</span>
          )}
        </td>
        <td className="settings-muted">{activityLabel(user)}</td>
        <td className="settings-row-actions">
          {registered ? (
            editing ? (
              <>
                <button type="button" className="primary-btn" onClick={() => void save()} disabled={busy}>
                  {busy ? "…" : "save"}
                </button>
                <button type="button" className="ghost-btn" onClick={() => setEditing(false)} disabled={busy}>
                  cancel
                </button>
              </>
            ) : (
              <>
                <button type="button" className="ghost-btn" title="Manage tokens" onClick={() => setShowTokens((v) => !v)}>
                  <KeyRound size={14} /> tokens
                </button>
                <button type="button" className="ghost-btn" title="Edit" onClick={() => setEditing(true)}>
                  <Pencil size={14} /> edit
                </button>
                <button type="button" className="ghost-btn danger" title="Delete" onClick={() => void remove()} disabled={busy}>
                  <Trash2 size={14} /> delete
                </button>
              </>
            )
          ) : (
            <button type="button" className="ghost-btn" onClick={() => void register()} disabled={busy}>
              {busy ? "…" : "register"}
            </button>
          )}
        </td>
      </tr>
      {err && (
        <tr>
          <td colSpan={6}>
            <div className="settings-error">{err}</div>
          </td>
        </tr>
      )}
      {showTokens && registered && (
        <tr>
          <td colSpan={6}>
            <UserTokensPanel subject={user.user_id} disabled={user.status !== "active"} />
          </td>
        </tr>
      )}
    </>
  );
}

// UserTokensPanel lists a user's member tokens and mints / revokes them. A
// minted token's plaintext is shown ONCE in the reveal box.
function UserTokensPanel({ subject, disabled }: { subject: string; disabled: boolean }) {
  const [tokens, setTokens] = useState<UserTokenMeta[] | null>(null);
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const [minted, setMinted] = useState<MintedUserToken | null>(null);
  const [copied, setCopied] = useState(false);

  const refresh = useCallback(async () => {
    setErr("");
    try {
      const resp = await listUserTokens(subject);
      setTokens(resp.tokens ?? []);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }, [subject]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const doMint = async () => {
    setBusy(true);
    setErr("");
    try {
      const res = await mintUserToken(subject);
      setMinted(res);
      setCopied(false);
      await refresh();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const doRevoke = async (defID: string) => {
    if (!confirm("Revoke this token? It stops authenticating immediately and cannot be undone.")) return;
    setBusy(true);
    setErr("");
    try {
      await revokeUserToken(subject, defID);
      await refresh();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
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
      // Clipboard may be unavailable (insecure context) — the token stays
      // visible for manual copy.
    }
  };

  return (
    <div className="users-tokens">
      <div className="users-tokens-head">
        <strong>tokens for {subject}</strong>
        <button type="button" className="primary-btn" onClick={() => void doMint()} disabled={busy || disabled}>
          <KeyRound size={14} /> {busy ? "minting…" : "mint token"}
        </button>
      </div>
      {disabled && (
        <div className="settings-muted">this user is disabled — re-enable it to mint new tokens.</div>
      )}
      {err && <div className="settings-error">{err}</div>}

      {minted && (
        <div className="token-reveal">
          <div className="token-reveal-head">
            <strong>New token for “{minted.name}”</strong>
            <span className="token-reveal-warn">{minted.warning}</span>
          </div>
          <div className="token-reveal-row">
            <code className="token-secret">{minted.token}</code>
            <button type="button" onClick={() => void copyToken()} className="primary-btn">
              {copied ? "copied ✓" : "copy"}
            </button>
            <button type="button" onClick={() => setMinted(null)} className="ghost-btn">
              dismiss
            </button>
          </div>
          <div className="token-reveal-meta">
            scopes <code>{minted.scopes.join(", ")}</code>
          </div>
        </div>
      )}

      {tokens && tokens.length > 0 && (
        <table className="settings-table users-tokens-table">
          <thead>
            <tr>
              <th>def_id</th>
              <th>scopes</th>
              <th>created</th>
              <th>status</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {tokens.map((t) => (
              <tr key={t.def_id}>
                <td>
                  <code>{t.def_id}</code>
                </td>
                <td className="settings-muted">{t.scopes.join(", ")}</td>
                <td className="settings-muted">{new Date(t.created_at).toLocaleString()}</td>
                <td>
                  <span className={"status-pill " + (t.active ? "status-ok" : "status-retired")}>
                    {t.active ? "active" : "revoked"}
                  </span>
                </td>
                <td className="settings-row-actions">
                  <button
                    type="button"
                    className="ghost-btn danger"
                    onClick={() => void doRevoke(t.def_id)}
                    disabled={busy || !t.active}
                  >
                    revoke
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {tokens && tokens.length === 0 && (
        <div className="settings-muted">no tokens minted for this user yet.</div>
      )}
    </div>
  );
}
