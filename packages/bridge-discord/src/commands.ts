// The slash commands, and registering them.
//
// In each allowed server they are registered as that server's commands: those
// take effect at once and show only where the bot answers. Commands in DMs
// can only be global, so with DMs on they are also registered globally,
// limited to the bot's DMs, so a server never lists them twice.

import { VOICE_HELP_TEXT, type Logger } from "@loomcycle/bridge-core";

import type { CommandSpec, DiscordApi } from "./api.js";
import type { DiscordConfig } from "./config.js";
import { CommandType, Context } from "./interaction.js";

const GUILD_INSTALL = 0;
export const MAX_PROMPT_CHARS = 4000;
/** The message command that takes a voice message (RFC GA): right-click it,
 *  Apps, Ask by voice. */
export const ASK_BY_VOICE = "Ask by voice";

export const HELP_TEXT = [
  "Commands:",
  "- **/ask** `prompt`: talk to the agent. When it asks you a question without options, answer with /ask too.",
  "- **/new**: start a new conversation (the agent forgets this one)",
  "- **/stop**: stop the current answer",
  "- **/help**: show this list",
  "",
  "In a server, everyone in a channel shares one conversation with the agent. The person who started it answers the agent's questions and can /new or /stop it.",
].join("\n");

/** DISCORD_VOICE_HELP is added to the list with voice on. The core's /voice
 *  settings are typed messages, which on Discord go through /ask. */
export const DISCORD_VOICE_HELP = [
  "",
  "Voice: right-click your own voice message, then **Apps** → **Ask by voice**. The voice settings go through /ask, as in `/ask /voice on`:",
].join("\n");

/** helpText is the platform's /help list: the core adds its voice settings
 *  after it when voice is on. */
export function helpText(voice: boolean): string {
  return voice ? `${HELP_TEXT}\n${DISCORD_VOICE_HELP}` : HELP_TEXT;
}

/** slashHelpText answers the /help command, which the bridge answers itself. */
export function slashHelpText(voice: boolean): string {
  return voice ? `${helpText(true)}\n${VOICE_HELP_TEXT}` : HELP_TEXT;
}

export function commandSpecs(contexts: number[], opts: { voice?: boolean } = {}): CommandSpec[] {
  const base = { type: CommandType.CHAT_INPUT, contexts, integration_types: [GUILD_INSTALL] };
  const voice: CommandSpec[] = opts.voice ? [{ ...base, type: CommandType.MESSAGE, name: ASK_BY_VOICE, description: "" }] : [];
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
    ...voice,
  ];
}

/** registerCommands replaces the application's commands in every allowed
 *  server, and globally (DMs only) when DMs are on. The bridge owns the
 *  application's commands: anything else registered there is replaced. Ask
 *  by voice is registered only with voice on, so no menu offers it in vain. */
export async function registerCommands(
  api: DiscordApi,
  cfg: Pick<DiscordConfig, "allowedGuilds" | "allowDms"> & { voice?: { enabled: boolean } },
  log: Logger,
): Promise<void> {
  const opts = { voice: cfg.voice?.enabled === true };
  for (const guild of cfg.allowedGuilds) {
    await api.putCommands(commandSpecs([Context.GUILD], opts), guild);
    log.info("registered commands", { guild });
  }
  if (cfg.allowDms) {
    await api.putCommands(commandSpecs([Context.BOT_DM], opts));
    log.info("registered commands", { scope: "dm" });
  }
}
