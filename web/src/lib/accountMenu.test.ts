import { describe, expect, it } from "vitest";
import { LOGOUT_HREF, accountMenuItems, isMenuItemCurrent } from "./accountMenu";

const labels = (items: { label: string }[]) => items.map((i) => i.label);

describe("accountMenuItems", () => {
  it("gives a signed-in viewer Your account, their own pages, then Log out", () => {
    expect(accountMenuItems(true, false, undefined, true)).toEqual([
      { kind: "link", id: "account", label: "Your account", href: "/account" },
      { kind: "link", id: "my-credentials", label: "My credentials", href: "/my-credentials" },
      { kind: "logout", label: "Log out", href: LOGOUT_HREF },
    ]);
  });

  it("offers no Your account where nobody is signed in (open mode)", () => {
    expect(labels(accountMenuItems(true, false, undefined, false))).toEqual(["My credentials", "Log out"]);
  });

  it("governs My credentials by the server's answer", () => {
    expect(labels(accountMenuItems(false, false, { "my-credentials": false }, true))).not.toContain("My credentials");
  });

  it("still lets a viewer the server denies their credentials log out", () => {
    // This menu is the only way out of a session.
    expect(accountMenuItems(false, false, { "my-credentials": false }, false)).toEqual([
      { kind: "logout", label: "Log out", href: LOGOUT_HREF },
    ]);
  });
});

describe("isMenuItemCurrent", () => {
  const items = accountMenuItems(true, false, undefined, true);
  const current = (pathname: string) =>
    items.filter((i) => isMenuItemCurrent(i, pathname)).map((i) => i.label);

  it("marks the viewer's own page, and never Log out", () => {
    expect(current("/account")).toEqual(["Your account"]);
    expect(current("/my-credentials")).toEqual(["My credentials"]);
    expect(current("/agents")).toEqual([]);
  });

  it("does not mistake a path that merely starts like a linked page", () => {
    expect(current("/account-archive")).toEqual([]);
  });
});
