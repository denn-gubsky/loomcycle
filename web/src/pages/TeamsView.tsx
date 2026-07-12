import { useCallback, useEffect, useRef, useState } from "react";
import {
  TeamDiagram,
  TeamNameSummary,
  createTeam,
  forkTeam,
  getTeamDef,
  listTeams,
  previewTeamDiagram,
  renderTeamDiagram,
} from "../api";
import { useTheme } from "../hooks/useTheme";
import Splitter from "../components/Splitter";

// TeamsView — the agent-team board.
//
// Layout: a fixed team list, then a draggable Splitter dividing an EDITOR pane
// (left) from a DIAGRAM pane (right). Selecting a team loads its editable
// definition into the editor and renders its stored diagram; the operator edits
// the graph JSON and clicks "Refresh diagram" to preview the unsaved edit
// (server syntax-checks + renders via render_diagram's dry-run overlay, no
// persist), then "Save new version" to fork+promote it. Create shows the editor
// with an EMPTY diagram (no stale one) until the first refresh.

// Starter templates — mirror the `team/examples` skill in the team-examples
// bundle. Their handler agents (sdlc/*, marketing/*) ship in that bundle, so a
// created team only RUNS if team-examples is selected; create validates the
// graph shape only, not agent existence.
const TEAM_TEMPLATES: { key: string; label: string; name: string; overlay: unknown }[] = [
  {
    key: "sdlc",
    label: "SDLC (software: architect → code → review → PR)",
    name: "sdlc",
    overlay: {
      entry: "architecture",
      max_iterations: 5,
      states: [
        { state: "architecture", handler: { kind: "agent", agent: "sdlc/architect" } },
        { state: "implementation", handler: { kind: "agent", agent: "sdlc/coder" } },
        { state: "review", handler: { kind: "agent", agent: "sdlc/reviewer" } },
        { state: "pr", handler: { kind: "terminal" } },
      ],
      transitions: [
        { from: "architecture", to: "implementation", on: "success" },
        { from: "implementation", to: "review", on: "success" },
        { from: "review", to: "pr", on: "success" },
        { from: "review", to: "implementation", on: "pushback:code-fix" },
      ],
    },
  },
  {
    key: "marketing",
    label: "Marketing (docs: draft → edit → publish)",
    name: "marketing",
    overlay: {
      entry: "draft",
      max_iterations: 4,
      states: [
        { state: "draft", handler: { kind: "agent", agent: "marketing/writer" } },
        { state: "edit", handler: { kind: "agent", agent: "marketing/editor" } },
        { state: "published", handler: { kind: "terminal" } },
      ],
      transitions: [
        { from: "draft", to: "edit", on: "success" },
        { from: "edit", to: "published", on: "success" },
        { from: "edit", to: "draft", on: "pushback:revise" },
      ],
    },
  },
];

const BLANK_OVERLAY = {
  entry: "start",
  states: [
    { state: "start", handler: { kind: "agent", agent: "your-agent" } },
    { state: "done", handler: { kind: "terminal" } },
  ],
  transitions: [{ from: "start", to: "done", on: "success" }],
};

const msg = (e: unknown) => (e instanceof Error ? e.message : String(e));
const pretty = (v: unknown) => JSON.stringify(v, null, 2);

// The diagram is rendered from one of two sources: the team's stored active
// version, or an unsaved preview overlay. Highlight re-renders whichever.
type DiagramSource =
  | { kind: "stored"; name: string }
  | { kind: "preview"; name: string; overlay: unknown }
  | null;

