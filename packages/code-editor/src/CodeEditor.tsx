import { useEffect, useImperativeHandle, useRef, useState, type Ref } from "react";
import type { EditorView } from "@codemirror/view";
import type { Compartment, Extension } from "@codemirror/state";

// A controlled CodeMirror 6 view over one text (RFC FW). It generalises
// loomboard's canvas JSON view (RFC DX): same props, same meanings, plus a
// language and soft wrap.
//
// CodeMirror is imported only when the editor first mounts, so a host's
// bundler gives it a chunk of its own and a page that never shows an editor
// never fetches it. Until it has loaded — or if it cannot load — a plain
// textarea edits the same text, so the view is never blank and never
// read-only for want of the editor.
//
// An edit calls `onChange`; a new `value` from outside replaces the content
// without echoing back as an edit.

export type EditorLanguage = "json" | "yaml" | "text";

export interface EditorDiagnostic {
  /** Offsets into `value`. */
  from: number;
  to: number;
  severity: "error" | "warning" | "info";
  message: string;
}

export interface CodeEditorHandle {
  /** Put the cursor at `offset`, scroll it into view and focus the editor. */
  goto(offset: number): void;
  /** Put the cursor at `offset` and scroll it into view WITHOUT taking the
   *  focus: something else selected it, and its keys must stay its own. */
  reveal(offset: number): void;
  /** Where the cursor is, once the editor has loaded. */
  cursor(): number | undefined;
}

export interface CodeEditorProps {
  value: string;
  onChange: (text: string) => void;
  language?: EditorLanguage;
  /** Soft-wrap long lines (default true). */
  wrap?: boolean;
  readOnly?: boolean;
  diagnostics?: readonly EditorDiagnostic[];
  /** Fold the value on the line holding this offset, once, when the editor
   *  first has the text to fold. */
  foldAt?: number;
  /** The operator moved the cursor (a click, an arrow key) — not a cursor
   *  `goto` / `reveal` placed, and not typing. */
  onCursor?: (offset: number) => void;
  handle?: Ref<CodeEditorHandle>;
  ariaLabel?: string;
  placeholder?: string;
  className?: string;
}

type CM = {
  view: typeof import("@codemirror/view");
  state: typeof import("@codemirror/state");
  lint: typeof import("@codemirror/lint");
  language: typeof import("@codemirror/language");
  json: typeof import("@codemirror/lang-json");
  yaml: typeof import("@codemirror/lang-yaml");
  highlight: typeof import("@lezer/highlight");
};

/** Loads CodeMirror once per page; every editor shares the promise. */
let loading: Promise<{ basicSetup: Extension; cm: CM }> | null = null;
function loadCodeMirror() {
  loading ??= Promise.all([
    import("codemirror"),
    import("@codemirror/view"),
    import("@codemirror/state"),
    import("@codemirror/lint"),
    import("@codemirror/language"),
    import("@codemirror/lang-json"),
    import("@codemirror/lang-yaml"),
    import("@lezer/highlight"),
  ]).then(([{ basicSetup }, view, state, lint, language, json, yaml, highlight]) => ({
    basicSetup,
    cm: { view, state, lint, language, json, yaml, highlight },
  }));
  // A failed load is retried by the next editor that mounts.
  loading.catch(() => {
    loading = null;
  });
  return loading;
}

