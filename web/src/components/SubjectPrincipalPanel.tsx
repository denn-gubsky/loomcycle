import { useEffect, useState } from "react";
import { bindSubjectPrincipal, loadSubjectPrincipal } from "../api";
import ErrorNotice from "./ErrorNotice";

// SubjectPrincipalPanel is the operator's "this subject is that user" control on a
// tenant document's page. Erasing the bound user finds the subject, reports it,
// clears the binding and announces it; the subject itself is never deleted. As with
// the merge panel, the server decides whether the open document is a subject (and
// refuses `not_a_subject` otherwise) and whether the user is registered in the
// tenant; the panel is offered where a binding can be asked for — a tenant
// document, to an operator (canMergeSubjects decides that for both).

/** The bound state and its two actions, without the loading: rendered as is by
 *  the panel and by its tests. */
export function SubjectPrincipalControl({
  principal,
  busy,
  onBind,
  onClear,
}: {
  /** The bound user; "" when the subject is not bound. */
  principal: string;
  busy: boolean;
  onBind: (principal: string) => void;
  onClear: () => void;
}) {
  const [user, setUser] = useState("");
  if (principal) {
    return (
      <div className="subject-principal-row">
        <span>
          Bound to user <code>{principal}</code>
        </span>
        <button type="button" onClick={onClear} disabled={busy}>
          {busy ? "Clearing…" : "Clear binding"}
        </button>
      </div>
    );
  }
  return (
    <details className="subject-principal">
      <summary>Bind to a user…</summary>
      <p className="settings-muted">
        Record which user of the tenant this subject is. Erasing that user then reports this subject,
        clears the binding and announces it; the subject and what the tenant knows about it are kept.
      </p>
      <div className="subject-principal-row">
        <input
          type="text"
          value={user}
          placeholder="User: subject id (u-ada)"
          onChange={(e) => setUser(e.target.value)}
          disabled={busy}
        />
        <button type="button" onClick={() => onBind(user.trim())} disabled={busy || user.trim() === ""}>
          {busy ? "Binding…" : "Bind"}
        </button>
      </div>
    </details>
  );
}

export default function SubjectPrincipalPanel({ documentId, tenant }: { documentId: string; tenant?: string }) {
  // null until the current binding is read; a read that fails leaves it unknown
  // and shows the error, rather than offering a bind over a binding we did not see.
  const [principal, setPrincipal] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  useEffect(() => {
    let live = true;
    setPrincipal(null);
    setError(null);
    loadSubjectPrincipal(documentId, tenant).then(
      (p) => live && setPrincipal(p),
      (e) => live && setError(e),
    );
    return () => {
      live = false;
    };
  }, [documentId, tenant]);

  async function apply(next: string) {
    setBusy(true);
    setError(null);
    try {
      const r = await bindSubjectPrincipal(documentId, next, tenant);
      setPrincipal(r.principal);
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  }

  function clear() {
    // An erasure of the bound user would no longer find this subject: ask once more.
    if (!window.confirm(`Clear this subject's binding to ${principal}? Erasing that user will no longer find this subject.`)) {
      return;
    }
    void apply("");
  }

  return (
    <div className="subject-principal-panel">
      {principal !== null && (
        <SubjectPrincipalControl principal={principal} busy={busy} onBind={(p) => void apply(p)} onClear={clear} />
      )}
      <ErrorNotice error={error} />
    </div>
  );
}
