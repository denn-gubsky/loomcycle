import { useEffect, useImperativeHandle, useMemo, useRef, useState, type ReactNode, type Ref } from "react";
import { CodeEditor, type CodeEditorHandle, type EditorDiagnostic } from "./CodeEditor";
import { locate, type Format, type JsonPath, type ParseError } from "./structured";
import { applyEdit, applyExternal, initState, switchFormat, type StructuredState } from "./structuredModel";
import { readPref, writePref } from "./storage";

// A JSON value edited as JSON or YAML (RFC FW). The host holds JSON text — the
// stored form — and gets JSON text back; YAML is a view of it. The rules live
// in structuredModel.ts.

export interface PathDiagnostic {
  /** Where the finding is: object keys and array indexes, outermost first. */
  path: JsonPath;
  severity: "error" | "warning" | "info";
  message: string;
}

export interface StructuredEditorHandle extends CodeEditorHandle {
  /** Put the cursor on the value at `path` (or its nearest enclosing place) and focus. */
  gotoPath(path: JsonPath): void;
}

export interface StructuredEditorProps {
  /** JSON text. */
  value: string;
  /** Called with JSON text. In YAML mode, only for edits that parse. */
  onChange: (json: string) => void;
  /** Controlled format. */
  format?: Format;
  /** Uncontrolled initial format (default "json"); a remembered choice wins. */
  defaultFormat?: Format;
  onFormatChange?: (format: Format) => void;
  /** Remembers format and wrap per browser under `<storageKey>.format` / `.wrap`. */
  storageKey?: string;
  readOnly?: boolean;
  diagnostics?: readonly PathDiagnostic[];
  /** The text stopped parsing (an error) or started again (null). */
  onParseError?: (error: ParseError | null) => void;
  /** Fold the value at this path when the editor opens (e.g. ["layout"]). */
  foldPath?: JsonPath;
  /** Host controls shown at the end of the toolbar. */
  toolbar?: ReactNode;
  handle?: Ref<StructuredEditorHandle>;
  ariaLabel?: string;
  placeholder?: string;
  className?: string;
}

export function StructuredEditor({
  value,
  onChange,
  format,
  defaultFormat = "json",
  onFormatChange,
  storageKey,
  readOnly,
  diagnostics,
  onParseError,
  foldPath,
  toolbar,
  handle,
  ariaLabel,
  placeholder,
  className,
}: StructuredEditorProps) {
  const [state, setState] = useState<StructuredState>(() => {
    const remembered = readPref(storageKey && `${storageKey}.format`);
    const want = format ?? (remembered === "json" || remembered === "yaml" ? remembered : defaultFormat);
    return initState(value, want);
  });
  const [wrap, setWrap] = useState(() => readPref(storageKey && `${storageKey}.wrap`) !== "false");
  const [refused, setRefused] = useState<ParseError | null>(null);
  const editor = useRef<CodeEditorHandle>(null);
  const stateRef = useRef(state);
  stateRef.current = state;

  // The host's value changed: our own echo keeps the text as typed.
  useEffect(() => {
    setState((s) => applyExternal(s, value));
  }, [value]);

  const onParseErrorRef = useRef(onParseError);
  onParseErrorRef.current = onParseError;
  useEffect(() => {
    onParseErrorRef.current?.(state.error);
  }, [state.error]);

  function show(to: Format) {
    const r = switchFormat(stateRef.current, to);
    if (r.refused) {
      setRefused(r.refused);
      onFormatChange?.(stateRef.current.format);
      return;
    }
    setRefused(null);
    setState(r.state);
    writePref(storageKey && `${storageKey}.format`, to);
    onFormatChange?.(to);
  }

  // A controlled format follows its prop.
  useEffect(() => {
    if (format && format !== stateRef.current.format) show(format);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [format]);

  function edit(text: string) {
    const r = applyEdit(stateRef.current, text);
    stateRef.current = r.state;
    setState(r.state);
    setRefused(null);
    if (r.emit !== undefined) onChange(r.emit);
  }

  function toggleWrap() {
    setWrap((w) => {
      writePref(storageKey && `${storageKey}.wrap`, String(!w));
      return !w;
    });
  }

  const marks = useMemo<EditorDiagnostic[]>(() => {
    if (state.error) return [{ from: state.error.offset, to: Math.max(state.error.to, state.error.offset + 1), severity: "error", message: state.error.message }];
    const out: EditorDiagnostic[] = [];
    for (const d of diagnostics ?? []) {
      const at = locate(state.text, state.format, d.path);
      if (at) out.push({ from: at.offset, to: Math.max(at.to, at.offset + 1), severity: d.severity, message: d.message });
    }
    return out;
  }, [state.text, state.format, state.error, diagnostics]);

  const foldAt = useMemo(() => {
    if (!foldPath) return undefined;
    return locate(state.text, state.format, foldPath)?.offset;
    // Only the opening text and format decide the fold; CodeEditor folds once.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [state.format]);

  useImperativeHandle(
    handle,
    () => ({
      goto: (o) => editor.current?.goto(o),
      reveal: (o) => editor.current?.reveal(o),
      cursor: () => editor.current?.cursor(),
      gotoPath(path) {
        const s = stateRef.current;
        const at = locate(s.text, s.format, path);
        if (at) editor.current?.goto(at.offset);
      },
    }),
    [],
  );

  const problem = refused ?? state.error;
  return (
    <div className={`loomcycle-structured-editor${className ? ` ${className}` : ""}`}>
      <div className="lc-ce-toolbar">
        <div className="lc-ce-formats" role="group" aria-label="Format">
          {(["yaml", "json"] as const).map((f) => (
            <button
              key={f}
              type="button"
              className="lc-ce-format"
              aria-pressed={state.format === f}
              onClick={() => show(f)}
            >
              {f.toUpperCase()}
            </button>
          ))}
        </div>
        <label className="lc-ce-wrap">
          <input type="checkbox" checked={wrap} onChange={toggleWrap} /> wrap
        </label>
        {state.format === "yaml" && <span className="lc-ce-note">Saved as JSON: YAML comments are not kept.</span>}
        {toolbar && <div className="lc-ce-toolbar-extra">{toolbar}</div>}
      </div>
      <CodeEditor
        value={state.text}
        onChange={edit}
        language={state.format}
        wrap={wrap}
        readOnly={readOnly}
        diagnostics={marks}
        foldAt={foldAt}
        handle={editor}
        ariaLabel={ariaLabel}
        placeholder={placeholder}
      />
      {problem && (
        <div className="lc-ce-error" role="status">
          {refused ? "Fix this before switching format — " : ""}
          line {problem.line}, column {problem.column}: {problem.message}
        </div>
      )}
    </div>
  );
}
