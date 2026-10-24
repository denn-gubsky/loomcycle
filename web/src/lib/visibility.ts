// Per-surface visibility class, shared by the left nav and the Settings tabs.
//
//   "all"    — every authenticated role, including a delegated user.
//   "tenant" — admin OR a substrate:tenant operator. The surface's reads are
//              tenant-scoped server-side and its writes are reachable by
//              substrate:tenant, so it lights up once visible.
//   "admin"  — super-admin only (operator plane, no per-tenant axis).
//
// ONE DEFINITION, because there were two. The nav has classified surfaces this way
// since the tenant-operator UI landed; the Settings tabs kept a binary `admin`
// boolean, which cannot express the middle tier. The result was a delegated user
// reaching /settings directly and being shown five tabs whose every call 403s —
// visible only by direct URL, since the nav's gear is already gated, but visible.
//
// ⚠️ THE RULE THAT MAKES THIS SAFE: a surface is marked "tenant" ONLY where the
// backing route gate actually admits substrate:tenant (requiredScopeFor →
// ScopeTenant, or a tenant-implied scope). Marking a surface "tenant" does not
// grant anything — the server decides — so a wrong label here does not open a
// hole; it produces a control that 403s, which is the exact failure this replaces.
//
// Since RFC EZ §4 the class is the FALLBACK: GET /v1/_me answers per page what
// the principal may actually reach (`effective.surfaces`, read through grantOf),
// and that answer decides wherever it is given — a member its tenant lets use
// channels sees them, an operator an access-control list denies does not. The
// class still decides for a server that predates the summary.
export type Visibility = "all" | "tenant" | "admin";

// Surfaces is GET /v1/_me's capability summary (`effective.surfaces`): for each
// page id, whether the principal may reach the route the page loads first, as
// the server's route gates and access-control lists would answer a request to
// it. It is what a page's visibility is decided by; the role rule below is the
// fallback for a page the server did not answer for.
export type Surfaces = Readonly<Record<string, boolean>>;

// canSee gates one surface. `granted` is the server's answer for the page
// (grantOf); when it is present it decides, so a member whose tenant lets it use
// channels sees Channels and an operator an ACL denies them does not. Without
// it — an older server, a page the server does not list — the role decides.
export function canSee(
  vis: Visibility,
  isAdmin: boolean,
  hasTenantScope: boolean,
  granted?: boolean,
): boolean {
  if (granted !== undefined) return granted;
  switch (vis) {
    case "all":
      return true;
    case "tenant":
      return isAdmin || hasTenantScope;
    case "admin":
      return isAdmin;
  }
}

// TENANT_SCOPE is the scope string that marks a tenant operator. Named because it
// appears in the principal's scope list verbatim and a typo would silently demote
// every tenant operator to a delegated user.
export const TENANT_SCOPE = "substrate:tenant";

// hasTenantScope reads the role off a principal's scope list.
//
// Takes the list rather than the principal so it cannot accidentally depend on any
// other field: role here is a function of scopes and nothing else.
export function hasTenantScope(scopes: string[] | undefined): boolean {
  return scopes?.includes(TENANT_SCOPE) === true;
}

// grantOf is the server's answer for one page, or undefined when it gave none.
// Own boolean properties only, so a page id that names an Object.prototype
// member cannot read as an answer.
export function grantOf(surfaces: Surfaces | undefined, id: string): boolean | undefined {
  if (!surfaces || !Object.prototype.hasOwnProperty.call(surfaces, id)) return undefined;
  const v = surfaces[id];
  return typeof v === "boolean" ? v : undefined;
}

// surfacesOf reads the summary off a principal. Takes the shape rather than the
// api Principal so this module stays free of the API layer.
export function surfacesOf(
  p: { effective?: { surfaces?: Record<string, boolean> } } | null | undefined,
): Surfaces | undefined {
  return p?.effective?.surfaces;
}
