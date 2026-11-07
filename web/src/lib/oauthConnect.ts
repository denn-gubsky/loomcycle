import type {
  CredentialMeta,
  CredentialScope,
  OAuthConnectStart,
  OAuthCredentialRef,
  OAuthState,
} from "../api";
import { ApiError } from "./apiError";
import { refusalText } from "./cryptoKeys";

// An OAuth credential (usage "oauth") is connected by its owner approving a
// device code at the authorization server. The server runs the token poll;
// this page only shows the code and watches the credential's state. The
// helpers here are the connect flow without React, so it can be tested on a
// fake clock.

// The authorization servers seen so far issue codes for about five minutes,
// and every new start replaces the code before it, so an owner who pasted an
// old one is refused ("activation denied") with no hint why.
export const OAUTH_CODE_HINT = "Codes last 5 minutes; use the newest.";

// SLOW_DOWN_STEP is what a slow_down adds to the poll spacing (RFC 8628 §3.5),
// mirrored here for a status read the server answered with "too many".
export const SLOW_DOWN_STEP_S = 5;
const DEFAULT_INTERVAL_S = 5;
const MAX_INTERVAL_S = 60;

export function isOAuthCredential(m: CredentialMeta): boolean {
  return m.usage === "oauth";
}

export function oauthStateOf(m: CredentialMeta): OAuthState | undefined {
  return isOAuthCredential(m) ? m.oauth?.state : undefined;
}

export interface StateBadge {
  label: string;
  className: string;
  title: string;
}

// stateBadge is the pill shown for an OAuth credential's state.
export function stateBadge(state: OAuthState | undefined): StateBadge {
  switch (state) {
    case "connected":
      return { label: "connected", className: "status-ok", title: "a grant is held; its access token is sent where the credential is named" };
    case "pending_connect":
      return { label: "pending connect", className: "status-warn", title: "not connected yet, or a connect is waiting for approval" };
    case "needs_reauth":
      return { label: "needs reauth", className: "status-bad", title: "the grant was lost; its owner connects it again" };
    case "revoked":
      return { label: "disconnected", className: "status-retired", title: "disconnected by its owner; connect it again to use it" };
    default:
      return { label: "unknown", className: "status-retired", title: "the server reported no state" };
  }
}

// connectLabel is the connect button's word for a credential in state.
export function connectLabel(state: OAuthState | undefined): string {
  return state === "connected" || state === "needs_reauth" ? "Reconnect" : "Connect account";
}

// httpsLink returns u when it is an https URL, else null. The link comes from
// the authorization server's answer, so a javascript: or http: value is shown
// as text, never made clickable.
export function httpsLink(u: string | undefined): string | null {
  if (!u) return null;
  try {
    return new URL(u).protocol === "https:" ? u : null;
  } catch {
    return null;
  }
}

// ReauthItem is one OAuth credential the viewer manages that needs its owner
// to connect it again.
export interface ReauthItem {
  scope: CredentialScope;
  name: string;
  reason?: string;
}

// reauthItems picks the credentials in needs_reauth from the lists the viewer
// may read, tenant scope first.
export function reauthItems(lists: { scope: CredentialScope; credentials: CredentialMeta[] }[]): ReauthItem[] {
  const out: ReauthItem[] = [];
  for (const l of lists) {
    for (const c of l.credentials) {
      if (oauthStateOf(c) === "needs_reauth") {
        out.push({ scope: l.scope, name: c.name, ...(c.oauth?.last_error ? { reason: c.oauth.last_error } : {}) });
      }
    }
  }
  return out;
}

// reconnectHref is the page an owner reconnects a credential of scope on: the
// tenant credentials page for a tenant one, the viewer's own page for theirs.
export function reconnectHref(scope: CredentialScope): string {
  return scope === "tenant" ? "/credentials" : "/my-credentials";
}