export default function TeamsView() {
  const [teams, setTeams] = useState<TeamNameSummary[]>([]);
  const [selected, setSelected] = useState<string>("");
  const [creating, setCreating] = useState(false);
  const [err, setErr] = useState<string>("");
  const [loading, setLoading] = useState(false);

  // Editor.
  const [editorText, setEditorText] = useState<string>("");
  const [createName, setCreateName] = useState<string>("");
  const [editorErr, setEditorErr] = useState<string>("");
  const [loadingDef, setLoadingDef] = useState(false);
  const [saving, setSaving] = useState(false);

  // Diagram.
  const [highlight, setHighlight] = useState<string>("");
  const [diagramSource, setDiagramSource] = useState<DiagramSource>(null);
  const [diagram, setDiagram] = useState<TeamDiagram | null>(null);
  const [diagErr, setDiagErr] = useState<string>("");

  // Rendered diagram (mermaid → SVG), theme-aware, with a source toggle.
  const { theme } = useTheme();
  const [svg, setSvg] = useState<string>("");
  const [renderErr, setRenderErr] = useState<string>("");
  const [showSource, setShowSource] = useState(false);
  const mmidRef = useRef(0);

  const fetchTeams = useCallback(async () => {
    setLoading(true);
    setErr("");
    try {
      const resp = await listTeams();
      setTeams(resp.names ?? []);
    } catch (e) {
      setErr(msg(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void fetchTeams();
  }, [fetchTeams]);

  // Load a def's editable JSON into the editor (canonical, pretty-printed).
  const loadDefIntoEditor = useCallback(async (defId: string) => {
    setLoadingDef(true);
    setEditorErr("");
    try {
      const detail = await getTeamDef(defId);
      const parsed = JSON.parse(detail.definition);
      setEditorText(pretty(parsed));
    } catch (e) {
      setEditorText("");
      setEditorErr("Failed to load definition: " + msg(e));
    } finally {
      setLoadingDef(false);
    }
  }, []);

  // Select a stored team → load its definition + render its stored diagram.
  const selectTeam = useCallback(
    (t: TeamNameSummary) => {
      setCreating(false);
      setSelected(t.name);
      setEditorErr("");
      setDiagErr("");
      setHighlight("");
      setDiagramSource({ kind: "stored", name: t.name });
      if (t.active_def_id) {
        void loadDefIntoEditor(t.active_def_id);
      } else {
        setEditorText("");
        setEditorErr("This team has no active version to edit.");
      }
    },
    [loadDefIntoEditor],
  );

  // Render whichever source is current (stored or preview), applying highlight.
  // A stored miss / an invalid preview both surface in the diagram pane; a
  // JSON-syntax error is caught earlier (editor pane) before we get here.
  useEffect(() => {
    if (!diagramSource) {
      setDiagram(null);
      return;
    }
    let cancelled = false;
    setDiagErr("");
    void (async () => {
      try {
        const hl = highlight || undefined;
        const d =
          diagramSource.kind === "stored"
            ? await renderTeamDiagram(diagramSource.name, hl)
            : await previewTeamDiagram(diagramSource.name, diagramSource.overlay, hl);
        if (!cancelled) setDiagram(d);
      } catch (e) {
        if (!cancelled) {
          setDiagram(null);
          setDiagErr(msg(e));
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [diagramSource, highlight]);

  // Render the Mermaid source to an SVG in-page. mermaid is lazy-imported (a
  // big dep — kept out of the main bundle), initialized per the app theme so the
  // edges/labels are legible on light or dark. On failure we fall back to the
  // source view. Guard against a stale render winning a race.
  useEffect(() => {
    if (!diagram) {
      setSvg("");
      return;
    }
    let cancelled = false;
    setRenderErr("");
    void (async () => {
      try {
        const mermaid = (await import("mermaid")).default;
        mermaid.initialize({
          startOnLoad: false,
          theme: theme === "dark" ? "dark" : "default",
          securityLevel: "strict",
        });
        const id = `team-mmd-${++mmidRef.current}`;
        const out = await mermaid.render(id, diagram.diagram);
        if (!cancelled) setSvg(out.svg);
      } catch (e) {
        if (!cancelled) {
          setSvg("");
          setRenderErr(msg(e));
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [diagram, theme]);

  function startCreate() {
    setCreating(true);
    setSelected("");
    setCreateName("");
    setEditorText(pretty(BLANK_OVERLAY));
    setEditorErr("");
    setDiagErr("");
    setHighlight("");
    setDiagramSource(null); // EMPTY diagram until the first refresh.
    setDiagram(null);
  }

  function cancelCreate() {
    setCreating(false);
    setSelected("");
    setEditorText("");
    setEditorErr("");
    setDiagramSource(null);
    setDiagram(null);
  }

  function applyTemplate(key: string) {
    const t = TEAM_TEMPLATES.find((x) => x.key === key);
    setEditorText(pretty(t ? t.overlay : BLANK_OVERLAY));
    if (t && !createName) setCreateName(t.name);
  }

  // Parse the editor text as JSON; on failure set the editor error and return
  // undefined. The server does the graph syntax check (dry-run / create / fork).
  function parseEditor(): unknown | undefined {
    try {
      return JSON.parse(editorText);
    } catch (e) {
      setEditorErr("Not valid JSON: " + msg(e));
      return undefined;
    }
  }

  function onRefresh() {
    setEditorErr("");
    const parsed = parseEditor();
    if (parsed === undefined) return;
    const name = creating ? createName.trim() || "team" : selected;
    setDiagramSource({ kind: "preview", name, overlay: parsed });
  }

  async function onSave() {
    setEditorErr("");
    const parsed = parseEditor();
    if (parsed === undefined) return;
    setSaving(true);
    try {
      const res = await forkTeam(selected, parsed);
      await fetchTeams();
      await loadDefIntoEditor(res.def_id);
      setDiagramSource({ kind: "stored", name: selected });
    } catch (e) {
      // The server 422s an invalid graph with the reason.
      setEditorErr(msg(e));
    } finally {
      setSaving(false);
    }
  }

  async function onCreate() {
    setEditorErr("");
    if (!createName.trim()) {
      setEditorErr("Name is required.");
      return;
    }
    const parsed = parseEditor();
    if (parsed === undefined) return;
    setSaving(true);
    try {
      const res = await createTeam(createName.trim(), parsed);
      await fetchTeams();
      setCreating(false);
      setSelected(res.name);
      await loadDefIntoEditor(res.def_id);
      setDiagramSource({ kind: "stored", name: res.name });
    } catch (e) {
      setEditorErr(msg(e));
    } finally {
      setSaving(false);
    }
  }

  const active = selected || creating;

  // ---- Editor pane (Splitter left) ----
  const editorPane = (
    <div
      style={{
        height: "100%",
        minHeight: 0,
        boxSizing: "border-box",
        display: "flex",
        flexDirection: "column",
        gap: "0.6rem",
        paddingRight: "0.75rem",
      }}
    >
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", flex: "0 0 auto", gap: "0.5rem" }}>
        <strong>{creating ? "Create team" : `Edit: ${selected}`}</strong>
        {creating && (
          <button onClick={cancelCreate} disabled={saving} style={{ fontSize: "0.8em", padding: "0.15rem 0.5rem" }}>
            Cancel
          </button>
        )}
      </div>

      {creating && (
        <div style={{ display: "flex", gap: "0.5rem", flex: "0 0 auto", flexWrap: "wrap" }}>
          <input
            value={createName}
            onChange={(e) => setCreateName(e.target.value)}
            placeholder="team name (e.g. sdlc)"
            style={{ padding: "0.35rem", flex: "1 1 160px", minWidth: 0 }}
          />
          <select defaultValue="" onChange={(e) => applyTemplate(e.target.value)} style={{ padding: "0.35rem" }}>
            <option value="">Template…</option>
            {TEAM_TEMPLATES.map((t) => (
              <option key={t.key} value={t.key}>
                {t.label}
              </option>
            ))}
          </select>
        </div>
      )}

      <textarea
        value={editorText}
        onChange={(e) => setEditorText(e.target.value)}
        spellCheck={false}
        placeholder={loadingDef ? "Loading…" : "Team graph JSON — entry / states / transitions"}
        style={{
          flex: 1,
          minHeight: 0,
          resize: "none",
          padding: "0.6rem",
          fontFamily: "var(--mono, monospace)",
          fontSize: "0.82em",
          whiteSpace: "pre",
          overflow: "auto",
          border: "1px solid var(--lc-rule, #ccc)",
          borderRadius: 6,
          background: "var(--lc-input-bg, transparent)",
          color: "var(--lc-text, inherit)",
        }}
      />

      {editorErr && (
        <div style={{ color: "var(--error, #e03131)", whiteSpace: "pre-wrap", flex: "0 0 auto", fontSize: "0.85em" }}>
          {editorErr}
        </div>
      )}

      <div style={{ display: "flex", gap: "0.5rem", flex: "0 0 auto", flexWrap: "wrap" }}>
        <button onClick={onRefresh} disabled={saving || loadingDef} style={{ fontWeight: 600 }}>
          Refresh diagram
        </button>
        {creating ? (
          <button onClick={() => void onCreate()} disabled={saving} style={{ fontWeight: 600 }}>
            {saving ? "Creating…" : "Create"}
          </button>
        ) : (
          <button onClick={() => void onSave()} disabled={saving || loadingDef} style={{ fontWeight: 600 }}>
            {saving ? "Saving…" : "Save new version"}
          </button>
        )}
      </div>
    </div>
  );

  // ---- Diagram pane (Splitter right) ----
  const diagramPane = (
    <div style={{ height: "100%", minHeight: 0, display: "flex", flexDirection: "column", paddingLeft: "0.75rem" }}>
      <div style={{ display: "flex", gap: "0.75rem", alignItems: "center", flexWrap: "wrap", flex: "0 0 auto" }}>
        <strong>{creating ? createName || "(new team)" : selected}</strong>
        <label style={{ fontSize: "0.85em", opacity: 0.8 }}>
          highlight state:{" "}
          <input
            value={highlight}
            onChange={(e) => setHighlight(e.target.value)}
            placeholder="(state id)"
            style={{ padding: "0.2rem 0.4rem" }}
          />
        </label>
      </div>
      {diagErr && (
        <div style={{ color: "var(--error, #e03131)", marginTop: "0.5rem", flex: "0 0 auto", whiteSpace: "pre-wrap" }}>
          {diagErr}
        </div>
      )}
      {renderErr && (
        <div style={{ color: "var(--error, #e03131)", marginTop: "0.5rem", flex: "0 0 auto" }}>
          Diagram render failed: {renderErr}
        </div>
      )}
      <div style={{ flex: 1, minHeight: 0, overflow: "auto", marginTop: "0.5rem" }}>
        {!diagram && !diagErr && (
          <p style={{ opacity: 0.6 }}>
            {creating
              ? "Edit the graph on the left, then Refresh diagram to preview."
              : "Refresh diagram to preview your edits."}
          </p>
        )}
        {diagram && svg && (
          <div
            // The rendered SVG. dangerouslySetInnerHTML is safe here
            // (double-sanitized): the source is server-generated + mmSanitize'd
            // (render.go strips newlines/-->/%% from ids/labels) and we render
            // with securityLevel:"strict" (mermaid's bundled DOMPurify).
            style={{
              padding: "0.75rem",
              border: "1px solid var(--lc-border, rgba(127,127,127,0.3))",
              borderRadius: 8,
              background: "var(--lc-surface, transparent)",
            }}
            dangerouslySetInnerHTML={{ __html: svg }}
          />
        )}
        {diagram && (
          <div style={{ marginTop: "0.5rem" }}>
            <button onClick={() => setShowSource((s) => !s)} style={{ fontSize: "0.8em", padding: "0.2rem 0.5rem" }}>
              {showSource ? "hide source" : "view source"}
            </button>
            {(showSource || (!svg && !renderErr)) && (
              <pre
                style={{
                  marginTop: "0.5rem",
                  background: "rgba(127,127,127,0.12)",
                  color: "inherit",
                  border: "1px solid var(--lc-border, rgba(127,127,127,0.3))",
                  borderRadius: 6,
                  padding: "0.75rem",
                  overflowX: "auto",
                  fontFamily: "var(--mono, monospace)",
                  fontSize: "0.85em",
                  whiteSpace: "pre",
                }}
              >
                {diagram.diagram}
              </pre>
            )}
          </div>
        )}
      </div>
    </div>
  );

  return (
    <div
      style={{
        height: "100%",
        minHeight: 0,
        boxSizing: "border-box",
        padding: "1rem",
        display: "flex",
        flexDirection: "column",
        gap: "1rem",
      }}
    >
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "flex-start", gap: "1rem", flexWrap: "wrap", flex: "0 0 auto" }}>
        <div>
          <h1 style={{ margin: 0 }}>teams</h1>
          <p style={{ opacity: 0.7, margin: "0.25rem 0" }}>
            Agent-team workflows. Select a team to edit its graph and preview the
            state-machine diagram; Refresh renders your unsaved edits, Save forks a
            new version.
          </p>
        </div>
        <button onClick={startCreate} disabled={creating} style={{ padding: "0.4rem 0.8rem", fontWeight: 600 }}>
          + create team
        </button>
      </div>

      {err && <div style={{ color: "var(--error, #e03131)", flex: "0 0 auto" }}>Failed to load teams: {err}</div>}

      <div style={{ flex: 1, minHeight: 0, display: "flex", gap: "1.5rem", alignItems: "stretch" }}>
        {/* Team list */}
        <div style={{ width: 240, flex: "0 0 auto", minHeight: 0, display: "flex", flexDirection: "column" }}>
          <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", flex: "0 0 auto" }}>
            <strong>teams ({teams.length})</strong>
            <button onClick={() => void fetchTeams()} disabled={loading}>
              {loading ? "…" : "refresh"}
            </button>
          </div>
          {teams.length === 0 && !loading && (
            <p style={{ opacity: 0.6 }}>
              No teams yet. Click <strong>+ create team</strong> (try a starter
              template), or ask the <code>team/assistant</code> agent.
            </p>
          )}
          <ul style={{ listStyle: "none", padding: 0, margin: "0.5rem 0", flex: 1, minHeight: 0, overflowY: "auto" }}>
            {teams.map((t) => {
              const key = t.tenant_id ? `${t.tenant_id}/${t.name}` : t.name;
              const isSel = !creating && selected === t.name;
              return (
                <li key={key}>
                  <button
                    onClick={() => selectTeam(t)}
                    style={{
                      display: "block",
                      width: "100%",
                      textAlign: "left",
                      padding: "0.4rem 0.6rem",
                      margin: "0.15rem 0",
                      // Selected uses the app's active-row pattern
                      // (.presets-item.active): accent border + translucent
                      // accent-soft fill so the theme foreground stays legible.
                      border: `1px solid ${isSel ? "var(--lc-accent)" : "var(--lc-rule, #ccc)"}`,
                      borderRadius: 6,
                      background: isSel ? "var(--lc-accent-soft)" : "transparent",
                      color: "var(--lc-text, inherit)",
                      cursor: "pointer",
                    }}
                  >
                    <div style={{ fontWeight: 600 }}>{t.name}</div>
                    <div style={{ fontSize: "0.8em", opacity: 0.65 }}>
                      v{t.latest_version} · {t.version_count} version
                      {t.version_count === 1 ? "" : "s"}
                      {t.active_retired ? " · retired" : ""}
                      {t.tenant_id ? ` · ${t.tenant_id}` : ""}
                    </div>
                  </button>
                </li>
              );
            })}
          </ul>
        </div>

        {/* Editor | diagram, divided by a draggable handle. */}
        <div style={{ flex: 1, minWidth: 0, minHeight: 0 }}>
          {active ? (
            <Splitter
              defaultLeftWidth={460}
              minLeftWidth={300}
              minRightWidth={280}
              storageKey="loomcycle.split.teams.editor"
            >
              {editorPane}
              {diagramPane}
            </Splitter>
          ) : (
            <p style={{ opacity: 0.6 }}>Select a team to edit, or click + create team.</p>
          )}
        </div>
      </div>
    </div>
  );
}
