import { useEffect, useId, useRef, useState } from "react";
import { ChevronDown } from "lucide-react";
import { moveActive } from "../lib/comboOptions";

// One row of a Combobox drop-down. `id` is the value a pick applies.
export interface ComboRow {
  id: string;
  label: string;
  // Muted text at the row's right edge (a count).
  hint?: string;
  // A row that is not a value from the list — "all tenants", "no user" — set in
  // the body font and muted so it does not read as an id.
  quiet?: boolean;
}

// Combobox is a text input with a drop-down of suggestions: the shared shell
// behind the top bar's tenant and user fields.
//
// TYPING STAYS FIRST-CLASS. Both lists are derived from runs, so the id an
// operator most needs to enter — a tenant or subject that has not run anything
// yet — is exactly the one the list cannot offer. The drop-down is a shortcut
// over the input, never a constraint on it: Enter with no row highlighted is
// left to the surrounding form, which applies the typed text.
//
// `rows: null` means there is no list to offer, and the field is the plain input
// it would otherwise be — no combobox role, no toggle.
interface Props {
  id?: string;
  value: string;
  // Every keystroke. The owner decides what a half-typed id means.
  onChange: (text: string) => void;
  // A row was chosen from the list.
  onPick: (id: string) => void;
  // The rows for a query, or null for no list. Called with "" until the operator
  // types (see `typed`).
  rows: ((query: string) => ComboRow[]) | null;
  onFocus?: () => void;
  onBlur?: () => void;
  placeholder?: string;
  title?: string;
  // Accessible names for the list and for the toggle button.
  listLabel: string;
  toggleLabel: string;
}

export default function Combobox({
  id,
  value,
  onChange,
  onPick,
  rows,
  onFocus,
  onBlur,
  placeholder,
  title,
  listLabel,
  toggleLabel,
}: Props) {
  const [open, setOpen] = useState(false);
  // Filter only once the operator has typed since opening. Otherwise reopening a
  // field with "acme" already in it would list "acme" alone, hiding every row
  // they might want to switch to.
  const [typed, setTyped] = useState(false);
  const [active, setActive] = useState(-1);
  const rootRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const listId = useId();

  const hasList = rows !== null;
  const showing = hasList && open;
  const options = hasList ? rows(typed ? value : "") : [];
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
  const pick = (row: ComboRow) => {
    onChange(row.id);
    onPick(row.id);
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
    <div className={"combo" + (hasList ? " has-list" : "")} ref={rootRef}>
      <input
        ref={inputRef}
        id={id}
        type="text"
        value={value}
        placeholder={placeholder}
        title={title}
        autoComplete="off"
        onFocus={onFocus}
        onClick={() => !open && openList()}
        onBlur={() => {
          close();
          onBlur?.();
        }}
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
          className="combo-toggle"
          // Out of the tab order: the input already opens the list with the arrow
          // keys, and a second stop per field doubles the bar's tab length.
          tabIndex={-1}
          aria-label={toggleLabel}
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
        <ul className="combo-list" role="listbox" id={listId} aria-label={listLabel}>
          {options.map((row, i) => (
            <li
              key={row.id || "__blank__"}
              id={optionId(i)}
              role="option"
              aria-selected={i === active}
              className={"combo-option" + (row.quiet ? " combo-quiet" : "")}
              // preventDefault keeps the input focused, so its blur does not close
              // the list before the click lands.
              onMouseDown={(e) => e.preventDefault()}
              // mousemove, not mouseenter: a list that re-renders under a
              // STATIONARY pointer fires mouseenter, which would highlight a row
              // the operator is not pointing at and let Enter pick it over the
              // text they just typed.
              onMouseMove={() => i !== active && setActive(i)}
              onClick={() => pick(row)}
            >
              <span>{row.label}</span>
              {row.hint !== undefined && <span className="combo-count">{row.hint}</span>}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
