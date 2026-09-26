import { useId, useState } from "react";
import {
  AGENT_HOOK_EVENTS,
  HOOK_EVENT_HINTS,
  TOOL_HOOK_EVENTS,
  asEventHooks,
  asToolHooks,
  entryProblem,
  isInline,
  pruneEventHooks,
  pruneToolHooks,
  type EventHooks,
  type HookEntry,
  type InlineWebhook,
} from "../lib/hooks";

// The hooks editor. Wherever hooks attach — an agent, one of its tools, a team
// state, a walk — the value is event -> ordered entries, and an entry is a
// HookDef name or an inline webhook. These controls edit exactly that shape.
//
// Every add names its event (and tool) and appends the entry in one step, so the
// value never holds an empty event or an empty tool: clearing the last entry
// removes the key, and the overlay stays sparse.

type EntryKind = "ref" | "webhook";

const blankEntry = (kind: EntryKind): HookEntry =>
  kind === "ref" ? "" : { name: "", url: "", fail_mode: "open" };

// HookEntryList edits one event's entries, in the order they run.
export function HookEntryList({
  entries, onChange, disabled, hookNames,
}: {
  entries: HookEntry[];
  onChange: (next: HookEntry[]) => void;
  disabled?: boolean;
  /** HookDef names offered as suggestions for a reference. */
  hookNames?: readonly string[];
}) {
  const listId = useId();
  const set = (i: number, e: HookEntry) => {
    const next = [...entries];
    next[i] = e;
    onChange(next);
  };
  const move = (i: number, d: -1 | 1) => {
    const j = i + d;
    if (j < 0 || j >= entries.length) return;
    const next = [...entries];
    [next[i], next[j]] = [next[j]!, next[i]!];
    onChange(next);
  };
  return (
    <div className="lc-df-hook-entries">
      {hookNames && hookNames.length > 0 && (
        <datalist id={listId}>
          {hookNames.map((n) => <option key={n} value={n} />)}
        </datalist>
      )}
      {entries.map((e, i) => {
        const problem = entryProblem(e);
        return (
          <div className="lc-df-hook-entry" key={i}>
            <div className="lc-df-hook-entry-head">
              <span className="lc-df-hook-kind">{isInline(e) ? "webhook" : "HookDef"}</span>
              <span className="lc-df-hook-order">
                <button type="button" className="lc-df-row-btn" disabled={disabled || i === 0}
                  aria-label="Move up" onClick={() => move(i, -1)}>↑</button>
                <button type="button" className="lc-df-row-btn" disabled={disabled || i === entries.length - 1}
                  aria-label="Move down" onClick={() => move(i, 1)}>↓</button>
                <button type="button" className="lc-df-row-btn" disabled={disabled}
                  aria-label="Remove hook" onClick={() => onChange(entries.filter((_, j) => j !== i))}>×</button>
              </span>
            </div>
            {isInline(e) ? (
              <WebhookFields value={e} disabled={disabled} onChange={(w) => set(i, w)} />
            ) : (
              <input
                className="lc-df-input" type="text" value={e} disabled={disabled}
                placeholder="hook-name or hook-name@3" list={hookNames?.length ? listId : undefined}
                aria-label="HookDef name"
                onChange={(ev) => set(i, ev.target.value)}
              />
            )}
            {problem && <p className="lc-df-error">{problem}</p>}
          </div>
        );
      })}
    </div>
  );
}

