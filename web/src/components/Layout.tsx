import { useEffect, useState } from "react";
import { Link, NavLink, Outlet, useOutletContext } from "react-router-dom";
import { Principal, UserSummary, getHealth, getWhoami, listUsers } from "../api";
import PauseControls from "./PauseControls";

const USER_ID_KEY = "loomcycle.userId";
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
  const [showManual, setShowManual] = useState(false);
  const [draft, setDraft] = useState(userId);
  const [version, setVersion] = useState<string | null>(null);

  // The authenticated principal (RFC L / multi-tenant UI authz), resolved
  // from GET /v1/_me on boot. Drives the role: super-admin (is_admin) sees
  // all tenants + every tab; a tenant sees only its own workspace. A 401
  // redirects to /login inside the api layer, so a failure here is a
  // non-auth error. `undefined` = still loading.
  const [principal, setPrincipal] = useState<Principal | null | undefined>(undefined);
  const [principalErr, setPrincipalErr] = useState<string | null>(null);
  const isAdmin = principal?.is_admin === true;

  useEffect(() => {
    localStorage.setItem(USER_ID_KEY, userId);
  }, [userId]);

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

  // Poll /v1/_users for the picker — ADMIN ONLY. /v1/_users is operator-
  // admin-gated, so a tenant token would just 403; skip it (a tenant uses
  // its own subject + the manual-entry box for other same-tenant users,
  // which the per-user reads tenant-filter server-side).
  useEffect(() => {
    if (!isAdmin) {
      setUsers([]);
      setUsersErr(null);
      return;
    }
    let cancelled = false;
    const fetchOnce = async () => {
      try {
        const resp = await listUsers();
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
  }, [isAdmin]);

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

  const knownUser = users.find((u) => u.user_id === userId);

  return (
    <div className="layout">
      <header className="topbar">
        <div className="brand">
          <Link to="/">loomcycle</Link>
          <span className="version">{version === null ? "…" : version}</span>
        </div>
        <nav className="nav-tabs">
          {/* runs is every role's workspace; the rest are operator-global /
              admin surfaces, hidden for a tenant (and 403 server-side). */}
          <NavLink to="/agents">runs</NavLink>
          {isAdmin && (
            <>
              <NavLink to="/library/agents">library</NavLink>
              <NavLink to="/channels">channels</NavLink>
              <NavLink to="/schedules">schedules</NavLink>
              <NavLink to="/interrupts">interrupts</NavLink>
              <NavLink to="/memory">memory</NavLink>
              <NavLink to="/snapshots">snapshots</NavLink>
              <NavLink to="/audit">audit</NavLink>
              <NavLink to="/activity">activity</NavLink>
            </>
          )}
        </nav>
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
        <div className="user-picker">
          {usersErr && (
            <span className="picker-err" title={usersErr}>
              users unavailable
            </span>
          )}
          {!showManual && isAdmin && (
            <>
              <label htmlFor="user_select">user</label>
              <select
                id="user_select"
                value={knownUser ? userId : ""}
                onChange={(e) => setUserId(e.target.value)}
              >
                <option value="">— pick a user —</option>
                {users.map((u) => (
                  <option key={u.user_id} value={u.user_id}>
                    {u.user_id} · {u.running_count > 0 ? `${u.running_count} running` : `${u.total_count} runs`}
                  </option>
                ))}
              </select>
            </>
          )}
          {!showManual && (
            <button
              type="button"
              className="manual-btn"
              title="Type a user_id manually"
              onClick={() => {
                setDraft(userId);
                setShowManual(true);
              }}
            >
              ✎
            </button>
          )}
          {showManual && (
            <form
              onSubmit={(e) => {
                e.preventDefault();
                setUserId(draft.trim());
                setShowManual(false);
              }}
            >
              <input
                type="text"
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                placeholder="paste a user_id…"
                autoFocus
              />
              <button type="submit">apply</button>
              <button type="button" onClick={() => setShowManual(false)}>
                cancel
              </button>
            </form>
          )}
        </div>
      </header>
      <main className="content">
        <Outlet context={{ userId, principal: principal ?? null }} />
      </main>
    </div>
  );
}

// Child routes read userId via this helper.
export function useUserId(): string {
  const ctx = useOutletContext<{ userId: string; principal: Principal | null }>();
  return ctx.userId;
}

// usePrincipal exposes the resolved identity to child views (role-aware
// rendering). Null only in the brief pre-resolution window or a non-auth
// error (the Layout gates rendering on it, so views generally see it set).
export function usePrincipal(): Principal | null {
  const ctx = useOutletContext<{ userId: string; principal: Principal | null }>();
  return ctx.principal;
}
