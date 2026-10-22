// The Discord adapter on the shared core: interactions over HTTP, no Gateway.

import { consoleLogger, createBridge, type Bridge, type BridgeAdapter, type BridgeOptions } from "@loomcycle/bridge-core";

import { DiscordApi } from "./api.js";
import { HELP_TEXT, registerCommands } from "./commands.js";
import type { DiscordConfig } from "./config.js";
import { interactionRoute } from "./interactions.js";
import { DiscordPort, type DiscordRef } from "./port.js";
import { publicKeyFromHex } from "./verify.js";

export const MAX_MESSAGE_CHARS = 2000;

export interface DiscordBridgeOptions extends BridgeOptions<DiscordRef> {
  /** Override how Discord's API is called (tests, a proxy). */
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

  const adapter: BridgeAdapter<DiscordRef> = {
    // `/ask prompt:/help` reaches the core as "/help": answer with the same
    // list as the /help command, not the core's typed-command one.
    platform: { name: "discord", label: "Discord", helpText: HELP_TEXT },
    // Without the Gateway there is no message to edit as it grows, and each
    // turn is one message per 2,000 characters.
    capabilities: { streaming: "never", maxMessageChars: MAX_MESSAGE_CHARS },
    port,
    async start(ctx) {
      await ctx.serve({
        "POST /discord/interactions": interactionRoute({ cfg, key, port, api, router: ctx.router, log: ctx.log, now, inlineCardMs: opts.inlineCardMs }),
      });
      if (cfg.registerCommands) await registerCommands(api, cfg, ctx.log);
    },
    async stop() {
      port.stop();
    },
  };
  return createBridge(cfg, adapter, { ...opts, logger: log });
}
