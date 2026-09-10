import { useMemo, useState } from "react";
import type { DefRegistry, DefValue, FieldSpec } from "./types";
import { FieldRow } from "./components/FieldRow";
import { clearField, countSet, fieldsInGroup, setField, visibleFields } from "./lib/value";

// The folded list: every parameter of a substrate primitive, one row each,
// folded under collapsible groups.
//
// It exists because a def has far more parameters than a form can show at once
// without becoming a wall. Folding keeps the whole surface REACHABLE (nothing
// hides behind a free-text JSON box) while keeping it SCANNABLE (a closed group
// costs one line). The same registry also drives the grouped form, so the two
// are two arrangements of one truth, not two implementations.

export interface FoldedFieldListProps {
  registry: DefRegistry;
  value: DefValue;
  onChange: (next: DefValue) => void;
  disabled?: boolean;
  /** Groups open on first render. Default: those with at least one value set,
   *  so reopening a def lands on what it actually overrides. */
  defaultOpenGroups?: readonly string[];
  /** Hide the search + modified-only toolbar (e.g. a narrow canvas inspector). */
  hideToolbar?: boolean;
  /** Parameters the HOST already renders elsewhere (e.g. a shared identity row
   *  carrying name + description). Omitted rather than removed from the
   *  registry, so the registry stays the complete description of the primitive
   *  and each host decides what it has already covered. */
  omitKeys?: readonly string[];
  /** Palette. Defaults to the dark tokens; a host that themes an ancestor with
   *  data-theme="light" gets light without passing this. */
  theme?: "dark" | "light";
  /** Extra class on the component root, for host layout. */
  className?: string;
}

export function FoldedFieldList({
  registry, value, onChange, disabled, defaultOpenGroups, hideToolbar, omitKeys, theme, className,
}: FoldedFieldListProps) {
  const omitted = useMemo(() => new Set(omitKeys ?? []), [omitKeys]);
  const [query, setQuery] = useState("");
  const [modifiedOnly, setModifiedOnly] = useState(false);
  const [showAdvanced, setShowAdvanced] = useState<Set<string>>(() => new Set());
  const [open, setOpen] = useState<Set<string>>(() => {
    if (defaultOpenGroups) return new Set(defaultOpenGroups);
    // Default-open exactly the groups this def actually overrides: the first
    // thing an operator wants on reopen is "what did I change here".
    const seeded = registry.groups
      .map((g) => g.name)
      .filter((name) => countSet(value, fieldsInGroup(registry, name)) > 0);
    return new Set(seeded);
  });

  const searching = query.trim() !== "" || modifiedOnly;

  const groups = useMemo(
    () =>
      registry.groups.map((g) => {
        const all = fieldsInGroup(registry, g.name).filter((f) => !omitted.has(f.key));
        const shown = visibleFields(all, value, query, modifiedOnly);
        // An `advanced` field folds away only while it is UNSET and no filter is
        // active. A rarely-tuned knob that this def actually overrides is not
        // rarely-tuned any more, and hiding it would make a set value invisible.
        const deferred = (f: FieldSpec) =>
          !!f.advanced && !searching && value[f.key] === undefined;
        return {
          spec: g,
          all,
          primary: shown.filter((f) => !deferred(f)),
          advanced: shown.filter(deferred),
          setCount: countSet(value, all),
        };
      }),
    [registry, value, query, modifiedOnly, searching, omitted],
  );

  const toggle = (name: string) =>
    setOpen((prev) => {
      const next = new Set(prev);
      if (next.has(name)) next.delete(name); else next.add(name);
      return next;
    });

  const toggleAdvanced = (name: string) =>
    setShowAdvanced((prev) => {
      const next = new Set(prev);
      if (next.has(name)) next.delete(name); else next.add(name);
      return next;
    });

  const totalShown = groups.reduce((n, g) => n + g.primary.length + g.advanced.length, 0);

  const rowFor = (f: FieldSpec) => (
    <FieldRow
      key={f.key}
      spec={f}
      value={value[f.key]}
      disabled={disabled}
      compact
      onChange={(next) =>
        onChange(next === undefined ? clearField(value, f.key) : setField(value, f.key, next))
      }
      onClear={() => onChange(clearField(value, f.key))}
    />
  );

  return (
    <div
      className={`loomcycle-def-fields${className ? ` ${className}` : ""}`}
      data-theme={theme}
      data-kind={registry.kind}
    >
      {!hideToolbar && (
        <div className="lc-df-toolbar">
          <input
            className="lc-df-input lc-df-search"
            type="search"
            value={query}
            placeholder="Search parameters…"
            disabled={disabled}
            onChange={(e) => setQuery(e.target.value)}
          />
          <label className="lc-df-toggle">
            <input
              type="checkbox"
              checked={modifiedOnly}
              disabled={disabled}
              onChange={(e) => setModifiedOnly(e.target.checked)}
            />
            <span>modified only</span>
          </label>
        </div>
      )}

      {searching && totalShown === 0 && (
        <p className="lc-df-empty">No parameter matches that filter.</p>
      )}

      {groups.map(({ spec, all, primary, advanced, setCount }) => {
        if (primary.length + advanced.length === 0) return null;
        // While filtering, force groups open — a match the operator cannot see
        // reads as "no result", which is worse than no search at all.
        const isOpen = searching || open.has(spec.name);
        const advOpen = showAdvanced.has(spec.name);
        const shownCount = primary.length + advanced.length;
        return (
          <section className="lc-df-group" key={spec.name}>
            <button
              type="button"
              className="lc-df-group-head"
              aria-expanded={isOpen}
              onClick={() => toggle(spec.name)}
            >
              <span className="lc-df-caret" aria-hidden="true">{isOpen ? "▾" : "▸"}</span>
              <span className="lc-df-group-name">{spec.name}</span>
              <span className="lc-df-group-count">
                ({searching ? `${shownCount}/${all.length}` : all.length})
              </span>
              {setCount > 0 && <span className="lc-df-group-set">{setCount} set</span>}
            </button>

            {isOpen && (
              <div className="lc-df-group-body">
                {spec.hint && <p className="lc-df-group-hint">{spec.hint}</p>}
                {primary.map(rowFor)}
                {advanced.length > 0 && (
                  <>
                    {advOpen && advanced.map(rowFor)}
                    <button
                      type="button"
                      className="lc-df-more"
                      aria-expanded={advOpen}
                      onClick={() => toggleAdvanced(spec.name)}
                    >
                      {advOpen ? "− hide" : `+ ${advanced.length} more`} advanced
                    </button>
                  </>
                )}
              </div>
            )}
          </section>
        );
      })}
    </div>
  );
}
