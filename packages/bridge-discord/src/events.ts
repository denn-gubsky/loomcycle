// POST /discord/events: Discord's Webhook Events, served only when the bridge
// is a loomcycle gate. The one event it acts on is APPLICATION_DEAUTHORIZED:
// a person removed the app, so the gate tells loomcycle they are gone
// (RFC FA §3.7c) and loomcycle disables them at once and removes them per the
// gate's policy.
//
// Discord signs these exactly as it signs interactions (Ed25519 over the
// timestamp header then the raw body, with the application's public key). It
// wants a 204 with no body within 3 seconds, so the revoke runs after the
// answer. A redelivered event revokes again, which changes nothing.

import { header, type Logger, type RawResponse, type Route } from "@loomcycle/bridge-core";
import type { KeyObject } from "node:crypto";

import { isSnowflake } from "./config.js";
import { personSub } from "./identity.js";
import { verifyInteraction } from "./verify.js";

export const EVENTS_PATH = "/discord/events";

/** The outer object's type: Discord's probe when the URL is saved, or an event. */
export const WebhookType = { PING: 0, EVENT: 1 } as const;

export const APPLICATION_DEAUTHORIZED = "APPLICATION_DEAUTHORIZED";

/** The parts of a webhook event the bridge reads; the body is signed by
 *  Discord, but its shape is still checked before use. */
export interface WebhookEvent {
  version?: number;
  application_id?: string;
  type?: number;
  event?: {
    type?: string;
    timestamp?: string;
    data?: { user?: { id?: string } };
  };
}

export interface EventDeps {
  key: KeyObject;
  /** The core's gate: names a platform id's subject, and revokes it. */
  gate: { subjectOf(sub: string): string; revoke(subject: string): Promise<void> };
  /** Called with the revoked subject, so cached groups go with it. */
  forget(subject: string): void;
  log: Logger;
  now: () => number;
}

// Discord asks for a Content-Type even on the empty answer.
const NO_CONTENT: RawResponse = { status: 204, headers: { "content-type": "application/json" } };

export function eventsRoute(deps: EventDeps): Route {
  return async (req) => {
    // Always verified, BRIDGE_DEV or not: what this route does is revoke.
    const ok = verifyInteraction(deps.key, header(req, "x-signature-ed25519"), header(req, "x-signature-timestamp"), req.body, deps.now());
    if (!ok) return { status: 401, body: "unauthorized" };
    let wh: WebhookEvent;
    try {
      wh = JSON.parse(req.body.toString("utf8")) as WebhookEvent;
    } catch {
      return { status: 400, body: "bad request" };
    }
    if (!wh || typeof wh !== "object") return { status: 400, body: "bad request" };
    if (wh.type !== WebhookType.EVENT || wh.event?.type !== APPLICATION_DEAUTHORIZED) return NO_CONTENT;
    const userId = wh.event.data?.user?.id;
    if (!isSnowflake(userId)) return NO_CONTENT;
    const subject = deps.gate.subjectOf(personSub(userId));
    deps.forget(subject);
    setImmediate(() => {
      deps.gate.revoke(subject).then(
        () => deps.log.info("discord user removed the app; revoked in loomcycle", { user: userId }),
        (err: unknown) => deps.log.warn("discord revoke failed", { status: (err as { status?: number })?.status }),
      );
    });
    return NO_CONTENT;
  };
}
