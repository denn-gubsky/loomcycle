// The slash commands, and registering them.
//
// In each allowed server they are registered as that server's commands: those
// take effect at once and show only where the bot answers. Commands in DMs
// can only be global, so with DMs on they are also registered globally,
// limited to the bot's DMs, so a server never lists them twice.

import type { Logger } from "@loomcycle/bridge-core";

import type { CommandSpec, DiscordApi } from "./api.js";
import type { DiscordConfig } from "./config.js";
import { Context } from "./interaction.js";

const GUILD_INSTALL = 0;
export const MAX_PROMPT_CHARS = 4000;

export const HELP_TEXT = [
  "Commands:",
  "- **/ask** `prompt`: talk to the agent. When it asks you a question without options, answer with /ask too.",
  "- **/new**: start a new conversation (the agent forgets this one)",
  "- **/stop**: stop the current answer",
  "- **/help**: show this list",
  "",
  "In a server, everyone in a channel shares one conversation with the agent. The person who started it answers the agent's questions and can /new or /stop it.",
].join("\n");

export function commandSpecs(contexts: number[]): CommandSpec[] {
  const base = { type: 1 as const, contexts, integration_types: [GUILD_INSTALL] };
  return [
    {
      ...base,
      name: "ask",
      description: "Talk to the agent",
      options: [{ type: 3, name: "prompt", description: "What to say to the agent", required: true, max_length: MAX_PROMPT_CHARS }],
    },
    { ...base, name: "new", description: "Start a new conversation with the agent" },
    { ...base, name: "stop", description: "Stop the agent's current answer" },
    { ...base, name: "help", description: "Show what the bot can do" },
  ];
}

/** registerCommands replaces the application's commands in every allowed
 *  server, and globally (DMs only) when DMs are on. The bridge owns the
 *  application's commands: anything else registered there is replaced. */
export async function registerCommands(api: DiscordApi, cfg: Pick<DiscordConfig, "allowedGuilds" | "allowDms">, log: Logger): Promise<void> {
  for (const guild of cfg.allowedGuilds) {
    await api.putCommands(commandSpecs([Context.GUILD]), guild);
    log.info("registered commands", { guild });
  }
  if (cfg.allowDms) {
    await api.putCommands(commandSpecs([Context.BOT_DM]));
    log.info("registered commands", { scope: "dm" });
  }
}
