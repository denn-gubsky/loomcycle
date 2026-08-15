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
export type Visibility = "all" | "tenant" | "admin";

// canSee gates one surface by the principal's role.
export function canSee(vis: Visibility, isAdmin: boolean, hasTenantScope: boolean): boolean {
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
