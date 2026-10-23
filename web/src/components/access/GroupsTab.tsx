import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Plus, Trash2, X } from "lucide-react";
import { addGroupMember, deleteGroup, getGroup, listGroups, removeGroupMember, upsertGroup, type GroupRow } from "../../api";
import { effectiveMembers, membershipTree, type MembersIndex, type TreeNode } from "../../lib/aclAccess";
import ErrorNotice from "../ErrorNotice";

// GroupsTab — the tenant's user groups: create / delete, direct members (users
// and nested groups) with add / remove, the nested-membership tree and the
// effective (transitive) members. The server returns direct members only, so
// the closure is computed here from every group's members (one GET per group;
// a tenant's group count is small, and the list is what the operator is
// looking at anyway).

export async function loadMembersIndex(groups: GroupRow[], tenant?: string): Promise<MembersIndex> {
  const details = await Promise.all(groups.map((g) => getGroup(g.name, tenant)));
  const idx: MembersIndex = {};
  details.forEach((d, i) => {
    idx[groups[i].name] = d.members ?? [];
  });
  return idx;
}

export default function GroupsTab({ tenant }: { tenant?: string }) {
  const [groups, setGroups] = useState<GroupRow[] | null>(null);
  const [idx, setIdx] = useState<MembersIndex>({});
  const [open, setOpen] = useState<string | null>(null);
  const [err, setErr] = useState<unknown>(null);

  const refresh = useCallback(async () => {
    setErr(null);
    try {
      const r = await listGroups(tenant);
      const gs = r.groups ?? [];
      setGroups(gs);
      setIdx(await loadMembersIndex(gs, tenant));
    } catch (e) {
      setErr(e);
      setGroups((g) => g ?? []);
    }
  }, [tenant]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const remove = async (name: string) => {
    if (!confirm(`Delete group "${name}"? Its memberships go with it; bindings to it stay and apply to no one.`)) return;
    try {
      await deleteGroup(name, tenant);
      if (open === name) setOpen(null);
      await refresh();
    } catch (e) {
      setErr(e);
    }
  };

  return (
    <div className="acl-tab">
      <CreateGroupForm tenant={tenant} onCreated={refresh} />
      <ErrorNotice error={err} />
      <GroupTable groups={groups} idx={idx} open={open} onToggle={(n) => setOpen(open === n ? null : n)} onDelete={(n) => void remove(n)} />
      {open && groups?.some((g) => g.name === open) && (
        <GroupPanel
          group={groups.find((g) => g.name === open)!}
          idx={idx}
          allGroups={groups.map((g) => g.name)}
          tenant={tenant}
          onChanged={refresh}
        />
      )}
    </div>
  );
}

function CreateGroupForm({ tenant, onCreated }: { tenant?: string; onCreated: () => Promise<void> }) {
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [err, setErr] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (busy || !name.trim()) return;
    setBusy(true);
    setErr(null);
    try {
      await upsertGroup({ name: name.trim(), description: description.trim() || undefined }, tenant);
      setName("");
      setDescription("");
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
        <Plus size={14} /> new group
      </span>
      <input type="text" aria-label="group name" placeholder="name, e.g. support" value={name} onChange={(e) => setName(e.target.value)} />
      <input type="text" aria-label="group description" placeholder="description (optional)" value={description} onChange={(e) => setDescription(e.target.value)} />
      <button type="submit" className="primary-btn" disabled={busy || !name.trim()}>
        {busy ? "…" : "create"}
      </button>
      {err ? (
        <div className="users-add-err">
          <ErrorNotice error={err} />
        </div>
      ) : null}
    </form>
  );
}

export function GroupTable({
  groups,
  idx,
  open,
  onToggle,
  onDelete,
}: {
  groups: GroupRow[] | null;
  idx: MembersIndex;
  open: string | null;
  onToggle: (name: string) => void;
  onDelete: (name: string) => void;
}) {
  if (groups === null) return <div className="settings-muted">loading…</div>;
  if (groups.length === 0) return <div className="settings-muted">no groups yet — create one above.</div>;
  return (
    <table className="settings-table acl-table">
      <thead>
        <tr>
          <th>group</th>
          <th>description</th>
          <th>direct members</th>
          <th>effective users</th>
          <th></th>
        </tr>
      </thead>
      <tbody>
        {groups.map((g) => {
          const members = idx[g.name] ?? [];
          return (
            <tr key={g.name} className={open === g.name ? "acl-row-selected" : undefined}>
              <td>
                <button type="button" className="acl-link" onClick={() => onToggle(g.name)}>
                  <code>{g.name}</code>
                </button>
                {g.external_id && (
                  <span className="status-pill acl-pill-builtin" title={`Synced from an identity provider (${g.external_id}); read-only here`}>
                    external
                  </span>
                )}
              </td>
              <td className="settings-muted">{g.description || "—"}</td>
              <td>
                {members.filter((m) => m.member_kind === "user").length} users, {members.filter((m) => m.member_kind === "group").length} groups
              </td>
              <td>{effectiveMembers(g.name, idx).length}</td>
              <td className="settings-row-actions">
                <button type="button" className="ghost-btn" onClick={() => onToggle(g.name)}>
                  {open === g.name ? "close" : "members"}
                </button>
                <button type="button" className="ghost-btn danger" title="Delete group" onClick={() => onDelete(g.name)}>
                  <Trash2 size={14} />
                </button>
              </td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

export function GroupPanel({
  group,
  idx,
  allGroups,
  tenant,
  onChanged,
}: {
  group: GroupRow;
  idx: MembersIndex;
  allGroups: string[];
  tenant?: string;
  onChanged: () => Promise<void>;
}) {
  const [kind, setKind] = useState<"user" | "group">("user");
  const [id, setId] = useState("");
  const [err, setErr] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const members = idx[group.name] ?? [];
  // An IdP-synced group is read-only to local writers (RFC EY §5); the
  // server does not refuse it yet, so the console holds the line.
  const external = !!group.external_id;

  const add = async (e: FormEvent) => {
    e.preventDefault();
    if (busy || !id.trim()) return;
    setBusy(true);
    setErr(null);
    try {
      await addGroupMember(group.name, kind, id.trim(), tenant);
      setId("");
      await onChanged();
    } catch (e2) {
      setErr(e2);
    } finally {
      setBusy(false);
    }
  };
  const drop = async (k: string, mid: string) => {
    setErr(null);
    try {
      await removeGroupMember(group.name, k, mid, tenant);
      await onChanged();
    } catch (e2) {
      setErr(e2);
    }
  };

  return (
    <div className="acl-panel">
      <h3>
        <code>{group.name}</code>
        {external && <span className="settings-muted"> — managed by an identity provider; members are read-only here</span>}
      </h3>
      <div className="acl-panel-cols">
        <div>
          <h4>direct members</h4>
          {members.length === 0 && <div className="settings-muted">no members yet.</div>}
          <ul className="acl-members">
            {members.map((m) => (
              <li key={m.member_kind + ":" + m.member_id}>
                <span className={"acl-kind acl-kind-" + m.member_kind}>{m.member_kind}</span> <code>{m.member_id}</code>
                {!external && (
                  <button type="button" className="ghost-btn acl-inline-btn" title="Remove member" onClick={() => void drop(m.member_kind, m.member_id)}>
                    <X size={12} />
                  </button>
                )}
              </li>
            ))}
          </ul>
          {!external && (
            <form className="acl-form-row" onSubmit={add}>
              <select aria-label="member kind" value={kind} onChange={(e) => setKind(e.target.value as "user" | "group")}>
                <option value="user">user</option>
                <option value="group">group</option>
              </select>
              <input
                type="text"
                aria-label="member id"
                list={kind === "group" ? "acl-group-names" : undefined}
                placeholder={kind === "user" ? "subject" : "group name"}
                value={id}
                onChange={(e) => setId(e.target.value)}
              />
              <datalist id="acl-group-names">
                {allGroups
                  .filter((g) => g !== group.name)
                  .map((g) => (
                    <option key={g} value={g} />
                  ))}
              </datalist>
              <button type="submit" className="primary-btn" disabled={busy || !id.trim()}>
                add
              </button>
            </form>
          )}
          <ErrorNotice error={err} />
        </div>
        <div>
          <h4>nesting</h4>
          <MembershipTree node={membershipTree(group.name, idx)} />
          <h4>effective users</h4>
          <EffectiveMembers group={group.name} idx={idx} />
        </div>
      </div>
    </div>
  );
}

export function MembershipTree({ node }: { node: TreeNode }) {
  return (
    <ul className="acl-tree">
      <li>
        <span className="acl-kind acl-kind-group">group</span> <code>{node.name}</code>
        {node.cycle && <span className="settings-muted"> (already above — a cycle)</span>}
        {node.truncated && <span className="settings-muted"> (deeper than the nesting cap)</span>}
        {(node.users.length > 0 || node.groups.length > 0) && (
          <ul>
            {node.users.map((u) => (
              <li key={"u:" + u}>
                <span className="acl-kind acl-kind-user">user</span> <code>{u}</code>
              </li>
            ))}
            {node.groups.map((g, i) => (
              <li key={"g:" + g.name + i} className="acl-tree-nested">
                <MembershipTree node={g} />
              </li>
            ))}
          </ul>
        )}
      </li>
    </ul>
  );
}

export function EffectiveMembers({ group, idx }: { group: string; idx: MembersIndex }) {
  const eff = effectiveMembers(group, idx);
  if (eff.length === 0) return <div className="settings-muted">no users, directly or through nested groups.</div>;
  return (
    <ul className="acl-members">
      {eff.map((m) => (
        <li key={m.subject}>
          <code>{m.subject}</code>
          <span className="settings-muted">{m.via.length === 0 ? " — direct" : ` — via ${m.via.join(" → ")}`}</span>
        </li>
      ))}
    </ul>
  );
}