function WebhookFields({
  value, onChange, disabled,
}: { value: InlineWebhook; onChange: (v: InlineWebhook) => void; disabled?: boolean }) {
  const headers = Object.entries(value.headers ?? {});
  const setHeaders = (rows: [string, string][]) => {
    const next = { ...value };
    if (rows.length === 0) delete next.headers;
    else next.headers = Object.fromEntries(rows);
    onChange(next);
  };
  return (
    <div className="lc-df-hook-webhook">
      <div className="lc-df-kv-row">
        <input className="lc-df-input lc-df-kv-key" type="text" value={value.name} disabled={disabled}
          placeholder="name" aria-label="Webhook name"
          onChange={(e) => onChange({ ...value, name: e.target.value })} />
        <input className="lc-df-input lc-df-kv-val" type="text" value={value.url} disabled={disabled}
          placeholder="https://…" aria-label="Webhook URL"
          onChange={(e) => onChange({ ...value, url: e.target.value })} />
      </div>
      <div className="lc-df-kv-row">
        <select className="lc-df-input lc-df-kv-key" value={value.fail_mode ?? "open"} disabled={disabled}
          aria-label="Fail mode"
          onChange={(e) => onChange({ ...value, fail_mode: e.target.value as "open" | "closed" })}>
          <option value="open">fail open — a failed call lets the action through</option>
          <option value="closed">fail closed — a failed call blocks it</option>
        </select>
        <input className="lc-df-input lc-df-kv-val" type="number" min={0} disabled={disabled}
          value={value.timeout_ms === undefined ? "" : String(value.timeout_ms)}
          placeholder="timeout ms (default)" aria-label="Timeout in milliseconds"
          onChange={(e) => {
            const next = { ...value };
            const raw = e.target.value.trim();
            if (raw === "") delete next.timeout_ms;
            else next.timeout_ms = Number(raw);
            onChange(next);
          }} />
      </div>
      {headers.map(([k, v], i) => (
        <div className="lc-df-kv-row" key={i}>
          <input className="lc-df-input lc-df-kv-key" type="text" value={k} disabled={disabled}
            placeholder="header" aria-label="Header name"
            onChange={(e) => { const r = [...headers] as [string, string][]; r[i] = [e.target.value, v]; setHeaders(r); }} />
          <input className="lc-df-input lc-df-kv-val" type="text" value={v} disabled={disabled}
            placeholder="value, or $cred:<name>" aria-label="Header value"
            onChange={(e) => { const r = [...headers] as [string, string][]; r[i] = [k, e.target.value]; setHeaders(r); }} />
          <button type="button" className="lc-df-row-btn" disabled={disabled} aria-label={`Remove header ${k}`}
            onClick={() => setHeaders(headers.filter((_, j) => j !== i) as [string, string][])}>×</button>
        </div>
      ))}
      <button type="button" className="lc-df-row-btn lc-df-add" disabled={disabled}
        onClick={() => setHeaders([...(headers as [string, string][]), ["", ""]])}>+ header</button>
    </div>
  );
}

// HookEventsControl edits an event -> entries map (an agent's hooks, a team
// state's hooks, a walk's hooks). `events` limits which events may be added —
// a walk only ends, so it takes run_end alone.
export function HookEventsControl({
  value, onChange, disabled, events = AGENT_HOOK_EVENTS, hookNames,
}: {
  value: unknown;
  onChange: (next: EventHooks | undefined) => void;
  disabled?: boolean;
  events?: readonly string[];
  hookNames?: readonly string[];
}) {
  const hooks = asEventHooks(value);
  const [addEvent, setAddEvent] = useState<string>(events[0] ?? "");
  const [addKind, setAddKind] = useState<EntryKind>("ref");
  // Show the events in the order they happen, then any the list does not know
  // (kept visible so nothing stored is hidden from the operator).
  const shown = [...events.filter((e) => hooks[e]), ...Object.keys(hooks).filter((e) => !events.includes(e))];
  const emit = (event: string, list: HookEntry[]) => onChange(pruneEventHooks({ ...hooks, [event]: list }));
  return (
    <div className="lc-df-hooks">
      {shown.map((event) => (
        <div className="lc-df-hook-event" key={event}>
          <div className="lc-df-hook-event-head">
            <code>{event}</code>
            {HOOK_EVENT_HINTS[event] && <span className="lc-df-hook-event-hint">{HOOK_EVENT_HINTS[event]}</span>}
          </div>
          <HookEntryList entries={hooks[event] ?? []} disabled={disabled} hookNames={hookNames}
            onChange={(list) => emit(event, list)} />
        </div>
      ))}
      <AddRow disabled={disabled} kind={addKind} onKind={setAddKind}
        onAdd={() => emit(addEvent, [...(hooks[addEvent] ?? []), blankEntry(addKind)])}>
        <select className="lc-df-input" value={addEvent} disabled={disabled} aria-label="Event"
          onChange={(e) => setAddEvent(e.target.value)}>
          {events.map((e) => <option key={e} value={e}>{e}</option>)}
        </select>
      </AddRow>
    </div>
  );
}

