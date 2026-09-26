// The hooks a TeamDef carries, read and written on its graph JSON. A team adds
// hooks in two places: its own top-level `hooks`, which belong to the walk and
// take only run_end (the walk makes no model or tool calls), and a state's
// handler `hooks` / `tool_hooks`, added to every run that state starts. Only the
// handler kinds that start runs may carry them; the runtime refuses the rest.

export const WALK = "walk";

/** The handler kinds that start runs, and so may carry hooks. */
export const RUN_STARTING_KINDS = ["agent", "parallel", "consolidator", "starter"];

export interface HookTarget {
  /** WALK, or a state's id. */
  id: string;
  label: string;
}

type Obj = Record<string, unknown>;
const isObj = (v: unknown): v is Obj => !!v && typeof v === "object" && !Array.isArray(v);

function states(def: unknown): Obj[] {
  if (!isObj(def) || !Array.isArray(def.states)) return [];
  return def.states.filter(isObj);
}

/** hookTargets lists where hooks may be attached in this graph, the walk first. */
export function hookTargets(def: unknown): HookTarget[] {
  const out: HookTarget[] = [{ id: WALK, label: "the walk (run_end)" }];
  for (const st of states(def)) {
    const h = isObj(st.handler) ? st.handler : {};
    if (typeof st.state === "string" && typeof h.kind === "string" && RUN_STARTING_KINDS.includes(h.kind)) {
      out.push({ id: st.state, label: `${st.state} (${h.kind})` });
    }
  }
  return out;
}

/** readTeamHooks returns the hooks at a target: the walk's, or a state's. */
export function readTeamHooks(def: unknown, target: string): { hooks?: unknown; tool_hooks?: unknown } {
  if (target === WALK) return isObj(def) ? { hooks: def.hooks } : {};
  const st = states(def).find((s) => s.state === target);
  const h = st && isObj(st.handler) ? st.handler : {};
  return { hooks: h.hooks, tool_hooks: h.tool_hooks };
}

/** writeTeamHooks returns the graph with one hooks key at a target replaced
 *  (undefined removes it). The input is not modified. A walk has no tool
 *  hooks, and an unknown state leaves the graph unchanged. */
export function writeTeamHooks(def: unknown, target: string, key: "hooks" | "tool_hooks", value: unknown): unknown {
  if (!isObj(def)) return def;
  const set = (o: Obj): Obj => {
    const next = { ...o };
    if (value === undefined) delete next[key];
    else next[key] = value;
    return next;
  };
  if (target === WALK) return key === "hooks" ? set(def) : def;
  if (!Array.isArray(def.states)) return def;
  return {
    ...def,
    states: def.states.map((s) =>
      isObj(s) && s.state === target ? { ...s, handler: set(isObj(s.handler) ? s.handler : {}) } : s,
    ),
  };
}
