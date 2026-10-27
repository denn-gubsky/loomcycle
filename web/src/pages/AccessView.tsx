import { Navigate, useNavigate, useParams } from "react-router-dom";
import AccessConsole, { type AccessTab } from "../components/access/AccessConsole";
import { usePrincipal } from "../components/Layout";
import TokenManager from "../components/TokenManager";
import { visibleGroupItems } from "../components/navItems";
import { hasTenantScope as principalHasTenantScope, surfacesOf } from "../lib/visibility";
import UsersView from "./UsersView";

const ACL_TABS: ReadonlySet<string> = new Set<AccessTab>(["groups", "policies", "bindings", "simulator"]);

// AccessView is /access/:section — everything about who may do what in one
// place: the tenant's users and their tokens, the access-control groups,
// policies and bindings, the simulator, and operator token minting. The left
// nav's access group lists the sections; each is gated by the route it loads
// first, so a viewer sees the sections the server lets them use.
//
// An absent, unknown or not-permitted section opens the viewer's first one, so a
// pasted link never lands on a page whose every call is refused.
export default function AccessView() {
  const principal = usePrincipal();
  // A null principal = open mode / pre-resolution → admin-equivalent, as in
  // SectionPage; Layout renders this only once the principal has resolved.
  const isAdmin = !principal || principal.is_admin;
  const visible = visibleGroupItems(
    "g-access",
    isAdmin,
    principalHasTenantScope(principal?.scopes),
    surfacesOf(principal),
  );
  const { section } = useParams<{ section?: string }>();
  const navigate = useNavigate();

  if (visible.length === 0) {
    return (
      <div className="settings-view">
        <div className="settings-panel">
          <h2>Access</h2>
          <p className="settings-help">
            Nothing here is available to your access level. Access administers a tenant's users and
            permissions; your token holds neither.
          </p>
        </div>
      </div>
    );
  }
  const active = visible.find((it) => it.id === section)?.id;
  if (!active) return <Navigate to={visible[0].to} replace />;

  return (
    <div className="settings-view">
      <div className="settings-body">
        {/* One slot for every section: the console keeps its state (a policy
            draft on its way to the simulator) while its section changes. */}
        {ACL_TABS.has(active) ? (
          <AccessConsole tab={active as AccessTab} onTab={(t) => navigate("/access/" + t)} />
        ) : active === "users" ? (
          <UsersView />
        ) : (
          <TokenManager />
        )}
      </div>
    </div>
  );
}
