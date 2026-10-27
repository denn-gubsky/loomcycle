import { useEffect, useId, useRef, useState } from "react";
import { Link, useLocation } from "react-router-dom";
import { CircleUser, KeyRound, LogOut, type LucideIcon } from "lucide-react";
import { type AccountMenuItem, accountMenuItems, isMenuItemCurrent } from "../lib/accountMenu";
import { type Surfaces } from "../lib/visibility";
import { signOut } from "../lib/uiSession";

/** The menu's "Log out": sign out from this click, and fall back to the
 *  confirm page at href when the server could not end the session. */
export async function handleLogoutClick(
  e: { preventDefault(): void },
  href: string,
  out: () => Promise<boolean> = () => signOut(),
  go: (url: string) => void = (url) => window.location.assign(url),
): Promise<void> {
  e.preventDefault();
  if (!(await out())) go(href);
}

// A Record over the link ids, not a lookup with a fallback: adding a link
// without an icon is then a type error here rather than a blank row in the menu.
const LINK_ICONS: Record<Extract<AccountMenuItem, { kind: "link" }>["id"], LucideIcon> = {
  account: CircleUser,
  "my-credentials": KeyRound,
};

// AccountMenu is the top bar's rightmost control: a button that opens the
// viewer's own pages (Your account, My credentials) and Log out.
//
// Rendered for EVERY role: Log out lives only here, so even a viewer with no
// other row gets the menu. The button is lit while the menu is open and on a
// page it lists — the left nav does not mark these pages, so nothing else does.
//
// Follows the ARIA menu-button pattern: Enter / Space / ArrowDown open onto the
// first item (ArrowUp onto the last), the arrows wrap, Home / End jump, Escape
// closes and returns focus to the button, Tab closes and moves on.
export default function AccountMenu({
  isAdmin,
  hasTenantScope,
  surfaces,
  signedIn,
}: {
  isAdmin: boolean;
  hasTenantScope: boolean;
  surfaces?: Surfaces;
  signedIn: boolean;
}) {
  const items = accountMenuItems(isAdmin, hasTenantScope, surfaces, signedIn);
  const label = "Account";
  // null = closed; otherwise the index to focus on opening.
  const [openAt, setOpenAt] = useState<number | null>(null);
  const open = openAt !== null;
  const rootRef = useRef<HTMLDivElement>(null);
  const buttonRef = useRef<HTMLButtonElement>(null);
  const itemRefs = useRef<(HTMLAnchorElement | null)[]>([]);
  const menuId = useId();
  const { pathname } = useLocation();

  // Close on navigation — including one the menu did not start (back/forward).
  useEffect(() => setOpenAt(null), [pathname]);

  useEffect(() => {
    if (openAt !== null) itemRefs.current[openAt]?.focus();
  }, [openAt]);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) setOpenAt(null);
    };
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, [open]);

  const focusItem = (i: number) => itemRefs.current[(i + items.length) % items.length]?.focus();

  const onItemKeyDown = (e: React.KeyboardEvent<HTMLAnchorElement>, i: number) => {
    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        focusItem(i + 1);
        break;
      case "ArrowUp":
        e.preventDefault();
        focusItem(i - 1);
        break;
      case "Home":
        e.preventDefault();
        focusItem(0);
        break;
      case "End":
        e.preventDefault();
        focusItem(items.length - 1);
        break;
      case " ":
        // A link activates on Enter natively but not on Space; a menu item must
        // do both.
        e.preventDefault();
        e.currentTarget.click();
        break;
      case "Escape":
        e.preventDefault();
        setOpenAt(null);
        buttonRef.current?.focus();
        break;
      case "Tab":
        setOpenAt(null);
        break;
    }
  };

  return (
    <div className="settings-menu" ref={rootRef}>
      <button
        ref={buttonRef}
        type="button"
        className={
          "settings-gear" +
          (open ||
          items.some((i) => isMenuItemCurrent(i, pathname))
            ? " active"
            : "")
        }
        title={label}
        aria-label={label}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? menuId : undefined}
        onClick={() => setOpenAt(open ? null : 0)}
        onKeyDown={(e) => {
          if (e.key === "ArrowDown") {
            e.preventDefault();
            setOpenAt(0);
          } else if (e.key === "ArrowUp") {
            e.preventDefault();
            setOpenAt(items.length - 1);
          }
        }}
      >
        <CircleUser size={16} />
      </button>
      {open && (
        <div className="settings-menu-list" role="menu" id={menuId} aria-label={label}>
          {items.map((item, i) => {
            const setRef = (el: HTMLAnchorElement | null) => {
              itemRefs.current[i] = el;
            };
            // A rule before Log out, so the way out reads apart from the pages.
            const startsGroup = i > 0 && items[i - 1].kind !== item.kind;
            const sep = startsGroup && <div className="settings-menu-sep" role="separator" />;
            if (item.kind === "logout") {
              // The click signs out itself (POST /v1/_auth/logout, then the
              // login page). The href is the confirm page, for a middle-click
              // or a failed sign-out.
              return (
                <div key="logout" role="none">
                  {sep}
                  <a
                    ref={setRef}
                    href={item.href}
                    role="menuitem"
                    tabIndex={-1}
                    className="settings-menu-item settings-menu-logout"
                    onKeyDown={(e) => onItemKeyDown(e, i)}
                    onClick={(e) => void handleLogoutClick(e, item.href)}
                  >
                    <LogOut size={15} />
                    {item.label}
                  </a>
                </div>
              );
            }
            const Icon = LINK_ICONS[item.id];
            return (
              <div key={item.kind + ":" + item.id} role="none">
                {sep}
                <Link
                  ref={setRef}
                  to={item.href}
                  role="menuitem"
                  tabIndex={-1}
                  className="settings-menu-item"
                  aria-current={isMenuItemCurrent(item, pathname) ? "page" : undefined}
                  // Selecting the page already open changes no path, so the
                  // navigation effect above would not close the menu.
                  onClick={() => setOpenAt(null)}
                  onKeyDown={(e) => onItemKeyDown(e, i)}
                >
                  <Icon size={15} />
                  {item.label}
                </Link>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
