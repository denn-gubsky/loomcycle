// The Discord bridge's configuration: the shared BRIDGE_* surface plus the
// Discord application's keys and fences, read in one pass so a refusal lists
// every problem together.

import { EnvReader, loadSharedConfig, type Env, type SharedConfig } from "@loomcycle/bridge-core";

export interface DiscordConfig extends SharedConfig {
  applicationId: string;
  /** The application's Ed25519 public key, 32 bytes as hex. */
  publicKey: string;
  /** Sent only to Discord's REST API, never logged. */
  botToken: string;
  /** Servers the bot answers in. Every other server is refused. */
  allowedGuilds: Set<string>;
  /** Anyone who can reach the app may use it in a DM with the bot. */
  allowDms: boolean;
  /** Allowed servers whose speakers are trusted. Elsewhere in a server they are not. */
  trustedGuilds: Set<string>;
  /** Register the slash commands when the bridge starts. */
  registerCommands: boolean;
  /** Discord's REST base, without a trailing slash. */
  apiBaseUrl: string;
}

export const DEFAULT_PORT = 8080;
export const DEFAULT_API_BASE_URL = "https://discord.com/api/v10";

const SNOWFLAKE = /^\d{1,20}$/;

export function isSnowflake(v: unknown): v is string {
  return typeof v === "string" && SNOWFLAKE.test(v);
}

/** The bot token is sent to this URL, so anything but HTTPS is allowed only
 *  to this machine (a local fake of the API, for testing). */
function apiBaseUrl(reader: EnvReader): string {
  const raw = reader.get("DISCORD_BRIDGE_API_BASE_URL") ?? DEFAULT_API_BASE_URL;
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    reader.problems.push("DISCORD_BRIDGE_API_BASE_URL must be a URL");
    return DEFAULT_API_BASE_URL;
  }
  const local = ["localhost", "127.0.0.1", "[::1]"].includes(url.hostname);
  if (url.protocol !== "https:" && !(url.protocol === "http:" && local)) {
    reader.problems.push("DISCORD_BRIDGE_API_BASE_URL must be https (http only to localhost)");
  }
  return raw.replace(/\/+$/, "");
}

function guilds(reader: EnvReader, name: string): Set<string> {
  const ids = reader.list(name);
  for (const id of ids) {
    if (!isSnowflake(id)) {
      reader.problems.push(`${name} must be a comma list of server (guild) ids`);
      break;
    }
  }
  return new Set(ids);
}

export function loadConfig(env: Env = process.env): DiscordConfig {
  const reader = new EnvReader(env);
  // A gate only when BRIDGE_GATE names one: a bridge deployed with a
  // loomcycle token keeps running as it was until it is reconfigured.
  const shared = loadSharedConfig(reader, { port: DEFAULT_PORT, gate: reader.get("BRIDGE_GATE") !== undefined });

  const applicationId = reader.required("DISCORD_APPLICATION_ID");
  if (applicationId && !isSnowflake(applicationId)) reader.problems.push("DISCORD_APPLICATION_ID must be the application's numeric id");
  const publicKey = reader.required("DISCORD_PUBLIC_KEY", "the application's public key, from the Developer Portal").toLowerCase();
  if (publicKey && !/^[0-9a-f]{64}$/.test(publicKey)) reader.problems.push("DISCORD_PUBLIC_KEY must be 64 hex characters");
  const botToken = reader.required("DISCORD_BOT_TOKEN");

  const allowedGuilds = guilds(reader, "DISCORD_BRIDGE_ALLOWED_GUILDS");
  const allowDms = reader.flag("DISCORD_BRIDGE_ALLOW_DMS");
  // Closed by default: the app's endpoint is public, and every message it
  // takes runs on the operator's model keys.
  if (allowedGuilds.size === 0 && !allowDms) {
    reader.problems.push("set DISCORD_BRIDGE_ALLOWED_GUILDS, DISCORD_BRIDGE_ALLOW_DMS=1, or both: the bridge is closed by default");
  }
  const trustedGuilds = guilds(reader, "DISCORD_BRIDGE_TRUSTED_GUILDS");
  for (const g of trustedGuilds) {
    if (!allowedGuilds.has(g)) {
      reader.problems.push("every server in DISCORD_BRIDGE_TRUSTED_GUILDS must also be in DISCORD_BRIDGE_ALLOWED_GUILDS");
      break;
    }
  }

  const cfg: DiscordConfig = {
    ...shared,
    applicationId,
    publicKey,
    botToken,
    allowedGuilds,
    allowDms,
    trustedGuilds,
    registerCommands: reader.flag("DISCORD_BRIDGE_REGISTER_COMMANDS"),
    apiBaseUrl: apiBaseUrl(reader),
  };
  reader.check("discord-bridge");
  return cfg;
}
