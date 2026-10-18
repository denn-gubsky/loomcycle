// The Telegram bridge's configuration: the shared BRIDGE_* surface plus the
// bot's own keys, read in one pass so a refusal lists every problem.

import { EnvReader, loadSharedConfig, type Env, type SharedConfig } from "@loomcycle/bridge-core";

export interface TelegramConfig extends SharedConfig {
  botToken: string;
  /** The webhook's secret_token; empty only with BRIDGE_DEV=1. */
  webhookSecret: string;
  allowedUsers: Set<string>;
  allowedChats: Set<string>;
  allowAnyone: boolean;
  /** Group chats whose speakers are trusted. Every other group is untrusted. */
  trustedChats: Set<string>;
  /** The bridge's public base URL; the webhook is <publicUrl>/telegram/webhook. */
  publicUrl?: string;
  setWebhook: boolean;
  /** Stream answers in private chats by editing one message. */
  streaming: boolean;
  apiBaseUrl: string;
}

export const DEFAULT_PORT = 8443;
export const DEFAULT_API_BASE_URL = "https://api.telegram.org";
export const WEBHOOK_PATH = "/telegram/webhook";

const BRIDGE = "telegram-bridge";
// Telegram's own rule for setWebhook's secret_token.
const SECRET_TOKEN = /^[A-Za-z0-9_-]{1,256}$/;
// A user or chat id: chats are negative (-100… for a supergroup).
const NUMERIC_ID = /^-?\d{1,20}$/;
// Telegram's documented token shape is "<bot id>:<secret>". Checked only so a
// pasted placeholder fails at start rather than on the first message.
const BOT_TOKEN = /^\d+:[A-Za-z0-9_-]+$/;

function ids(reader: EnvReader, name: string): Set<string> {
  const list = reader.list(name);
  const bad = list.filter((v) => !NUMERIC_ID.test(v));
  if (bad.length > 0) reader.problems.push(`${name} must be a comma list of numeric Telegram ids`);
  return new Set(list.filter((v) => NUMERIC_ID.test(v)));
}

function httpsBase(reader: EnvReader, name: string, value: string | undefined, opts: { allowHttp: boolean }): string | undefined {
  if (value === undefined) return undefined;
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    reader.problems.push(`${name} must be a URL`);
    return undefined;
  }
  // The bot token travels in the API URL's path, so plain http is allowed
  // only to this machine (a local Bot API server, a test fake).
  const loopback = ["localhost", "127.0.0.1", "[::1]"].includes(url.hostname);
  if (url.protocol !== "https:" && !(opts.allowHttp && url.protocol === "http:" && loopback)) {
    reader.problems.push(opts.allowHttp ? `${name} must be an https:// URL, or http:// to localhost` : `${name} must be an https:// URL`);
    return undefined;
  }
  if (url.search || url.hash) {
    reader.problems.push(`${name} must not have a query or fragment`);
    return undefined;
  }
  return value.replace(/\/+$/, "");
}

/** loadConfig reads the environment and throws a ConfigError naming every
 *  problem at once. */
export function loadConfig(env: Env = process.env): TelegramConfig {
  const reader = new EnvReader(env);
  const shared = loadSharedConfig(reader, { port: DEFAULT_PORT });

  const botToken = reader.required("TELEGRAM_BOT_TOKEN", "from @BotFather");
  // The value is never echoed: a problem names the variable only.
  if (botToken && !BOT_TOKEN.test(botToken)) reader.problems.push("TELEGRAM_BOT_TOKEN does not look like a bot token (<id>:<secret>)");

  const setWebhook = reader.flag("TELEGRAM_BRIDGE_SET_WEBHOOK");
  const secretRaw = reader.get("TELEGRAM_BRIDGE_WEBHOOK_SECRET");
  // BRIDGE_DEV turns authentication off, so it alone may leave the secret
  // unset; setting the webhook always needs one.
  if (secretRaw === undefined) {
    if (!shared.dev || setWebhook) reader.problems.push("TELEGRAM_BRIDGE_WEBHOOK_SECRET is required (the webhook's secret_token)");
  } else if (!SECRET_TOKEN.test(secretRaw)) {
    reader.problems.push("TELEGRAM_BRIDGE_WEBHOOK_SECRET must be 1-256 characters of A-Z, a-z, 0-9, _ and -");
  }

  const allowedUsers = ids(reader, "TELEGRAM_BRIDGE_ALLOWED_USERS");
  const allowedChats = ids(reader, "TELEGRAM_BRIDGE_ALLOWED_CHATS");
  const trustedChats = ids(reader, "TELEGRAM_BRIDGE_TRUSTED_CHATS");
  const allowAnyone = reader.flag("TELEGRAM_BRIDGE_ALLOW_ANYONE");
  // Closed by default: a bot anyone on Telegram can find must not run on the
  // operator's model keys unless the operator says so.
  if (!allowAnyone && allowedUsers.size === 0 && allowedChats.size === 0) {
    reader.problems.push(
      "set TELEGRAM_BRIDGE_ALLOWED_USERS and/or TELEGRAM_BRIDGE_ALLOWED_CHATS, or TELEGRAM_BRIDGE_ALLOW_ANYONE=1 to let anyone on Telegram use the bot",
    );
  }

  const publicUrl = httpsBase(reader, "TELEGRAM_BRIDGE_PUBLIC_URL", reader.get("TELEGRAM_BRIDGE_PUBLIC_URL"), { allowHttp: false });
  if (setWebhook && !reader.get("TELEGRAM_BRIDGE_PUBLIC_URL")) {
    reader.problems.push("TELEGRAM_BRIDGE_SET_WEBHOOK=1 needs TELEGRAM_BRIDGE_PUBLIC_URL");
  }

  const streaming = reader.flag("TELEGRAM_BRIDGE_STREAMING");

  const apiBaseUrl =
    httpsBase(reader, "TELEGRAM_BRIDGE_API_BASE_URL", reader.get("TELEGRAM_BRIDGE_API_BASE_URL"), { allowHttp: true }) ?? DEFAULT_API_BASE_URL;

  reader.check(BRIDGE);
  return {
    ...shared,
    botToken,
    webhookSecret: secretRaw ?? "",
    allowedUsers,
    allowedChats,
    allowAnyone,
    trustedChats,
    publicUrl,
    setWebhook,
    streaming,
    apiBaseUrl,
  };
}
