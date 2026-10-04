import { useCallback, useEffect, useId, useRef, useState } from "react";
import { ChevronDown } from "lucide-react";
import { listTenants } from "../api";
import {
  filterTenants,
  moveActive,
  normaliseTenants,
  type TenantOption,
} from "../lib/tenantOptions";

// TenantCombobox is the admin tenant-focus field: a text input the admin can type
// any tenant id into, plus a drop-down of the tenants the directory knows about.
//
// TYPING STAYS FIRST-CLASS, because the list is not the set of tenants. GET
// /v1/_tenants is derived from runs, so a tenant that has never run anything is
// absent from it — and that tenant is exactly the one an admin is likely to be
// setting up. The drop-down is a shortcut over the input, never a constraint on it.
//
// It degrades to the plain input it replaced whenever there is no list to offer:
// the viewer is not an admin (`enabled` false — the endpoint is admin-only and the
// list of tenants is itself cross-tenant information), or the call failed.
interface Props {
  id?: string;
  value: string;
  // Every keystroke. The owner decides what a half-typed id means.
  onChange: (text: string) => void;
  // A row was chosen from the list; "" is the "all tenants" row.
  onPick: (tenant: string) => void;
  enabled: boolean;
  placeholder?: string;
  title?: string;
}

export default function TenantCombobox({
  id,
  value,
  onChange,
  onPick,
  enabled,
  placeholder,
  title,
}: Props) {
  // null = no list to offer (not loaded yet, or the load failed) → plain input.
  const [known, setKnown] = useState<TenantOption[] | null>(null);
  const [open, setOpen] = useState(false);
  // Filter only once the admin has typed since opening. Otherwise reopening the
  // field with "acme" already in it would list "acme" alone, hiding every tenant
  // they might want to switch to.
  const [typed, setTyped] = useState(false);
  const [active, setActive] = useState(-1);
  const rootRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const listId = useId();

  const load = useCallback(() => {
    if (!enabled) return;
    listTenants()
      .then((r) => setKnown(normaliseTenants(r.tenants)))
      .catch(() => setKnown(null));
  }, [enabled]);

  // Load once up front so the toggle is there before the field is first touched;
  // each focus refreshes it (see onFocus).
  useEffect(load, [load]);

  const hasList = enabled && known !== null;
  const showing = hasList && open;
  const options = hasList ? filterTenants(known, typed ? value : "") : [];
  const optionId = (i: number) => `${listId}-opt-${i}`;

  const close = () => {
    setOpen(false);
    setActive(-1);
  };
  const openList = () => {
    setOpen(true);
    setTyped(false);
    setActive(-1);
  };
  const pick = (opt: TenantOption) => {
    onChange(opt.id);
    onPick(opt.id);
    close();
  };

  // Close on a press anywhere outside. Blur covers the keyboard path; this covers
  // a list opened from the toggle, where the input may not hold focus.
  useEffect(() => {
    if (!showing) return;
    const onDown = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) {
        setOpen(false);
        setActive(-1);
      }
    };
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, [showing]);

  useEffect(() => {
    if (showing && active >= 0) {
      document.getElementById(`${listId}-opt-${active}`)?.scrollIntoView({ block: "nearest" });
    }
  }, [showing, active, listId]);

  const onKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (!hasList) return;
    switch (e.key) {
      case "ArrowDown":
      case "ArrowUp":
        e.preventDefault();
        if (!showing) {
          setOpen(true);
          setTyped(false);
        }
        setActive(moveActive(showing ? active : -1, options.length, e.key));
        break;
      case "Home":
      case "End":
        // Only steal the caret keys while a row is highlighted; otherwise they
        // keep their text-editing meaning.
        if (showing && active >= 0) {
          e.preventDefault();
          setActive(moveActive(active, options.length, e.key));
        }
        break;
      case "Enter":
        if (showing && active >= 0 && options[active]) {
          // A highlighted row wins over the typed text; without one, Enter falls
          // through to the surrounding form and submits what was typed.
          e.preventDefault();
          pick(options[active]);
        } else {
          close();
        }
        break;
      case "Escape":
        if (showing) {
          e.preventDefault();
          close();
        }
        break;
    }
  };

  return (
    <div className={"tenant-combo" + (hasList ? " has-list" : "")} ref={rootRef}>
      <input
        ref={inputRef}
        id={id}
        type="text"
        value={value}
        placeholder={placeholder}
        title={title}
        autoComplete="off"
        // Refresh on every focus: tenants appear as they start their first run,
        // and a list fetched once at page load would never show them.
        onFocus={load}
        onClick={() => !open && openList()}
        onBlur={close}
        onChange={(e) => {
          onChange(e.target.value);
          setOpen(true);
          setTyped(true);
          setActive(-1);
        }}
        onKeyDown={onKeyDown}
        {...(hasList
          ? {
              role: "combobox",
              "aria-autocomplete": "list" as const,
              "aria-expanded": showing,
              "aria-controls": listId,
              "aria-activedescendant": showing && active >= 0 ? optionId(active) : undefined,
            }
          : {})}
      />
      {hasList && (
        <button
          type="button"
          className="tenant-combo-toggle"
          // Out of the tab order: the input already opens the list with the arrow
          // keys, and a second stop per field doubles the bar's tab length.
          tabIndex={-1}
          aria-label="Show known tenants"
          // Keep focus in the input so the list's keyboard handling keeps working.
          onMouseDown={(e) => e.preventDefault()}
          onClick={() => {
            inputRef.current?.focus();
            if (open) close();
            else openList();
          }}
        >
          <ChevronDown size={14} />
        </button>
      )}
      {showing && (
        <ul className="tenant-combo-list" role="listbox" id={listId} aria-label="Known tenants">
          {options.map((opt, i) => (
            <li
              key={opt.id || "__all__"}
              id={optionId(i)}
              role="option"
              aria-selected={i === active}
              className={"tenant-combo-option" + (opt.id === "" ? " tenant-combo-all" : "")}
              // preventDefault keeps the input focused, so its blur does not close
              // the list before the click lands.
              onMouseDown={(e) => e.preventDefault()}
              // mousemove, not mouseenter: a list that re-renders under a
              // STATIONARY pointer fires mouseenter, which would highlight a row
              // the admin is not pointing at and let Enter pick it over the text
              // they just typed.
              onMouseMove={() => i !== active && setActive(i)}
              onClick={() => pick(opt)}
            >
              <span>{opt.id === "" ? "all tenants" : opt.id}</span>
              {opt.runs !== undefined && (
                <span className="tenant-combo-count">
                  {opt.runs} {opt.runs === 1 ? "run" : "runs"}
                </span>
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
