import {
  canSee,
  hasTenantScope as principalHasTenantScope,
  type Visibility,
} from "../lib/visibility";
import { useEffect, useState } from "react";
import { Link, NavLink, Outlet, useOutletContext } from "react-router-dom";
import {
  Activity,
  Bell,
  Brain,
  CalendarClock,
  Camera,
  Coins,
  FolderTree,
  HardDrive,
  KeyRound,
  Library,
  ListTree,
  type LucideIcon,
  Moon,
  PanelLeftClose,
  PanelLeftOpen,
  Play,
  Plug,
  Radio,
  ScrollText,
  Sun,
  Users,
  Workflow,
} from "lucide-react";
import { Principal, UserSummary, getHealth, getWhoami, listUsers } from "../api";
import { useTheme } from "../hooks/useTheme";
import PauseControls from "./PauseControls";
import { type UserOption, appliedUserId, filterUsers, userOptions } from "../lib/userOptions";
import Combobox, { type ComboRow } from "./Combobox";
import SettingsMenu from "./SettingsMenu";
import TenantCombobox from "./TenantCombobox";

const USER_ID_KEY = "loomcycle.userId";
const SIDEBAR_KEY = "loomcycle.sidebar.collapsed";

// Left-sidebar navigation model (RFC AS §4 — per-surface visibility class,
// replacing the old binary `adminOnly`):
//   "all"    — every authenticated role (run/runs: the principal-scoped workspace).
//   "tenant" — admin OR a substrate:tenant operator. The surface's reads are
//              tenant-scoped server-side (the operator sees only its own tenant)
//              and its writes are already reachable by substrate:tenant (RFC AF),
//              so the item lights up once it's visible.
//   "admin"  — super-admin only (operator plane / no per-tenant axis).
//
// A "tenant" item is ONLY assigned where the backing route gate actually admits
// substrate:tenant (requiredScopeFor → ScopeTenant or a tenantImplied scope):
// library (#575/#577), integrations + schedules (the *def/names + scheduledef
// def plane, #576 / isTenantConfinedDefPath), volumes (/v1/_volumes), paths
// (/v1/_path), interrupts (/v1/users/{id}/interrupts — runs:read, tenantImplied).
// memory (/v1/_memory/*) is "tenant": RFC BL gave memory rows a tenant_id, every
// handler sources the tenant from the principal (a tenant operator sees only its
// own tenant's memory), and RFC BV re-gated the routes to ScopeTenant — so the
// item lights up for a tenant operator. channels (/v1/_channels) is now "tenant"
// too: migration 0066 gave channel_messages/_cursors/channels a tenant_id, the
// handlers source the tenant from the principal (list filters by tenant; the
// cross-tenant `global` scope stays admin-only to create), and the routes were
// re-gated to ScopeTenant — closing the earlier admin-only carve-out.
// The type and the predicate live in ../lib/visibility — the Settings tabs use the
// same two, and a second copy is how the two surfaces drifted apart in the first place.
interface NavItem {
  to: string;
  label: string;
  Icon: LucideIcon;
  vis: Visibility;
}
const NAV_ITEMS: NavItem[] = [
  { to: "/run", label: "run", Icon: Play, vis: "all" },
  { to: "/agents", label: "runs", Icon: ListTree, vis: "all" },
  // RFC CN — every login (incl. an isolated substrate:user user) can self-serve
  // its OWN scope=user API tokens here; the operator Settings → Credentials tab
  // (tenant authoring) stays separate.
  { to: "/my-credentials", label: "credentials", Icon: KeyRound, vis: "all" },
  { to: "/library/agents", label: "library", Icon: Library, vis: "tenant" },
  { to: "/integrations/webhooks", label: "integrations", Icon: Plug, vis: "tenant" },
  { to: "/volumes/persistent", label: "volumes", Icon: HardDrive, vis: "tenant" },
  { to: "/paths", label: "paths", Icon: FolderTree, vis: "tenant" },
  { to: "/channels", label: "channels", Icon: Radio, vis: "tenant" },
  { to: "/schedules", label: "schedules", Icon: CalendarClock, vis: "tenant" },
  { to: "/teams", label: "teams", Icon: Workflow, vis: "tenant" },
  // users: the RFC BX P2c tenant-operator console — manage first-class users +
  // mint/revoke their bearer tokens. "tenant": the /v1/_users(/…) routes are
  // ScopeTenant and the handlers confine to the caller's own tenant (a tenant
  // operator sees only its own users; admin sees all + ?tenant= focus).
  { to: "/users", label: "users", Icon: Users, vis: "tenant" },
  { to: "/interrupts", label: "interrupts", Icon: Bell, vis: "tenant" },
  { to: "/memory", label: "memory", Icon: Brain, vis: "tenant" },
  { to: "/snapshots", label: "snapshots", Icon: Camera, vis: "admin" },
  // audit is tenant-visible (RFC AS): handleListEvents tenant-scopes the result
  // via the event's owning session, so a tenant sees only its own events.
  { to: "/audit", label: "audit", Icon: ScrollText, vis: "tenant" },
  // usage: token/cost report. Tenant-visible (RFC AV): GET /v1/_usage is
  // ScopeTenant-gated and the handler tenant-scopes the aggregation (a tenant
  // operator sees only its own tenant's spend; admin sees all + ?tenant=).
  { to: "/usage", label: "usage", Icon: Coins, vis: "tenant" },
  { to: "/activity", label: "activity", Icon: Activity, vis: "admin" },
  // NOTE: routing + limits moved OUT of the main nav INTO Settings tabs (the
  // /routing + /limits routes still exist for deep links, but SettingsView is
  // now the primary surface — see SettingsView's routing/limits tabs).
];

