import { useCallback, useEffect, useState } from "react";
import {
  TeamDiagram,
  TeamNameSummary,
  listTeams,
  renderTeamDiagram,
} from "../api";

// TeamsView — the agent-team board (RFC AP / BD).
//
// Lists TeamDefs (/v1/_teamdef/names) and, for a selected team, renders its
// Mermaid stateDiagram-v2 source via the render_diagram op — the workflow's
// states, transitions, and colour scheme. An optional "highlight state" marks
// one state (e.g. a running chunk's current state) with a bold outline.
//
// v1 shows the diagram SOURCE (dep-free): it renders in any Markdown/Mermaid
// viewer and is copy-pasteable. A live in-page graph render (the `mermaid` lib)
// + binding to a running Document board's current state are follow-ups.

export default function TeamsView() {
  const [teams, setTeams] = useState<TeamNameSummary[]>([]);
  const [selected, setSelected] = useState<string>("");
  const [highlight, setHighlight] = useState<string>("");
  const [diagram, setDiagram] = useState<TeamDiagram | null>(null);
  const [err, setErr] = useState<string>("");
  const [diagErr, setDiagErr] = useState<string>("");
  const [loading, setLoading] = useState(false);

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

  return (
    <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "1rem" }}>
      <div>
        <h1>teams</h1>
        <p style={{ opacity: 0.7, margin: "0.25rem 0" }}>
          Agent-team workflows (RFC AP). Select a team to see its state-machine
          diagram — states, transitions, and the colour scheme.
        </p>
      </div>

      {err && <div style={{ color: "var(--error, #e03131)" }}>Failed to load teams: {err}</div>}

      <div style={{ display: "flex", gap: "1.5rem", alignItems: "flex-start", flexWrap: "wrap" }}>
        {/* Team list */}
        <div style={{ minWidth: 220, flex: "0 0 auto" }}>
          <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
            <strong>teams ({teams.length})</strong>
            <button onClick={() => void fetchTeams()} disabled={loading}>
              {loading ? "…" : "refresh"}
            </button>
          </div>
          {teams.length === 0 && !loading && (
            <p style={{ opacity: 0.6 }}>
              No teams yet. Create one with the <code>TeamDef</code> tool or the{" "}
              <code>team/assistant</code> agent.
            </p>
          )}
          <ul style={{ listStyle: "none", padding: 0, margin: "0.5rem 0" }}>
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
                      border: "1px solid var(--border, #ccc)",
                      borderRadius: 6,
                      background: isSel ? "var(--accent-bg, #eef)" : "transparent",
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

        {/* Selected team diagram */}
        <div style={{ flex: "1 1 420px", minWidth: 320 }}>
          {!selected && <p style={{ opacity: 0.6 }}>Select a team to view its diagram.</p>}
          {selected && (
            <div>
              <div style={{ display: "flex", gap: "0.75rem", alignItems: "center", flexWrap: "wrap" }}>
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
                <div style={{ color: "var(--error, #e03131)", marginTop: "0.5rem" }}>
                  {diagErr}
                </div>
              )}
              {diagram && (
                <>
                  <p style={{ fontSize: "0.8em", opacity: 0.6, margin: "0.5rem 0 0.25rem" }}>
                    Mermaid <code>stateDiagram-v2</code> — paste into any Mermaid viewer, or
                    view where Mermaid renders (GitHub, the docs viewer).
                  </p>
                  <pre
                    style={{
                      background: "var(--code-bg, #f6f8fa)",
                      border: "1px solid var(--border, #ddd)",
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
                </>
              )}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