export type ConnectPhase =
  | { kind: "starting" }
  | {
      kind: "waiting";
      userCode: string;
      // The link to open (the complete form, which carries the code, when the
      // server gives one and it is https), and the plain one to show.
      link: string | null;
      verificationURI: string;
      expiresAt: number;
    }
  | { kind: "connected"; connectedAs?: string }
  | { kind: "expired" }
  | { kind: "failed"; reason: string };

export interface ConnectDeps {
  start(ref: OAuthCredentialRef): Promise<OAuthConnectStart>;
  status(ref: OAuthCredentialRef): Promise<CredentialMeta>;
  // sleep waits ms, or returns early once signal aborts.
  sleep(ms: number, signal: AbortSignal): Promise<void>;
  now(): number;
}

// abortableSleep is ConnectDeps.sleep for the browser.
export function abortableSleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) return resolve();
    const t = setTimeout(done, ms);
    function done() {
      clearTimeout(t);
      signal.removeEventListener("abort", done);
      resolve();
    }
    signal.addEventListener("abort", done);
  });
}

function messageOf(e: unknown): string {
  return refusalText(e instanceof Error ? e.message : String(e));
}

// retryable is a status read that failed for a reason that passes: the
// server asked to slow down, is unavailable, or the network dropped.
function retryable(e: unknown): boolean {
  if (e instanceof ApiError) return e.status === 429 || e.status >= 500;
  return true;
}

// runOAuthConnect starts a connect for ref and watches it to an end, telling
// onPhase each step. It reads the status once per the server's interval,
// spacing the reads further on a slow_down or a passing failure, and stops at
// connected, a refusal, the code's expiry, or when signal aborts (a newer
// start, or the page going away) — then it reports nothing more.
export async function runOAuthConnect(
  ref: OAuthCredentialRef,
  deps: ConnectDeps,
  onPhase: (p: ConnectPhase) => void,
  signal: AbortSignal,
): Promise<void> {
  const report = (p: ConnectPhase) => {
    if (!signal.aborted) onPhase(p);
  };
  report({ kind: "starting" });
  let start: OAuthConnectStart;
  try {
    start = await deps.start(ref);
  } catch (e) {
    report({ kind: "failed", reason: messageOf(e) });
    return;
  }
  if (signal.aborted) return;
  let interval = start.interval > 0 ? Math.min(start.interval, MAX_INTERVAL_S) : DEFAULT_INTERVAL_S;
  const expiresAt = deps.now() + Math.max(start.expires_in, 1) * 1000;
  report({
    kind: "waiting",
    userCode: start.user_code,
    link: httpsLink(start.verification_uri_complete) ?? httpsLink(start.verification_uri),
    verificationURI: start.verification_uri,
    expiresAt,
  });
  for (;;) {
    await deps.sleep(interval * 1000, signal);
    if (signal.aborted) return;
    let meta: CredentialMeta;
    try {
      meta = await deps.status(ref);
    } catch (e) {
      if (!retryable(e)) {
        report({ kind: "failed", reason: messageOf(e) });
        return;
      }
      interval = Math.min(interval + SLOW_DOWN_STEP_S, MAX_INTERVAL_S);
      meta = { name: ref.name, scope: ref.scope, usage: "oauth", oauth: { resource: "", state: "pending_connect" } };
    }
    if (signal.aborted) return;
    const o = meta.oauth;
    switch (o?.state) {
      case "connected":
        report({ kind: "connected", ...(o.connected_as ? { connectedAs: o.connected_as } : {}) });
        return;
      case "needs_reauth":
        // The server words an expired code "device code expired; start again".
        if (o.last_error && /expired/i.test(o.last_error)) report({ kind: "expired" });
        else report({ kind: "failed", reason: o.last_error || "the authorization server refused the connect" });
        return;
      case "revoked":
        report({ kind: "failed", reason: "the credential was disconnected while the code was waiting" });
        return;
    }
    // The server's poller marks an unapproved code expired on its next step;
    // past that by two intervals, nothing will come of this code.
    if (deps.now() >= expiresAt + 2 * interval * 1000) {
      report({ kind: "expired" });
      return;
    }
  }
}