function userRow(u: UserOption): ComboRow {
  if (u.id === "") return { id: "", label: "no user", quiet: true };
  return { id: u.id, label: u.id, hint: u.hint };
}

// Refresh the user picker every 30 s. Activity stats (running counts)
// drift fast on busy deployments; the dropdown is rendered with the
// most recent counts each time it opens.
const REFRESH_MS = 30_000;

export default function Layout() {
  // user_id is the gating context for the run-list query. Persisted in
  // localStorage so the operator doesn't have to re-pick on every
  // navigation. The bearer token is in the HttpOnly cookie set by the
  // server's ?token=... redirect; we don't manage it here.
  const [userId, setUserId] = useState<string>(() => localStorage.getItem(USER_ID_KEY) ?? "");
  const [users, setUsers] = useState<UserSummary[]>([]);
  const [usersErr, setUsersErr] = useState<string | null>(null);
  // What the user field shows: the applied userId, or text being typed that has
  // not been applied yet.
  const [draftUser, setDraftUser] = useState(userId);
  const [version, setVersion] = useState<string | null>(null);
  const { theme, toggle: toggleTheme } = useTheme();

  // The authenticated principal (RFC L / multi-tenant UI authz), resolved
  // from GET /v1/_me on boot. Drives the role: super-admin (is_admin) sees
  // all tenants + every tab; a tenant sees only its own workspace. A 401
  // redirects to /login inside the api layer, so a failure here is a
  // non-auth error. `undefined` = still loading.
  const [principal, setPrincipal] = useState<Principal | null | undefined>(undefined);
  const [principalErr, setPrincipalErr] = useState<string | null>(null);
  const isAdmin = principal?.is_admin === true;
  // RFC AS §4: a substrate:tenant operator additionally sees the tenant-scoped
  // surfaces. Admin, legacy, and open-mode principals all report is_admin:true
  // (handleWhoami), so they already see every item via canSee's admin branch
  // — no open-mode special case needed here.
  const hasTenantScope = principalHasTenantScope(principal?.scopes);

  // Super-admin tenant-focus (?tenant=): "" = all tenants (admin's default
  // global view). A tenant principal can't set this — the backend forces
  // its own tenant regardless — so it stays "" for non-admins and the
  // switcher is admin-only. Threaded into the user picker + the runs view.
  const [focusTenant, setFocusTenant] = useState<string>("");
  const [draftTenant, setDraftTenant] = useState<string>("");

  // Left-sidebar collapse: icons-only (collapsed) vs icons+labels. Persisted
  // so the operator's choice survives navigation and reload.
  const [navCollapsed, setNavCollapsed] = useState<boolean>(
    () => localStorage.getItem(SIDEBAR_KEY) === "1",
  );

  useEffect(() => {
    localStorage.setItem(USER_ID_KEY, userId);
    // userId also changes from outside the field (a tenant-focus switch resets
    // it; a tenant's own subject is defaulted on boot) — keep the field in step.
    setDraftUser(userId);
  }, [userId]);

  useEffect(() => {
    localStorage.setItem(SIDEBAR_KEY, navCollapsed ? "1" : "0");
  }, [navCollapsed]);

  // Resolve identity first — everything below branches on the role.
  useEffect(() => {
    let cancelled = false;
    getWhoami()
      .then((p) => {
        if (cancelled) return;
        setPrincipal(p);
        // A tenant's workspace defaults to its own subject's runs so the
        // runs view is populated immediately without picking a user.
        if (!p.is_admin && !p.open_mode) {
          setUserId((cur) => cur || p.subject);
        }
      })
      .catch((e) => {
        if (!cancelled) {
          setPrincipal(null);
          setPrincipalErr(e instanceof Error ? e.message : String(e));
        }
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // Fetch the running binary's version once on mount.
  useEffect(() => {
    let cancelled = false;
    getHealth()
      .then((h) => !cancelled && setVersion(h.version || "unknown"))
      .catch(() => !cancelled && setVersion("offline"));
    return () => {
      cancelled = true;
    };
  }, []);

  // Poll /v1/_users for the picker. Since v0.16.x /v1/_users is tenant-
  // scoped (any authenticated principal): a tenant sees only its own
  // tenant's users; an admin sees all, or one tenant via ?tenant= when
  // focused. Wait for the principal to resolve before the first fetch.
  useEffect(() => {
    if (!principal) {
      setUsers([]);
      setUsersErr(null);
      return;
    }
    let cancelled = false;
    const fetchOnce = async () => {
      try {
        // focusTenant only takes effect for admins (ignored server-side
        // for tenants); "" → the caller's own scope (all for admin).
        const resp = await listUsers(focusTenant || undefined);
        if (!cancelled) {
          setUsers(resp.users ?? []);
          setUsersErr(null);
        }
      } catch (e) {
        if (!cancelled) setUsersErr(e instanceof Error ? e.message : String(e));
      }
    };
    fetchOnce();
    const t = setInterval(fetchOnce, REFRESH_MS);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, [principal, focusTenant]);

  // Identity gate: hold the shell until we know the role (a 401 has
  // already redirected to /login by here). A non-auth failure shows a
  // clear error rather than a half-rendered, mis-scoped UI.
  if (principal === undefined && principalErr === null) {
    return <div className="auth-splash">Authenticating…</div>;
  }
  if (principalErr !== null) {
    return (
      <div className="auth-splash auth-error">
        Could not load your identity: {principalErr}
        <div>
          <a href="/ui/login">Sign in again</a>
        </div>
      </div>
    );
  }

  const knownUsers = userOptions(users);

  return (
    <div className="layout">
      <aside className={"sidebar" + (navCollapsed ? " sidebar-collapsed" : "")}>
        {/* Per-surface visibility (RFC AS §4): run/runs for every role; the
            tenant-scoped surfaces for admin OR a substrate:tenant operator;
            the operator-plane surfaces for admin only. canSee encodes the
            class → role gate; the server still enforces it (defence in depth). */}
        <nav className="sidebar-nav">
          {NAV_ITEMS.filter((it) => canSee(it.vis, isAdmin, hasTenantScope)).map(({ to, label, Icon }) => (
            <NavLink key={to} to={to} title={label}>
              <Icon size={18} className="sidebar-icon" />
              <span className="sidebar-label">{label}</span>
            </NavLink>
          ))}
        </nav>
        <button
          type="button"
          className="sidebar-toggle"
          title={navCollapsed ? "Expand menu" : "Collapse menu"}
          onClick={() => setNavCollapsed((c) => !c)}
        >
          {navCollapsed ? <PanelLeftOpen size={18} /> : <PanelLeftClose size={18} />}
          {!navCollapsed && <span className="sidebar-label">collapse</span>}
        </button>
      </aside>
      <div className="main-col">
        <header className="topbar">
          <div className="brand">
            <Link to="/" aria-label="loomcycle home">
              {/* Wordmark from web/public (served under the Vite base "/ui/").
                  Two variants — the wordmark is recoloured per theme so it reads
                  on the topbar; the loom-mark keeps its brand colours in both.
                  light → black-ink wordmark, dark → near-white wordmark. */}
              <img
                src={theme === "light" ? "/ui/loomcycle-logo-light.svg" : "/ui/loomcycle-logo.svg"}
                alt="loomcycle"
                className="brand-logo"
              />
            </Link>
            <span className="version">{version === null ? "…" : version}</span>
          </div>
          <button
            type="button"
            className="theme-toggle"
            onClick={toggleTheme}
            title={theme === "dark" ? "Switch to light theme" : "Switch to dark theme"}
            aria-label="Toggle color theme"
          >
            {theme === "dark" ? <Sun size={16} /> : <Moon size={16} />}
          </button>
          {isAdmin && <PauseControls />}
          {/* Role/tenant badge — super-admin sees all tenants; a tenant is
              scoped to its own. */}
          {principal && (
            <span
              className={"role-badge " + (isAdmin ? "role-admin" : "role-tenant")}
              title={`subject: ${principal.subject}`}
            >
              {isAdmin ? "super-admin" : `tenant: ${principal.tenant_id}`}
            </span>
          )}
          {/* Super-admin tenant-focus switcher: narrows the workspace to one
              tenant (or all when blank). Changing focus resets the picked
              user since user_ids don't carry across tenants. Admin-only —
              tenants are locked to their own tenant by the backend. */}
          {isAdmin && (
            <form
              className="tenant-switcher"
              onSubmit={(e) => {
                e.preventDefault();
                setFocusTenant(draftTenant.trim());
                setUserId("");
              }}
            >
              <label htmlFor="tenant_focus">tenant</label>
              <TenantCombobox
                id="tenant_focus"
                value={draftTenant}
                onChange={setDraftTenant}
                onPick={(t) => {
                  // Picking a row is a complete answer — apply it without a
                  // second Enter. "" (the "all tenants" row) clears the focus.
                  setDraftTenant(t);
                  setFocusTenant(t);
                  setUserId("");
                }}
                enabled={isAdmin}
                placeholder="all tenants"
                title="Focus one tenant's workspace; blank = all"
              />
              {focusTenant && (
                <button
                  type="button"
                  className="manual-btn"
                  title="Clear tenant focus (show all)"
                  onClick={() => {
                    setFocusTenant("");
                    setDraftTenant("");
                    setUserId("");
                  }}
                >
                  ✕
                </button>
              )}
            </form>
          )}
          {/* User field: a combobox over the users this tenant scope has seen
              (GET /v1/_users, already narrowed by the tenant focus). Picking a
              row applies it; so does typing ANY id and pressing Enter — the list
              is derived from runs, so a subject with no runs yet (or one whose
              documents the operator needs to reach) can only be typed. The
              "no user" row, or Enter on a blank field, clears it. */}
          <form
            className="user-picker"
            onSubmit={(e) => {
              e.preventDefault();
              setUserId(appliedUserId(draftUser));
            }}
          >
            {usersErr && (
              <span className="picker-err" title={usersErr}>
                users unavailable
              </span>
            )}
            <label htmlFor="user_select">user</label>
            <Combobox
              id="user_select"
              value={draftUser}
              onChange={setDraftUser}
              onPick={setUserId}
              rows={(q) => filterUsers(knownUsers, q).map(userRow)}
              // Unapplied text is discarded on leaving the field, so what it
              // shows at rest is always the user the views are actually scoped
              // to — never a half-typed id that was not applied.
              onBlur={() => setDraftUser(userId)}
              placeholder="pick or type a user id"
              title="Pick a known user, or type any user id and press Enter"
              listLabel="Known users"
              toggleLabel="Show known users"
            />
          </form>
          {/* Settings menu — rightmost. The gear opens the viewer's Settings
              sections (filtered by role, exactly as the hub's tabs are) and ends
              in Log out. Rendered for EVERY role: Log out lives only here, so a
              delegated user with no section still gets the menu. The backend
              gates each surface server-side (defence in depth). */}
          <SettingsMenu isAdmin={isAdmin} hasTenantScope={hasTenantScope} />
        </header>
        <main className="content">
          <Outlet context={{ userId, principal: principal ?? null, focusTenant }} />
        </main>
      </div>
    </div>
  );
}

interface LayoutContext {
  userId: string;
  principal: Principal | null;
  focusTenant: string;
}

// Child routes read userId via this helper.
export function useUserId(): string {
  return useOutletContext<LayoutContext>().userId;
}

// usePrincipal exposes the resolved identity to child views (role-aware
// rendering). Null only in the brief pre-resolution window or a non-auth
// error (the Layout gates rendering on it, so views generally see it set).
export function usePrincipal(): Principal | null {
  return useOutletContext<LayoutContext>().principal;
}

// useFocusTenant is the super-admin tenant-focus (?tenant=); "" = all
// tenants / the caller's own scope. Views thread it into tenant-scoped
// reads (e.g. listAgents) so the admin's switcher narrows the workspace.
export function useFocusTenant(): string {
  return useOutletContext<LayoutContext>().focusTenant;
}
