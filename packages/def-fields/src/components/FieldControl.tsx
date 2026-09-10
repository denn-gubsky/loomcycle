import { useState } from "react";
import type { FieldSpec } from "../types";

// One control per FieldType. Both surfaces (grouped form, folded list) render
// through this switch, so a field looks and behaves identically wherever it is
// shown and a new type only has to be implemented once.
//
// `value === undefined` means UNSET (inherit). Controls render their neutral
// empty state for it and only call onChange with a concrete value once the
// operator actually types — clearing back to inherit is the row's job, not the
// control's, so a control never has to encode the sparse-overlay semantics.

export interface FieldControlProps {
  spec: FieldSpec;
  value: unknown;
  onChange: (next: unknown) => void;
  disabled?: boolean;
  /** Rendered for object / object-array children so nesting reuses the row
   *  chrome (label + hint + inherit affordance) rather than re-implementing it. */
  renderChild?: (child: FieldSpec, value: unknown, onChange: (v: unknown) => void) => React.ReactNode;
}

const asString = (v: unknown): string => (typeof v === "string" ? v : "");
const asStringList = (v: unknown): string[] => (Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : []);
const asRecord = (v: unknown): Record<string, string> =>
  v && typeof v === "object" && !Array.isArray(v)
    ? Object.fromEntries(Object.entries(v as Record<string, unknown>).map(([k, x]) => [k, typeof x === "string" ? x : String(x ?? "")]))
    : {};
const asObjectArray = (v: unknown): Record<string, unknown>[] =>
  Array.isArray(v) ? v.filter((x): x is Record<string, unknown> => !!x && typeof x === "object" && !Array.isArray(x)) : [];

export function FieldControl({ spec, value, onChange, disabled, renderChild }: FieldControlProps) {
  switch (spec.type) {
    case "textarea":
      return (
        <textarea
          className="lc-df-input lc-df-textarea"
          value={asString(value)}
          placeholder={spec.placeholder}
          disabled={disabled}
          rows={4}
          onChange={(e) => onChange(e.target.value)}
        />
      );

    case "int":
    case "float":
      return (
        <input
          className="lc-df-input lc-df-int"
          type="number"
          step={spec.type === "float" ? "any" : 1}
          inputMode={spec.type === "float" ? "decimal" : "numeric"}
          value={typeof value === "number" ? String(value) : ""}
          placeholder={spec.placeholder}
          min={spec.min}
          max={spec.max}
          disabled={disabled}
          onChange={(e) => {
            const raw = e.target.value;
            // An emptied number box means "unset" — emit undefined so the row
            // deletes the key rather than persisting a bogus 0.
            if (raw.trim() === "") return onChange(undefined);
            const n = Number(raw);
            onChange(Number.isFinite(n) ? n : undefined);
          }}
        />
      );

    case "bool":
      return (
        <label className="lc-df-bool">
          <input
            type="checkbox"
            checked={value === true}
            disabled={disabled}
            onChange={(e) => onChange(e.target.checked)}
          />
          <span>{value === true ? "on" : "off"}</span>
        </label>
      );

    case "enum":
      return (
        <select
          className="lc-df-input"
          value={asString(value)}
          disabled={disabled}
          onChange={(e) => onChange(e.target.value === "" ? undefined : e.target.value)}
        >
          <option value="">(inherit)</option>
          {(spec.options ?? []).map((o) => (
            <option key={o} value={o}>{o}</option>
          ))}
        </select>
      );

    case "string-list":
      return <StringListControl value={asStringList(value)} disabled={disabled} placeholder={spec.placeholder} onChange={onChange} />;

    case "kv":
      return <KeyValueControl value={asRecord(value)} disabled={disabled} onChange={onChange} />;

    case "object":
      return (
        <div className="lc-df-nested">
          {(spec.fields ?? []).map((child) => {
            const parent = (value && typeof value === "object" && !Array.isArray(value) ? value : {}) as Record<string, unknown>;
            return renderChild?.(child, parent[child.key], (v) => {
              const next = { ...parent };
              if (v === undefined) delete next[child.key];
              else next[child.key] = v;
              onChange(Object.keys(next).length === 0 ? undefined : next);
            });
          })}
        </div>
      );

    case "object-array":
      return <ObjectArrayControl spec={spec} items={asObjectArray(value)} disabled={disabled} onChange={onChange} renderChild={renderChild} />;

    case "json":
      return <JsonControl value={value} disabled={disabled} placeholder={spec.placeholder} onChange={onChange} />;

    case "text":
    default:
      return (
        <input
          className="lc-df-input"
          type="text"
          value={asString(value)}
          placeholder={spec.placeholder}
          disabled={disabled}
          onChange={(e) => onChange(e.target.value)}
        />
      );
  }
}

