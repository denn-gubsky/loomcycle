// Constant-time comparison for request authentication.

import { createHash, timingSafeEqual } from "node:crypto";

/** safeEqual compares two secrets (a shared token, a hex signature) in time
 *  that depends on neither value. Both are hashed first, so their lengths
 *  differing leaks nothing either; a missing value never matches. */
export function safeEqual(a: string | undefined, b: string | undefined): boolean {
  if (a === undefined || b === undefined) return false;
  const ha = createHash("sha256").update(a, "utf8").digest();
  const hb = createHash("sha256").update(b, "utf8").digest();
  return timingSafeEqual(ha, hb) && a.length === b.length;
}
