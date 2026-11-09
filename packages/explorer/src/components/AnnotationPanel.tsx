import { useState } from "react";
import type { Annotation, AnnotationLevel } from "../types";
import {
  ABSTRACT_MAX_CHARS,
  BRIEF_MAX_CHARS,
  annotationDraftProblem,
  codePoints,
  writtenAtLabel,
  writtenByLabel,
} from "../lib/annotation";

// AnnotationBrief is the one-line brief shown under a title. A stale record is
// still shown, with a muted marker, so the reader can weigh it.
export function AnnotationBrief({ record }: { record?: Annotation }) {
  if (!record) return null;
  return (
    <p className="annotation-brief" title="Annotation brief">
      {record.brief}
      {record.stale && <StaleBadge />}
    </p>
  );
}

function StaleBadge() {
  return (
    <span
      className="annotation-badge annotation-stale"
      title="The text has changed since this annotation was written"
    >
      stale
    </span>
  );
}

export interface AnnotationPanelProps {
  // The records of one chunk, ordered branch → document → path. Empty → the
  // panel draws only for a reader who can write or regenerate one.
  records: Annotation[];
  // newLevel is the level a hand-written annotation takes when there is none.
  newLevel: AnnotationLevel;
  // onSave writes an edit (always as a manual record); absent → no Edit.
  onSave?: (level: AnnotationLevel, brief: string, abstract: string) => Promise<void>;
  // onRegenerate asks the host to have the annotation rewritten; absent → no
  // Regenerate.
  onRegenerate?: (level: AnnotationLevel) => Promise<void>;
  // Starts expanded (tests and a host that wants the abstract open).
  defaultOpen?: boolean;
}

