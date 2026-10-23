import { useEffect, useState } from "react";
import { getAclCatalog } from "../../api";
import { aclModeText } from "../../lib/aclAccess";
import type { AclCatalog } from "../../lib/aclRules";
import { useFocusTenant, usePrincipal } from "../Layout";
import ErrorNotice from "../ErrorNotice";
import BindingsTab from "./BindingsTab";
import GroupsTab from "./GroupsTab";
import PoliciesTab from "./PoliciesTab";
import SimulatorTab from "./SimulatorTab";

// AccessConsole — Settings → Access (RFC EZ §4): who is in which group, the
// rule lists, which list binds to whom, and a simulator to ask the engine a
// question before (or after) changing any of it. Confined to the caller's
// tenant by the server; an admin works in the topbar's focused tenant.

export type AccessTab = "groups" | "policies" | "bindings" | "simulator";

const TABS: { id: AccessTab; label: string }[] = [
  { id: "groups", label: "Groups" },
  { id: "policies", label: "Policies" },
  { id: "bindings", label: "Bindings" },
  { id: "simulator", label: "Simulator" },
];

export default function AccessConsole() {
  const principal = usePrincipal();
  const focusTenant = useFocusTenant();
  // Admin's ?tenant= focus; a tenant operator is confined server-side and the
  // parameter is ignored for it.
  const tenant = principal?.is_admin && focusTenant ? focusTenant : undefined;
  const [catalog, setCatalog] = useState<AclCatalog | null>(null);
  const [catalogErr, setCatalogErr] = useState<unknown>(null);
  const [tab, setTab] = useState<AccessTab>("groups");
  const [draft, setDraft] = useState("");
  const [useDraft, setUseDraft] = useState(false);

  useEffect(() => {
    setCatalogErr(null);
    getAclCatalog(tenant)
      .then(setCatalog)
      .catch((e) => setCatalogErr(e));
  }, [tenant]);

  return (
    <AccessConsoleFrame
      mode={principal?.acl_mode}
      // Without a focus the server acts in the caller's own tenant; open mode
      // has no principal, so that is the shared "" tenant (/v1/_me's
      // "default" there is synthetic).
      tenantLabel={tenant ?? (principal?.open_mode ? "" : principal?.tenant_id ?? "")}
      tab={tab}
      onTab={setTab}
      catalogErr={catalogErr}
    >
      {tab === "groups" && <GroupsTab tenant={tenant} />}
      {tab === "policies" && (
        <PoliciesTab
          tenant={tenant}
          catalog={catalog}
          onSimulate={(yaml) => {
            setDraft(yaml);
            setUseDraft(true);
            setTab("simulator");
          }}
        />
      )}
      {tab === "bindings" && <BindingsTab tenant={tenant} />}
      {tab === "simulator" && (
        <SimulatorTab tenant={tenant} catalog={catalog} draft={draft} onDraft={setDraft} useDraft={useDraft} onUseDraft={setUseDraft} />
      )}
    </AccessConsoleFrame>
  );
}

// AccessConsoleFrame is the console's chrome — the mode line, the tenant it
// acts in, the tab strip — separate from the data so it renders statically.
export function AccessConsoleFrame({
  mode,
  tenantLabel,
  tab,
  onTab,
  catalogErr,
  children,
}: {
  mode?: string;
  tenantLabel?: string;
  tab: AccessTab;
  onTab: (t: AccessTab) => void;
  catalogErr?: unknown;
  children?: React.ReactNode;
}) {
  const m = aclModeText(mode);
  return (
    <div className="settings-panel acl-console">
      <h2>Access</h2>
      <p className="settings-help">
        Groups of users, rule lists that allow or deny actions on resources, and the bindings that attach a list to a user, a group, a
        token or the whole tenant. A run gets the agent’s own permissions narrowed by its caller’s rules.
      </p>
      <div className="acl-mode">
        <span className="acl-mode-label">
          mode <code>{m.label}</code>
        </span>
        <span>{m.sentence}</span>
      </div>
      {tenantLabel !== undefined && (
        <div className="settings-muted acl-tenant">
          tenant: <code>{tenantLabel || "(shared)"}</code>
        </div>
      )}
      <ErrorNotice error={catalogErr} />
      <nav className="settings-tabs acl-tabs">
        {TABS.map((t) => (
          <button key={t.id} type="button" className={"settings-tab" + (tab === t.id ? " active" : "")} onClick={() => onTab(t.id)}>
            {t.label}
          </button>
        ))}
      </nav>
      {children}
    </div>
  );
}
