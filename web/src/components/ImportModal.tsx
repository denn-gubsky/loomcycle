import { useMemo, useState } from "react";

import {
  createDef,
  forkDef,
  type LibraryEntry,
  type ServerCapabilities,
} from "../api";
import {
  detectKind,
  parseSource,
  resolvePreview,
  type ImportCandidate,
  type PreviewRow,
} from "../lib/claudeImport";
import { explainServerError } from "./LibraryEditModal";

// LocalAgentSeed is what "Use with a local LLM" hands back to LibraryView to
// prefill an agent-create modal (RFC AU §"Use with a local LLM").
export interface LocalAgentSeed {
  skills: string[];
  mcpServers: string[];
}

export interface ImportModalProps {
  // The tenant's current Library entries — used for dry-run collision
  // resolution (create vs fork vs reclaim). Both kinds, since an mcp.json can
  // define servers regardless of which tab launched the import.
  skills: LibraryEntry[];
  mcp: LibraryEntry[];
  capabilities?: ServerCapabilities;
  isTenant: boolean;
  onClose: () => void;
  onImported: () => void; // refresh the Library
  onWireLocalAgent?: (seed: LocalAgentSeed) => void;
}

type Step = "source" | "preview" | "results";
type SourceKind = "auto" | "skill" | "mcp";

interface RowResult {
  status: "pending" | "ok" | "error" | "skipped";
  message?: string;
}

