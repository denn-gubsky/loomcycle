import { useState } from "react";
import { signOut } from "../lib/uiSession";

// LogoutView is /ui/logout, kept for bookmarks and old links. It does NOT sign
// out on load: any site can link here, and the page's own same-origin POST
// would carry the session cookie, so loading it must change nothing. It asks
// first, and the button sends POST /v1/_auth/logout. The settings menu's "Log
// out" signs out directly (a click on this server's own page).
export default function LogoutView() {
  const [failed, setFailed] = useState(false);
  const confirm = async () => {
    setFailed(!(await signOut()));
  };
  return (
    <div className="login-page">
      <div className="login-card">
        <div className="login-brand">loomcycle</div>
        <h1 className="login-title">Sign out?</h1>
        <p className="login-sub">This ends your session on the server.</p>
        {failed && (
          <p className="login-error" role="alert">
            The session could not be ended on the server. Try again; until it succeeds, this browser stays signed in.
          </p>
        )}
        <button className="login-btn" type="button" onClick={confirm}>
          Sign out
        </button>
        <a className="login-hint" href="/ui">
          Stay signed in
        </a>
      </div>
    </div>
  );
}
