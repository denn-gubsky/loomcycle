// The configuration every bridge shares, read from the environment.
//
// A bridge package reads its own platform's keys with the same helpers, so a
// refusal lists every problem at once, and the bridge refuses to start rather
// than run half-configured.

import { trimTrailingSlashes } from "./text.js";

export type AccessMode = "isolated" | "tenant";

export interface SharedConfig {
  /** loomcycle's base URL, reachable only from the bridge (private network). */
  loomcycleBaseUrl: string;
  /** A substrate:tenant token, used only to register users and mint their
   *  tokens. "" for a gate bridge, which holds no loomcycle token. */
  loomcycleToken: string;
  /** Set when the bridge is an external gate of loomcycle (RFC FA §3.5):
   *  loomcycle provisions its users and issues their tokens. */
  gate?: GateConfig;
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

export interface GateConfig {
  /** The gate's name in loomcycle's auth.gates; subjects are "<name>:<id>". */
  name: string;
  /** loomcycle's issuer URL (its LOOMCYCLE_PUBLIC_URL): every assertion's aud. */
  audience: string;
  /** Where the signing key is: the BRIDGE_GATE_KEY / BRIDGE_GATE_SECRET /
   *  BRIDGE_GATE_KEY_ID values, read once at start by loadGateKey. */
  keyEnv: Record<string, string | undefined>;
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
export function loadSharedConfig(reader: EnvReader, defaults: { port: number; gate?: boolean }): SharedConfig {
  const loomcycleBaseUrl = trimTrailingSlashes(reader.required("LOOMCYCLE_BASE_URL"));
  const gate = defaults.gate ? loadGateConfig(reader) : undefined;
  // A gate bridge holds no loomcycle credential at all: a leftover operator
  // token beside the gate key is refused, so it cannot stay deployed unused.
  const loomcycleToken = gate ? "" : reader.required("LOOMCYCLE_TOKEN");
  if (gate && reader.get("LOOMCYCLE_TOKEN")) {
    reader.problems.push("LOOMCYCLE_TOKEN must not be set: this bridge is a loomcycle gate, which issues its users' tokens; remove the token");
  }
  const agent = reader.required("BRIDGE_AGENT");
  if (gate && reader.get("BRIDGE_USER_ACCESS_MODE")) {
    reader.problems.push("BRIDGE_USER_ACCESS_MODE does not apply to a gate bridge: set new_user.access_mode in the gate's identity_map in loomcycle");
  }
  const userAccessMode = reader.oneOf("BRIDGE_USER_ACCESS_MODE", ["isolated", "tenant"] as const, "isolated");
  const store = reader.get("BRIDGE_STORE") ?? "memory";
  if (store !== "memory" && !(store.startsWith("file:") && store.length > "file:".length)) {
    reader.problems.push('BRIDGE_STORE must be "memory" or "file:<path>"');
  }
  return {
    loomcycleBaseUrl,
    loomcycleToken,
    ...(gate ? { gate } : {}),
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

const GATE_NAME = /^[a-z0-9][a-z0-9_-]{0,31}$/;

/** loadGateConfig reads BRIDGE_GATE (the gate's name in loomcycle),
 *  BRIDGE_GATE_AUDIENCE (loomcycle's LOOMCYCLE_PUBLIC_URL) and checks a key is
 *  named; the key itself is read at start. */
function loadGateConfig(reader: EnvReader): GateConfig {
  const name = reader.required("BRIDGE_GATE", "the gate's name in loomcycle's auth.gates");
  if (name && !GATE_NAME.test(name)) reader.problems.push("BRIDGE_GATE must be a loomcycle gate name (lowercase letters, digits, - and _)");
  const audience = trimTrailingSlashes(reader.required("BRIDGE_GATE_AUDIENCE", "loomcycle's LOOMCYCLE_PUBLIC_URL"));
  if (audience) {
    let ok = false;
    try {
      const u = new URL(audience);
      ok = u.protocol === "https:" || (u.protocol === "http:" && ["localhost", "127.0.0.1", "[::1]"].includes(u.hostname));
    } catch {
      ok = false;
    }
    if (!ok) reader.problems.push("BRIDGE_GATE_AUDIENCE must be an https:// URL (http:// only to localhost)");
  }
  const keyEnv = {
    BRIDGE_GATE_KEY: reader.get("BRIDGE_GATE_KEY"),
    BRIDGE_GATE_SECRET: reader.get("BRIDGE_GATE_SECRET"),
    BRIDGE_GATE_KEY_ID: reader.get("BRIDGE_GATE_KEY_ID"),
  };
  if (!keyEnv.BRIDGE_GATE_KEY && !keyEnv.BRIDGE_GATE_SECRET) {
    reader.problems.push("BRIDGE_GATE_KEY (or BRIDGE_GATE_SECRET) is required: the key loomcycle pinned for this gate");
  }
  if (keyEnv.BRIDGE_GATE_KEY && keyEnv.BRIDGE_GATE_SECRET) reader.problems.push("set BRIDGE_GATE_KEY or BRIDGE_GATE_SECRET, not both");
  return { name, audience, keyEnv };
}