// AnnotationPanel shows a chunk's abstract and its provenance — level, language,
// model, when and by whom it was written, whether a person wrote it, whether it
// is stale — collapsed under one summary line. The abstract is rendered as plain
// text: it is model output, never markup.
export default function AnnotationPanel({
  records,
  newLevel,
  onSave,
  onRegenerate,
  defaultOpen,
}: AnnotationPanelProps) {
  const [open, setOpen] = useState(!!defaultOpen);
  const [editing, setEditing] = useState<AnnotationLevel | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  if (records.length === 0 && !onSave && !onRegenerate) return null;

  const regenerate = async (level: AnnotationLevel) => {
    if (!onRegenerate) return;
    setBusy(true);
    setErr(null);
    try {
      await onRegenerate(level);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const anyStale = records.some((r) => r.stale);
  const anyManual = records.some((r) => r.manual);

  return (
    <details
      className="annotation-panel"
      open={open || editing !== null}
      onToggle={(e) => setOpen(e.currentTarget.open)}
    >
      <summary>
        <span className="annotation-title">Annotation</span>
        {records.length === 0 ? (
          <span className="annotation-none">none</span>
        ) : (
          records.map((r) => (
            <span className="annotation-badge" key={r.level}>
              {r.level}
            </span>
          ))
        )}
        {anyManual && (
          <span className="annotation-badge annotation-manual" title="Written or edited by a person">
            manual
          </span>
        )}
        {anyStale && <StaleBadge />}
      </summary>
      {records.map((r) =>
        editing === r.level ? (
          <AnnotationEditor
            key={r.level}
            level={r.level}
            brief={r.brief}
            abstract={r.abstract}
            onCancel={() => setEditing(null)}
            onSave={async (b, a) => {
              await onSave!(r.level, b, a);
              setEditing(null);
            }}
          />
        ) : (
          <div className="annotation-record" key={r.level}>
            {records.length > 1 && <p className="annotation-brief">{r.brief}</p>}
            {r.abstract && <p className="annotation-abstract">{r.abstract}</p>}
            <dl className="annotation-meta">
              <dt>level</dt>
              <dd>{r.level}</dd>
              {r.language && (
                <>
                  <dt>language</dt>
                  <dd>{r.language}</dd>
                </>
              )}
              {r.model && (
                <>
                  <dt>model</dt>
                  <dd>{r.model}</dd>
                </>
              )}
              {r.written_at && (
                <>
                  <dt>written</dt>
                  <dd title={r.written_at}>{writtenAtLabel(r.written_at)}</dd>
                </>
              )}
              {r.written_by && (
                <>
                  <dt>by</dt>
                  <dd>{writtenByLabel(r.written_by)}</dd>
                </>
              )}
              {r.manual && (
                <>
                  <dt>source</dt>
                  <dd>written by hand</dd>
                </>
              )}
              {r.stale && (
                <>
                  <dt>state</dt>
                  <dd>stale: the text changed after this was written</dd>
                </>
              )}
            </dl>
            {(onSave || onRegenerate) && (
              <div className="annotation-actions">
                {onSave && (
                  <button type="button" disabled={busy} onClick={() => setEditing(r.level)}>
                    edit
                  </button>
                )}
                {onRegenerate && (
                  <button
                    type="button"
                    disabled={busy}
                    onClick={() => void regenerate(r.level)}
                    title="Ask for it to be written again from the current text"
                  >
                    {busy ? "regenerating…" : "regenerate"}
                  </button>
                )}
              </div>
            )}
          </div>
        ),
      )}
      {records.length === 0 &&
        (editing !== null && onSave ? (
          <AnnotationEditor
            level={editing}
            brief=""
            abstract=""
            onCancel={() => setEditing(null)}
            onSave={async (b, a) => {
              await onSave(editing, b, a);
              setEditing(null);
            }}
          />
        ) : (
          <div className="annotation-record">
            <p className="annotation-empty">No annotation has been written for this yet.</p>
            <div className="annotation-actions">
              {onSave && (
                <button type="button" disabled={busy} onClick={() => setEditing(newLevel)}>
                  write
                </button>
              )}
              {onRegenerate && (
                <button type="button" disabled={busy} onClick={() => void regenerate(newLevel)}>
                  {busy ? "generating…" : "generate"}
                </button>
              )}
            </div>
          </div>
        ))}
      {err && <p className="annotation-err">{err}</p>}
    </details>
  );
}

// AnnotationEditor is the inline brief + abstract form. It checks the length
// caps before sending; a refusal from the runtime is shown under the form.
function AnnotationEditor({
  level,
  brief: initialBrief,
  abstract: initialAbstract,
  onSave,
  onCancel,
}: {
  level: AnnotationLevel;
  brief: string;
  abstract: string;
  onSave: (brief: string, abstract: string) => Promise<void>;
  onCancel: () => void;
}) {
  const [brief, setBrief] = useState(initialBrief);
  const [abstract, setAbstract] = useState(initialAbstract);
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const problem = annotationDraftProblem(brief, abstract);

  const save = async () => {
    if (problem) {
      setErr(problem);
      return;
    }
    setSaving(true);
    setErr(null);
    try {
      await onSave(brief, abstract);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  };

  return (
    <form
      className="annotation-editor"
      onSubmit={(e) => {
        e.preventDefault();
        void save();
      }}
    >
      <label>
        <span>
          Brief ({level}) · {codePoints(brief)}/{BRIEF_MAX_CHARS}
        </span>
        <input value={brief} onChange={(e) => setBrief(e.target.value)} placeholder="One sentence" />
      </label>
      <label>
        <span>
          Abstract · {codePoints(abstract)}/{ABSTRACT_MAX_CHARS}
        </span>
        <textarea
          rows={6}
          value={abstract}
          onChange={(e) => setAbstract(e.target.value)}
          placeholder="One paragraph"
        />
      </label>
      <p className="annotation-hint">Saved as written by hand: the background walk will not replace it.</p>
      {err && <p className="annotation-err">{err}</p>}
      <div className="annotation-actions">
        <button type="button" onClick={onCancel} disabled={saving}>
          cancel
        </button>
        <button type="submit" className="primary" disabled={saving || !!problem}>
          {saving ? "saving…" : "save"}
        </button>
      </div>
    </form>
  );
}
