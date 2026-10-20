// Slack request signing: X-Slack-Signature is "v0=" + hex HMAC-SHA256 of
// "v0:<X-Slack-Request-Timestamp>:<raw body>" under the app's signing secret.
// The timestamp must be within five minutes of now, or a captured request
// could be replayed forever.

import { createHmac } from "node:crypto";

import { header, safeEqual, type RawRequest } from "@loomcycle/bridge-core";

/** Slack's replay window. */
export const MAX_SKEW_SECONDS = 5 * 60;

/** slackSignature computes the v0 signature of a raw body. */
export function slackSignature(secret: string, timestamp: string, body: Buffer | string): string {
  const mac = createHmac("sha256", secret);
  mac.update(`v0:${timestamp}:`, "utf8");
  mac.update(body);
  return `v0=${mac.digest("hex")}`;
}

/** verifySlackRequest checks a request's signature and timestamp against the
 *  raw bytes Slack sent. Anything missing, malformed, stale or wrong is false. */
export function verifySlackRequest(req: RawRequest, secret: string, nowMs: number): boolean {
  const ts = header(req, "x-slack-request-timestamp");
  const sig = header(req, "x-slack-signature");
  if (!ts || !sig || !/^\d{1,12}$/.test(ts)) return false;
  if (Math.abs(nowMs / 1000 - Number(ts)) > MAX_SKEW_SECONDS) return false;
  return safeEqual(sig, slackSignature(secret, ts, req.body));
}
