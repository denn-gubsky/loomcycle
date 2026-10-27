import { canSee, grantOf, type Surfaces, type Visibility } from "./visibility";

// The top bar's account menu: the viewer's own pages, and Log out. Everything
// else — the workspace and the administration of the tenant or the runtime — is
// in the left nav (../components/navItems).

export const LOGOUT_HREF = "/ui/logout";

// The viewer's own pages, in the account menu. Each is a page of its own,
// reached from that menu rather than the left nav. "Your account" is not in this
// list: it is shown for every signed-in session (accountMenuItems), so it has no
// page in the server's capability summary — and this list is what the summary is
// held equal to.
export type AccountLinkId = "my-credentials";

export interface AccountLinkDef {
  id: AccountLinkId;
  label: string;
  href: string;
  // The same three-tier class the left nav uses, assigned from the route gate.
  vis: Visibility;
}

export const ACCOUNT_LINKS: AccountLinkDef[] = [
  // RFC CN — every login (incl. an isolated substrate:user user) can self-serve
  // its OWN scope=user API tokens here; the tenant's credential authoring is the
  // left nav's tenant credentials page.
  { id: "my-credentials", label: "My credentials", href: "/my-credentials", vis: "all" },
];

export const ACCOUNT_HREF = "/account";

// One row of the account menu: one of the viewer's own pages, or Log out.
export type AccountMenuItem =
  | { kind: "link"; id: AccountLinkId | "account"; label: string; href: string }
  | { kind: "logout"; label: string; href: string };

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
): AccountMenuItem[] {
  return [
    ...(signedIn ? [{ kind: "link", id: "account", label: "Your account", href: ACCOUNT_HREF } as const] : []),
    ...ACCOUNT_LINKS.filter((l) => canSee(l.vis, isAdmin, hasTenantScope, grantOf(surfaces, l.id))).map(
      (l): AccountMenuItem => ({ kind: "link", id: l.id, label: l.label, href: l.href }),
    ),
    { kind: "logout", label: "Log out", href: LOGOUT_HREF },
  ];
}

// isMenuItemCurrent says whether a menu row is the page being shown — what the
// menu marks, and what lights its button. A linked page matches its address and
// anything beneath it.
export function isMenuItemCurrent(item: AccountMenuItem, pathname: string): boolean {
  switch (item.kind) {
    case "link":
      return pathname === item.href || pathname.startsWith(item.href + "/");
    case "logout":
      return false;
  }
}
