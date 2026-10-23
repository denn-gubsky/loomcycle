// The one place a failed /v1/* response becomes an Error the UI can show.
//
// Every api.ts helper throws through apiError, so a refusal that means "you may
// not" reads the same on every page: an access-control denial names the action,
// the resource and the list and rule that decided (the 403 body writeACLDenied
// writes), a scope refusal names the scope, a self-elevation refusal says why.
// Pages keep rendering `e.message`; nothing per page has to learn the bodies.
//
// Every other error keeps the exact message the helpers produced before
// ("<status> <statusText>: <first 200 bytes of the body>"), because callers read
// the status back out of that prefix (walkView.httpStatusOf, a "404" check in
// the schedule pane). The ACL forms keep the same prefix for the same reason.

export interface AclDenialReason {
  acl: string;
  version?: number;
  // The rule's index in its list, as the server numbers it (0-based) — the same
  // number the server's own `error` text, the audit log and `explain` use.
  rule: number;
  effect: string;
}

export interface AclDenial {
  action: string;
  resource: string;
  // Every rule that matched, as the server reports them — including the
  // ALLOWS a deny overrode. denyingReasons picks the ones that decided.
  reasons: AclDenialReason[];
}

// denyingReasons is the rules that refused: the matching denies. Empty when
// nothing denied (the refusal is "no rule allows it", enforce mode).
export function denyingReasons(d: AclDenial): AclDenialReason[] {
  return d.reasons.filter((r) => r.effect === "deny");
}

// authz_subject is the 403 the decision routes give a non-operator that names
// another subject (on a 503 the same code is an outage, not a refusal).
export type ForbiddenKind = "acl_denied" | "insufficient_scope" | "acl_self_elevation" | "authz_subject";

export class ApiError extends Error {
  readonly status: number;
  readonly code?: string;
  // Set only on a 403 acl_denied.
  readonly acl?: AclDenial;
  readonly requiredScope?: string;

  constructor(message: string, status: number, code?: string, acl?: AclDenial, requiredScope?: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.acl = acl;
    this.requiredScope = requiredScope;
  }
}

export interface ApiErrorOptions {
  // serverMessage: use the body's `error` text alone as the message (the
  // substrate and operator-token helpers' long-standing behaviour), falling
  // back to the status line when the body has none.
  serverMessage?: boolean;
}

function str(v: unknown): string | undefined {
  return typeof v === "string" ? v : undefined;
}

function reasonsOf(v: unknown): AclDenialReason[] {
  if (!Array.isArray(v)) return [];
  const out: AclDenialReason[] = [];
  for (const r of v) {
    if (!r || typeof r !== "object") continue;
    const o = r as Record<string, unknown>;
    if (typeof o.acl !== "string") continue;
    out.push({
      acl: o.acl,
      ...(typeof o.version === "number" ? { version: o.version } : {}),
      rule: typeof o.rule === "number" ? o.rule : 0,
      effect: str(o.effect) ?? "",
    });
  }
  return out;
}

// describeAclDenial is the human sentence for a denial, without the status.
export function describeAclDenial(d: AclDenial): string {
  const head = `may not ${d.action || "?"} ${d.resource || "?"}`;
  const denies = denyingReasons(d);
  if (denies.length === 0) return `${head}: no rule allows it`;
  const rules = denies.map((r) => `list "${r.acl}" rule ${r.rule}`).join("; ");
  return `${head}: denied by ${rules}`;
}

export function apiError(status: number, statusText: string, raw: string, opts: ApiErrorOptions = {}): ApiError {
  const statusLine = `${status} ${statusText}`;
  let body: Record<string, unknown> | null = null;
  try {
    const parsed: unknown = JSON.parse(raw);
    if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) body = parsed as Record<string, unknown>;
  } catch {
    // not JSON — the status line carries the body text
  }
  const code = str(body?.code);
  const serverText = str(body?.error);
  const prefixed = (text: string) => (opts.serverMessage ? text : `${statusLine}: ${text}`);

  if (status === 403 && code === "acl_denied") {
    const acl: AclDenial = {
      action: str(body?.action) ?? "",
      resource: str(body?.resource) ?? "",
      reasons: reasonsOf(body?.reasons),
    };
    return new ApiError(prefixed(`access denied — ${describeAclDenial(acl)}`), status, code, acl);
  }
  if (status === 403 && code === "insufficient_scope") {
    const scope = str(body?.required_scope);
    const why = serverText ?? "this needs a scope your token does not hold";
    return new ApiError(prefixed(`not permitted — ${why}${scope ? ` (needs the ${scope} scope)` : ""}`), status, code, undefined, scope);
  }
  if (status === 403 && code === "authz_subject") {
    return new ApiError(prefixed(`not permitted — ${serverText ?? "you may only ask about yourself"}`), status, code);
  }
  if (status === 403 && code === "acl_self_elevation") {
    const why = (serverText ?? "the change would widen your own access").replace(/^refused:\s*/, "");
    return new ApiError(prefixed(`refused — ${why}`), status, code);
  }
  if (opts.serverMessage && serverText) return new ApiError(serverText, status, code);
  return new ApiError(`${statusLine}: ${raw.slice(0, 200)}`, status, code);
}

// forbiddenKind says whether an error is a "you may not" refusal, and which.
export function forbiddenKind(e: unknown): ForbiddenKind | null {
  if (!(e instanceof ApiError) || e.status !== 403) return null;
  switch (e.code) {
    case "acl_denied":
    case "insufficient_scope":
    case "acl_self_elevation":
    case "authz_subject":
      return e.code;
    default:
      return null;
  }
}
