// Inbound authentication and redelivery.
//
// Telegram signs nothing: it sends the secret_token given to setWebhook in a
// header on every update, and that header is the whole check. It is compared
// in constant time, and a request without it never reaches the parser.

import { header, safeEqual, type RawRequest } from "@loomcycle/bridge-core";

export const SECRET_HEADER = "x-telegram-bot-api-secret-token";

/** verifySecret reports whether the request carries the webhook's secret. A
 *  repeated header arrives joined ("a, b"), so it never matches. */
export function verifySecret(req: RawRequest, secret: string): boolean {
  if (!secret) return false;
  return safeEqual(header(req, SECRET_HEADER), secret);
}

/** SeenSet remembers recent update ids, so an update Telegram redelivers (it
 *  retries until it gets a 2xx) is handled once. Bounded in size and age:
 *  forgetting an old id costs at most one repeated answer. */
export class SeenSet {
  private readonly seen = new Map<number, number>();

  constructor(
    private readonly max = 10_000,
    private readonly ttlMs = 60 * 60 * 1000,
    private readonly now: () => number = Date.now,
  ) {}

  /** firstTime records id and reports whether it is new. */
  firstTime(id: number): boolean {
    const t = this.now();
    // Oldest first: insertion order is arrival order.
    for (const [k, at] of this.seen) {
      if (t - at < this.ttlMs && this.seen.size < this.max) break;
      this.seen.delete(k);
    }
    if (this.seen.has(id)) return false;
    this.seen.set(id, t);
    return true;
  }

  /** forget drops id, so a redelivery of an update that failed is handled. */
  forget(id: number): void {
    this.seen.delete(id);
  }
}
