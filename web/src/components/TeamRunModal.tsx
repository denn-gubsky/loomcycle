import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  type DocumentRow,
  type TeamNameSummary,
  documentQueryChunks,
  documentQueryDocuments,
  getTeamDef,
  runTeam,
} from "../api";
import { runRowHref } from "../lib/runLineage";
import {
  type ChunkOption,
  type EntryForm,
  type FieldValue,
  type FormField,
  type FormValues,
  buildInput,
  chunkOptions,
  chunkPickerEnabled,
  entryForm,
  initialValues,
  setFieldValue,
} from "../lib/teamStart";

// TeamRunModal starts a walk of a team from its input form. It reads the team's
// ACTIVE version (what op=run starts — not the editor's unsaved text), renders
// the entry state's form, and starts the walk detached so the page never waits
// on it; the walk view then opens on the returned run id. The server checks
// the input and names a bad field in its refusal, shown next to the form.

const msg = (e: unknown) => (e instanceof Error ? e.message : String(e));

export default function TeamRunModal({ team, onClose }: { team: TeamNameSummary; onClose: () => void }) {
  const navigate = useNavigate();
  const [form, setForm] = useState<EntryForm | null>(null);
  const [loadErr, setLoadErr] = useState("");
  const [values, setValues] = useState<FormValues>({});
  const [text, setText] = useState("");
  const [err, setErr] = useState("");
  const [submitting, setSubmitting] = useState(false);

  useEffect(() => {
    const defId = team.active_def_id;
    if (!defId) return;
    let cancelled = false;
    void (async () => {
      try {
        const d = await getTeamDef(defId);
        // Like the editor: `definition` normally arrives inlined as an object.
        const def = typeof d.definition === "string" ? JSON.parse(d.definition) : d.definition;
        if (cancelled) return;
        const f = entryForm(def);
        setForm(f);
        if (f.kind === "schema") setValues(initialValues(f.fields));
      } catch (e) {
        if (!cancelled) setLoadErr(msg(e));
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [team.active_def_id]);

  const submit = async () => {
    if (!form) return;
    setErr("");
    let input = text;
    if (form.kind === "schema") {
      const built = buildInput(form.fields, values);
      if (!built.ok) {
        setErr(built.error);
        return;
      }
      input = built.input;
    }
    setSubmitting(true);
    try {
      const res = await runTeam(team.name, input);
      navigate(runRowHref({ runId: res.run_id, agentId: `team:${team.name}` }));
    } catch (e) {
      setErr(msg(e));
      setSubmitting(false);
    }
  };

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal library-modal" onClick={(e) => e.stopPropagation()}>
        <h3>Run team: {team.name}</h3>
        {!team.active_def_id && <div className="error-banner">This team has no active version to run.</div>}
        {loadErr && <div className="error-banner">Failed to load the team: {loadErr}</div>}
        {team.active_def_id && !form && !loadErr && <p style={{ opacity: 0.7 }}>loading…</p>}

        {form?.kind === "schema" && (
          <TeamRunFields
            fields={form.fields}
            values={values}
            disabled={submitting}
            onChange={(name, v) => setValues((cur) => setFieldValue(form.fields, cur, name, v))}
          />
        )}
        {form?.kind === "text" && (
          <div className="library-modal-fields">
            <label className="library-modal-field">
              <span>Input</span>
              <textarea
                rows={8}
                value={text}
                onChange={(e) => setText(e.target.value)}
                disabled={submitting}
                placeholder="What the team should work on"
              />
            </label>
          </div>
        )}

        {err && <div className="error-banner">{err}</div>}

        <div className="modal-buttons">
          <button type="button" onClick={onClose} disabled={submitting}>
            Cancel
          </button>
          <button type="button" className="primary" onClick={() => void submit()} disabled={submitting || !form}>
            {submitting ? "Starting…" : "Run"}
          </button>
        </div>
      </div>
    </div>
  );
}

export interface TeamRunFieldsProps {
  fields: FormField[];
  values: FormValues;
  disabled: boolean;
  onChange: (name: string, v: FieldValue) => void;
}

// TeamRunFields renders the schema form's controls; data for the pickers is
// loaded by the picker controls themselves.
export function TeamRunFields({ fields, values, disabled, onChange }: TeamRunFieldsProps) {
  return (
    <div className="library-modal-fields">
      {fields.map((f) => (
        <FieldControl key={f.name} field={f} fields={fields} values={values} disabled={disabled} onChange={onChange} />
      ))}
    </div>
  );
}

function FieldLabel({ field }: { field: FormField }) {
  return (
    <span>
      {field.label}
      {field.required && (
        <span className="team-run-required" title="required" aria-label="required">
          {" "}*
        </span>
      )}
      {field.description && <span className="library-modal-field-hint"> — {field.description}</span>}
    </span>
  );
}

function FieldControl({
  field, fields, values, disabled, onChange,
}: {
  field: FormField;
  fields: FormField[];
  values: FormValues;
  disabled: boolean;
  onChange: (name: string, v: FieldValue) => void;
}) {
  const v = values[field.name];
  const s = typeof v === "string" ? v : "";
  const set = (x: FieldValue) => onChange(field.name, x);

  if (field.kind === "boolean") {
    return (
      <label className="library-modal-field memory-modal-checkbox-field">
        <FieldLabel field={field} />
        <input type="checkbox" checked={v === true} onChange={(e) => set(e.target.checked)} disabled={disabled} />
      </label>
    );
  }

  let control;
  switch (field.kind) {
    case "number":
    case "integer":
      control = (
        <input type="number" step={field.kind === "integer" ? 1 : "any"} value={s} onChange={(e) => set(e.target.value)} disabled={disabled} />
      );
      break;
    case "enum":
      control = (
        <select value={s} onChange={(e) => set(e.target.value)} disabled={disabled}>
          <option value="">—</option>
          {(field.enumValues ?? []).map((ev, i) => (
            <option key={i} value={String(i)}>
              {typeof ev === "string" ? ev : JSON.stringify(ev)}
            </option>
          ))}
        </select>
      );
      break;
    case "json":
      control = (
        <textarea rows={4} value={s} onChange={(e) => set(e.target.value)} disabled={disabled} spellCheck={false} placeholder="JSON value" />
      );
      break;
    case "document":
      control = <DocumentPicker field={field} value={s} disabled={disabled} onChange={set} />;
      break;
    case "chunk": {
      const docName = field.picker?.kind === "chunk" ? field.picker.document : "";
      const docField = fields.find((x) => x.name === docName);
      control = (
        <ChunkPicker
          field={field}
          value={s}
          documentId={String(values[docName] ?? "")}
          documentLabel={docField?.label ?? docName}
          disabled={disabled || !chunkPickerEnabled(field, values)}
          onChange={set}
        />
      );
      break;
    }
    default:
      control = <input type="text" value={s} onChange={(e) => set(e.target.value)} disabled={disabled} />;
  }
  return (
    <label className="library-modal-field">
      <FieldLabel field={field} />
      {control}
    </label>
  );
}

// DocumentPicker lists the documents in the picker's scope (optionally under a
// Path prefix). If the list cannot be read it degrades to a text box, so an id
// can still be typed in.
function DocumentPicker({
  field, value, disabled, onChange,
}: {
  field: FormField;
  value: string;
  disabled: boolean;
  onChange: (v: string) => void;
}) {
  const picker = field.picker?.kind === "document" ? field.picker : undefined;
  const scope = picker?.scope ?? "user";
  const underPath = picker?.underPath;
  const [docs, setDocs] = useState<DocumentRow[] | null>(null);
  const [loadErr, setLoadErr] = useState("");

  useEffect(() => {
    let cancelled = false;
    setDocs(null);
    setLoadErr("");
    documentQueryDocuments(scope, underPath).then(
      (r) => {
        if (!cancelled) setDocs(r.documents ?? []);
      },
      (e) => {
        if (!cancelled) setLoadErr(msg(e));
      },
    );
    return () => {
      cancelled = true;
    };
  }, [scope, underPath]);

  if (loadErr) {
    return (
      <>
        <input type="text" value={value} onChange={(e) => onChange(e.target.value)} disabled={disabled} placeholder="document id" />
        <span className="library-modal-field-hint">Could not list documents ({loadErr}); enter an id.</span>
      </>
    );
  }
  const where = `${scope} documents${underPath ? ` under ${underPath}` : ""}`;
  return (
    <>
      <select value={value} onChange={(e) => onChange(e.target.value)} disabled={disabled || docs === null}>
        <option value="">{docs === null ? "loading…" : docs.length === 0 ? `no ${where}` : "— choose a document —"}</option>
        {(docs ?? []).map((d) => (
          <option key={d.document_id} value={d.document_id}>
            {d.title || d.document_id}
          </option>
        ))}
      </select>
      <span className="library-modal-field-hint">From {where}.</span>
    </>
  );
}

// ChunkPicker lists the chosen document's chunks, indented by depth; it waits
// (disabled) until that document is chosen.
function ChunkPicker({
  field, value, documentId, documentLabel, disabled, onChange,
}: {
  field: FormField;
  value: string;
  documentId: string;
  documentLabel: string;
  disabled: boolean;
  onChange: (v: string) => void;
}) {
  const picker = field.picker?.kind === "chunk" ? field.picker : undefined;
  const scope = picker?.scope ?? "user";
  const depth = picker?.depth;
  const [opts, setOpts] = useState<ChunkOption[] | null>(null);
  const [loadErr, setLoadErr] = useState("");

  useEffect(() => {
    setOpts(null);
    setLoadErr("");
    if (!documentId) return;
    let cancelled = false;
    documentQueryChunks(documentId, scope).then(
      (r) => {
        if (!cancelled) setOpts(chunkOptions(r.chunks ?? [], depth));
      },
      (e) => {
        if (!cancelled) setLoadErr(msg(e));
      },
    );
    return () => {
      cancelled = true;
    };
  }, [documentId, scope, depth]);

  if (loadErr) {
    return (
      <>
        <input type="text" value={value} onChange={(e) => onChange(e.target.value)} disabled={disabled} placeholder="chunk id" />
        <span className="library-modal-field-hint">Could not list chunks ({loadErr}); enter an id.</span>
      </>
    );
  }
  const placeholder = !documentId
    ? `choose ${documentLabel} first`
    : opts === null
      ? "loading…"
      : opts.length === 0
        ? "no chunks"
        : "— choose a chunk —";
  return (
    <select value={value} onChange={(e) => onChange(e.target.value)} disabled={disabled || opts === null}>
      <option value="">{placeholder}</option>
      {(opts ?? []).map((o) => (
        <option key={o.id} value={o.id}>
          {"  ".repeat(o.depth - 1)}
          {o.title}
        </option>
      ))}
    </select>
  );
}
