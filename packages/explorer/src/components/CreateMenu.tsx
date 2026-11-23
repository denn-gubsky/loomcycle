import { useEffect, useId, useRef, useState, type KeyboardEvent } from "react";

// CreateMenu is the Path header's one "+" button (RFC GZ): a small menu that
// creates a folder or a document in `dir`. The create dialog it opens names
// that folder in its title, so the target stays visible where it is used.

export interface CreateMenuProps {
  dir: string;
  onNewFolder: () => void;
  onNewDocument: () => void;
}

export default function CreateMenu({ dir, onNewFolder, onNewDocument }: CreateMenuProps) {
  const items = [
    { key: "folder", label: "folder", title: `New folder in ${dir}`, run: onNewFolder },
    { key: "document", label: "document", title: `New document in ${dir}`, run: onNewDocument },
  ];
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

  const onItemKeyDown = (e: KeyboardEvent<HTMLButtonElement>, i: number) => {
    const n = items.length;
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      itemRefs.current[(i + (e.key === "ArrowDown" ? 1 : -1) + n) % n]?.focus();
    } else if (e.key === "Escape") {
      e.preventDefault();
      close();
    } else if (e.key === "Tab") {
      setOpenAt(null);
    }
  };

  return (
    <div className="paths-create-menu" ref={rootRef}>
      <button
        ref={buttonRef}
        type="button"
        className={open ? "active" : undefined}
        title={`New folder or document in ${dir}`}
        aria-label="New"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? menuId : undefined}
        onClick={() => setOpenAt(open ? null : 0)}
        onKeyDown={(e) => {
          if (e.key === "ArrowDown") {
            e.preventDefault();
            setOpenAt(0);
          } else if (e.key === "Escape" && open) {
            e.preventDefault();
            setOpenAt(null);
          }
        }}
      >
        +
      </button>
      {open && (
        <div className="paths-create-list" role="menu" id={menuId} aria-label="New">
          {items.map((item, i) => (
            <button
              key={item.key}
              ref={(el) => {
                itemRefs.current[i] = el;
              }}
              type="button"
              role="menuitem"
              tabIndex={-1}
              title={item.title}
              onClick={() => {
                setOpenAt(null);
                item.run();
              }}
              onKeyDown={(e) => onItemKeyDown(e, i)}
            >
              {item.label}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
