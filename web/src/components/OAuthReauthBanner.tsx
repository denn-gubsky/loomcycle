import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { CredentialScope, listCredentials } from "../api";
import { ReauthItem, reauthItems, reconnectHref } from "../lib/oauthConnect";

// An OAuth credential that lost its grant (a refused refresh, an expired
// code, a restore from a snapshot) refuses every call that names it until its
// owner connects it again, and nothing else on the page says so. This banner
// sits above every page while any credential the viewer manages is in
// needs_reauth: the viewer's own user-scope ones, and the tenant's when the
// viewer operates the tenant. It is derived from the credential lists the
// viewer may read anyway, so it can never show another tenant's or user's.
const POLL_MS = 60_000;

// CREDENTIALS_CHANGED is dispatched on window by the credentials page after it
// changes a credential, so the banner re-reads at once instead of a minute on.
export const CREDENTIALS_CHANGED = "loomcycle:credentials-changed";

export default function OAuthReauthBanner({ canManageTenant }: { canManageTenant: boolean }) {
  const [items, setItems] = useState<ReauthItem[]>([]);

  useEffect(() => {
    let cancelled = false;
    const scopes: CredentialScope[] = canManageTenant ? ["tenant", "user"] : ["user"];
    const poll = async () => {
      // One scope failing (not permitted, no secret key) leaves the other.
      const lists = await Promise.all(
        scopes.map((scope) =>
          listCredentials(scope).then(
            (r) => ({ scope, credentials: r.credentials ?? [] }),
            () => ({ scope, credentials: [] }),
          ),
        ),
      );
      if (!cancelled) setItems(reauthItems(lists));
    };
    void poll();
    const t = setInterval(poll, POLL_MS);
    window.addEventListener(CREDENTIALS_CHANGED, poll);
    return () => {
      cancelled = true;
      clearInterval(t);
      window.removeEventListener(CREDENTIALS_CHANGED, poll);
    };
  }, [canManageTenant]);

  return <OAuthReauthNotice items={items} />;
}

// OAuthReauthNotice renders the banner for items (nothing when empty).
export function OAuthReauthNotice({ items }: { items: ReauthItem[] }) {
  if (items.length === 0) return null;
  return (
    <div className="oauth-reauth-banner" role="alert">
      <strong>
        {items.length === 1 ? "An account connection needs" : "Account connections need"} to be connected again.
      </strong>{" "}
      Calls that use {items.length === 1 ? "it" : "them"} are refused until then.
      <ul>
        {items.map((it) => (
          <li key={`${it.scope}/${it.name}`}>
            <code>{it.name}</code> ({it.scope}){it.reason ? ` — ${it.reason}` : ""}{" "}
            <Link to={reconnectHref(it.scope)} className="ghost-btn">
              Reconnect
            </Link>
          </li>
        ))}
      </ul>
    </div>
  );
}
