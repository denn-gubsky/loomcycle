// Bridge configuration, read from the environment (RFC EN §8).
//
// The shared surface (LOOMCYCLE_*, BRIDGE_*) is the core's. The Teams SDK
// reads its own credentials (CLIENT_ID, CLIENT_SECRET, TENANT_ID) from the
// environment; this module checks only what the bridge itself needs, and
// refuses to start rather than run half-configured.

import { readFileSync } from "node:fs";

import { EnvReader, loadSharedConfig, type Env, type SharedConfig } from "@loomcycle/bridge-core";

import { parseCertificate, type CertificateTokenOptions } from "./filetoken.js";
import { DEFAULT_FILE_HOSTS } from "./voice.js";

export interface TeamsConfig {
  shared: SharedConfig;
  /** Entra tenant ids allowed to talk to the bot, lower-cased. */
  allowedTenants: ReadonlySet<string>;
  /** Host suffixes an attached file's download link may point to (voice
   *  messages, RFC GA), each starting with a dot. */
  fileHosts: readonly string[];
  /** TEAMS_BRIDGE_DUMP_ACTIVITIES=1: log every inbound activity, redacted
   *  (dump.ts), to see what Teams sent. Off by default: the dump holds what
   *  people write. */
  dumpActivities: boolean;
  /** CLIENT_CERTIFICATE_PATH (RFC HF §3.7): the certificate the bot's file
   *  token is acquired with, for a file link without tempauth. */
  fileCertificate?: Omit<CertificateTokenOptions, "loginEndpoint" | "fetch" | "now">;
  /** The Graph fallback for files Teams does not send (RFC HF). */
  graphFiles: GraphFilesConfig;
}

/** How the bridge reaches a file Teams sent no file.download.info for
 *  (RFC HF §3.1). Read only with BRIDGE_FILES=1; off otherwise. */
export interface GraphFilesConfig {
  /** TEAMS_BRIDGE_FILES_GRAPH: off (the default), app (the bot's own
   *  tenant-wide Graph token) or delegated (each person signs in once, and
   *  a lookup uses their own token). */
  mode: "off" | "app" | "delegated";
  /** The Teams app's manifest id (TEAMS_BRIDGE_APP_ID, default CLIENT_ID,
   *  which the shipped manifest uses as its id), lower-cased: how the bridge
   *  finds the person's chat with the bot in Graph. Set with Graph on. */
  appId?: string;
  /** TEAMS_BRIDGE_GRAPH_CONNECTION: the OAuth connection setting on the
   *  Azure Bot a person signs in through. Set in delegated mode. */
  connection?: string;
  /** TEAMS_BRIDGE_NO_FILE_HINT: what the bridge adds to its reply when a
   *  message looked like a file and none came (RFC HF §3.8). */
  noFileHint?: string;
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
  const dumpActivities = r.flag("TEAMS_BRIDGE_DUMP_ACTIVITIES");
  const graphFiles = loadGraphFiles(r, shared.files?.enabled === true);
  const fileCertificate = loadFileCertificate(r);
  r.check("teams-bridge");
  return { shared, allowedTenants, fileHosts, dumpActivities, graphFiles, ...(fileCertificate ? { fileCertificate } : {}) };
}

/** loadFileCertificate reads CLIENT_CERTIFICATE_PATH (RFC HF §3.7): a PEM
 *  file with the bot's certificate and its unencrypted RSA private key. The
 *  token request names the bot's app (CLIENT_ID) and its tenant (TENANT_ID),
 *  the names the Teams SDK reads too. A problem names the file, never its
 *  content. */
function loadFileCertificate(r: EnvReader): TeamsConfig["fileCertificate"] {
  const path = r.get("CLIENT_CERTIFICATE_PATH");
  if (path === undefined) return undefined;
  const clientId = r.get("CLIENT_ID");
  const tenantId = r.get("TENANT_ID");
  if (!clientId) r.problems.push("CLIENT_CERTIFICATE_PATH needs CLIENT_ID, the bot's app id");
  if (!tenantId) r.problems.push("CLIENT_CERTIFICATE_PATH needs TENANT_ID, the tenant the bot's app is registered in");
  let pem: string;
  try {
    pem = readFileSync(path, "utf8");
  } catch {
    r.problems.push(`CLIENT_CERTIFICATE_PATH: cannot read ${path}`);
    return undefined;
  }
  try {
    const certificate = parseCertificate(pem);
    return clientId && tenantId ? { clientId, tenantId, certificate } : undefined;
  } catch (err) {
    r.problems.push(`CLIENT_CERTIFICATE_PATH: ${path} ${(err as Error).message}`);
    return undefined;
  }
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

const GUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/** loadGraphFiles reads the RFC HF settings. With files off they are not
 *  read at all, so a stray value cannot turn Graph on. The app id goes into
 *  a Graph filter, so only a GUID (what Teams requires of a manifest id) is
 *  taken. */
function loadGraphFiles(r: EnvReader, filesOn: boolean): GraphFilesConfig {
  if (!filesOn) return { mode: "off" };
  const hint = r.get("TEAMS_BRIDGE_NO_FILE_HINT")?.trim();
  const noFileHint = hint ? { noFileHint: hint } : {};
  const raw = r.get("TEAMS_BRIDGE_FILES_GRAPH")?.trim().toLowerCase();
  if (!raw || raw === "off") return { mode: "off", ...noFileHint };
  if (raw !== "app" && raw !== "delegated") {
    r.problems.push('TEAMS_BRIDGE_FILES_GRAPH must be one of "off", "app", "delegated"');
    return { mode: "off", ...noFileHint };
  }
  const appId = (r.get("TEAMS_BRIDGE_APP_ID") ?? r.get("CLIENT_ID") ?? "").trim().toLowerCase();
  if (!appId) r.problems.push(`TEAMS_BRIDGE_FILES_GRAPH=${raw} needs TEAMS_BRIDGE_APP_ID (the Teams app's manifest id) or CLIENT_ID`);
  else if (!GUID.test(appId)) r.problems.push("TEAMS_BRIDGE_APP_ID must be the Teams app's manifest id, a GUID");
  if (raw === "app") return { mode: "app", appId, ...noFileHint };
  // Without a connection there is nothing to sign in through and every
  // lookup would fail, so the bridge refuses to start rather than run so.
  const connection = r.get("TEAMS_BRIDGE_GRAPH_CONNECTION")?.trim();
  if (!connection) r.problems.push("TEAMS_BRIDGE_FILES_GRAPH=delegated needs TEAMS_BRIDGE_GRAPH_CONNECTION, the OAuth connection setting on the Azure Bot");
  return { mode: "delegated", appId, connection, ...noFileHint };
}
