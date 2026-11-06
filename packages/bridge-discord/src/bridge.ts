// The Discord adapter on the shared core: interactions over HTTP, no Gateway.

import { consoleLogger, createBridge, type Bridge, type BridgeAdapter, type BridgeOptions } from "@loomcycle/bridge-core";

import { DiscordApi } from "./api.js";
import { helpText, registerCommands } from "./commands.js";
import type { DiscordConfig } from "./config.js";
import { EVENTS_PATH, eventsRoute } from "./events.js";
import { GroupLookup } from "./groups.js";
import { interactionRoute } from "./interactions.js";
import { DiscordPort, type DiscordRef } from "./port.js";
import { publicKeyFromHex } from "./verify.js";
import { DiscordVoice } from "./voice.js";

export const MAX_MESSAGE_CHARS = 2000;

export interface DiscordBridgeOptions extends BridgeOptions<DiscordRef> {
  /** Override how Discord's API and its attachment CDN are reached (tests, a proxy). */
  fetch?: typeof fetch;
  sleep?: (ms: number) => Promise<void>;
  /** How long a button press may take before it is acked and finished later. */
  inlineCardMs?: number;
}

export function createDiscordBridge(cfg: DiscordConfig, opts: DiscordBridgeOptions = {}): Bridge<DiscordRef> {
  const log = opts.logger ?? consoleLogger;
  const now = opts.now ?? Date.now;
  const api = new DiscordApi({
    baseUrl: cfg.apiBaseUrl,
    applicationId: cfg.applicationId,
    botToken: cfg.botToken,
    log,
    fetch: opts.fetch,
    sleep: opts.sleep,
  });
  const port = new DiscordPort(api, log, { now });
  const key = publicKeyFromHex(cfg.publicKey);
  const fences = {
    applicationId: cfg.applicationId,
    allowedGuilds: cfg.allowedGuilds,
    allowDms: cfg.allowDms,
    trustedGuilds: cfg.trustedGuilds,
    dev: cfg.dev,
    ...(cfg.gate ? { gate: cfg.gate.name } : {}),
  };
  // Voice messages (RFC GA §5.3), with BRIDGE_VOICE=1 only.
  const voice = cfg.voice?.enabled ? new DiscordVoice(api, port, { fetch: opts.fetch }) : undefined;
  const groups = cfg.gate ? new GroupLookup({ api, gate: cfg.gate.name, trustedGuilds: cfg.trustedGuilds, now }) : undefined;

  const adapter: BridgeAdapter<DiscordRef> = {
    // `/ask prompt:/help` reaches the core as "/help": answer with the same
    // list as the /help command, not the core's typed-command one.
    // An "Ask by voice" is a deferred interaction: it must be answered even
    // in a server channel, where the core is otherwise quiet about a voice
    // message it does not take.
    platform: { name: "discord", label: "Discord", helpText: helpText(voice !== undefined), answerEveryVoice: true },
    // Without the Gateway there is no message to edit as it grows, and each
    // turn is one message per 2,000 characters.
    capabilities: { streaming: "never", maxMessageChars: MAX_MESSAGE_CHARS },
    port,
    async start(ctx) {
      const gate = ctx.gate;
      await ctx.serve({
        "POST /discord/interactions": interactionRoute({
          cfg: fences,
          key,
          port,
          api,
          router: ctx.router,
          log: ctx.log,
          now,
          inlineCardMs: opts.inlineCardMs,
          ...(voice ? { voice: { port: voice, untrusted: cfg.voice?.untrusted ?? true } } : {}),
        }),
        // Webhook Events only for a gate, which is what tells loomcycle of a
        // person who removed the app.
        ...(gate ? { [`POST ${EVENTS_PATH}`]: eventsRoute({ key, gate, forget: (s) => groups?.forget(s), log: ctx.log, now }) } : {}),
      });
      if (cfg.registerCommands) await registerCommands(api, cfg, ctx.log);
    },
    async stop() {
      port.stop();
    },
    ...(groups ? { groupsFor: (subject: string, ref: DiscordRef) => groups.groupsFor(subject, ref) } : {}),
    ...(voice ? { voice } : {}),
  };
  return createBridge(cfg, adapter, { ...opts, logger: log });
}