// ImportModal — RFC AU. A three-step flow (source → dry-run preview → commit)
// that ingests a Claude Code SKILL.md or mcp.json into the tenant's substrate
// via the existing create/fork endpoints. All parsing is client-side
// (claudeImport.ts); the server 422s remain authoritative.
export default function ImportModal(props: ImportModalProps) {
  const [step, setStep] = useState<Step>("source");
  const [text, setText] = useState("");
  const [filename, setFilename] = useState<string | undefined>(undefined);
  const [sourceKind, setSourceKind] = useState<SourceKind>("auto");
  const [parseErrors, setParseErrors] = useState<string[]>([]);
  const [parseWarnings, setParseWarnings] = useState<string[]>([]);
  const [candidates, setCandidates] = useState<ImportCandidate[]>([]);
  const [results, setResults] = useState<Record<number, RowResult>>({});
  const [committing, setCommitting] = useState(false);

  const stdioAllowed = props.capabilities?.mcp_allow_dynamic_stdio === true;
  const httpAllowlistConfigured =
    props.capabilities?.http_host_allowlist_configured === true;

  const rows: PreviewRow[] = useMemo(
    () =>
      resolvePreview(
        candidates,
        { skills: props.skills, mcp: props.mcp },
        { stdioAllowed, httpAllowlistConfigured, isTenant: props.isTenant },
      ),
    [candidates, props.skills, props.mcp, stdioAllowed, httpAllowlistConfigured, props.isTenant],
  );

  const importable = rows.filter((r) => r.action !== "blocked").length;

  const doParse = () => {
    const kind = sourceKind === "auto" ? detectKind(text, filename) : sourceKind;
    if (kind === "unknown") {
      setParseErrors([
        "Could not tell if this is a SKILL.md or an mcp.json — pick the type below.",
      ]);
      setParseWarnings([]);
      return;
    }
    const res = parseSource(text, kind, filename);
    setParseErrors(res.errors);
    setParseWarnings(res.warnings);
    if (res.candidates.length === 0) return;
    setCandidates(res.candidates);
    setResults({});
    setStep("preview");
  };

  const onFile = async (f: File | null) => {
    if (!f) return;
    setFilename(f.name);
    setText(await f.text());
    setParseErrors([]);
  };

  const renameCandidate = (i: number, name: string) =>
    setCandidates((cs) => cs.map((c, idx) => (idx === i ? { ...c, name } : c)));

  const commit = async () => {
    setCommitting(true);
    setStep("results");
    const next: Record<number, RowResult> = {};
    for (let i = 0; i < rows.length; i++) {
      const row = rows[i]!;
      if (row.action === "blocked") {
        next[i] = { status: "skipped", message: row.note };
        setResults({ ...next });
        continue;
      }
      next[i] = { status: "pending" };
      setResults({ ...next });
      const kind = row.candidate.kind === "skill" ? "skilldef" : "mcpserverdef";
      try {
        // Imported defs are promoted (active) immediately — the operator
        // imported them to use them.
        if (row.action === "fork") {
          await forkDef(kind, row.candidate.name, row.candidate.overlay, true, row.parentDefId);
        } else {
          await createDef(kind, row.candidate.name, row.candidate.overlay, true);
        }
        next[i] = { status: "ok" };
      } catch (e) {
        next[i] = { status: "error", message: explainServerError(e) };
      }
      setResults({ ...next });
    }
    setCommitting(false);
    props.onImported();
  };

  const importedSkills = rows
    .filter((r, i) => results[i]?.status === "ok" && r.candidate.kind === "skill")
    .map((r) => r.candidate.name);
  const importedMcp = rows
    .filter((r, i) => results[i]?.status === "ok" && r.candidate.kind === "mcp-server")
    .map((r) => r.candidate.name);

  return (
    <div className="modal-overlay" onClick={committing ? undefined : props.onClose}>
      <div className="modal library-modal" onClick={(e) => e.stopPropagation()}>
        <h3>Import Claude Code skill / MCP server</h3>

        {step === "source" && (
          <>
            <p className="library-modal-field-hint">
              Paste or upload a <code>SKILL.md</code> or an{" "}
              <code>mcp.json</code> / <code>.mcp.json</code>. Everything imports
              into <strong>your tenant's</strong> storage.
            </p>
            <div className="library-form-row">
              <label htmlFor="import-file">upload a file</label>
              <input
                id="import-file"
                type="file"
                accept=".md,.json,text/markdown,application/json"
                onChange={(e) => onFile(e.target.files?.[0] ?? null)}
                disabled={committing}
              />
            </div>
            <div className="library-form-row">
              <label htmlFor="import-text">…or paste the content</label>
              <textarea
                id="import-text"
                className="library-prompt-textarea mono"
                value={text}
                onChange={(e) => {
                  setText(e.target.value);
                  setFilename(undefined);
                }}
                rows={12}
                spellCheck={false}
                placeholder={"---\nname: my-skill\ndescription: …\nallowed-tools: [WebFetch]\n---\n# Instructions…"}
                disabled={committing}
              />
            </div>
            <div className="library-form-row">
              <label>type</label>
              <div className="library-radio-group">
                {(["auto", "skill", "mcp"] as SourceKind[]).map((k) => (
                  <label key={k}>
                    <input
                      type="radio"
                      name="import-kind"
                      checked={sourceKind === k}
                      onChange={() => setSourceKind(k)}
                      disabled={committing}
                    />{" "}
                    {k === "auto" ? "auto-detect" : k === "skill" ? "skill" : "mcp config"}
                  </label>
                ))}
              </div>
            </div>
            {parseErrors.map((e, i) => (
              <div key={i} className="modal-err">
                {e}
              </div>
            ))}
            {parseWarnings.map((w, i) => (
              <div key={i} className="library-models-warning">
                {w}
              </div>
            ))}
            <div className="modal-buttons">
              <button type="button" onClick={props.onClose}>
                Cancel
              </button>
              <button
                type="button"
                className="primary"
                onClick={doParse}
                disabled={!text.trim()}
              >
                Preview →
              </button>
            </div>
          </>
        )}

        {step === "preview" && (
          <>
            <p className="library-modal-field-hint">
              {importable} of {rows.length} will be imported. Edit a name to
              avoid a collision; blocked rows are skipped.
            </p>
            {parseWarnings.map((w, i) => (
              <div key={i} className="library-models-warning">
                {w}
              </div>
            ))}
            <div className="import-preview-list">
              {rows.map((row, i) => (
                <div
                  key={i}
                  className={
                    row.action === "blocked"
                      ? "import-row import-row-blocked"
                      : "import-row"
                  }
                >
                  <div className="import-row-head">
                    <span className={`import-action-badge import-action-${row.action}`}>
                      {row.action}
                    </span>
                    <span className="def-pill mono">{row.candidate.kind}</span>
                    {row.candidate.transport && (
                      <span className="def-pill mono">{row.candidate.transport}</span>
                    )}
                    <input
                      type="text"
                      className="mono import-name-input"
                      value={row.candidate.name}
                      onChange={(e) => renameCandidate(i, e.target.value)}
                      disabled={committing}
                    />
                  </div>
                  {row.note && <div className="import-row-note">{row.note}</div>}
                  {row.warnings.map((w, j) => (
                    <div key={j} className="import-row-warning">
                      {w}
                    </div>
                  ))}
                </div>
              ))}
            </div>
            <div className="modal-buttons">
              <button type="button" onClick={() => setStep("source")} disabled={committing}>
                ← Back
              </button>
              <button
                type="button"
                className="primary"
                onClick={commit}
                disabled={committing || importable === 0}
              >
                Import {importable} →
              </button>
            </div>
          </>
        )}

        {step === "results" && (
          <>
            <div className="import-preview-list">
              {rows.map((row, i) => {
                const r = results[i];
                return (
                  <div key={i} className="import-row">
                    <div className="import-row-head">
                      <span className={`import-result-${r?.status ?? "pending"}`}>
                        {r?.status === "ok"
                          ? "✓"
                          : r?.status === "error"
                            ? "✗"
                            : r?.status === "skipped"
                              ? "—"
                              : "…"}
                      </span>
                      <span className="def-pill mono">{row.candidate.name}</span>
                    </div>
                    {r?.message && <div className="import-row-warning">{r.message}</div>}
                  </div>
                );
              })}
            </div>

            {!committing && (importedSkills.length > 0 || importedMcp.length > 0) && (
              <div className="library-models-warning">
                {importedMcp.length > 0 ? (
                  <>
                    Local models run skills fine, but small local models may{" "}
                    <strong>silently ignore MCP tool calls</strong> — use a
                    tool-capable model (<code>local-medium</code> /{" "}
                    <code>local-coder</code>) or a cloud model.
                  </>
                ) : (
                  <>Skills run on any local model.</>
                )}
              </div>
            )}

            <div className="modal-buttons">
              {!committing &&
                props.onWireLocalAgent &&
                (importedSkills.length > 0 || importedMcp.length > 0) && (
                  <button
                    type="button"
                    onClick={() =>
                      props.onWireLocalAgent!({
                        skills: importedSkills,
                        mcpServers: importedMcp,
                      })
                    }
                  >
                    Use with a local LLM →
                  </button>
                )}
              <button
                type="button"
                className="primary"
                onClick={props.onClose}
                disabled={committing}
              >
                Done
              </button>
            </div>
          </>
        )}
      </div>
    </div>
  );
}
