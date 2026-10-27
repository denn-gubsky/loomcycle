import { useEffect, useState } from "react";
import { Link, useLocation } from "react-router-dom";
import { ChevronDown, ChevronRight } from "lucide-react";
import { type NavItem, type VisibleZone, ZONE_LABELS, isNavItemCurrent } from "./navItems";

const GROUPS_KEY = "loomcycle.nav.groups";

type OpenState = Record<string, boolean>;

// Browser storage can be missing or refuse (a private window, blocked site
// data); the menu then simply starts with every group open.
function loadOpen(): OpenState {
  try {
    const raw = localStorage.getItem(GROUPS_KEY);
    const v: unknown = raw ? JSON.parse(raw) : {};
    return v && typeof v === "object" ? (v as OpenState) : {};
  } catch {
    return {};
  }
}

function saveOpen(s: OpenState) {
  try {
    localStorage.setItem(GROUPS_KEY, JSON.stringify(s));
  } catch {
    // Not remembered across reloads; the menu still works.
  }
}

// SidebarNav is the left menu: zones (workspace, administration), each a list of
// collapsible groups of pages. A group the viewer sees one page of is shown as
// that page alone — a heading over a single row is noise. Zone headings appear
// only when the viewer has both zones.
//
// Collapsed to icons, the menu has no room for headings: it lists the pages'
// icons, with a rule between groups so the grouping still reads.
//
// Each group remembers whether it was left open; the group holding the current
// page opens on arrival, so the page being shown is never hidden in a closed one.
export default function SidebarNav({ tree, collapsed }: { tree: VisibleZone[]; collapsed: boolean }) {
  const { pathname } = useLocation();
  const [open, setOpen] = useState<OpenState>(loadOpen);

  const currentGroup = tree
    .flatMap((z) => z.groups)
    .find((g) => g.items.some((it) => isNavItemCurrent(it, pathname)))?.group.key;

  useEffect(() => {
    if (!currentGroup) return;
    setOpen((s) => {
      if (s[currentGroup] !== false) return s;
      const next = { ...s, [currentGroup]: true };
      saveOpen(next);
      return next;
    });
  }, [currentGroup]);

  const toggle = (key: string) =>
    setOpen((s) => {
      const next = { ...s, [key]: s[key] === false };
      saveOpen(next);
      return next;
    });

  const leaf = (it: NavItem, nested: boolean) => {
    const current = isNavItemCurrent(it, pathname);
    return (
      <Link
        key={it.id}
        to={it.to}
        title={it.label}
        className={(nested ? "nav-leaf nav-leaf-nested" : "nav-leaf") + (current ? " active" : "")}
        aria-current={current ? "page" : undefined}
      >
        <it.Icon size={nested ? 16 : 18} className="sidebar-icon" />
        <span className="sidebar-label">{it.label}</span>
      </Link>
    );
  };

  if (collapsed) {
    return (
      <nav className="sidebar-nav" aria-label="Main">
        {tree
          .flatMap((z) => z.groups)
          .map((g, i) => (
            <div key={g.group.key} className={"nav-rail-group" + (i > 0 ? " nav-rail-sep" : "")}>
              {g.items.map((it) => leaf(it, false))}
            </div>
          ))}
      </nav>
    );
  }

  const showZones = tree.length > 1;
  return (
    <nav className="sidebar-nav" aria-label="Main">
      {tree.map((z) => (
        <div key={z.zone} className="nav-zone">
          {showZones && <div className="nav-zone-label">{ZONE_LABELS[z.zone]}</div>}
          {z.groups.map(({ group, items }) => {
            if (items.length === 1) return leaf(items[0], false);
            const isOpen = open[group.key] !== false;
            const listId = "nav-" + group.key;
            return (
              <div key={group.key} className="nav-group">
                <button
                  type="button"
                  className={"nav-group-head" + (currentGroup === group.key ? " has-current" : "")}
                  aria-expanded={isOpen}
                  aria-controls={listId}
                  onClick={() => toggle(group.key)}
                >
                  <group.Icon size={18} className="sidebar-icon" />
                  <span className="sidebar-label">{group.label}</span>
                  {isOpen ? (
                    <ChevronDown size={14} className="nav-group-chevron" />
                  ) : (
                    <ChevronRight size={14} className="nav-group-chevron" />
                  )}
                </button>
                {isOpen && (
                  <div id={listId} className="nav-group-items">
                    {items.map((it) => leaf(it, true))}
                  </div>
                )}
              </div>
            );
          })}
        </div>
      ))}
    </nav>
  );
}
