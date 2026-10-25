import { useEffect, useState } from "react";
import { signOut } from "../lib/uiSession";

// PendingView is /ui/pending: a person an identity provider let in, whose
// account an operator has not approved yet (RFC FA §3.7a). The session they
// hold reaches nothing but GET /v1/_me, so this page is all they see until an
// operator approves them in the Users console.
export default function PendingView() {
  const [subject, setSubject] = useState("");
  const [approved, setApproved] = useState(false);
  useEffect(() => {
    fetch("/v1/_me", { credentials: "same-origin" })
      .then((r) => {
        if (r.status === 401) {
          window.location.replace("/ui/login");
          return null;
        }
        return r.ok ? r.json() : null;
      })
      .then((me: { subject?: string; auth?: { status?: string } } | null) => {
        if (!me) return;
        setSubject(me.subject ?? "");
        setApproved(me.auth?.status !== "pending");
      })
      .catch(() => {});
  }, []);
  return (
    <div className="login-page">
      <div className="login-card">
        <div className="login-brand">loomcycle</div>
        {approved ? (
          <>
            <h1 className="login-title">You are approved</h1>
            <a className="login-btn" href="/ui/">
              Continue
            </a>
          </>
        ) : (
          <>
            <h1 className="login-title">Awaiting approval</h1>
            <p className="login-sub">
              You are signed in{subject ? <> as <code>{subject}</code></> : null}, but an operator has not approved your
              account yet. You can reach nothing until they do; reload this page after they tell you.
            </p>
            <button className="login-btn" type="button" onClick={() => void signOut()}>
              Sign out
            </button>
          </>
        )}
      </div>
    </div>
  );
}
