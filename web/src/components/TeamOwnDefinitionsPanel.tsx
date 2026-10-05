import { useMemo, useState } from "react";
import { FoldedFieldList, agentDefRegistry } from "@loomcycle/def-fields";
import "@loomcycle/def-fields/styles.css";
import {
  LOCAL_KINDS,
  type LocalKind,
  countOwnDefinitions,
  readLocalAgent,
  teamOwnDefinitions,
  writeLocalAgent,
} from "../lib/teamLocal";

const LOCAL_KIND_LABELS: Record<LocalKind, string> = {
  agents: "Agents",
  skills: "Skills",
  channels: "Channels",
  schedules: "Schedules",
  webhooks: "Webhooks",
};

// TeamOwnDefinitionsPanel lists what the graph JSON declares for the team
// itself: its variables and its own agents, skills, channels, schedules and
// webhooks. Like the hooks panel it reads the editor text, so it follows
// unsaved edits. An agent can be edited here with the same field list the
// Library uses for an agent definition, which writes its body back into the
// JSON; everything else is edited in the JSON itself.
export default function TeamOwnDefinitionsPanel({
  editorText, setEditorText, team, tenant, disabled,
}: {
  editorText: string;
  setEditorText: (s: string) => void;
  team: string;
  tenant: string;
  disabled: boolean;
}) {
  const [editing, setEditing] = useState<string>("");
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
            <code>./name</code>; they run as <code>{team}/name</code>. Edit them in the JSON above, or an
            agent with its field list here.
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
                    {k === "agents" && (
                      <button
                        type="button"
                        onClick={() => setEditing(editing === e.name ? "" : e.name)}
                        disabled={disabled}
                        aria-expanded={editing === e.name}
                        style={{ marginLeft: "0.4rem", fontSize: "0.85em", padding: "0 0.4rem" }}
                      >
                        {editing === e.name ? "close" : "edit"}
                      </button>
                    )}
                    {k === "agents" && editing === e.name && parsed.ok && (
                      <LocalAgentEditor
                        // Keyed per agent: the list seeds its open groups from
                        // the body it mounts with.
                        key={e.name}
                        def={parsed.def}
                        name={e.name}
                        disabled={disabled}
                        onChange={(next) => setEditorText(JSON.stringify(next, null, 2))}
                      />
                    )}
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

// LocalAgentEditor is the Library's agent field list over one of the team's
// own agents. Each change rewrites that agent's body in the graph JSON (the
// other agents and the rest of the graph untouched), so the JSON stays the
// one source of truth and Save new version saves it like any other edit.
export function LocalAgentEditor({
  def, name, disabled, onChange,
}: {
  def: unknown;
  name: string;
  disabled: boolean;
  onChange: (next: unknown) => void;
}) {
  const body = readLocalAgent(def, name);
  if (!body) return null;
  return (
    <div style={{ margin: "0.3rem 0 0.6rem" }}>
      <FoldedFieldList
        registry={agentDefRegistry}
        value={body}
        onChange={(next) => onChange(writeLocalAgent(def, name, next))}
        disabled={disabled}
      />
    </div>
  );
}