// ToolHooksControl edits tool -> (pre | post | post_failure) -> entries: each
// tool's own hooks. `tools` are offered as suggestions (the agent's tools); the
// runtime refuses a hook on a tool the agent does not have.
export function ToolHooksControl({
  value, onChange, disabled, tools, hookNames,
}: {
  value: unknown;
  onChange: (next: Record<string, EventHooks> | undefined) => void;
  disabled?: boolean;
  tools?: readonly string[];
  hookNames?: readonly string[];
}) {
  const byTool = asToolHooks(value);
  const listId = useId();
  const [addTool, setAddTool] = useState("");
  const [addEvent, setAddEvent] = useState<string>(TOOL_HOOK_EVENTS[0]);
  const [addKind, setAddKind] = useState<EntryKind>("ref");
  const emit = (tool: string, event: string, list: HookEntry[]) =>
    onChange(pruneToolHooks({ ...byTool, [tool]: { ...(byTool[tool] ?? {}), [event]: list } }));
  const remove = (tool: string) => {
    const next = { ...byTool };
    delete next[tool];
    onChange(pruneToolHooks(next));
  };
  const toolName = addTool.trim();
  return (
    <div className="lc-df-hooks">
      {Object.entries(byTool).map(([tool, ev]) => (
        <div className="lc-df-hook-tool" key={tool}>
          <div className="lc-df-hook-event-head">
            <code>{tool}</code>
            <button type="button" className="lc-df-row-btn" disabled={disabled} aria-label={`Remove ${tool}'s hooks`}
              onClick={() => remove(tool)}>×</button>
          </div>
          {[...TOOL_HOOK_EVENTS.filter((e) => ev[e]), ...Object.keys(ev).filter((e) => !(TOOL_HOOK_EVENTS as readonly string[]).includes(e))].map((event) => (
            <div className="lc-df-hook-event" key={event}>
              <div className="lc-df-hook-event-head">
                <code>{event}</code>
                {HOOK_EVENT_HINTS[event] && <span className="lc-df-hook-event-hint">{HOOK_EVENT_HINTS[event]}</span>}
              </div>
              <HookEntryList entries={ev[event] ?? []} disabled={disabled} hookNames={hookNames}
                onChange={(list) => emit(tool, event, list)} />
            </div>
          ))}
        </div>
      ))}
      {tools && tools.length > 0 && (
        <datalist id={listId}>
          {tools.map((t) => <option key={t} value={t} />)}
        </datalist>
      )}
      <AddRow disabled={disabled || toolName === ""} kind={addKind} onKind={setAddKind}
        onAdd={() => {
          emit(toolName, addEvent, [...(byTool[toolName]?.[addEvent] ?? []), blankEntry(addKind)]);
          setAddTool("");
        }}>
        <input className="lc-df-input" type="text" value={addTool} disabled={disabled}
          placeholder="tool" aria-label="Tool" list={tools?.length ? listId : undefined}
          onChange={(e) => setAddTool(e.target.value)} />
        <select className="lc-df-input" value={addEvent} disabled={disabled} aria-label="Event"
          onChange={(e) => setAddEvent(e.target.value)}>
          {TOOL_HOOK_EVENTS.map((e) => <option key={e} value={e}>{e}</option>)}
        </select>
      </AddRow>
    </div>
  );
}

function AddRow({
  children, kind, onKind, onAdd, disabled,
}: {
  children: React.ReactNode;
  kind: EntryKind;
  onKind: (k: EntryKind) => void;
  onAdd: () => void;
  disabled?: boolean;
}) {
  return (
    <div className="lc-df-hook-add">
      {children}
      <select className="lc-df-input" value={kind} aria-label="Hook kind"
        onChange={(e) => onKind(e.target.value as EntryKind)}>
        <option value="ref">a HookDef</option>
        <option value="webhook">an inline webhook</option>
      </select>
      <button type="button" className="lc-df-row-btn lc-df-add" disabled={disabled} onClick={onAdd}>+ add hook</button>
    </div>
  );
}
