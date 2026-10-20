// Authenticating an interaction: Discord signs the timestamp header followed
// by the raw body with the application's Ed25519 key.
//
// This is a public-key check, so there is no secret to compare: node:crypto's
// verify decides. Discord publishes no freshness window; the bridge refuses a
// timestamp more than five minutes off, so a captured request cannot be
// replayed later (and the interaction-id dedup refuses it inside the window).

import { createPublicKey, verify, type KeyObject } from "node:crypto";

export const MAX_SKEW_SECONDS = 300;

/** publicKeyFromHex builds the application's key from the Developer Portal's
 *  hex form: 32 raw bytes, which a JWK carries as base64url. */
export function publicKeyFromHex(hex: string): KeyObject {
  if (!/^[0-9a-f]{64}$/i.test(hex)) throw new Error("public key must be 64 hex characters");
  const x = Buffer.from(hex, "hex").toString("base64url");
  return createPublicKey({ key: { kty: "OKP", crv: "Ed25519", x }, format: "jwk" });
}

export function verifyInteraction(
  key: KeyObject,
  signature: string | undefined,
  timestamp: string | undefined,
  body: Buffer,
  nowMs: number,
): boolean {
  if (!signature || !timestamp) return false;
  if (!/^[0-9a-f]{128}$/i.test(signature) || !/^\d{1,12}$/.test(timestamp)) return false;
  if (Math.abs(nowMs / 1000 - Number(timestamp)) > MAX_SKEW_SECONDS) return false;
  try {
    return verify(null, Buffer.concat([Buffer.from(timestamp, "utf8"), body]), key, Buffer.from(signature, "hex"));
  } catch {
    return false;
  }
}
