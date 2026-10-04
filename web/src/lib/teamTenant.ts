import type { Principal, TeamNameSummary } from "../api";

// An administrator's team list holds every tenant's teams, but the server
// resolves a team NAME in the caller's own tenant and nowhere else. These
// helpers tell the Teams page which rows a by-name call can reach, so it
// addresses the others by version id where the server takes one and says so
// where it does not.

/** The tenant the server resolves this caller's by-name team calls in. With no
 *  bearer configured that is the shared tenant "", though the identity read
 *  reports "default". */
export function ownTenantOf(p: Principal | null): string {
  if (!p || p.open_mode) return "";
  return p.tenant_id ?? "";
}

/** A row's owning tenant; the list omits the field for the shared tenant. */
export function teamTenantOf(t: Pick<TeamNameSummary, "tenant_id">): string {
  return t.tenant_id ?? "";
}

/** Same team: two tenants may each have a team of one name. */
export function sameTeam(t: Pick<TeamNameSummary, "name" | "tenant_id">, name: string, tenant: string): boolean {
  return t.name === name && teamTenantOf(t) === tenant;
}

export interface TeamReach {
  /** Run and Delete take a name only: they reach the caller's own tenant. */
  byName: boolean;
  /** Save forks by name; for a shared-tenant team that saves a copy in the
   *  caller's tenant, and for another tenant's team it finds nothing. */
  save: boolean;
  /** Why something is off; "" when everything is reachable. */
  notice: string;
}

/** teamReach says what the caller can do to a team owned by rowTenant. */
export function teamReach(rowTenant: string, ownTenant: string): TeamReach {
  if (rowTenant === ownTenant) return { byName: true, save: true, notice: "" };
  if (rowTenant === "") {
    return {
      byName: false,
      save: true,
      notice:
        "This team is in the shared tenant. Save new version saves a copy in your own tenant; Run and Delete act on a team by name in your own tenant, so they cannot reach this one.",
    };
  }
  return {
    byName: false,
    save: false,
    notice: `This team belongs to tenant "${rowTenant}". Run, Save new version and Delete act on a team by name in your own tenant, so they cannot reach it: sign in with a token for "${rowTenant}" to use them.`,
  };
}
