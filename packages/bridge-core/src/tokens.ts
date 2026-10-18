// Per-user loomcycle tokens (RFC EN §4, generalised by RFC EQ).
//
// loomcycle makes a token's subject authoritative over a request's user_id, so
// one shared service token cannot act as many users. The bridge therefore
// registers each platform user and mints them a short-lived token that can only
// start the configured agent, interactively, at a bounded rate. Tokens live in
// this process's memory only; a restart mints fresh ones and `maxActive`
// retires the old.

import type { AccessMode } from "./config.js";
import type { AdminClient } from "./ports.js";

export interface TokenBrokerOptions {
  agent: string;
  /** The label every token this bridge mints carries: "<platform>-bridge".
   *  loomcycle stores it as `u-<name>`, and retires a user's oldest token of
   *  that name past `maxActive`. */
  tokenName: string;
  ttlSeconds: number;
  ratePerMinute: number;
  rateBurst: number;
  accessMode: AccessMode;
  /** Re-mint this long before a token expires. */
  renewBeforeMs?: number;
  now?: () => number;
}

interface Cached {
  token: string;
  expiresAt: number;
}

const DEFAULT_RENEW_BEFORE_MS = 10 * 60 * 1000;

export class TokenBroker {
  private readonly cache = new Map<string, Cached>();
  private readonly inflight = new Map<string, Promise<string>>();
  private readonly registered = new Set<string>();
  private readonly renewBeforeMs: number;
  private readonly now: () => number;

  constructor(
    private readonly admin: AdminClient,
    private readonly opts: TokenBrokerOptions,
  ) {
    this.renewBeforeMs = opts.renewBeforeMs ?? DEFAULT_RENEW_BEFORE_MS;
    this.now = opts.now ?? Date.now;
  }

  /** tokenFor returns a valid token for subject, minting one when needed.
   *  Concurrent calls for one subject share a single mint. */
  async tokenFor(subject: string, displayName?: string): Promise<string> {
    const hit = this.cache.get(subject);
    if (hit && hit.expiresAt - this.renewBeforeMs > this.now()) return hit.token;
    const pending = this.inflight.get(subject);
    if (pending) return pending;
    const p = this.mint(subject, displayName).finally(() => this.inflight.delete(subject));
    this.inflight.set(subject, p);
    return p;
  }

  /** forget drops a cached token, so the next call mints (after a 401). */
  forget(subject: string): void {
    this.cache.delete(subject);
  }

  private async mint(subject: string, displayName?: string): Promise<string> {
    if (!this.registered.has(subject)) {
      try {
        await this.admin.createUser({
          subject,
          display_name: displayName,
          access_mode: this.opts.accessMode,
        });
      } catch (err) {
        // Already registered: the expected case for every user after their
        // first message on any bridge instance.
        if (statusOf(err) !== 409) throw err;
      }
      this.registered.add(subject);
    }
    const minted = await this.admin.mintUserToken(subject, {
      ttlSeconds: this.opts.ttlSeconds,
      name: this.opts.tokenName,
      // Two, not one: a token minted by a restarted bridge must not cut off
      // the stream an older process may still be draining.
      maxActive: 2,
      agents: [this.opts.agent],
      // `interactive` is itself a lockable override, and the bridge must set
      // it; every other per-run setting stays the agent definition's.
      overrides: ["interactive"],
      rate: { perMinute: this.opts.ratePerMinute, burst: this.opts.rateBurst },
    });
    const parsed = minted.expires_at ? Date.parse(minted.expires_at) : NaN;
    const expiresAt = Number.isFinite(parsed) ? parsed : this.now() + this.opts.ttlSeconds * 1000;
    this.cache.set(subject, { token: minted.token, expiresAt });
    return minted.token;
  }
}

export function statusOf(err: unknown): number | undefined {
  const s = (err as { status?: unknown } | null)?.status;
  return typeof s === "number" ? s : undefined;
}
