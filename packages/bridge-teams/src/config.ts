// Bridge configuration, read from the environment (RFC EN §8).
//
// The shared surface (LOOMCYCLE_*, BRIDGE_*) is the core's. The Teams SDK
// reads its own credentials (CLIENT_ID, CLIENT_SECRET, TENANT_ID) from the
// environment; this module checks only what the bridge itself needs, and
// refuses to start rather than run half-configured.

import { EnvReader, loadSharedConfig, type Env, type SharedConfig } from "@loomcycle/bridge-core";

import { DEFAULT_FILE_HOSTS } from "./voice.js";

export interface TeamsConfig {
  shared: SharedConfig;
  /** Entra tenant ids allowed to talk to the bot, lower-cased. */
  allowedTenants: ReadonlySet<string>;
  /** Host suffixes an attached file's download link may point to (voice
   *  messages, RFC GA), each starting with a dot. */
  fileHosts: readonly string[];
}

export const DEFAULT_PORT = 3978;

export function loadConfig(env: Env = process.env): TeamsConfig {
  const r = new EnvReader(env);
  // A gate only when BRIDGE_GATE names one: a bridge deployed with a
  // loomcycle token keeps running as it was until it is reconfigured.
  const shared = loadSharedConfig(r, { port: DEFAULT_PORT, gate: r.get("BRIDGE_GATE") !== undefined });
  const raw = r.required("TEAMS_BRIDGE_ALLOWED_TENANTS");
  const allowedTenants = new Set(r.list("TEAMS_BRIDGE_ALLOWED_TENANTS", { lower: true }));
  if (raw && allowedTenants.size === 0) r.problems.push("TEAMS_BRIDGE_ALLOWED_TENANTS lists no tenant");
  if (!shared.dev && !r.get("CLIENT_ID")) {
    // Without credentials the Teams SDK can neither validate inbound requests
    // against our bot id nor send replies.
    r.problems.push("CLIENT_ID is required (or set BRIDGE_DEV=1 for local testing)");
  }
  const fileHosts = loadFileHosts(r);
  r.check("teams-bridge");
  return { shared, allowedTenants, fileHosts };
}

/** loadFileHosts reads TEAMS_BRIDGE_FILE_HOSTS: domain suffixes such as
 *  `.sharepoint.com` or `.sharepoint.us` (a leading dot is added when
 *  missing). A suffix of fewer than two labels (`.com`) would let a file
 *  link point almost anywhere, so it is refused. */
function loadFileHosts(r: EnvReader): string[] {
  if (r.get("TEAMS_BRIDGE_FILE_HOSTS") === undefined) return [...DEFAULT_FILE_HOSTS];
  const hosts = r.list("TEAMS_BRIDGE_FILE_HOSTS", { lower: true }).map((h) => (h.startsWith(".") ? h : `.${h}`));
  const bad = hosts.filter((h) => !/^(\.[a-z0-9-]+){2,}$/.test(h));
  if (hosts.length === 0) r.problems.push("TEAMS_BRIDGE_FILE_HOSTS lists no host");
  for (const h of bad) r.problems.push(`TEAMS_BRIDGE_FILE_HOSTS: "${h}" is not a domain suffix of at least two labels`);
  return hosts;
}
