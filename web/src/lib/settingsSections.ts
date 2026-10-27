import { canSee, grantOf, type Surfaces, type Visibility } from "./visibility";

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
  | "presets"
  | "runtime"
  | "lanes"
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
  // Access control (groups, policies, bindings, the simulator) and token minting
  // are pages of /access in the left nav's access group, with users — not tabs here.
  //
  // ScopeAdmin: presets/runtime/lanes/health fall through to the /v1/_* catch-all;
  // and repair-tenant is explicitly admin because it rewrites rows across every
  // scope in one statement.
  { id: "presets", label: "Presets", vis: "admin" },
  { id: "runtime", label: "Runtime", vis: "admin" },
  { id: "lanes", label: "Lanes", vis: "admin" },
  { id: "maintenance", label: "Maintenance", vis: "admin" },
  { id: "health", label: "Health", vis: "admin" },
];

// visibleSections is the sections a viewer may reach, in hub order. `surfaces` is
// GET /v1/_me's capability summary: where it answers for a section (by its id),
// that answer decides; `vis` is the fallback for a server that gave none.
export function visibleSections(
  isAdmin: boolean,
  hasTenantScope: boolean,
  surfaces?: Surfaces,
): SectionDef[] {
  return SECTIONS.filter((s) => canSee(s.vis, isAdmin, hasTenantScope, grantOf(surfaces, s.id)));
}

// settingsHref is the deep link to one section: /settings/<id>.
export function settingsHref(id: Section): string {
  return `/settings/${id}`;
}

// resolveSection picks the tab to render for the /settings/:section? param.
//
// The param is a preference; visibility is a rule. An absent, unknown or
// not-permitted section falls back to the first tab the viewer CAN see, so a
// pasted /settings/presets link opens a tenant operator's own first tab rather
// than a panel whose every call 403s. undefined = the viewer sees no tab at all.
export function resolveSection(
  param: string | undefined,
  visible: ReadonlyArray<SectionDef>,
): Section | undefined {
  return (visible.find((s) => s.id === param) ?? visible[0])?.id;
}

export const LOGOUT_HREF = "/ui/logout";

// The viewer's own pages, in the top bar's account menu. Each is a page of its
// own, reached from that menu rather than the left nav. "Your account" is not in
// this list: it is shown for every signed-in session (accountMenuItems), so it
// has no page in the server's capability summary — and this list is what the
// summary is held equal to.
export type AccountLinkId = "my-credentials";

export interface AccountLinkDef {
  id: AccountLinkId;
  label: string;
  href: string;
  // Same three-tier class, assigned from the route gate like the sections above.
  vis: Visibility;
}

export const ACCOUNT_LINKS: AccountLinkDef[] = [
  // RFC CN — every login (incl. an isolated substrate:user user) can self-serve
  // its OWN scope=user API tokens here; the operator Settings → Credentials tab
  // (tenant authoring) stays separate.
  { id: "my-credentials", label: "My credentials", href: "/my-credentials", vis: "all" },
];

export const ACCOUNT_HREF = "/account";

// One row of a top-bar menu: a settings section, one of the viewer's own pages,
// or Log out.
export type SettingsMenuItem =
  | { kind: "section"; id: Section; label: string; href: string }
  | { kind: "link"; id: AccountLinkId | "account"; label: string; href: string }
  | { kind: "logout"; label: string; href: string };

// settingsMenuItems is the gear's menu: the viewer's settings sections (the hub's
// tabs), and nothing else. Empty for a viewer with no section, who then gets no
// gear — their pages and Log out are in the account menu.
export function settingsMenuItems(
  isAdmin: boolean,
  hasTenantScope: boolean,
  surfaces?: Surfaces,
): SettingsMenuItem[] {
  return visibleSections(isAdmin, hasTenantScope, surfaces).map(
    (s): SettingsMenuItem => ({ kind: "section", id: s.id, label: s.label, href: settingsHref(s.id) }),
  );
}

// accountMenuItems is the account menu: Your account (for a signed-in session —
// an open-mode server has nobody signed in), the viewer's own pages, then Log out.
//
// LOG OUT IS UNCONDITIONAL: this menu is the only way out of a session, so every
// role gets it, holding at least that row.
export function accountMenuItems(
  isAdmin: boolean,
  hasTenantScope: boolean,
  surfaces: Surfaces | undefined,
  signedIn: boolean,
): SettingsMenuItem[] {
  return [
    ...(signedIn ? [{ kind: "link", id: "account", label: "Your account", href: ACCOUNT_HREF } as const] : []),
    ...ACCOUNT_LINKS.filter((l) => canSee(l.vis, isAdmin, hasTenantScope, grantOf(surfaces, l.id))).map(
      (l): SettingsMenuItem => ({ kind: "link", id: l.id, label: l.label, href: l.href }),
    ),
    { kind: "logout", label: "Log out", href: LOGOUT_HREF },
  ];
}

// isMenuItemCurrent says whether a menu row is the page being shown — what the
// menu marks, and what lights its button. A section matches its exact address; a
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
