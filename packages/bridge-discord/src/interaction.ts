// The parts of a Discord interaction the bridge reads. Everything is
// optional on purpose: the body is signed by Discord, but its shape is still
// checked before use.

export const InteractionType = {
  PING: 1,
  APPLICATION_COMMAND: 2,
  MESSAGE_COMPONENT: 3,
} as const;

export const CallbackType = {
  PONG: 1,
  CHANNEL_MESSAGE_WITH_SOURCE: 4,
  DEFERRED_CHANNEL_MESSAGE_WITH_SOURCE: 5,
  DEFERRED_UPDATE_MESSAGE: 6,
  UPDATE_MESSAGE: 7,
} as const;

/** Interaction context types: where a command was used. */
export const Context = { GUILD: 0, BOT_DM: 1, PRIVATE_CHANNEL: 2 } as const;

export const EPHEMERAL = 1 << 6;

export interface DiscordUser {
  id?: string;
  username?: string;
  global_name?: string | null;
  bot?: boolean;
}

export interface CommandOption {
  name?: string;
  type?: number;
  value?: unknown;
}

export interface Interaction {
  id?: string;
  application_id?: string;
  type?: number;
  token?: string;
  guild_id?: string;
  channel_id?: string;
  channel?: { id?: string; type?: number; parent_id?: string | null };
  member?: { user?: DiscordUser; nick?: string | null };
  user?: DiscordUser;
  context?: number;
  data?: {
    name?: string;
    options?: CommandOption[];
    custom_id?: string;
  };
}
