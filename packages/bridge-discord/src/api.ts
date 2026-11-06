// Discord's REST API, the few calls the bridge makes, over fetch.
//
// Two kinds of credential: the interaction webhook routes are authorised by
// the interaction token in their path and get no Authorization header; the
// channel routes and command registration use the bot token. Neither is ever
// put in a log line or an error: an error names the route's shape only.

import type { Logger } from "@loomcycle/bridge-core";

import type { MessageBody } from "./cards.js";

export interface DiscordApiOptions {
  baseUrl: string;
  applicationId: string;
  botToken: string;
  log: Logger;
  fetch?: typeof fetch;
  sleep?: (ms: number) => Promise<void>;
}

export class DiscordApiError extends Error {
  constructor(
    readonly status: number,
    readonly route: string,
    /** Discord's JSON error code (10062 "Unknown interaction", say), when sent. */
    readonly code?: number,
  ) {
    super(`discord ${route}: http ${status}`);
    this.name = "DiscordApiError";
  }
}

/** The parts of a guild member object the bridge reads; shape-checked at use. */
export interface GuildMember {
  roles?: unknown;
  user?: { id?: string; bot?: boolean };
}

/** Discord asks bot clients to identify themselves this way. */
export const USER_AGENT = "DiscordBot (https://github.com/denn-gubsky/loomcycle, 0.1.0)";

/** A 429 is waited out at most this many times, each wait at most this long. */
const MAX_RETRIES = 3;
const MAX_WAIT_MS = 30_000;

export interface CommandSpec {
  name: string;
  /** Empty for a message command: Discord refuses one with a description. */
  description: string;
  /** 1: a slash command; 3: a message command (a message's Apps menu). */
  type: 1 | 3;
  contexts: number[];
  integration_types: number[];
  options?: Array<{ type: 3; name: string; description: string; required: boolean; max_length?: number }>;
}

/** A voice message (flag IS_VOICE_MESSAGE): Discord allows no content,
 *  embeds or components beside its one attachment. */
export interface VoiceMessagePayload {
  flags: number;
  attachments: Array<{ id: number; filename: string; duration_secs: number; waveform: string }>;
  message_reference?: { message_id: string; fail_if_not_exists: boolean };
}

export class DiscordApi {
  private readonly fetch: typeof fetch;
  private readonly sleep: (ms: number) => Promise<void>;

  constructor(private readonly opts: DiscordApiOptions) {
    this.fetch = opts.fetch ?? globalThis.fetch;
    this.sleep = opts.sleep ?? ((ms) => new Promise((r) => setTimeout(r, ms)));
  }

  editOriginal(interactionToken: string, body: MessageBody): Promise<unknown> {
    return this.call("PATCH", `/webhooks/${this.opts.applicationId}/${interactionToken}/messages/@original`, "PATCH webhooks/@original", body, false);
  }

  followUp(interactionToken: string, body: MessageBody): Promise<unknown> {
    return this.call("POST", `/webhooks/${this.opts.applicationId}/${interactionToken}`, "POST webhooks/followup", body, false);
  }

  editFollowUp(interactionToken: string, messageId: string, body: MessageBody): Promise<unknown> {
    return this.call("PATCH", `/webhooks/${this.opts.applicationId}/${interactionToken}/messages/${messageId}`, "PATCH webhooks/messages", body, false);
  }

  createMessage(channelId: string, body: MessageBody): Promise<unknown> {
    return this.call("POST", `/channels/${channelId}/messages`, "POST channels/messages", body, true);
  }

  editMessage(channelId: string, messageId: string, body: MessageBody): Promise<unknown> {
    return this.call("PATCH", `/channels/${channelId}/messages/${messageId}`, "PATCH channels/messages", body, true);
  }

  /** createVoiceMessage posts audio as a voice message, with the bot token:
   *  multipart, payload_json beside the file as files[0]. */
  createVoiceMessage(channelId: string, payload: VoiceMessagePayload, file: { bytes: Uint8Array; filename: string; contentType: string }): Promise<unknown> {
    const form = new FormData();
    form.set("payload_json", JSON.stringify(payload));
    form.set("files[0]", new Blob([new Uint8Array(file.bytes)], { type: file.contentType }), file.filename);
    return this.call("POST", `/channels/${channelId}/messages`, "POST channels/messages (voice)", form, true);
  }

  typing(channelId: string): Promise<unknown> {
    return this.call("POST", `/channels/${channelId}/typing`, "POST channels/typing", undefined, true);
  }

  /** guildMember reads one member of a server (their roles), with the bot
   *  token. A person not in the server is a 404. */
  guildMember(guildId: string, userId: string): Promise<GuildMember | undefined> {
    return this.call("GET", `/guilds/${guildId}/members/${userId}`, "GET guilds/members", undefined, true) as Promise<GuildMember | undefined>;
  }

  /** putCommands replaces the application's commands: globally, or in one server. */
  putCommands(commands: CommandSpec[], guildId?: string): Promise<unknown> {
    const path = guildId
      ? `/applications/${this.opts.applicationId}/guilds/${guildId}/commands`
      : `/applications/${this.opts.applicationId}/commands`;
    return this.call("PUT", path, guildId ? "PUT guild commands" : "PUT global commands", commands, true);
  }

  private async call(method: string, path: string, route: string, body: unknown, bot: boolean): Promise<unknown> {
    const headers: Record<string, string> = { "user-agent": USER_AGENT };
    // A FormData body sets its own multipart content type, boundary included.
    const form = body instanceof FormData;
    if (body !== undefined && !form) headers["content-type"] = "application/json";
    if (bot) headers.authorization = `Bot ${this.opts.botToken}`;
    for (let attempt = 0; ; attempt++) {
      const res = await this.fetch(`${this.opts.baseUrl}${path}`, {
        method,
        headers,
        body: body === undefined ? undefined : form ? body : JSON.stringify(body),
      });
      const text = await res.text();
      const json = parseJson(text);
      if (res.status === 429 && attempt < MAX_RETRIES) {
        const wait = retryAfterMs(json, res.headers.get("retry-after"));
        if (wait !== undefined && wait <= MAX_WAIT_MS) {
          this.opts.log.warn("discord rate limited, waiting", { route, wait_ms: wait });
          await this.sleep(wait);
          continue;
        }
      }
      if (!res.ok) {
        const code = typeof (json as { code?: unknown })?.code === "number" ? (json as { code: number }).code : undefined;
        throw new DiscordApiError(res.status, route, code);
      }
      return json;
    }
  }
}

function parseJson(text: string): unknown {
  if (!text) return undefined;
  try {
    return JSON.parse(text);
  } catch {
    return undefined;
  }
}

/** retryAfterMs reads how long a 429 asks to wait: the body's retry_after
 *  (seconds, fractional), else the Retry-After header (seconds). */
export function retryAfterMs(body: unknown, header: string | null): number | undefined {
  const fromBody = (body as { retry_after?: unknown } | undefined)?.retry_after;
  const seconds = typeof fromBody === "number" ? fromBody : header !== null ? Number(header) : NaN;
  return Number.isFinite(seconds) && seconds >= 0 ? Math.ceil(seconds * 1000) : undefined;
}
