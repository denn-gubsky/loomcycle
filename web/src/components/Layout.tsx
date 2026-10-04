import { canSee, hasTenantScope as principalHasTenantScope } from "../lib/visibility";
import { useEffect, useState } from "react";
import { Link, NavLink, Outlet, useOutletContext } from "react-router-dom";
import { Moon, PanelLeftClose, PanelLeftOpen, Sun } from "lucide-react";
import { Principal, UserSummary, getHealth, getWhoami, listUsers } from "../api";
import { useTheme } from "../hooks/useTheme";
import PauseControls from "./PauseControls";
import { type UserOption, appliedUserId, filterUsers, userOptions } from "../lib/userOptions";
import { NAV_ITEMS } from "./navItems";
import Combobox, { type ComboRow } from "./Combobox";
import SettingsMenu from "./SettingsMenu";
import TenantCombobox from "./TenantCombobox";

const USER_ID_KEY = "loomcycle.userId";
const SIDEBAR_KEY = "loomcycle.sidebar.collapsed";

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