// StringListControl edits []string as removable chips plus an add box. Chips
// beat a comma-joined text input because several of these lists hold values
// that may themselves contain commas (tool patterns, stop sequences).
function StringListControl({
  value, onChange, disabled, placeholder,
}: { value: string[]; onChange: (v: unknown) => void; disabled?: boolean; placeholder?: string }) {
  const [draft, setDraft] = useState("");
  const commit = () => {
    const v = draft.trim();
    if (v === "") return;
    onChange([...value, v]);
    setDraft("");
  };
  return (
    <div className="lc-df-chips">
      {value.map((item, i) => (
        <span className="lc-df-chip" key={`${item}-${i}`}>
          {item}
          <button
            type="button"
            className="lc-df-chip-x"
            disabled={disabled}
            aria-label={`Remove ${item}`}
            onClick={() => {
              const next = value.filter((_, j) => j !== i);
              onChange(next.length === 0 ? undefined : next);
            }}
          >×</button>
        </span>
      ))}
      <input
        className="lc-df-input lc-df-chip-add"
        type="text"
        value={draft}
        placeholder={placeholder ?? "add…"}
        disabled={disabled}
        onChange={(e) => setDraft(e.target.value)}
        onBlur={commit}
        onKeyDown={(e) => {
          if (e.key === "Enter") { e.preventDefault(); commit(); }
        }}
      />
    </div>
  );
}

// KeyValueControl edits a string map (env, headers). Rows are keyed by index so
// renaming a key doesn't remount and steal focus mid-edit.
function KeyValueControl({
  value, onChange, disabled,
}: { value: Record<string, string>; onChange: (v: unknown) => void; disabled?: boolean }) {
  const entries = Object.entries(value);
  const emit = (next: [string, string][]) => {
    const obj: Record<string, string> = {};
    for (const [k, v] of next) if (k.trim() !== "") obj[k] = v;
    onChange(Object.keys(obj).length === 0 ? undefined : obj);
  };
  return (
    <div className="lc-df-kv">
      {entries.map(([k, v], i) => (
        <div className="lc-df-kv-row" key={i}>
          <input
            className="lc-df-input lc-df-kv-key" type="text" value={k} placeholder="key" disabled={disabled}
            onChange={(e) => { const next = [...entries] as [string, string][]; next[i] = [e.target.value, v]; emit(next); }}
          />
          <input
            className="lc-df-input lc-df-kv-val" type="text" value={v} placeholder="value" disabled={disabled}
            onChange={(e) => { const next = [...entries] as [string, string][]; next[i] = [k, e.target.value]; emit(next); }}
          />
          <button
            type="button" className="lc-df-row-btn" disabled={disabled} aria-label={`Remove ${k}`}
            onClick={() => emit(entries.filter((_, j) => j !== i) as [string, string][])}
          >×</button>
        </div>
      ))}
      <button
        type="button" className="lc-df-row-btn lc-df-add" disabled={disabled}
        onClick={() => emit([...(entries as [string, string][]), ["", ""]])}
      >+ add entry</button>
    </div>
  );
}

// ObjectArrayControl edits []object (core_blocks, exposed_agents…): each item is
// a small card of the child fields.
function ObjectArrayControl({
  spec, items, onChange, disabled, renderChild,
}: {
  spec: FieldSpec; items: Record<string, unknown>[]; onChange: (v: unknown) => void; disabled?: boolean;
  renderChild?: FieldControlProps["renderChild"];
}) {
  const emit = (next: Record<string, unknown>[]) => onChange(next.length === 0 ? undefined : next);
  return (
    <div className="lc-df-objarray">
      {items.map((item, i) => (
        <div className="lc-df-objarray-item" key={i}>
          <div className="lc-df-objarray-head">
            <span className="lc-df-objarray-idx">#{i + 1}</span>
            <button
              type="button" className="lc-df-row-btn" disabled={disabled} aria-label={`Remove item ${i + 1}`}
              onClick={() => emit(items.filter((_, j) => j !== i))}
            >×</button>
          </div>
          {(spec.fields ?? []).map((child) =>
            renderChild?.(child, item[child.key], (v) => {
              const nextItem = { ...item };
              if (v === undefined) delete nextItem[child.key];
              else nextItem[child.key] = v;
              const next = [...items];
              next[i] = nextItem;
              emit(next);
            }),
          )}
        </div>
      ))}
      <button
        type="button" className="lc-df-row-btn lc-df-add" disabled={disabled}
        onClick={() => emit([...items, {}])}
      >+ add</button>
    </div>
  );
}

// JsonControl edits one free-form structured value. It keeps the raw text in
// local state so a half-typed document is not destroyed by a reparse on every
// keystroke, and only emits when the text actually parses — an invalid draft
// shows an inline error and leaves the last good value in the overlay.
function JsonControl({
  value, onChange, disabled, placeholder,
}: { value: unknown; onChange: (v: unknown) => void; disabled?: boolean; placeholder?: string }) {
  const pretty = value === undefined ? "" : JSON.stringify(value, null, 2);
  const [text, setText] = useState(pretty);
  const [err, setErr] = useState<string | null>(null);
  const [dirty, setDirty] = useState(false);
  // Re-sync from props while the operator is not mid-edit (e.g. a def reload).
  if (!dirty && text !== pretty) setText(pretty);
  return (
    <div className="lc-df-json">
      <textarea
        className={`lc-df-input lc-df-textarea${err ? " lc-df-invalid" : ""}`}
        value={text}
        rows={6}
        spellCheck={false}
        placeholder={placeholder ?? "{ }"}
        disabled={disabled}
        onChange={(e) => {
          const raw = e.target.value;
          setText(raw); setDirty(true);
          if (raw.trim() === "") { setErr(null); onChange(undefined); return; }
          try { onChange(JSON.parse(raw)); setErr(null); }
          catch (ex) { setErr(ex instanceof Error ? ex.message : "invalid JSON"); }
        }}
        onBlur={() => setDirty(false)}
      />
      {err && <p className="lc-df-error">{err}</p>}
    </div>
  );
}
