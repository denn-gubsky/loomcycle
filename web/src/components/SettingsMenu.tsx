import { useEffect, useId, useRef, useState } from "react";
import { Link, useLocation } from "react-router-dom";
import {
  Archive,
  CircleUser,
  Eraser,
  Gauge,
  HeartPulse,
  KeyRound,
  LogOut,
  type LucideIcon,
  Network,
  Package,
  Power,
  Settings,
  Split,
  Ticket,
  Users,
  Wrench,
} from "lucide-react";
import {
  type MenuLinkId,
  type Section,
  isMenuItemCurrent,
  settingsMenuItems,
} from "../lib/settingsSections";

// A Record over Section, not a lookup with a fallback: adding a section without
// an icon is then a type error here rather than a blank row in the menu.
const SECTION_ICONS: Record<Section, LucideIcon> = {
  credentials: KeyRound,
  limits: Gauge,
  routing: Split,
  ontology: Network,
  retention: Archive,
  erasure: Eraser,
  tokens: Ticket,
  presets: Package,
  runtime: Power,
  maintenance: Wrench,
  health: HeartPulse,
};

const LINK_ICONS: Record<MenuLinkId, LucideIcon> = {
  users: Users,
};

// SettingsMenu is the top bar's rightmost control: a button that opens a vertical
// menu of the viewer's Settings sections, then the pages reached from here
// rather than the left nav (Users), ending in Log out.
//
// Rendered for EVERY role. The sections are filtered by role (the same list the
// hub's tabs use), but Log out is not — it replaced a standalone sign-out button —
// so a delegated user, who has no section, gets a one-row account menu rather
// than no way to end the session.
//
// Follows the ARIA menu-button pattern: Enter / Space / ArrowDown open onto the
// first item (ArrowUp onto the last), the arrows wrap, Home / End jump, Escape
// closes and returns focus to the button, Tab closes and moves on.
export default function SettingsMenu({
  isAdmin,
  hasTenantScope,
}: {
  isAdmin: boolean;
  hasTenantScope: boolean;
}) {
  const items = settingsMenuItems(isAdmin, hasTenantScope);
  // Anything besides Log out makes this a settings menu; Log out alone makes it
  // an account menu.
  const hasSections = items.some((i) => i.kind !== "logout");
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

  const label = hasSections ? "Settings" : "Account";
  const TriggerIcon = hasSections ? Settings : CircleUser;

  return (
    <div className="settings-menu" ref={rootRef}>
      <button
        ref={buttonRef}
        type="button"
        className={
          "settings-gear" +
          // Lit while open, anywhere in the hub (a bare /settings has no row of
          // its own), and on a page the menu links to — the left nav used to
          // mark that page, and now nothing else does.
          (open ||
          pathname.startsWith("/settings") ||
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
        <TriggerIcon size={16} />
      </button>
      {open && (
        <div className="settings-menu-list" role="menu" id={menuId} aria-label={label}>
          {items.map((item, i) => {
            const setRef = (el: HTMLAnchorElement | null) => {
              itemRefs.current[i] = el;
            };
            // A rule wherever the group changes: sections | linked pages | Log out.
            const startsGroup = i > 0 && items[i - 1].kind !== item.kind;
            const sep = startsGroup && <div className="settings-menu-sep" role="separator" />;
            if (item.kind === "logout") {
              // A full-page anchor, not a router link: the session cookie is
              // HttpOnly, so only the server's /ui/logout handler can clear it.
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
                  >
                    <LogOut size={15} />
                    {item.label}
                  </a>
                </div>
              );
            }
            const Icon = item.kind === "section" ? SECTION_ICONS[item.id] : LINK_ICONS[item.id];
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
