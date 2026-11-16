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
  /** Run takes a name: the caller's own tenant's team of that name, else the
   *  shared tenant's. */
  run: boolean;
  /** Delete takes a name and reaches the caller's own tenant only. */
  del: boolean;
  /** Save forks by name; for a shared-tenant team that saves a copy in the
   *  caller's tenant, and for another tenant's team it finds nothing. */
  save: boolean;
  /** Why something is off; "" when everything is reachable. */
  notice: string;
}

export interface TeamReachOpts {
  /** The team's name, for the notice. */
  name?: string;
  /** The row's active version was written by the server's configuration
   *  (`active_source: "config"`): it is changed in the yaml, not here. */
  fromConfig?: boolean;
  /** The caller's own tenant has a team of this name too: a run by name gets
   *  that one, not this shared one. */
  ownHasName?: boolean;
}

/** teamReach says what the caller can do to a team owned by rowTenant. */
export function teamReach(rowTenant: string, ownTenant: string, opts: TeamReachOpts = {}): TeamReach {
  const key = opts.name ? `teams.${opts.name}` : "teams:";
  if (rowTenant === ownTenant) {
    if (opts.fromConfig) {
      return {
        run: true,
        del: false,
        save: false,
        notice: `This team comes from the server's configuration (${key}); change it there. Save new version and Delete are refused while the configuration holds it.`,
      };
    }
    return { run: true, del: true, save: true, notice: "" };
  }
  if (rowTenant === "") {
    const origin = opts.fromConfig ? "the server's configuration, in the shared tenant" : "the shared tenant";
    const run = opts.ownHasName
      ? "Your own tenant has a team of this name, so Run starts that one, not this one"
      : "Run starts it in your tenant";
    return {
      run: !opts.ownHasName,
      del: false,
      save: true,
      notice: `This team is from ${origin}. ${run}; Save new version saves a copy in your own tenant, which then runs in its place; Delete acts on a team in your own tenant, so it cannot reach this one.`,
    };
  }
  return {
    run: false,
    del: false,
    save: false,
    notice: `This team belongs to tenant "${rowTenant}". Run, Save new version and Delete act on a team by name in your own tenant, so they cannot reach it: sign in with a token for "${rowTenant}" to use them.`,
  };
}
