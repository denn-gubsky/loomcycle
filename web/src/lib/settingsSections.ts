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

// A page of its own that is reached from the settings menu rather than the left
// nav. NOT a hub section: it has no tab in SettingsView and no /settings/<id>
// address — the menu simply links to it.
export type MenuLinkId = "users";

export interface MenuLinkDef {
  id: MenuLinkId;
  label: string;
  href: string;
  // Same three-tier class, assigned from the route gate like the sections above.
  vis: Visibility;
}

export const MENU_LINKS: MenuLinkDef[] = [
  // The tenant-operator users console (manage first-class users + mint/revoke
  // their bearer tokens). "tenant": the /v1/_users(/…) routes are ScopeTenant and
  // the handlers confine to the caller's own tenant (a tenant operator sees only
  // its own users; admin sees all + ?tenant= focus).
  { id: "users", label: "Users", href: "/users", vis: "tenant" },
];

// One row of the top bar's settings menu.
export type SettingsMenuItem =
  | { kind: "section"; id: Section; label: string; href: string }
  | { kind: "link"; id: MenuLinkId; label: string; href: string }
  | { kind: "logout"; label: string; href: string };

// settingsMenuItems is the menu for a viewer, in three groups: their settings
// sections (the hub's tabs), then the linked pages, then Log out. The menu draws
// a rule wherever `kind` changes, so the groups read as groups.
//
// Linked pages sit in their own group, after the sections: the sections are tabs
// of one page and a link leaves it, and mixing them would suggest a "Users" tab
// the hub does not have.
//
// LOG OUT IS UNCONDITIONAL. The menu replaced a standalone sign-out button that
// every role had, so the menu is now the only way out of a session: a delegated
// user, who has no settings section and no linked page, must still get a menu,
// holding that one row.
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
    ...MENU_LINKS.filter((l) => canSee(l.vis, isAdmin, hasTenantScope)).map(
      (l): SettingsMenuItem => ({ kind: "link", id: l.id, label: l.label, href: l.href }),
    ),
    { kind: "logout", label: "Log out", href: LOGOUT_HREF },
  ];
}

// isMenuItemCurrent says whether a menu row is the page being shown — what the
// menu marks, and what lights the gear. A section matches its exact address; a
// linked page also matches anything beneath it.
export function isMenuItemCurrent(item: SettingsMenuItem, pathname: string): boolean {
  switch (item.kind) {
    case "section":
      return pathname === item.href;
    case "link":
      return pathname === item.href || pathname.startsWith(item.href + "/");
    case "logout":
      return false;
  }
}
