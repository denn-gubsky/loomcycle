import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Plus } from "lucide-react";
import { createAclBinding, listAclBindings, listAclDefs, listGroups, retireAclBinding, type AclBinding } from "../../api";
import { bindingState } from "../../lib/aclAccess";
import ErrorNotice from "../ErrorNotice";

// BindingsTab — who gets which rule list: a user, a group, a token definition
// or the whole tenant, optionally until an expiry. Deleting retires the
// binding (the server keeps the row for the audit trail). Bindings the config
// declares are listed beside the stored ones, read-only.

const KINDS = ["user", "group", "token", "tenant"] as const;
type Kind = (typeof KINDS)[number];

export default function BindingsTab({ tenant }: { tenant?: string }) {
  const [bindings, setBindings] = useState<AclBinding[] | null>(null);
  const [includeRetired, setIncludeRetired] = useState(false);
  const [lists, setLists] = useState<string[]>([]);
  const [groups, setGroups] = useState<string[]>([]);
  const [err, setErr] = useState<unknown>(null);

  const refresh = useCallback(async () => {
    setErr(null);
    try {
      const r = await listAclBindings(includeRetired, tenant);
      setBindings(r.bindings ?? []);
    } catch (e) {
      setErr(e);
      setBindings((b) => b ?? []);
    }
  }, [includeRetired, tenant]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  // Suggestions only: a failure here leaves free-text entry, so it is not an error.
  // The built-in lists are not offered: the resolver binds them by token class
  // and skips a stored binding that names one.
  useEffect(() => {
    listAclDefs(false, tenant)
      .then((r) => setLists((r.acls ?? []).map((a) => a.name)))
      .catch(() => setLists([]));
    listGroups(tenant)
      .then((r) => setGroups((r.groups ?? []).map((g) => g.name)))
      .catch(() => setGroups([]));
  }, [tenant]);

  const retire = async (b: AclBinding) => {
    if (!b.binding_id || b.source === "config") return;
    if (!confirm(`Remove the binding of "${b.acl}" to ${describeSubject(b)}?`)) return;
    try {
      await retireAclBinding(b.binding_id, tenant);
      await refresh();
    } catch (e) {
      setErr(e);
    }
  };

  return (
    <div className="acl-tab">
      <CreateBindingForm tenant={tenant} lists={lists} groups={groups} onCreated={refresh} />
      <div className="acl-toolbar">
        <label className="acl-check">
          <input type="checkbox" checked={includeRetired} onChange={(e) => setIncludeRetired(e.target.checked)} /> show removed
        </label>
        <button type="button" className="ghost-btn" onClick={() => void refresh()}>
          refresh
        </button>
      </div>
      <ErrorNotice error={err} />
      <BindingTable bindings={bindings} onRetire={(b) => void retire(b)} />
    </div>
  );
}

// bindingKey identifies a binding in a list. A config binding has no
// binding_id, so it is keyed on what it binds; two config bindings differ in
// at least one of these (the config validator refuses an exact duplicate).
export function bindingKey(b: AclBinding): string {
  return b.binding_id ?? [b.source ?? "store", b.acl, b.subject_kind, b.subject_id ?? ""].join("|");
}

export function describeSubject(b: Pick<AclBinding, "subject_kind" | "subject_id">): string {
  return b.subject_kind === "tenant" ? "the whole tenant" : `${b.subject_kind} ${b.subject_id ?? ""}`;
}

function CreateBindingForm({
  tenant,
  lists,
  groups,
  onCreated,
}: {
  tenant?: string;
  lists: string[];
  groups: string[];
  onCreated: () => Promise<void>;
}) {
  const [acl, setAcl] = useState("");
  const [kind, setKind] = useState<Kind>("user");
  const [subject, setSubject] = useState("");
  const [expires, setExpires] = useState("");
  const [err, setErr] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const needsSubject = kind !== "tenant";
  const ready = !!acl.trim() && (!needsSubject || !!subject.trim());

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (busy || !ready) return;
    setBusy(true);
    setErr(null);
    try {
      await createAclBinding(
        {
          acl: acl.trim(),
          subject_kind: kind,
          subject_id: needsSubject ? subject.trim() : undefined,
          // datetime-local is the browser's local time; the server wants RFC 3339.
          expires_at: expires ? new Date(expires).toISOString() : undefined,
        },
        tenant,
      );
      setSubject("");
      setExpires("");
      await onCreated();
    } catch (e2) {
      setErr(e2);
    } finally {
      setBusy(false);
    }
  };

  return (
    <form className="users-add" onSubmit={submit}>
      <span className="users-add-label">
        <Plus size={14} /> bind
      </span>
      <input type="text" aria-label="rule list" list="acl-list-names" placeholder="rule list" value={acl} onChange={(e) => setAcl(e.target.value)} />
      <datalist id="acl-list-names">
        {lists.map((l) => (
          <option key={l} value={l} />
        ))}
      </datalist>
      <span className="settings-muted">to</span>
      <select aria-label="subject kind" value={kind} onChange={(e) => setKind(e.target.value as Kind)}>
        {KINDS.map((k) => (
          <option key={k} value={k}>
            {k === "tenant" ? "whole tenant" : k}
          </option>
        ))}
      </select>
      {needsSubject && (
        <input
          type="text"
          aria-label="subject id"
          list={kind === "group" ? "acl-binding-groups" : undefined}
          placeholder={kind === "user" ? "subject" : kind === "group" ? "group name" : "token def id"}
          value={subject}
          onChange={(e) => setSubject(e.target.value)}
        />
      )}
      <datalist id="acl-binding-groups">
        {groups.map((g) => (
          <option key={g} value={g} />
        ))}
      </datalist>
      <label className="acl-inline-label">
        expires
        <input type="datetime-local" aria-label="expires at" value={expires} onChange={(e) => setExpires(e.target.value)} />
      </label>
      <button type="submit" className="primary-btn" disabled={busy || !ready}>
        {busy ? "…" : "bind"}
      </button>
      {err ? (
        <div className="users-add-err">
          <ErrorNotice error={err} />
        </div>
      ) : null}
    </form>
  );
}

