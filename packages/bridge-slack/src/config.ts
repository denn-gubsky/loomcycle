// The Slack bridge's configuration: the shared BRIDGE_* surface plus Slack's
// own keys, read in one pass so a refusal names every problem at once.

import { EnvReader, loadSharedConfig, type Env, type SharedConfig, type Trust } from "@loomcycle/bridge-core";

export interface SlackConfig {
  shared: SharedConfig;
  /** The bot token (xoxb-…). Sent only to apiBaseUrl. */
  botToken: string;
  /** The app's signing secret: every inbound request is checked against it. */
  signingSecret: string;
  /** Workspace (team) ids the bot answers in. Never empty: closed by default. */
  allowedTeams: ReadonlySet<string>;
  /** How a channel's or group DM's speakers are marked. A DM is always trusted. */
  sharedTrust: Trust;
  /** Stream DM replies with Slack's native streaming; otherwise one message per turn. */
  streaming: boolean;
  /** Slack's Web API, without a trailing slash. */
  apiBaseUrl: string;
}

export const DEFAULT_PORT = 3000;
export const DEFAULT_API_BASE_URL = "https://slack.com/api";

const TEAM_ID = /^[A-Z0-9]{2,32}$/;
const LOOPBACK = new Set(["localhost", "127.0.0.1", "[::1]"]);

export function loadConfig(env: Env = process.env): SlackConfig {
  const r = new EnvReader(env);
  const shared = loadSharedConfig(r, { port: DEFAULT_PORT });

  const botToken = r.required("SLACK_BOT_TOKEN", "the app's Bot User OAuth Token");
  // Never echo the value: only say what kind of token it is not.
  if (botToken && !botToken.startsWith("xoxb-")) r.problems.push("SLACK_BOT_TOKEN must be a bot token (xoxb-…)");
  const signingSecret = r.required("SLACK_SIGNING_SECRET", "Basic Information → App Credentials");

  const teams = r.list("SLACK_BRIDGE_ALLOWED_TEAMS").map((t) => t.toUpperCase());
  if (teams.length === 0) {
    r.problems.push("SLACK_BRIDGE_ALLOWED_TEAMS is required (comma list of workspace ids, e.g. T0123ABCD)");
  } else if (!teams.every((t) => TEAM_ID.test(t))) {
    r.problems.push("SLACK_BRIDGE_ALLOWED_TEAMS must list workspace ids such as T0123ABCD");
  }

  const sharedTrust = r.oneOf("SLACK_BRIDGE_SHARED_TRUST", ["trusted", "untrusted"] as const, "trusted");
  // Streaming is on unless set to 0; flag() still refuses a typo.
  const streaming = r.get("SLACK_BRIDGE_STREAMING") === undefined ? true : r.flag("SLACK_BRIDGE_STREAMING");

  const apiBaseUrl = (r.get("SLACK_BRIDGE_API_BASE_URL") ?? DEFAULT_API_BASE_URL).replace(/\/+$/, "");
  checkApiBaseUrl(apiBaseUrl, r.problems);

  r.check("slack-bridge");
  return { shared, botToken, signingSecret, allowedTeams: new Set(teams), sharedTrust, streaming, apiBaseUrl };
}

/** The bot token travels to this URL on every call, so plain http is refused
 *  except to this machine (a local fake Slack, for testing). */
function checkApiBaseUrl(raw: string, problems: string[]): void {
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    problems.push("SLACK_BRIDGE_API_BASE_URL must be a URL");
    return;
  }
  if (u.protocol === "https:") return;
  if (u.protocol === "http:" && LOOPBACK.has(u.hostname)) return;
  problems.push("SLACK_BRIDGE_API_BASE_URL must be https (http only to localhost)");
}
