import { useMemo } from "react";
import { LOCAL_KINDS, type LocalKind, countOwnDefinitions, teamOwnDefinitions } from "../lib/teamLocal";

const LOCAL_KIND_LABELS: Record<LocalKind, string> = {
  agents: "Agents",
  skills: "Skills",
  channels: "Channels",
  schedules: "Schedules",
  webhooks: "Webhooks",
};

// TeamOwnDefinitionsPanel lists what the graph JSON declares for the team
// itself: its variables and its own agents, skills, channels, schedules and
// webhooks. Read-only — they are edited in the JSON above. Like the hooks
// panel it reads the editor text, so it follows unsaved edits.
export default function TeamOwnDefinitionsPanel({ editorText, team, tenant }: { editorText: string; team: string; tenant: string }) {
  const parsed = useMemo<{ ok: true; def: unknown } | { ok: false }>(() => {
    try {
      return { ok: true, def: JSON.parse(editorText) };
    } catch {
      return { ok: false };
    }
  }, [editorText]);
  if (editorText.trim() === "") return null;
  const own = parsed.ok ? teamOwnDefinitions(parsed.def, team, tenant) : undefined;
  const n = own ? countOwnDefinitions(own) : 0;
  const row = { margin: "0.15rem 0", fontSize: "0.85em", overflowWrap: "anywhere" } as const;
  return (
    <details className="team-own-defs" style={{ flex: "0 0 auto", maxHeight: "45%", overflow: "auto" }}>
      <summary style={{ cursor: "pointer", fontSize: "0.9em" }}>
        This team&apos;s own definitions{own ? ` (${n})` : ""}
      </summary>
      {!own ? (
        <p style={{ fontSize: "0.85em", opacity: 0.8 }}>Fix the JSON above to see them here.</p>
      ) : n === 0 ? (
        <p style={{ fontSize: "0.85em", opacity: 0.8 }}>
          None. A team declares its variables under <code>vars</code>, and its own agents, skills, channels,
          schedules and webhooks under <code>local</code>, in the JSON above.
        </p>
      ) : (
        <div style={{ display: "flex", flexDirection: "column", gap: "0.5rem", marginTop: "0.4rem" }}>
          <p style={{ fontSize: "0.8em", opacity: 0.8, margin: 0 }}>
            Declared in this definition and nowhere else: no global list shows them. States name them as{" "}
            <code>./name</code>; they run as <code>{team}/name</code>. Edit them in the JSON above.
          </p>
          {own.vars.length > 0 && (
            <section>
              <strong style={{ fontSize: "0.85em" }}>Variables ({own.vars.length})</strong>
              <ul style={{ listStyle: "none", padding: 0, margin: "0.2rem 0" }}>
                {own.vars.map((v) => (
                  <li key={v.name} style={row}>
                    <code>{v.name}</code> — default:{" "}
                    {v.defaultValue === "" ? <span style={{ opacity: 0.7 }}>(empty)</span> : v.defaultValue}
                  </li>
                ))}
              </ul>
            </section>
          )}
          {LOCAL_KINDS.filter((k) => own.local[k].length > 0).map((k) => (
            <section key={k}>
              <strong style={{ fontSize: "0.85em" }}>
                {LOCAL_KIND_LABELS[k]} ({own.local[k].length})
              </strong>
              <ul style={{ listStyle: "none", padding: 0, margin: "0.2rem 0" }}>
                {own.local[k].map((e) => (
                  <li key={e.name} style={row}>
                    <code>./{e.name}</code> <span style={{ opacity: 0.8 }}>— {e.summary}</span>
                  </li>
                ))}
              </ul>
            </section>
          ))}
        </div>
      )}
    </details>
  );
}