export function BindingTable({ bindings, onRetire, now = Date.now() }: { bindings: AclBinding[] | null; onRetire: (b: AclBinding) => void; now?: number }) {
  if (bindings === null) return <div className="settings-muted">loading…</div>;
  if (bindings.length === 0) return <div className="settings-muted">no bindings yet — a rule list does nothing until it is bound.</div>;
  return (
    <table className="settings-table acl-table">
      <thead>
        <tr>
          <th>rule list</th>
          <th>subject</th>
          <th>expires</th>
          <th>state</th>
          <th>created by</th>
          <th></th>
        </tr>
      </thead>
      <tbody>
        {bindings.map((b) => {
          const state = bindingState(b, now);
          const config = b.source === "config";
          return (
            <tr key={bindingKey(b)} className={config ? "acl-row-builtin" : undefined}>
              <td>
                <code>{b.acl}</code>
                {config && <ConfigPill what="binding" />}
              </td>
              <td>
                <span className={"acl-kind acl-kind-" + b.subject_kind}>{b.subject_kind}</span>{" "}
                {b.subject_kind === "tenant" ? <span className="settings-muted">whole tenant</span> : <code>{b.subject_id}</code>}
              </td>
              <td className="settings-muted">{b.expires_at ? new Date(b.expires_at).toLocaleString() : "never"}</td>
              <td>
                <span className={"status-pill " + (state === "active" ? "status-ok" : state === "expired" ? "status-warn" : "status-retired")}>
                  {state === "retired" ? "removed" : state}
                </span>
              </td>
              <td className="settings-muted">{config ? "config" : b.created_by || "—"}</td>
              <td className="settings-row-actions">
                {!config && (
                  <button type="button" className="ghost-btn danger" disabled={state === "retired"} onClick={() => onRetire(b)}>
                    remove
                  </button>
                )}
              </td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

// ConfigPill marks an entry the config declares: listed, never written here.
export function ConfigPill({ what }: { what: "list" | "group" | "binding" }) {
  return (
    <span
      className="status-pill acl-pill-builtin"
      title={`Declared in the server's config (acls: / groups: / acl_bindings:); change the ${what} there. Read-only here.`}
    >
      config · read-only
    </span>
  );
}
