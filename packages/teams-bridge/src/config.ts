// Bridge configuration, read from the environment (RFC EN §8).
//
// The Teams SDK reads its own credentials (CLIENT_ID, CLIENT_SECRET, TENANT_ID)
// from the environment; this module validates only what the bridge itself
// needs, and refuses to start rather than run half-configured.

export type AccessMode = "isolated" | "tenant";

export interface BridgeConfig {
  /** loomcycle's base URL, reachable only from the bridge (private network). */
  loomcycleBaseUrl: string;
  /** A substrate:tenant token, used only to register users and mint their tokens. */
  loomcycleToken: string;
  /** The loomcycle agent every conversation runs. */
  agent: string;
  /** Entra tenant ids allowed to talk to the bot, lower-cased. */
  allowedTenants: ReadonlySet<string>;
  /** Lifetime of a minted per-user token, in seconds. */
  tokenTtlSeconds: number;
  /** Per-user run-start rate on the minted token. */
  ratePerMinute: number;
  rateBurst: number;
  /** Access mode for users the bridge registers. */
  userAccessMode: AccessMode;
  /** A conversation's run is cancelled after this long without a message. */
  idleMinutes: number;
  /** "memory" or "file:<path>". */
  store: string;
  /** Listen port. */
  port: number;
  /** Permits the Teams SDK's unauthenticated mode, for local testing only. */
  dev: boolean;
}

export class ConfigError extends Error {
  constructor(readonly problems: string[]) {
    super(`teams-bridge configuration: ${problems.join("; ")}`);
    this.name = "ConfigError";
  }
}

type Env = Record<string, string | undefined>;

export function loadConfig(env: Env = process.env): BridgeConfig {
  const problems: string[] = [];
  const required = (name: string): string => {
    const v = env[name]?.trim();
    if (!v) problems.push(`${name} is required`);
    return v ?? "";
  };
  const int = (name: string, def: number, min: number): number => {
    const raw = env[name]?.trim();
    if (!raw) return def;
    const n = Number(raw);
    if (!Number.isInteger(n) || n < min) {
      problems.push(`${name} must be an integer >= ${min}`);
      return def;
    }
    return n;
  };

  const loomcycleBaseUrl = required("LOOMCYCLE_BASE_URL").replace(/\/+$/, "");
  const loomcycleToken = required("LOOMCYCLE_TOKEN");
  const agent = required("TEAMS_BRIDGE_AGENT");
  const tenantsRaw = required("TEAMS_BRIDGE_ALLOWED_TENANTS");
  const allowedTenants = new Set(
    tenantsRaw
      .split(",")
      .map((t) => t.trim().toLowerCase())
      .filter(Boolean),
  );
  if (tenantsRaw && allowedTenants.size === 0) {
    problems.push("TEAMS_BRIDGE_ALLOWED_TENANTS lists no tenant");
  }

  const mode = (env.TEAMS_BRIDGE_USER_ACCESS_MODE?.trim() || "isolated") as AccessMode;
  if (mode !== "isolated" && mode !== "tenant") {
    problems.push('TEAMS_BRIDGE_USER_ACCESS_MODE must be "isolated" or "tenant"');
  }

  const store = env.TEAMS_BRIDGE_STORE?.trim() || "memory";
  if (store !== "memory" && !(store.startsWith("file:") && store.length > "file:".length)) {
    problems.push('TEAMS_BRIDGE_STORE must be "memory" or "file:<path>"');
  }

  const dev = env.TEAMS_BRIDGE_DEV?.trim() === "1";
  if (!dev && !env.CLIENT_ID?.trim()) {
    // Without credentials the Teams SDK can neither validate inbound requests
    // against our bot id nor send replies.
    problems.push("CLIENT_ID is required (or set TEAMS_BRIDGE_DEV=1 for local testing)");
  }

  const cfg: BridgeConfig = {
    loomcycleBaseUrl,
    loomcycleToken,
    agent,
    allowedTenants,
    tokenTtlSeconds: int("TEAMS_BRIDGE_TOKEN_TTL_SECONDS", 43200, 900),
    ratePerMinute: int("TEAMS_BRIDGE_RATE_PER_MINUTE", 6, 1),
    rateBurst: int("TEAMS_BRIDGE_RATE_BURST", 10, 1),
    userAccessMode: mode,
    idleMinutes: int("TEAMS_BRIDGE_IDLE_MINUTES", 1440, 1),
    store,
    port: int("PORT", 3978, 1),
    dev,
  };
  if (problems.length > 0) throw new ConfigError(problems);
  return cfg;
}
