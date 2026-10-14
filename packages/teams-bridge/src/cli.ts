#!/usr/bin/env node
// loomcycle-teams-bridge: run the bridge from environment variables (RFC EN §8).

import { createTeamsBridge } from "./bridge.js";
import { ConfigError, loadConfig } from "./config.js";

async function main(): Promise<void> {
  let cfg;
  try {
    cfg = loadConfig();
  } catch (err) {
    if (err instanceof ConfigError) {
      for (const p of err.problems) console.error(`teams-bridge: ${p}`);
      process.exit(2);
    }
    throw err;
  }
  const bridge = createTeamsBridge(cfg);
  const shutdown = (signal: string) => {
    console.error(`teams-bridge: ${signal}, stopping`);
    bridge.stop().then(
      () => process.exit(0),
      () => process.exit(1),
    );
  };
  process.once("SIGINT", () => shutdown("SIGINT"));
  process.once("SIGTERM", () => shutdown("SIGTERM"));
  await bridge.start();
}

main().catch((err: unknown) => {
  console.error("teams-bridge: failed to start:", (err as Error)?.message ?? err);
  process.exit(1);
});
