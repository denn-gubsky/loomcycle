import { canSee, type Visibility } from "./visibility";

// The Settings hub's sections, shared by the hub's tabs (SettingsView) and the
// top bar's settings menu (SettingsMenu).
//
// ONE LIST, because the menu must show a viewer exactly the tabs the hub would.
// Two lists would agree on the day they were written and then drift: a section
// added to the hub but not the menu is unreachable from the bar, and one added to
// the menu but not the hub is a link to a tab that silently falls back to another.
export type Section =
  | "credentials"
  | "limits"
  | "routing"
  | "ontology"
  | "retention"
  | "erasure"
  | "tokens"
  | "presets"
  | "runtime"
  | "maintenance"
  | "health";

export interface SectionDef {
  id: Section;
  label: string;
  // Which roles may reach this tab, using the same three-tier class the left nav
  // uses. It replaces a binary `admin` boolean, which could not express the middle
  // tier: a substrate:tenant operator is not an admin, but is not a delegated user
  // either, and every tab below is at least tenant-gated on the server.
  //
  // ⚠️ ASSIGNED FROM THE ROUTE GATE, never from taste. A tab is "tenant" only where
  // requiredScopeFor on its backing endpoint returns ScopeTenant. Mislabelling one
  // grants nothing — the server still refuses — but it produces a control that
  // 403s, which is exactly the defect this replaces.
  vis: Visibility;
}

export const SECTIONS: SectionDef[] = [
  // Every tab here is at least tenant-gated: there is no "all" settings surface,
  // because a delegated user administers nothing.
  //
  // ScopeTenant on the server: /v1/_credentialdef (isTenantConfinedDefPath),
  // /v1/_limits, /v1/_routing and /v1/_ontology. (Memory has its own console at
  // /memory — the shared @loomcycle/memory-view package — rather than a tab here.)
  { id: "credentials", label: "Credentials", vis: "tenant" },
  { id: "limits", label: "Limits", vis: "tenant" },
  { id: "routing", label: "Routing", vis: "tenant" },
  { id: "ontology", label: "Ontology", vis: "tenant" },
  { id: "retention", label: "Retention", vis: "tenant" },
  { id: "erasure", label: "Erasure", vis: "tenant" },
  // ScopeAdmin: token minting has no tenant axis and is deliberately excluded from
  // the tenant-confined def set; presets/runtime/health fall through to the /v1/_*
  // catch-all; and repair-tenant is explicitly admin because it rewrites rows across
  // every scope in one statement.
  { id: "tokens", label: "Tokens", vis: "admin" },
  { id: "presets", label: "Presets", vis: "admin" },
  { id: "runtime", label: "Runtime", vis: "admin" },
  { id: "maintenance", label: "Maintenance", vis: "admin" },
  { id: "health", label: "Health", vis: "admin" },
];

// visibleSections is the sections a viewer may reach, in hub order.
export function visibleSections(isAdmin: boolean, hasTenantScope: boolean): SectionDef[] {
  return SECTIONS.filter((s) => canSee(s.vis, isAdmin, hasTenantScope));
}

// settingsHref is the deep link to one section: /settings/<id>.
export function settingsHref(id: Section): string {
  return `/settings/${id}`;
}

// resolveSection picks the tab to render for the /settings/:section? param.
//
// The param is a preference; visibility is a rule. An absent, unknown or
// not-permitted section falls back to the first tab the viewer CAN see, so a
// pasted /settings/tokens link opens a tenant operator's own first tab rather
// than a panel whose every call 403s. undefined = the viewer sees no tab at all.
export function resolveSection(
  param: string | undefined,
  visible: ReadonlyArray<SectionDef>,
): Section | undefined {
  return (visible.find((s) => s.id === param) ?? visible[0])?.id;
}

export const LOGOUT_HREF = "/ui/logout";

// One row of the top bar's settings menu.
export type SettingsMenuItem =
  | { kind: "section"; id: Section; label: string; href: string }
  | { kind: "logout"; label: string; href: string };

// settingsMenuItems is the menu for a viewer: their settings sections, then Log
// out.
//
// LOG OUT IS UNCONDITIONAL. The menu replaced a standalone sign-out button that
// every role had, so the menu is now the only way out of a session: a delegated
// user, who has no settings section, must still get a menu, holding that one row.
export function settingsMenuItems(isAdmin: boolean, hasTenantScope: boolean): SettingsMenuItem[] {
  return [
    ...visibleSections(isAdmin, hasTenantScope).map(
      (s): SettingsMenuItem => ({
        kind: "section",
        id: s.id,
        label: s.label,
        href: settingsHref(s.id),
      }),
    ),
    { kind: "logout", label: "Log out", href: LOGOUT_HREF },
  ];
}
