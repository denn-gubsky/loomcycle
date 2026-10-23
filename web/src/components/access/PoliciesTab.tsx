import { useCallback, useEffect, useState } from "react";
import { FlaskConical, Plus } from "lucide-react";
import { getAclDef, listAclDefs, putAclDef, retireAclDef, validateAclRules, type AclDefRow, type AclList } from "../../api";
import { isSetTime } from "../../lib/aclAccess";
import {
  emptyRuleDraft,
  listNameError,
  rulesFromSlots,
  rulesFromText,
  rulesToText,
  slotsFromRules,
  validateDraft,
  draftDocFor,
  type AclCatalog,
  type Slot,
} from "../../lib/aclRules";
import ErrorNotice from "../ErrorNotice";
import { RuleEditor } from "./RuleEditor";

// PoliciesTab — ACL rule lists: the tenant's stored lists and the built-in
// baselines (code constants, read-only), and an editor whose rules are
// authored either in the structured editor (catalog-driven pickers) or in the
// JSON/YAML view. Save validates with op=validate first, then op=create.

export interface Editing {
  name: string;
  description: string;
  isNew: boolean;
  builtin: boolean;
  retired: boolean;
  version?: number;
  slots: Slot[];
}

export default function PoliciesTab({
  tenant,
  catalog,
  onSimulate,
}: {
  tenant?: string;
  catalog: AclCatalog | null;
  onSimulate: (draftYaml: string) => void;
}) {
  const [rows, setRows] = useState<AclDefRow[] | null>(null);
  const [builtin, setBuiltin] = useState<AclList[]>([]);
  const [includeRetired, setIncludeRetired] = useState(false);
  const [err, setErr] = useState<unknown>(null);
  const [editing, setEditing] = useState<Editing | null>(null);
  // Bumped each time a list is opened or started, and NOT on save: the editor
  // keeps its state (and its "saved" note) across a save that bumps the version.
  const [openSeq, setOpenSeq] = useState(0);
  const startEditing = (e: Editing) => {
    setOpenSeq((n) => n + 1);
    setEditing(e);
  };

  const refresh = useCallback(async () => {
    setErr(null);
    try {
      const r = await listAclDefs(includeRetired, tenant);
      setRows(r.acls ?? []);
      setBuiltin(r.builtin ?? []);
    } catch (e) {
      setErr(e);
      setRows([]);
    }
  }, [includeRetired, tenant]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const open = async (name: string) => {
    if (!catalog) return;
    setErr(null);
    try {
      const row = await getAclDef(name, tenant);
      startEditing({
        name: row.name,
        description: row.description ?? "",
        isNew: false,
        builtin: false,
        retired: isSetTime(row.retired_at),
        version: row.version,
        slots: slotsFromRules(catalog, row.rules ?? []),
      });
    } catch (e) {
      setErr(e);
    }
  };

  return (
    <div className="acl-tab">
      <div className="acl-toolbar">
        <button
          type="button"
          className="primary-btn"
          disabled={!catalog}
          onClick={() =>
            catalog &&
            startEditing({ name: "", description: "", isNew: true, builtin: false, retired: false, slots: [{ kind: "draft", draft: emptyRuleDraft(catalog) }] })
          }
        >
          <Plus size={14} /> new list
        </button>
        <label className="acl-check">
          <input type="checkbox" checked={includeRetired} onChange={(e) => setIncludeRetired(e.target.checked)} /> show retired
        </label>
        <button type="button" className="ghost-btn" onClick={() => void refresh()}>
          refresh
        </button>
      </div>
      <ErrorNotice error={err} />

      <PolicyList
        rows={rows}
        builtin={builtin}
        selected={editing?.name}
        onOpen={(n) => void open(n)}
        onOpenBuiltin={(b) =>
          catalog &&
          startEditing({ name: b.name, description: b.description ?? "", isNew: false, builtin: true, retired: false, slots: slotsFromRules(catalog, b.rules ?? []) })
        }
      />

      {editing && catalog && (
        <PolicyEditor
          key={openSeq}
          catalog={catalog}
          initial={editing}
          tenant={tenant}
          onClose={() => setEditing(null)}
          onSaved={(row) => {
            setEditing({ ...editing, name: row.name, isNew: false, retired: false, version: row.version, slots: slotsFromRules(catalog, row.rules ?? []) });
            void refresh();
          }}
          onRetired={() => {
            setEditing(null);
            void refresh();
          }}
          onSimulate={onSimulate}
        />
      )}
    </div>
  );
}

export function PolicyList({
  rows,
  builtin,
  selected,
  onOpen,
  onOpenBuiltin,
}: {
  rows: AclDefRow[] | null;
  builtin: AclList[];
  selected?: string;
  onOpen: (name: string) => void;
  onOpenBuiltin: (b: AclList) => void;
}) {
  if (rows === null) return <div className="settings-muted">loading…</div>;
  return (
    <table className="settings-table acl-table">
      <thead>
        <tr>
          <th>name</th>
          <th>rules</th>
          <th>version</th>
          <th>description</th>
          <th>status</th>
        </tr>
      </thead>
      <tbody>
        {rows.length === 0 && (
          <tr>
            <td colSpan={5} className="settings-muted">
              no rule lists in this tenant yet — create one with “new list”. The built-in lists below apply until you do.
            </td>
          </tr>
        )}
        {rows.map((r) => (
          <tr key={r.name} className={selected === r.name ? "acl-row-selected" : undefined}>
            <td>
              <button type="button" className="acl-link" onClick={() => onOpen(r.name)}>
                <code>{r.name}</code>
              </button>
            </td>
            <td>{(r.rules ?? []).length}</td>
            <td className="settings-muted">v{r.version}</td>
            <td className="settings-muted">{r.description || "—"}</td>
            <td>
              <span className={"status-pill " + (isSetTime(r.retired_at) ? "status-retired" : "status-ok")}>
                {isSetTime(r.retired_at) ? "retired" : "live"}
              </span>
            </td>
          </tr>
        ))}
        {builtin.map((b) => (
          <tr key={b.name} className={"acl-row-builtin" + (selected === b.name ? " acl-row-selected" : "")}>
            <td>
              <button type="button" className="acl-link" onClick={() => onOpenBuiltin(b)}>
                <code>{b.name}</code>
              </button>
            </td>
            <td>{(b.rules ?? []).length}</td>
            <td className="settings-muted">—</td>
            <td className="settings-muted">{b.description || "—"}</td>
            <td>
              <span className="status-pill acl-pill-builtin" title="A code constant: the baseline for a token class. Read-only.">
                built-in · read-only
              </span>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

export function PolicyEditor({
  catalog,
  initial,
  tenant,
  onClose,
  onSaved,
  onRetired,
  onSimulate,
}: {
  catalog: AclCatalog;
  initial: Editing;
  tenant?: string;
  onClose: () => void;
  onSaved: (row: AclDefRow) => void;
  onRetired: () => void;
  onSimulate: (draftYaml: string) => void;
}) {
  const [name, setName] = useState(initial.name);
  const [description, setDescription] = useState(initial.description);
  const [slots, setSlots] = useState<Slot[]>(initial.slots);
  const [format, setFormat] = useState<"yaml" | "json">("yaml");
  // null = the text view mirrors the structured rules; a string = the
  // operator is editing the text, not yet applied.
  const [text, setText] = useState<string | null>(null);
  const [textErr, setTextErr] = useState("");
  const [status, setStatus] = useState<{ ok: boolean; msg: string } | null>(null);
  const [err, setErr] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const readOnly = initial.builtin;

  const rules = rulesFromSlots(catalog, slots);
  const shownText = text ?? rulesToText(rules, format);
  const draftErrors = slots.some((s) => s.kind === "draft" && validateDraft(catalog, s.draft).length > 0);
  const nameErr = initial.isNew ? listNameError(name) : null;

  const setSlot = (i: number, s: Slot) => setSlots(slots.map((x, j) => (j === i ? s : x)));
  const move = (i: number, delta: -1 | 1) => {
    const next = [...slots];
    const [s] = next.splice(i, 1);
    next.splice(i + delta, 0, s);
    setSlots(next);
  };

  const applyText = () => {
    const res = rulesFromText(shownText);
    if (!res.ok) {
      setTextErr(res.error);
      return;
    }
    setSlots(slotsFromRules(catalog, res.rules));
    setText(null);
    setTextErr("");
  };

  const validate = async (): Promise<boolean> => {
    setErr(null);
    setStatus(null);
    try {
      const r = await validateAclRules(rules, tenant);
      setStatus({ ok: true, msg: `valid — ${r.rules} rule${r.rules === 1 ? "" : "s"} (catalog v${r.catalog_version})` });
      return true;
    } catch (e) {
      setErr(e);
      return false;
    }
  };

  const save = async () => {
    if (busy || nameErr || draftErrors || text !== null) return;
    setBusy(true);
    try {
      if (!(await validate())) return;
      const row = await putAclDef({ name: name.trim(), description: description.trim() || undefined, rules }, tenant);
      setStatus({ ok: true, msg: `saved ${row.name} v${row.version}` });
      onSaved(row);
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(false);
    }
  };

  const retire = async () => {
    if (!confirm(`Retire list "${initial.name}"? Bindings to it stay and match nothing until it is saved again.`)) return;
    setBusy(true);
    try {
      await retireAclDef(initial.name, tenant);
      onRetired();
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="acl-editor">
      <div className="acl-editor-head">
        <h3>
          {initial.isNew ? "new rule list" : <code>{initial.name}</code>}
          {initial.version ? <span className="settings-muted"> v{initial.version}</span> : null}
          {readOnly && <span className="status-pill acl-pill-builtin">built-in · read-only</span>}
          {initial.retired && <span className="status-pill status-retired">retired — saving brings it back</span>}
        </h3>
        <button type="button" className="ghost-btn" onClick={onClose}>
          close
        </button>
      </div>
      <div className="acl-form-row">
        <input
          type="text"
          aria-label="list name"
          placeholder="list name, e.g. support/tier-1"
          value={name}
          disabled={!initial.isNew}
          onChange={(e) => setName(e.target.value)}
        />
        <input
          type="text"
          aria-label="list description"
          placeholder="description (optional)"
          value={description}
          disabled={readOnly}
          onChange={(e) => setDescription(e.target.value)}
        />
      </div>
      {nameErr && name !== "" && <div className="settings-error">{nameErr}</div>}
      <p className="settings-help">
        Rules are unordered: any matching <code>deny</code> wins over every <code>allow</code>, and a question no rule answers falls to
        the mode (the scopes in compat/shadow, a refusal in enforce). The <em>rule N</em> number is the rule’s index, the one a
        refusal names.
      </p>

      {slots.map((s, i) =>
        s.kind === "draft" ? (
          <RuleEditor
            key={i}
            catalog={catalog}
            draft={s.draft}
            index={i}
            count={slots.length}
            readOnly={readOnly}
            onChange={(d) => setSlot(i, { kind: "draft", draft: d })}
            onRemove={slots.length > 1 ? () => setSlots(slots.filter((_, j) => j !== i)) : undefined}
            onMove={(delta) => move(i, delta)}
          />
        ) : (
          <div className="acl-rule acl-rule-raw" key={i}>
            <div className="acl-rule-head">
              <span className="acl-rule-index">rule {i}</span>
              <span className="settings-muted">{s.why}</span>
            </div>
            <pre className="acl-raw">{JSON.stringify(s.rule, null, 2)}</pre>
          </div>
        ),
      )}
      {!readOnly && (
        <button type="button" className="ghost-btn" onClick={() => setSlots([...slots, { kind: "draft", draft: emptyRuleDraft(catalog) }])}>
          <Plus size={14} /> rule
        </button>
      )}

      <details className="acl-text-view" open={text !== null}>
        <summary>JSON / YAML view</summary>
        <div className="acl-text-tools">
          <select aria-label="format" value={format} onChange={(e) => setFormat(e.target.value as "yaml" | "json")} disabled={text !== null}>
            <option value="yaml">YAML</option>
            <option value="json">JSON</option>
          </select>
          {text !== null && (
            <>
              <button type="button" className="primary-btn" onClick={applyText}>
                apply to the rules
              </button>
              <button
                type="button"
                className="ghost-btn"
                onClick={() => {
                  setText(null);
                  setTextErr("");
                }}
              >
                discard text edits
              </button>
            </>
          )}
        </div>
        <textarea
          className="acl-text"
          aria-label="rules as text"
          spellCheck={false}
          rows={Math.min(30, Math.max(8, shownText.split("\n").length + 1))}
          value={shownText}
          readOnly={readOnly}
          onChange={(e) => setText(e.target.value)}
        />
        {textErr && <div className="settings-error">{textErr}</div>}
      </details>

      <div className="acl-editor-actions">
        <button type="button" className="ghost-btn" onClick={() => void validate()} disabled={busy || text !== null}>
          validate
        </button>
        <button
          type="button"
          className="ghost-btn"
          title="Evaluate a question against this list as drafted — nothing is saved"
          onClick={() => onSimulate(draftDocFor({ name: name.trim() || "draft", ...(description.trim() ? { description: description.trim() } : {}), rules }))}
          disabled={text !== null}
        >
          <FlaskConical size={14} /> simulate unsaved
        </button>
        {!readOnly && (
          <button
            type="button"
            className="primary-btn"
            onClick={() => void save()}
            disabled={busy || !!nameErr || draftErrors || text !== null}
            title={text !== null ? "Apply or discard the text edits first" : draftErrors ? "Fix the rule errors first" : undefined}
          >
            {busy ? "saving…" : "save"}
          </button>
        )}
        {!readOnly && !initial.isNew && !initial.retired && (
          <button type="button" className="ghost-btn danger" onClick={() => void retire()} disabled={busy}>
            retire
          </button>
        )}
        {status && <span className={status.ok ? "settings-flash" : "settings-error"}>{status.msg}</span>}
      </div>
      <ErrorNotice error={err} />
    </div>
  );
}
