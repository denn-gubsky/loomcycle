import { ArrowDown, ArrowUp, Plus, Trash2, X } from "lucide-react";
import {
  WEEKDAYS,
  catalogType,
  emptySelector,
  selectorShape,
  setResourceType,
  validateDraft,
  verbsFor,
  type AclCatalog,
  type ConditionDraft,
  type RuleDraft,
  type SelectorDraft,
  type TriState,
} from "../../lib/aclRules";

// RuleEditor is the structured editor for ONE rule. Every picker is fed by the
// catalog: the type list, the scope list of the chosen type, and the action
// checkboxes (only the verbs every chosen resource type has). Changing a
// resource's type resets its selector and drops actions the new set lacks, so
// the draft never holds a pair the catalog refuses.

const GLOB_HINT: Record<string, string> = {
  agent: "chat/* or support/**",
  tool: "Bash, or * for every tool",
  volume: "shared-ro or scratch-*",
  mcp: "jobs/get* (server/tool)",
  skill: "support/**",
};

function namePlaceholder(cat: AclCatalog, s: SelectorDraft): string {
  const t = catalogType(cat, s.type);
  if (!t) return "";
  if (selectorShape(t) === "scoped") return t.separator === ":" ? "optional path glob, e.g. /kb/**" : "optional name glob, e.g. chat/*";
  return GLOB_HINT[s.type] ?? "name or glob (* = every one)";
}

