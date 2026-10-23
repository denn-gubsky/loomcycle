import { useEffect, useState } from "react";
import { listAclBindings, listGroups, type AclBinding } from "../api";
import { bindingsForUser, groupsOfSubject, type MembersIndex } from "../lib/aclAccess";
import { loadMembersIndex } from "./access/GroupsTab";
import { bindingKey } from "./access/BindingsTab";

// The Users page's access column: each user's groups and the rule lists bound
// to it, read from the same routes the Access console uses. Loaded once per
// page (the groups, every group's members, the bindings) and joined per user.

export interface AccessSnapshot {
  idx: MembersIndex;
  bindings: AclBinding[];
}

// useAccessSnapshot loads the snapshot for the focused tenant. A failure (no
// store, or a server without access control) leaves the column saying so
// rather than blanking the page.
export function useAccessSnapshot(tenant: string | undefined, reloadKey: number) {
  const [snap, setSnap] = useState<AccessSnapshot | null>(null);
  const [err, setErr] = useState<string>("");
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const [g, b] = await Promise.all([listGroups(tenant), listAclBindings(false, tenant)]);
        const idx = await loadMembersIndex(g.groups ?? [], tenant);
        if (!cancelled) {
          setSnap({ idx, bindings: b.bindings ?? [] });
          setErr("");
        }
      } catch (e) {
        if (!cancelled) setErr(e instanceof Error ? e.message : String(e));
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [tenant, reloadKey]);
  return { snap, err };
}

const SOURCE_LABEL = { user: "own", group: "via group", tenant: "tenant-wide" } as const;

export default function UserAccessCell({ subject, snap, now }: { subject: string; snap: AccessSnapshot | null; now?: number }) {
  if (!snap) return <span className="settings-muted">—</span>;
  const groups = groupsOfSubject(subject, snap.idx);
  const bindings = bindingsForUser(subject, groups, snap.bindings, now);
  return (
    <div className="user-access">
      <div>
        {groups.length === 0 ? (
          <span className="settings-muted">no groups</span>
        ) : (
          groups.map((g) => (
            <span key={g.group} className="user-access-chip" title={g.via ? `through group ${g.via}` : "direct member"}>
              {g.group}
              {g.via && <span className="settings-muted"> via {g.via}</span>}
            </span>
          ))
        )}
      </div>
      <div>
        {bindings.length === 0 ? (
          <span className="settings-muted">no lists bound</span>
        ) : (
          bindings.map((b) => (
            <span
              key={bindingKey(b.binding)}
              className={"user-access-chip user-access-binding" + (b.state === "expired" ? " user-access-expired" : "")}
              title={b.state === "expired" ? "expired" : undefined}
            >
              <code>{b.binding.acl}</code>{" "}
              <span className="settings-muted">
                {SOURCE_LABEL[b.source]}
                {b.group ? ` ${b.group}` : ""}
                {b.state === "expired" ? " (expired)" : ""}
              </span>
            </span>
          ))
        )}
      </div>
    </div>
  );
}