export function CodeEditor({
  value,
  onChange,
  language = "text",
  wrap = true,
  readOnly,
  diagnostics,
  foldAt,
  onCursor,
  handle,
  ariaLabel,
  placeholder,
  className,
}: CodeEditorProps) {
  const host = useRef<HTMLDivElement>(null);
  const view = useRef<EditorView | null>(null);
  const cm = useRef<CM | null>(null);
  const compartments = useRef<{ language: Compartment; wrap: Compartment; readOnly: Compartment } | null>(null);
  const external = useRef(false);
  const onChangeRef = useRef(onChange);
  onChangeRef.current = onChange;
  const onCursorRef = useRef(onCursor);
  onCursorRef.current = onCursor;
  const [loaded, setLoaded] = useState(false);
  const [failed, setFailed] = useState(false);
  // The props as of mount, for the editor's first state; later changes go
  // through the effects below.
  const initial = useRef({ value, language, wrap, readOnly, ariaLabel, placeholder });
  const folded = useRef(false);

  useEffect(() => {
    let cancelled = false;
    loadCodeMirror()
      .then(({ basicSetup, cm: mods }) => {
        if (cancelled || !host.current) return;
        cm.current = mods;
        const c = {
          language: new mods.state.Compartment(),
          wrap: new mods.state.Compartment(),
          readOnly: new mods.state.Compartment(),
        };
        compartments.current = c;
        const init = initial.current;
        const v = new mods.view.EditorView({
          parent: host.current,
          state: mods.state.EditorState.create({
            doc: init.value,
            extensions: [
              basicSetup,
              mods.language.syntaxHighlighting(mods.highlight.classHighlighter),
              mods.lint.lintGutter(),
              c.language.of(languageExtension(mods, init.language)),
              c.wrap.of(init.wrap ? mods.view.EditorView.lineWrapping : []),
              c.readOnly.of(readOnlyExtensions(mods, !!init.readOnly)),
              init.placeholder ? mods.view.placeholder(init.placeholder) : [],
              init.ariaLabel ? mods.view.EditorView.contentAttributes.of({ "aria-label": init.ariaLabel }) : [],
              mods.view.EditorView.updateListener.of((u) => {
                if (u.docChanged && !external.current) onChangeRef.current(u.state.doc.toString());
                // `external` also covers goto / reveal: only the operator's own
                // cursor moves are reported.
                else if (u.selectionSet && !u.docChanged && !external.current) onCursorRef.current?.(u.state.selection.main.head);
              }),
              theme(mods),
            ],
          }),
        });
        view.current = v;
        setLoaded(true);
      })
      .catch((e) => {
        console.warn("[code-editor] CodeMirror could not load; editing as plain text:", e);
        if (!cancelled) setFailed(true);
      });
    return () => {
      cancelled = true;
      view.current?.destroy();
      view.current = null;
    };
  }, []);

  // A new value from outside replaces the content, without echoing as an edit.
  useEffect(() => {
    const v = view.current;
    if (!v || v.state.doc.toString() === value) return;
    external.current = true;
    try {
      v.dispatch({ changes: { from: 0, to: v.state.doc.length, insert: value } });
    } finally {
      external.current = false;
    }
  }, [value, loaded]);

  useEffect(() => {
    const v = view.current;
    const c = compartments.current;
    if (!v || !cm.current || !c) return;
    v.dispatch({
      effects: [
        c.language.reconfigure(languageExtension(cm.current, language)),
        c.wrap.reconfigure(wrap ? cm.current.view.EditorView.lineWrapping : []),
        c.readOnly.reconfigure(readOnlyExtensions(cm.current, !!readOnly)),
      ],
    });
  }, [language, wrap, readOnly, loaded]);

  useEffect(() => {
    const v = view.current;
    if (!v || !cm.current || folded.current || foldAt === undefined || v.state.doc.toString() !== value) return;
    folded.current = true;
    foldLineAt(v, cm.current.language, foldAt);
  }, [foldAt, value, loaded]);

  useEffect(() => {
    const v = view.current;
    if (!v || !cm.current) return;
    const len = v.state.doc.length;
    const diags = (diagnostics ?? []).map((d) => ({
      from: Math.min(d.from, len),
      to: Math.min(Math.max(d.to, d.from), len),
      severity: d.severity,
      message: d.message,
    }));
    v.dispatch(cm.current.lint.setDiagnostics(v.state, diags));
  }, [diagnostics, loaded, value]);

  useImperativeHandle(
    handle,
    () => ({
      goto(offset: number) {
        const v = view.current;
        if (v) {
          place(v, offset);
          v.focus();
          return;
        }
        const ta = host.current?.querySelector("textarea");
        if (ta) {
          ta.focus();
          ta.setSelectionRange(offset, offset);
        }
      },
      reveal(offset: number) {
        if (view.current) place(view.current, offset);
      },
      cursor() {
        return view.current?.state.selection.main.head;
      },
    }),
    [],
  );

  // A cursor this component places, kept apart from one the operator moves.
  function place(v: EditorView, offset: number) {
    external.current = true;
    try {
      v.dispatch({ selection: { anchor: Math.min(offset, v.state.doc.length) }, scrollIntoView: true });
    } finally {
      external.current = false;
    }
  }

  return (
    <div className={`loomcycle-code-editor${className ? ` ${className}` : ""}`} data-loaded={loaded || undefined}>
      <div ref={host} className="lc-ce-host">
        {!loaded && (
          <textarea
            className="lc-ce-plain"
            data-wrap={wrap || undefined}
            aria-label={ariaLabel}
            placeholder={placeholder}
            spellCheck={false}
            value={value}
            readOnly={readOnly}
            onChange={(e) => onChange(e.target.value)}
          />
        )}
      </div>
      {failed && <div className="lc-ce-hint">The editor could not load; this is plain text editing.</div>}
    </div>
  );
}

