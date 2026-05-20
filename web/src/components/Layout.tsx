import { useEffect, useState } from "react";
import { Link, NavLink, Outlet, useOutletContext } from "react-router-dom";
import { UserSummary, getHealth, listUsers } from "../api";
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
  // Fetched from /healthz once on mount. Falls back to the static
  // shipped version on failure (offline server or pre-v0.8.21 binary
  // that only returns {"ok":true}). Undefined while in-flight.
  const [version, setVersion] = useState<string | null>(null);

  useEffect(() => {
    localStorage.setItem(USER_ID_KEY, userId);
  }, [userId]);

  // Fetch the running binary's version once on mount. Not polled —
  // the version doesn't change without a process restart, at which
  // point the UI bundle is likely reloaded too.
  useEffect(() => {
    let cancelled = false;
    getHealth()
      .then((h) => {
        if (!cancelled) setVersion(h.version || "unknown");
      })
      .catch(() => {
        if (!cancelled) setVersion("offline");
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // Poll /v1/_users so the dropdown reflects who has active runs in
  // near-real-time. Server response is bounded (LIMIT 200) and fast.
  useEffect(() => {
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
  }, []);

  const knownUser = users.find((u) => u.user_id === userId);

  return (
    <div className="layout">
      <header className="topbar">
        <div className="brand">
          <Link to="/">loomcycle</Link>
          <span className="version">
            {version === null ? "…" : version}
          </span>
        </div>
        <nav className="nav-tabs">
          <NavLink to="/agents">runs</NavLink>
          <NavLink to="/interrupts">interrupts</NavLink>
          <NavLink to="/memory">memory</NavLink>
          <NavLink to="/snapshots">snapshots</NavLink>
          <NavLink to="/audit">audit</NavLink>
          <NavLink to="/activity">activity</NavLink>
        </nav>
        <PauseControls />
        <div className="user-picker">
          {usersErr && <span className="picker-err" title={usersErr}>users unavailable</span>}
          {!showManual && (
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
              <button
                type="button"
                className="manual-btn"
                title="Type a user_id manually (e.g. for users with no runs yet)"
                onClick={() => {
                  setDraft(userId);
                  setShowManual(true);
                }}
              >
                ✎
              </button>
            </>
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
        <Outlet context={{ userId }} />
      </main>
    </div>
  );
}

// Small helper: child routes import this to read userId.
export function useUserId(): string {
  const ctx = useOutletContext<{ userId: string }>();
  return ctx.userId;
}
