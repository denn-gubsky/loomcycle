import { useCallback, useEffect, useRef, useState } from "react";
import {
  TeamDiagram,
  TeamNameSummary,
  createTeam,
  listTeams,
  renderTeamDiagram,
} from "../api";
import { useTheme } from "../hooks/useTheme";
import Splitter from "../components/Splitter";

// TeamsView — the agent-team board (RFC AP / BD).
//
// Lists TeamDefs (/v1/_teamdef/names), renders a selected team's Mermaid
// stateDiagram-v2 source (render_diagram), and creates a team (op=create) —
// either from scratch or pre-filled from a bundled starter template.
//
// The diagram is RENDERED in-page via mermaid (lazy-loaded so it stays out of
// the main bundle), theme-aware (follows the app's light/dark theme), with a
// "view source" toggle for the raw stateDiagram-v2. Binding the highlight to a
// running Document board's live state is a follow-up.

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

export default function TeamsView() {
  const [teams, setTeams] = useState<TeamNameSummary[]>([]);
  const [selected, setSelected] = useState<string>("");
  const [highlight, setHighlight] = useState<string>("");
  const [diagram, setDiagram] = useState<TeamDiagram | null>(null);
  const [err, setErr] = useState<string>("");
  const [diagErr, setDiagErr] = useState<string>("");
  const [loading, setLoading] = useState(false);

  // Rendered diagram (mermaid → SVG), theme-aware, with a source toggle.
  const { theme } = useTheme();
  const [svg, setSvg] = useState<string>("");
  const [renderErr, setRenderErr] = useState<string>("");
  const [showSource, setShowSource] = useState(false);
  const mmidRef = useRef(0);

  // Create-team dialog.
  const [showCreate, setShowCreate] = useState(false);
  const [createName, setCreateName] = useState("");
  const [createJSON, setCreateJSON] = useState(JSON.stringify(BLANK_OVERLAY, null, 2));
  const [createErr, setCreateErr] = useState("");
  const [creating, setCreating] = useState(false);

  const fetchTeams = useCallback(async () => {
    setLoading(true);
    setErr("");
    try {
      const resp = await listTeams();
      setTeams(resp.names ?? []);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void fetchTeams();
  }, [fetchTeams]);

  const fetchDiagram = useCallback(async (name: string, hl: string) => {
    if (!name) return;
    setDiagErr("");
    try {
      setDiagram(await renderTeamDiagram(name, hl || undefined));
    } catch (e) {
      setDiagram(null);
      setDiagErr(e instanceof Error ? e.message : String(e));
    }
  }, []);

  useEffect(() => {
    if (selected) void fetchDiagram(selected, highlight);
  }, [selected, highlight, fetchDiagram]);

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
          setRenderErr(e instanceof Error ? e.message : String(e));
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [diagram, theme]);

  function applyTemplate(key: string) {
    if (!key) {
      setCreateJSON(JSON.stringify(BLANK_OVERLAY, null, 2));
      return;
    }
    const t = TEAM_TEMPLATES.find((x) => x.key === key);
    if (t) {
      if (!createName) setCreateName(t.name);
      setCreateJSON(JSON.stringify(t.overlay, null, 2));
    }
  }

  async function handleCreate() {
    setCreateErr("");
    if (!createName.trim()) {
      setCreateErr("Name is required.");
      return;
    }
    let overlay: unknown;
    try {
      overlay = JSON.parse(createJSON);
    } catch (e) {
      setCreateErr("Overlay is not valid JSON: " + (e instanceof Error ? e.message : String(e)));
      return;
    }
    setCreating(true);
    try {
      const created = await createTeam(createName.trim(), overlay);
      setShowCreate(false);
      setCreateName("");
      setCreateJSON(JSON.stringify(BLANK_OVERLAY, null, 2));
      await fetchTeams();
      setSelected(created.name);
    } catch (e) {
      // The server 422s an invalid graph with the reason in the message.
      setCreateErr(e instanceof Error ? e.message : String(e));
    } finally {
      setCreating(false);
    }
  }

  // The selected team's diagram — a self-contained full-height pane: a fixed
  // header (name + highlight input) over a scrollable diagram region. Used both
  // standalone and as the left pane of the Splitter, so it owns `height:100%`
  // + an inner scroll rather than relying on the parent's flex.
  const diagramPane = (
    <div style={{ height: "100%", minHeight: 0, display: "flex", flexDirection: "column" }}>
      {!selected && <p style={{ opacity: 0.6 }}>Select a team to view its diagram.</p>}
      {selected && (
        <>
          <div style={{ display: "flex", gap: "0.75rem", alignItems: "center", flexWrap: "wrap", flex: "0 0 auto" }}>
            <strong>{selected}</strong>
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
            <div style={{ color: "var(--error, #e03131)", marginTop: "0.5rem", flex: "0 0 auto" }}>
              {diagErr}
            </div>
          )}
          {renderErr && (
            <div style={{ color: "var(--error, #e03131)", marginTop: "0.5rem", flex: "0 0 auto" }}>
              Diagram render failed: {renderErr}
            </div>
          )}
          {/* Scroll region — a tall/wide diagram scrolls here so the header +
              highlight input stay pinned. */}
          <div style={{ flex: 1, minHeight: 0, overflow: "auto", marginTop: "0.5rem" }}>
            {diagram && svg && (
              <div
                // The rendered SVG. mermaid's theme (dark/default) matches the
                // app, so edges + labels are legible; node fills come from the
                // def's colour scheme.
                //
                // dangerouslySetInnerHTML is safe here (double-sanitized): the
                // input `diagram.diagram` is server-generated + mmSanitize'd
                // (render.go strips newlines/-->/%% from ids/labels), and we
                // render with securityLevel:"strict", which runs the SVG output
                // through mermaid's bundled DOMPurify. No untrusted raw HTML.
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
                <button
                  onClick={() => setShowSource((s) => !s)}
                  style={{ fontSize: "0.8em", padding: "0.2rem 0.5rem" }}
                >
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
        </>
      )}
    </div>
  );

  // The create-team editor — the Splitter's right pane. A card (surface + border)
  // with a fixed header + footer and a scrollable form body, so a long overlay
  // JSON never pushes the Create/Cancel buttons off-screen.
  const editorPane = (
    <div
      style={{
        height: "100%",
        minHeight: 0,
        boxSizing: "border-box",
        background: "var(--lc-surface, transparent)",
        color: "var(--lc-text, inherit)",
        border: "1px solid var(--lc-rule, #ccc)",
        borderRadius: "var(--lc-radius, 8px)",
        padding: "1rem",
        display: "flex",
        flexDirection: "column",
        gap: "0.75rem",
      }}
    >
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", flex: "0 0 auto" }}>
        <h2 style={{ margin: 0, fontSize: "1.1rem" }}>Create team</h2>
        <button
          onClick={() => !creating && setShowCreate(false)}
          disabled={creating}
          title="close"
          style={{ padding: "0.15rem 0.5rem", lineHeight: 1 }}
        >
          ✕
        </button>
      </div>

      <div style={{ flex: 1, minHeight: 0, overflow: "auto", display: "flex", flexDirection: "column", gap: "0.75rem" }}>
        <p style={{ opacity: 0.7, margin: 0, fontSize: "0.85em" }}>
          Author a TeamDef — a workflow state-machine graph. The graph is validated
          on create; an invalid graph is refused with the reason.
        </p>

        <label style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
          <span>Start from a template</span>
          <select
            defaultValue=""
            onChange={(e) => applyTemplate(e.target.value)}
            style={{ padding: "0.35rem" }}
          >
            <option value="">Blank</option>
            {TEAM_TEMPLATES.map((t) => (
              <option key={t.key} value={t.key}>
                {t.label}
              </option>
            ))}
          </select>
          <span style={{ fontSize: "0.78em", opacity: 0.6 }}>
            Starter templates reference the <code>team-examples</code> bundle's handler
            agents — a team only runs if that bundle is loaded.
          </span>
        </label>

        <label style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
          <span>Name</span>
          <input
            value={createName}
            onChange={(e) => setCreateName(e.target.value)}
            placeholder="e.g. sdlc"
            style={{ padding: "0.4rem" }}
          />
        </label>

        <label style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
          <span>Graph (overlay JSON — entry / states / transitions)</span>
          <textarea
            value={createJSON}
            onChange={(e) => setCreateJSON(e.target.value)}
            spellCheck={false}
            rows={16}
            style={{
              padding: "0.5rem",
              fontFamily: "var(--mono, monospace)",
              fontSize: "0.82em",
              whiteSpace: "pre",
              overflowWrap: "normal",
            }}
          />
        </label>

        {createErr && (
          <div style={{ color: "var(--error, #e03131)", whiteSpace: "pre-wrap" }}>{createErr}</div>
        )}
      </div>

      <div style={{ display: "flex", gap: "0.5rem", justifyContent: "flex-end", flex: "0 0 auto" }}>
        <button onClick={() => setShowCreate(false)} disabled={creating}>
          Cancel
        </button>
        <button onClick={() => void handleCreate()} disabled={creating} style={{ fontWeight: 600 }}>
          {creating ? "Creating…" : "Create"}
        </button>
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
            Agent-team workflows. Select a team to see its state-machine diagram —
            states, transitions, and the colour scheme.
          </p>
        </div>
        <button onClick={() => setShowCreate((s) => !s)} style={{ padding: "0.4rem 0.8rem", fontWeight: 600 }}>
          {showCreate ? "close editor" : "+ create team"}
        </button>
      </div>

      {err && <div style={{ color: "var(--error, #e03131)", flex: "0 0 auto" }}>Failed to load teams: {err}</div>}

      {/* Full-height body: fixed team list + a diagram/editor region divided by a
          draggable Splitter. The row is bounded (flex:1 + minHeight:0 under a
          height:100% root) so the Splitter's height:100% resolves and each pane
          scrolls internally instead of overlapping. */}
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
              const isSel = selected === t.name;
              return (
                <li key={key}>
                  <button
                    onClick={() => setSelected(t.name)}
                    style={{
                      display: "block",
                      width: "100%",
                      textAlign: "left",
                      padding: "0.4rem 0.6rem",
                      margin: "0.15rem 0",
                      // Selected uses the app's active-row pattern
                      // (.presets-item.active): accent border + translucent
                      // accent-soft fill so the theme foreground stays legible.
                      // The old `--accent-bg`/`#eef` alias doesn't exist → it fell
                      // back to a solid light fill with near-white text = invisible.
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

        {/* Diagram, optionally split with the editor by a draggable handle. */}
        <div style={{ flex: 1, minWidth: 0, minHeight: 0 }}>
          {showCreate ? (
            <Splitter
              defaultLeftWidth={420}
              minLeftWidth={240}
              minRightWidth={320}
              storageKey="loomcycle.split.teams"
            >
              {diagramPane}
              {editorPane}
            </Splitter>
          ) : (
            diagramPane
          )}
        </div>
      </div>
    </div>
  );
}