// No linter of its own: a CodeMirror linter replaces every diagnostic on each
// run, which would wipe the host's `diagnostics`. Parse errors come from the
// host (StructuredEditor reports its own).
function languageExtension(cm: CM, language: EditorLanguage): Extension {
  if (language === "json") return cm.json.json();
  if (language === "yaml") return cm.yaml.yaml();
  return [];
}

function readOnlyExtensions(cm: CM, readOnly: boolean): Extension {
  return [cm.state.EditorState.readOnly.of(readOnly), cm.view.EditorView.editable.of(!readOnly)];
}

// Colours come from --lc-ce-* variables (styles.css), so a host themes the
// editor — dark mode included — with CSS alone.
function theme(cm: CM): Extension {
  return cm.view.EditorView.theme({
    "&": { height: "100%", backgroundColor: "var(--_ce-bg)", color: "var(--_ce-fg)", fontSize: "var(--_ce-font-size)" },
    "&.cm-focused": { outline: "none" },
    ".cm-scroller": { fontFamily: "var(--_ce-font)", lineHeight: "1.5" },
    ".cm-content": { caretColor: "var(--_ce-fg)" },
    ".cm-cursor, .cm-dropCursor": { borderLeftColor: "var(--_ce-fg)" },
    ".cm-gutters": { backgroundColor: "var(--_ce-gutter-bg)", color: "var(--_ce-gutter-fg)", borderRight: "1px solid var(--_ce-rule)" },
    ".cm-activeLine": { backgroundColor: "var(--_ce-active-line)" },
    ".cm-activeLineGutter": { backgroundColor: "var(--_ce-active-line)", color: "var(--_ce-fg)" },
    "&.cm-focused > .cm-scroller > .cm-selectionLayer .cm-selectionBackground, .cm-selectionBackground, .cm-content ::selection": {
      backgroundColor: "var(--_ce-selection)",
    },
    ".cm-selectionMatch": { backgroundColor: "var(--_ce-match)" },
    "&.cm-focused .cm-matchingBracket": { backgroundColor: "var(--_ce-match)", outline: "1px solid var(--_ce-rule)" },
    ".cm-foldPlaceholder": { backgroundColor: "var(--_ce-gutter-bg)", color: "var(--_ce-gutter-fg)", border: "1px solid var(--_ce-rule)" },
    ".cm-panels": { backgroundColor: "var(--_ce-gutter-bg)", color: "var(--_ce-fg)" },
    ".cm-panels.cm-panels-top": { borderBottom: "1px solid var(--_ce-rule)" },
    ".cm-panels.cm-panels-bottom": { borderTop: "1px solid var(--_ce-rule)" },
    ".cm-textfield": { backgroundColor: "var(--_ce-bg)", color: "var(--_ce-fg)", border: "1px solid var(--_ce-rule)" },
    ".cm-button": { backgroundImage: "none", backgroundColor: "var(--_ce-gutter-bg)", color: "var(--_ce-fg)", border: "1px solid var(--_ce-rule)" },
    ".cm-tooltip": { backgroundColor: "var(--_ce-gutter-bg)", color: "var(--_ce-fg)", border: "1px solid var(--_ce-rule)" },
    ".cm-searchMatch": { backgroundColor: "var(--_ce-match)", outline: "1px solid var(--_ce-rule)" },
    ".cm-placeholder": { color: "var(--_ce-gutter-fg)" },
  });
}

/** Fold the value on the line holding `offset`, if it can. */
function foldLineAt(v: EditorView, language: CM["language"], offset: number) {
  const line = v.state.doc.lineAt(Math.min(offset, v.state.doc.length));
  // The syntax tree is built incrementally; make sure it covers the line.
  language.ensureSyntaxTree(v.state, v.state.doc.length, 200);
  const range = language.foldable(v.state, line.from, line.to);
  if (range) v.dispatch({ effects: language.foldEffect.of(range) });
}
