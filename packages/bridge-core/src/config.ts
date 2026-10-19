// The configuration every bridge shares, read from the environment.
//
// A bridge package reads its own platform's keys with the same helpers, so a
// refusal lists every problem at once, and the bridge refuses to start rather
// than run half-configured.

export type AccessMode = "isolated" | "tenant";

export interface SharedConfig {
  /** loomcycle's base URL, reachable only from the bridge (private network). */
  loomcycleBaseUrl: string;
  /** A substrate:tenant token, used only to register users and mint their tokens. */
  loomcycleToken: string;
  /** The loomcycle agent every conversation runs. */
  agent: string;
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
  /** Local testing only: the platform's request authentication is off. */
  dev: boolean;
}

export class ConfigError extends Error {
  constructor(
    readonly bridge: string,
    readonly problems: string[],
  ) {
    super(`${bridge} configuration: ${problems.join("; ")}`);
    this.name = "ConfigError";
  }
}

export type Env = Record<string, string | undefined>;

/** EnvReader collects problems instead of throwing on the first, so a bridge
 *  reports every missing or malformed variable in one go. */
export class EnvReader {
  readonly problems: string[] = [];

  constructor(private readonly env: Env) {}

  /** The trimmed value, or undefined when unset or blank. */
  get(name: string): string | undefined {
    const v = this.env[name]?.trim();
    return v ? v : undefined;
  }

  required(name: string, hint?: string): string {
    const v = this.get(name);
    if (!v) this.problems.push(hint ? `${name} is required (${hint})` : `${name} is required`);
    return v ?? "";
  }

  int(name: string, def: number, min: number, max = Number.MAX_SAFE_INTEGER): number {
    const raw = this.get(name);
    if (!raw) return def;
    const n = Number(raw);
    if (!Number.isInteger(n) || n < min || n > max) {
      this.problems.push(max === Number.MAX_SAFE_INTEGER ? `${name} must be an integer >= ${min}` : `${name} must be an integer from ${min} to ${max}`);
      return def;
    }
    return n;
  }

  /** "1" is on; unset is off; anything else is a problem, so a typo never
   *  silently means "off". */
  flag(name: string): boolean {
    const raw = this.get(name);
    if (raw === undefined) return false;
    if (raw === "1") return true;
    if (raw === "0") return false;
    this.problems.push(`${name} must be 1 or 0`);
    return false;
  }

  /** A comma list, trimmed, blanks dropped; lower-cased when asked. */
  list(name: string, opts: { lower?: boolean } = {}): string[] {
    const raw = this.get(name);
    if (!raw) return [];
    return raw
      .split(",")
      .map((s) => (opts.lower ? s.trim().toLowerCase() : s.trim()))
      .filter(Boolean);
  }

  oneOf<T extends string>(name: string, allowed: readonly T[], def: T): T {
    const raw = this.get(name);
    if (!raw) return def;
    if (!(allowed as readonly string[]).includes(raw)) {
      this.problems.push(`${name} must be one of ${allowed.map((a) => `"${a}"`).join(", ")}`);
      return def;
    }
    return raw as T;
  }

  /** Throws a ConfigError naming every problem collected so far. */
  check(bridge: string): void {
    if (this.problems.length > 0) throw new ConfigError(bridge, [...this.problems]);
  }
}

/** loadSharedConfig reads the `BRIDGE_*` surface and loomcycle's two keys.
 *  It does not throw: call `reader.check()` once the platform's own keys are
 *  read too, so every problem is reported together. */
export function loadSharedConfig(reader: EnvReader, defaults: { port: number }): SharedConfig {
  const loomcycleBaseUrl = reader.required("LOOMCYCLE_BASE_URL").replace(/\/+$/, "");
  const loomcycleToken = reader.required("LOOMCYCLE_TOKEN");
  const agent = reader.required("BRIDGE_AGENT");
  const userAccessMode = reader.oneOf("BRIDGE_USER_ACCESS_MODE", ["isolated", "tenant"] as const, "isolated");
  const store = reader.get("BRIDGE_STORE") ?? "memory";
  if (store !== "memory" && !(store.startsWith("file:") && store.length > "file:".length)) {
    reader.problems.push('BRIDGE_STORE must be "memory" or "file:<path>"');
  }
  return {
    loomcycleBaseUrl,
    loomcycleToken,
    agent,
    tokenTtlSeconds: reader.int("BRIDGE_TOKEN_TTL_SECONDS", 43200, 900),
    ratePerMinute: reader.int("BRIDGE_RATE_PER_MINUTE", 6, 1),
    rateBurst: reader.int("BRIDGE_RATE_BURST", 10, 1),
    userAccessMode,
    idleMinutes: reader.int("BRIDGE_IDLE_MINUTES", 1440, 1),
    store,
    port: reader.int("PORT", defaults.port, 1, 65535),
    dev: reader.flag("BRIDGE_DEV"),
  };
}
