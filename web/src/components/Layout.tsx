import { useEffect, useState } from "react";
import { Link, Outlet } from "react-router-dom";

const USER_ID_KEY = "loomcycle.userId";

export default function Layout() {
  // user_id is required for the run-list query; we store it in
  // localStorage so the operator doesn't have to retype it on every
  // navigation. The bearer token is in the HttpOnly cookie set by
  // the server's ?token=... redirect; we don't manage it here.
  const [userId, setUserId] = useState<string>(() => localStorage.getItem(USER_ID_KEY) ?? "");
  const [draft, setDraft] = useState(userId);

  useEffect(() => {
    localStorage.setItem(USER_ID_KEY, userId);
  }, [userId]);

  return (
    <div className="layout">
      <header className="topbar">
        <div className="brand">
          <Link to="/">loomcycle</Link>
          <span className="version">v0.7.3</span>
        </div>
        <form
          className="user-input"
          onSubmit={(e) => {
            e.preventDefault();
            setUserId(draft.trim());
          }}
        >
          <label htmlFor="user_id">user_id</label>
          <input
            id="user_id"
            type="text"
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            placeholder="paste a user_id…"
          />
          <button type="submit" disabled={draft.trim() === userId}>
            apply
          </button>
        </form>
      </header>
      <main className="content">
        <Outlet context={{ userId }} />
      </main>
    </div>
  );
}

// Small helper: child routes import this to read userId.
import { useOutletContext } from "react-router-dom";
export function useUserId(): string {
  const ctx = useOutletContext<{ userId: string }>();
  return ctx.userId;
}
