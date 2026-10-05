// teamLocal — what a team definition declares for itself, read off its graph
// JSON for the Teams page: its variables and its `local` agents, skills,
// channels, schedules and webhooks. Each entry gets a one-line summary.
//
// Only names and shapes are read. A webhook's secrets are env-var NAMES in the
// definition (`signing_secret_env`, `bearer_token_env`) and only those fields
// are read, so a summary can never carry a secret value.

import { declaredVars, type VarField } from "./teamStart";

type Obj = Record<string, unknown>;
const isObj = (v: unknown): v is Obj => typeof v === "object" && v !== null && !Array.isArray(v);
const str = (v: unknown): string => (typeof v === "string" ? v.trim() : "");

/** The kinds a team may declare under `local`, in the order the page lists them. */
export const LOCAL_KINDS = ["agents", "skills", "channels", "schedules", "webhooks"] as const;
export type LocalKind = (typeof LOCAL_KINDS)[number];

export interface LocalEntry {
  name: string;
  summary: string;
}

export interface TeamOwnDefinitions {
  vars: VarField[];
  local: Record<LocalKind, LocalEntry[]>;
}

/** teamOwnDefinitions reads a definition's `vars` and `local` block. `team` and
 *  `tenant` (the team's owning tenant, "" for the shared one) build each
 *  webhook's route. */
export function teamOwnDefinitions(definition: unknown, team: string, tenant: string): TeamOwnDefinitions {
  const local = isObj(definition) && isObj(definition.local) ? definition.local : {};
  const entries = (kind: LocalKind, summarize: (name: string, body: Obj) => string): LocalEntry[] => {
    const block = local[kind];
    if (!isObj(block)) return [];
    return Object.entries(block).map(([name, body]) => ({ name, summary: summarize(name, isObj(body) ? body : {}) }));
  };
  return {
    vars: declaredVars(definition),
    local: {
      agents: entries("agents", (_, b) => agentSummary(b)),
      skills: entries("skills", (_, b) => skillSummary(b)),
      channels: entries("channels", (_, b) => channelSummary(b)),
      schedules: entries("schedules", (_, b) => scheduleSummary(b)),
      webhooks: entries("webhooks", (name, b) => webhookSummary(b, webhookRoute(tenant, team, name))),
    },
  };
}

/** countOwnDefinitions is how many variables and local entries there are. */
export function countOwnDefinitions(d: TeamOwnDefinitions): number {
  return d.vars.length + LOCAL_KINDS.reduce((n, k) => n + d.local[k].length, 0);
}

/** agentSummary: where it routes, then its tools. */
export function agentSummary(body: Obj): string {
  const provider = str(body.provider);
  const model = str(body.model);
  const tier = str(body.tier);
  let routing: string;
  if (provider && model) routing = `${provider}/${model}`;
  else if (model) routing = `model ${model}`;
  else if (provider) routing = tier ? `${provider}, tier ${tier}` : provider;
  else if (tier) routing = `tier ${tier}`;
  else routing = "default routing";
  const tools = Array.isArray(body.tools) ? body.tools.filter((t): t is string => typeof t === "string") : [];
  return `${routing} · ${tools.length > 0 ? `tools: ${tools.join(", ")}` : "no tools"}`;
}

/** skillSummary: its description, or the tools it needs when it has none. */
export function skillSummary(body: Obj): string {
  const description = str(body.description);
  if (description) return description;
  const tools = Array.isArray(body.tools) ? body.tools.filter((t): t is string => typeof t === "string") : [];
  return tools.length > 0 ? `(no description) · tools: ${tools.join(", ")}` : "(no description)";
}

/** channelSummary: its scope. */
export function channelSummary(body: Obj): string {
  const scope = str(body.scope);
  return scope ? `scope ${scope}` : "no scope set";
}

/** scheduleSummary: the cadence and the channel each tick goes to. */
export function scheduleSummary(body: Obj): string {
  return `${str(body.schedule) || "(no cadence)"} → ${str(body.channel) || "(no channel)"}`;
}

/** webhookRoute is the path a team's own webhook answers on while a walk of
 *  the team runs: with the tenant segment for a team in a tenant, without it
 *  for one in the shared tenant. */
export function webhookRoute(tenant: string, team: string, name: string): string {
  const seg = encodeURIComponent;
  return tenant
    ? `/v1/_teams/${seg(tenant)}/${seg(team)}/webhooks/${seg(name)}`
    : `/v1/_teams/${seg(team)}/webhooks/${seg(name)}`;
}

/** webhookSummary: the route, how a delivery authenticates (naming the env
 *  var that holds the secret, never a value) and the channel it publishes to. */
export function webhookSummary(body: Obj, route: string): string {
  const auth = isObj(body.auth) ? body.auth : {};
  const kind = str(auth.kind) || "hmac";
  let how: string;
  if (kind === "hmac") {
    const env = str(auth.signing_secret_env);
    how = env ? `hmac, secret in ${env}` : "hmac, no secret env set";
  } else if (kind === "bearer") {
    const env = str(auth.bearer_token_env);
    how = env ? `bearer, token in ${env}` : "bearer, no token env set";
  } else if (kind === "none") {
    how = "no auth";
  } else {
    how = `auth ${kind}`;
  }
  return `POST ${route} · ${how} → ${str(body.channel) || "(no channel)"}`;
}

/** readLocalAgent is one of the team's own agents' body (the overlay an agent
 *  definition takes), or undefined when the definition declares no such agent. */
export function readLocalAgent(definition: unknown, name: string): Obj | undefined {
  if (!isObj(definition) || !isObj(definition.local) || !isObj(definition.local.agents)) return undefined;
  const body = definition.local.agents[name];
  return isObj(body) ? body : undefined;
}

/** writeLocalAgent returns the definition with one local agent's body
 *  replaced, everything else — the other agents, the other local kinds, the
 *  graph — as it was. The input is not modified. A name the definition does
 *  not declare leaves it unchanged: this edits agents, it does not add them. */
export function writeLocalAgent(definition: unknown, name: string, body: Obj): unknown {
  if (readLocalAgent(definition, name) === undefined) return definition;
  const def = definition as Obj;
  const local = def.local as Obj;
  return { ...def, local: { ...local, agents: { ...(local.agents as Obj), [name]: body } } };
}
