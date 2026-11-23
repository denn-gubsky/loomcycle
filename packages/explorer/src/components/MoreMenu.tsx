import { useEffect, useId, useRef, useState, type KeyboardEvent, type ReactNode } from "react";

// MoreMenu is the document header's "more" button (RFC GZ): one menu holding
// every document action that is not "edit". Items come in groups; a rule
// separates the groups.
//
// Follows the ARIA menu-button pattern, as DownloadMenu did: Enter / Space /
// ArrowDown open onto the first item (ArrowUp onto the last), the arrows wrap,
// Home / End jump, Escape closes and returns focus to the button, Tab and an
// outside click close it. A disabled item stays in the list (aria-disabled),
// so the reader can see why an action is not there, and does nothing.

export interface MoreMenuItem {
  key: string;
  label: ReactNode;
  // title explains the action (and, when disabled, why).
  title?: string;
  disabled?: boolean;
  // danger draws the item apart (delete).
  danger?: boolean;
  run: () => void;
}

export interface MoreMenuProps {
  // groups of items; empty groups are dropped.
  groups: MoreMenuItem[][];
  // busy names an action still running (a download being rendered): the button
  // shows it and stays disabled until it ends.
  busy?: string;
  label?: string;
}

export default function MoreMenu({ groups, busy, label = "More actions" }: MoreMenuProps) {
  const shown = groups.filter((g) => g.length > 0);
  const items = shown.flat();
  const [openAt, setOpenAt] = useState<number | null>(null);
  const open = openAt !== null;
  const rootRef = useRef<HTMLDivElement>(null);
  const buttonRef = useRef<HTMLButtonElement>(null);
  const itemRefs = useRef<(HTMLButtonElement | null)[]>([]);
  const menuId = useId();

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

  const close = () => {
    setOpenAt(null);
    buttonRef.current?.focus();
  };

  const focusItem = (i: number) => itemRefs.current[(i + items.length) % items.length]?.focus();

  const onItemKeyDown = (e: KeyboardEvent<HTMLButtonElement>, i: number) => {
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
      case "Escape":
        e.preventDefault();
        close();
        break;
      case "Tab":
        setOpenAt(null);
        break;
    }
  };

  let index = 0;
  return (
    <div className="doc-more-menu" ref={rootRef}>
      <button
        ref={buttonRef}
        type="button"
        className={`doc-more-button${open ? " active" : ""}`}
        title={busy ? `${busy}…` : label}
        aria-label={label}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? menuId : undefined}
        aria-busy={busy ? true : undefined}
        disabled={!!busy || items.length === 0}
        onClick={() => setOpenAt(open ? null : 0)}
        onKeyDown={(e) => {
          if (e.key === "ArrowDown") {
            e.preventDefault();
            setOpenAt(0);
          } else if (e.key === "ArrowUp") {
            e.preventDefault();
            setOpenAt(items.length - 1);
          } else if (e.key === "Escape" && open) {
            e.preventDefault();
            setOpenAt(null);
          }
        }}
      >
        {busy ? <span className="doc-more-busy">{busy}…</span> : <MoreGlyph />}
      </button>
      {open && (
        <div className="doc-more-list" role="menu" id={menuId} aria-label={label}>
          {shown.map((group, g) => (
            <div className="doc-more-group" role="group" key={g}>
              {g > 0 && <div className="doc-more-rule" role="separator" />}
              {group.map((item) => {
                const i = index++;
                return (
                  <button
                    key={item.key}
                    ref={(el) => {
                      itemRefs.current[i] = el;
                    }}
                    type="button"
                    role="menuitem"
                    tabIndex={-1}
                    className={item.danger ? "danger" : undefined}
                    title={item.title}
                    aria-disabled={item.disabled || undefined}
                    data-key={item.key}
                    onClick={() => {
                      if (item.disabled) return;
                      close();
                      item.run();
                    }}
                    onKeyDown={(e) => onItemKeyDown(e, i)}
                  >
                    {item.label}
                  </button>
                );
              })}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

// Three dots, drawn like the tree icons so it follows the theme.
function MoreGlyph() {
  return (
    <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true" focusable="false" fill="currentColor">
      <circle cx="3.5" cy="8" r="1.4" />
      <circle cx="8" cy="8" r="1.4" />
      <circle cx="12.5" cy="8" r="1.4" />
    </svg>
  );
}
