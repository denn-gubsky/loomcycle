import { useEffect, useState } from "react";
import { endSession } from "../lib/uiSession";

// LogoutView ends the Web UI session. /ui/logout is a plain navigation (the
// settings menu links to it), and a GET must not change state, so this page
// does the POST /v1/_auth/logout itself — which revokes the session on the
// server, clears the cookies and sends Clear-Site-Data — and then goes to the
// login page.
export default function LogoutView() {
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    let live = true;
    endSession().then((ok) => {
      if (!live) return;
      if (ok) {
        window.location.replace("/ui/login");
      } else {
        setFailed(true);
      }
    });
    return () => {
      live = false;
    };
  }, []);
  return (
    <div className="login-page">
      <div className="login-card">
        <div className="login-brand">loomcycle</div>
        {failed ? (
          <>
            <p className="login-error" role="alert">
              The session could not be ended on the server. Try again; until it succeeds, this browser stays signed in.
            </p>
            <button className="login-btn" type="button" onClick={() => window.location.reload()}>
              Try again
            </button>
          </>
        ) : (
          <p className="login-sub">Signing out…</p>
        )}
      </div>
    </div>
  );
}
