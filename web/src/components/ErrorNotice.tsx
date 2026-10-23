import { ApiError, denyingReasons, forbiddenKind } from "../lib/apiError";

// ErrorNotice renders a thrown API error. A "you may not" refusal (403
// acl_denied / insufficient_scope / acl_self_elevation) reads as a refusal —
// with the action, resource and denying rules for acl_denied — rather than as
// a crash; anything else is the usual error line. The message text itself is
// already readable (lib/apiError), so a page that renders `e.message` alone
// loses nothing but the layout.
export default function ErrorNotice({ error }: { error: unknown }) {
  if (error === null || error === undefined || error === "") return null;
  const kind = forbiddenKind(error);
  if (kind === "acl_denied" && error instanceof ApiError && error.acl) {
    const d = error.acl;
    const denies = denyingReasons(d);
    return (
      <div className="acl-denied" role="alert">
        <div className="acl-denied-head">Access denied</div>
        <div>
          You may not <code>{d.action || "?"}</code> <code>{d.resource || "?"}</code>.
        </div>
        {denies.length > 0 ? (
          <ul className="acl-denied-reasons">
            {denies.map((r, i) => (
              <li key={i}>
                denied by list <code>{r.acl}</code>
                {r.version ? <span className="settings-muted"> v{r.version}</span> : null}, rule {r.rule}
              </li>
            ))}
          </ul>
        ) : (
          <div className="settings-muted">No rule allows it.</div>
        )}
      </div>
    );
  }
  const message = error instanceof Error ? error.message : String(error);
  if (kind) {
    return (
      <div className="acl-denied" role="alert">
        <div className="acl-denied-head">{kind === "acl_self_elevation" ? "Refused" : "Not permitted"}</div>
        <div>{message.replace(/^\d{3} [^:]*: /, "")}</div>
      </div>
    );
  }
  return <div className="settings-error">{message}</div>;
}