export function RuleEditor({
  catalog,
  draft,
  index,
  count,
  readOnly,
  onChange,
  onRemove,
  onMove,
}: {
  catalog: AclCatalog;
  draft: RuleDraft;
  index: number;
  count: number;
  readOnly?: boolean;
  onChange: (d: RuleDraft) => void;
  onRemove?: () => void;
  onMove?: (delta: -1 | 1) => void;
}) {
  const verbs = verbsFor(
    catalog,
    draft.resources.map((r) => r.type),
  );
  const errors = validateDraft(catalog, draft);
  const setRes = (i: number, patch: Partial<SelectorDraft>) =>
    onChange({ ...draft, resources: draft.resources.map((r, j) => (j === i ? { ...r, ...patch } : r)) });
  const setWhen = (patch: Partial<ConditionDraft>) => onChange({ ...draft, when: { ...draft.when, ...patch } });
  const toggle = (list: string[], v: string) => (list.includes(v) ? list.filter((x) => x !== v) : [...list, v]);
  const toggleAction = (v: string) => onChange({ ...draft, actions: verbs.filter((x) => (x === v ? !draft.actions.includes(v) : draft.actions.includes(x))) });
  // How many conditions are set, as the saved rule's `when` keys count them
  // (days, the window and tz are one `time` condition).
  const w = draft.when;
  const whenCount = [
    w.transport.length > 0,
    w.days.length > 0 || !!w.from || !!w.to || !!w.tz,
    w.interactive !== "",
    !!w.tier.trim(),
    !!w.userTier.trim(),
    !!w.sourceCidr.trim(),
    w.ownerIsSelf !== "",
    !!w.agent.trim(),
    w.depthMax.trim() !== "",
  ].filter(Boolean).length;

  return (
    <fieldset className={"acl-rule acl-rule-" + draft.effect} disabled={readOnly}>
      <div className="acl-rule-head">
        <span className="acl-rule-index">rule {index}</span>
        <select
          aria-label="effect"
          value={draft.effect}
          onChange={(e) => onChange({ ...draft, effect: e.target.value as "allow" | "deny" })}
        >
          <option value="allow">allow</option>
          <option value="deny">deny</option>
        </select>
        <input
          className="acl-rule-desc"
          type="text"
          placeholder="description (optional)"
          value={draft.description}
          onChange={(e) => onChange({ ...draft, description: e.target.value })}
        />
        {!readOnly && (
          <span className="acl-rule-tools">
            {onMove && (
              <>
                <button type="button" className="ghost-btn" title="Move up" disabled={index === 0} onClick={() => onMove(-1)}>
                  <ArrowUp size={14} />
                </button>
                <button type="button" className="ghost-btn" title="Move down" disabled={index === count - 1} onClick={() => onMove(1)}>
                  <ArrowDown size={14} />
                </button>
              </>
            )}
            {onRemove && (
              <button type="button" className="ghost-btn danger" title="Remove rule" onClick={onRemove}>
                <Trash2 size={14} />
              </button>
            )}
          </span>
        )}
      </div>

      <div className="acl-rule-section">
        <span className="acl-rule-label">resources</span>
        <div className="acl-rule-resources">
          {draft.resources.map((r, i) => {
            const t = catalogType(catalog, r.type);
            const shape = t ? selectorShape(t) : "glob";
            return (
              <div className="acl-rule-resource" key={i}>
                <select aria-label="resource type" value={r.type} onChange={(e) => onChange(setResourceType(catalog, draft, i, e.target.value))}>
                  {catalog.types.map((ct) => (
                    <option key={ct.type} value={ct.type}>
                      {ct.type}
                    </option>
                  ))}
                </select>
                {shape !== "glob" && t && (
                  <select aria-label="scope" value={r.scope} onChange={(e) => setRes(i, { scope: e.target.value })}>
                    {[...t.scopes!, "*"].map((sc) => (
                      <option key={sc} value={sc}>
                        {sc === "*" ? "* (every scope)" : sc}
                      </option>
                    ))}
                  </select>
                )}
                {shape !== "enum" && (
                  <input
                    aria-label="selector"
                    type="text"
                    value={r.name}
                    placeholder={namePlaceholder(catalog, r)}
                    onChange={(e) => setRes(i, { name: e.target.value })}
                  />
                )}
                {draft.resources.length > 1 && !readOnly && (
                  <button
                    type="button"
                    className="ghost-btn"
                    title="Remove resource"
                    onClick={() => {
                      const resources = draft.resources.filter((_, j) => j !== i);
                      const keep = verbsFor(
                        catalog,
                        resources.map((x) => x.type),
                      );
                      onChange({ ...draft, resources, actions: draft.actions.filter((a) => keep.includes(a)) });
                    }}
                  >
                    <X size={14} />
                  </button>
                )}
              </div>
            );
          })}
          {!readOnly && (
            <button
              type="button"
              className="ghost-btn acl-rule-add"
              onClick={() => {
                const t = catalogType(catalog, draft.resources[0]?.type ?? "") ?? catalog.types[0];
                if (t) onChange({ ...draft, resources: [...draft.resources, emptySelector(t)] });
              }}
            >
              <Plus size={14} /> resource
            </button>
          )}
        </div>
      </div>

      <div className="acl-rule-section">
        <span className="acl-rule-label">actions</span>
        <div className="acl-rule-checks">
          {verbs.length === 0 ? (
            <span className="settings-muted">these resource types share no action — split them into separate rules</span>
          ) : (
            verbs.map((v) => (
              <label key={v} className="acl-check">
                <input type="checkbox" checked={draft.actions.includes(v)} onChange={() => toggleAction(v)} /> {v}
              </label>
            ))
          )}
        </div>
      </div>

      <details className="acl-rule-when" open={whenCount > 0}>
        <summary>
          conditions (when){whenCount > 0 ? ` — ${whenCount} set` : " — always"}
        </summary>
        <div className="acl-when-grid">
          <span className="acl-rule-label">transport</span>
          <div className="acl-rule-checks">
            {catalog.transports.map((tr) => (
              <label key={tr} className="acl-check">
                <input type="checkbox" checked={draft.when.transport.includes(tr)} onChange={() => setWhen({ transport: toggle(draft.when.transport, tr) })} />{" "}
                {tr}
              </label>
            ))}
          </div>
          <span className="acl-rule-label">days</span>
          <div className="acl-rule-checks">
            {WEEKDAYS.map((d) => (
              <label key={d} className="acl-check">
                <input type="checkbox" checked={draft.when.days.includes(d)} onChange={() => setWhen({ days: toggle(draft.when.days, d) })} /> {d}
              </label>
            ))}
          </div>
          <span className="acl-rule-label">between</span>
          <div className="acl-rule-inline">
            <input aria-label="from" type="text" placeholder="HH:MM" value={draft.when.from} onChange={(e) => setWhen({ from: e.target.value })} />
            <span>–</span>
            <input aria-label="to" type="text" placeholder="HH:MM" value={draft.when.to} onChange={(e) => setWhen({ to: e.target.value })} />
            <input aria-label="time zone" type="text" placeholder="tz, e.g. Europe/Kyiv" value={draft.when.tz} onChange={(e) => setWhen({ tz: e.target.value })} />
          </div>
          <span className="acl-rule-label">interactive</span>
          <TriSelect label="interactive" value={draft.when.interactive} onChange={(v) => setWhen({ interactive: v })} />
          <span className="acl-rule-label">owner is self</span>
          <TriSelect label="owner is self" value={draft.when.ownerIsSelf} onChange={(v) => setWhen({ ownerIsSelf: v })} />
          <span className="acl-rule-label">agent</span>
          <input aria-label="agent glob" type="text" placeholder="agent glob, e.g. ci/*" value={draft.when.agent} onChange={(e) => setWhen({ agent: e.target.value })} />
          <span className="acl-rule-label">max depth</span>
          <input aria-label="max depth" type="text" inputMode="numeric" placeholder="spawn depth, e.g. 1" value={draft.when.depthMax} onChange={(e) => setWhen({ depthMax: e.target.value })} />
          <span className="acl-rule-label">tier</span>
          <input aria-label="tier" type="text" placeholder="comma-separated" value={draft.when.tier} onChange={(e) => setWhen({ tier: e.target.value })} />
          <span className="acl-rule-label">user tier</span>
          <input aria-label="user tier" type="text" placeholder="comma-separated" value={draft.when.userTier} onChange={(e) => setWhen({ userTier: e.target.value })} />
          <span className="acl-rule-label">source CIDR</span>
          <input aria-label="source cidr" type="text" placeholder="10.0.0.0/8, …" value={draft.when.sourceCidr} onChange={(e) => setWhen({ sourceCidr: e.target.value })} />
        </div>
      </details>

      {errors.length > 0 && !readOnly && (
        <ul className="acl-rule-errors">
          {errors.map((e) => (
            <li key={e}>{e}</li>
          ))}
        </ul>
      )}
    </fieldset>
  );
}

function TriSelect({ label, value, onChange }: { label: string; value: TriState; onChange: (v: TriState) => void }) {
  return (
    <select aria-label={label} value={value} onChange={(e) => onChange(e.target.value as TriState)}>
      <option value="">any</option>
      <option value="true">yes</option>
      <option value="false">no</option>
    </select>
  );
}
