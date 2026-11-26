// Voice in for Teams (RFC GA §5.4, decision D12): an audio FILE a person
// attaches in their 1:1 chat with the bot. The same download carries any
// other attached file to the run's inbox (RFC HE): filesOf, teamsFilePort. Teams offers no voice-note button
// in a bot chat, and a channel's voice notes are reachable only through
// tenant-wide Graph rights, so this is the most Teams allows a bot. Replies
// stay text: the port has no sendVoice.
//
// The file sits in the person's own OneDrive. The activity carries a
// downloadUrl that lives a few minutes, so it is fetched at once, and only
// from the SharePoint hosts the operator allows. A link with a `tempauth`
// query (every link until 2027-03-01) is pre-authorised and fetched with no
// credential of ours; a link without one is fetched with the bot's file token
// for that host (RFC HF §3.7, filetoken.ts), sent on the first request only.

import { downloadPinned, PinnedDownloadError, type FileIn, type FilePort, type Logger, type VoiceIn, type VoicePort } from "@loomcycle/bridge-core";

import { FileTokenError, type FileTokenSource } from "./filetoken.js";

export const FILE_DOWNLOAD_INFO = "application/vnd.microsoft.teams.file.download.info";

/** The audio a person may attach, by Teams' fileType, and the media type the
 *  transcription route is told. Only what loomcycle reads whole without
 *  ffmpeg: a format it would have to convert is left to today's behaviour. */
export const AUDIO_FILE_TYPES: Readonly<Record<string, string>> = {
  m4a: "audio/mp4",
  mp4: "audio/mp4",
  mp3: "audio/mpeg",
  wav: "audio/wav",
  ogg: "audio/ogg",
  webm: "audio/webm",
  flac: "audio/flac",
};

/** Where a pre-authorised Teams file link may point by default: the tenant's
 *  SharePoint and OneDrive (`<tenant>-my.sharepoint.com`) hosts. */
export const DEFAULT_FILE_HOSTS = [".sharepoint.com"];

/** What the port downloads from: the link and the file's own name. */
export interface TeamsAudioRef {
  url: string;
  name?: string;
}

/** The subset of a Teams attachment this module reads. */
export interface TeamsAttachment {
  contentType?: string;
  name?: string;
  content?: unknown;
}

/** audioFileOf is the first attached audio file an activity carries, or
 *  undefined. Teams gives no size or length up front, so the byte cap is
 *  applied while reading. */
export function audioFileOf(attachments: readonly TeamsAttachment[] | undefined): VoiceIn | undefined {
  for (const a of attachments ?? []) {
    if (a.contentType !== FILE_DOWNLOAD_INFO || !a.content || typeof a.content !== "object") continue;
    const c = a.content as { downloadUrl?: unknown; fileType?: unknown };
    if (typeof c.downloadUrl !== "string" || !c.downloadUrl || typeof c.fileType !== "string") continue;
    const mime = AUDIO_FILE_TYPES[c.fileType.trim().toLowerCase().replace(/^\./, "")];
    if (!mime) continue;
    const ref: TeamsAudioRef = { url: c.downloadUrl, ...(a.name ? { name: a.name } : {}) };
    return { kind: "audio_file", ref, mime };
  }
  return undefined;
}

/** What the file port downloads from: the link and the file's own name. */
export type TeamsFileRef = TeamsAudioRef;

/** filesOf is every attached file an activity carries for the file path
 *  (RFC HE), in order. With voice on an audio file goes the voice path and
 *  is left out here; with voice off it is an ordinary file, taken when its
 *  type is in BRIDGE_FILES_TYPES (decision D4). Teams gives no size up
 *  front, so the byte cap is applied while reading. */
export function filesOf(attachments: readonly TeamsAttachment[] | undefined, voiceOn: boolean): FileIn[] {
  const out: FileIn[] = [];
  for (const a of attachments ?? []) {
    if (a.contentType !== FILE_DOWNLOAD_INFO || !a.content || typeof a.content !== "object") continue;
    const c = a.content as { downloadUrl?: unknown; fileType?: unknown };
    if (typeof c.downloadUrl !== "string" || !c.downloadUrl) continue;
    const fileType = typeof c.fileType === "string" ? c.fileType : undefined;
    if (voiceOn && fileType && AUDIO_FILE_TYPES[fileType.trim().toLowerCase().replace(/^\./, "")]) continue;
    const ref: TeamsFileRef = { url: c.downloadUrl, ...(a.name ? { name: a.name } : {}) };
    out.push({ ref, ...(a.name ? { name: a.name } : {}), ...(fileType ? { type: fileType } : {}) });
  }
  return out;
}

/** hostAllowed says a URL is https on a host under one of the suffixes
 *  (each starting with a dot, so `evilsharepoint.com` never matches). */
export function hostAllowed(url: URL, suffixes: readonly string[]): boolean {
  if (url.protocol !== "https:" || url.username || url.password) return false;
  const host = url.hostname.toLowerCase();
  return suffixes.some((s) => host.endsWith(s));
}

/** A download that did not happen. It never carries the link, whose query
 *  is a credential while it lives, nor a token: only the host refused, the
 *  status, or (reason "token") why the file token was not acquired. */
export class TeamsFileError extends Error {
  constructor(
    readonly reason: "host" | "redirect" | "status" | "type" | "size" | "token",
    readonly detail: { host?: string; status?: number; cause?: string } = {},
  ) {
    super(`teams file download refused: ${reason}`);
    this.name = "TeamsFileError";
  }
}

export interface TeamsVoicePortOptions<TRef> {
  /** Allowed host suffixes (TEAMS_BRIDGE_FILE_HOSTS). */
  hosts: readonly string[];
  /** The fetch downloads go through (tests). */
  fetch?: typeof fetch;
  /** The bot's file token, for a link without tempauth (RFC HF). Without
   *  one such a link is refused. */
  token?: FileTokenSource;
  /** The "listening" indicator: Teams' typing activity. */
  typing?: (ref: TRef) => Promise<void>;
  /** Where a refused download is logged, with its reason and host, so an
   *  operator can tell a host to add to TEAMS_BRIDGE_FILE_HOSTS from a file
   *  that was too large. */
  log?: Logger;
}

/** teamsVoicePort takes voice in only: no sendVoice, out "none". */
export function teamsVoicePort<TRef>(opts: TeamsVoicePortOptions<TRef>): VoicePort<TRef> {
  const doFetch = opts.fetch ?? fetch;
  const typing = opts.typing;
  return {
    capabilities: { voiceFormats: [], out: "none", maxUploadBytes: 0 },
    ...(typing ? { indicate: (ref: TRef) => typing(ref) } : {}),
    async download(v, maxBytes, signal) {
      const ref = v.ref as TeamsAudioRef;
      let bytes: Uint8Array;
      try {
        bytes = await download(doFetch, ref.url, opts.hosts, maxBytes, signal, opts.token);
      } catch (err) {
        if (err instanceof TeamsFileError) opts.log?.warn("teams file download refused", { reason: err.reason, ...err.detail });
        throw err;
      }
      return { bytes, mime: v.mime ?? "application/octet-stream", ...(ref.name ? { name: ref.name } : {}) };
    },
  };
}

export interface TeamsFilePortOptions {
  /** Allowed host suffixes (TEAMS_BRIDGE_FILE_HOSTS). */
  hosts: readonly string[];
  /** The fetch downloads go through (tests). */
  fetch?: typeof fetch;
  /** The bot's file token, for a link without tempauth (RFC HF). */
  token?: FileTokenSource;
  /** Where a refused download is logged, with its reason and host. */
  log?: Logger;
}

/** teamsFilePort downloads an attached file (RFC HE) the way the voice port
 *  downloads a recording: the link, with the file token only when it has no
 *  tempauth and only on the first request, only the allowed hosts on every
 *  hop, a sign-in page refused, the cap while reading. A body over the cap throws a TeamsFileError with reason
 *  "size", which the core tells the person is too large. */
export function teamsFilePort(opts: TeamsFilePortOptions): FilePort {
  const doFetch = opts.fetch ?? fetch;
  return {
    async download(f, maxBytes, signal) {
      const ref = f.ref as TeamsFileRef;
      try {
        return { bytes: await download(doFetch, ref.url, opts.hosts, maxBytes, signal, opts.token) };
      } catch (err) {
        if (err instanceof TeamsFileError) opts.log?.warn("teams file download refused", { reason: err.reason, ...err.detail });
        throw err;
      }
    },
  };
}

/** hasTempauth says a link is pre-authorised: it carries a `tempauth`
 *  query, as every Teams file link does until 2027-03-01. */
export function hasTempauth(url: URL): boolean {
  for (const k of url.searchParams.keys()) if (k.toLowerCase() === "tempauth") return true;
  return false;
}

/** isPersonalSite says a URL is in a OneDrive personal site: a host whose
 *  first label is `<tenant>-my` (`contoso-my.sharepoint.com`, or the same
 *  under a sovereign suffix) and a path under `/personal/`, which is where a
 *  file a person attaches in a 1:1 chat lives.
 *
 *  It bounds the bot's file token. That token reaches whatever SharePoint the
 *  bot's app is permitted, so a crafted downloadUrl would otherwise make the
 *  bot a confused deputy that fetches any file the app may read. The app is
 *  meant to hold only the File.ReadSelected.User RSC permission (files people
 *  shared with the bot) and no SharePoint application role; this check keeps
 *  the token off team sites whatever the app holds. It does not tell whose
 *  OneDrive the URL names: that is the sender's-own-drive check RFC HF P1 makes
 *  for Graph app mode, a follow-up for this path. */
export function isPersonalSite(url: URL): boolean {
  const first = url.hostname.toLowerCase().split(".")[0] ?? "";
  return first.length > "-my".length && first.endsWith("-my") && url.pathname.toLowerCase().startsWith("/personal/");
}

/** download fetches a link, following a redirect only to another allowed
 *  host, and reads at most maxBytes (more is an error, found while
 *  streaming). A pre-authorised link goes with no credential and no added
 *  header. Any other link goes with the file token for its own host, which is
 *  acquired only after the host passed the allowed list and the link is in a
 *  OneDrive personal site (isPersonalSite), and sent on the first
 *  request alone: a redirect, even to the same host, drops it. */
async function download(
  doFetch: typeof fetch,
  link: string,
  hosts: readonly string[],
  maxBytes: number,
  signal: AbortSignal,
  token: FileTokenSource | undefined,
): Promise<Uint8Array> {
  let fetchVia = doFetch;
  let url: URL | undefined;
  try {
    url = new URL(link);
  } catch {
    // downloadPinned refuses it below, as a link that is not a URL.
  }
  if (url && !hasTempauth(url)) {
    if (!hostAllowed(url, hosts) || !isPersonalSite(url)) throw new TeamsFileError("host", { host: url.hostname });
    if (!token) throw new TeamsFileError("token", { cause: "none" });
    let bearer: string;
    try {
      bearer = await token(`https://${url.host}/.default`);
    } catch (err) {
      const e = err instanceof FileTokenError ? err : undefined;
      throw new TeamsFileError("token", { cause: e?.reason ?? "failed", ...(e?.detail.status !== undefined ? { status: e.detail.status } : {}) });
    }
    fetchVia = firstRequestWith(doFetch, url.href, `Bearer ${bearer}`);
  }
  try {
    const got = await downloadPinned(link, {
      allowed: (u) => hostAllowed(u, hosts),
      // An expired or refused link can answer 200 with a sign-in page.
      accept: (ct) => !ct.toLowerCase().startsWith("text/html"),
      maxBytes,
      signal,
      fetch: fetchVia,
    });
    return got.bytes;
  } catch (err) {
    if (!(err instanceof PinnedDownloadError)) throw err;
    const { host, status, redirected } = err.detail;
    switch (err.reason) {
      case "url":
        throw new TeamsFileError("host");
      case "host":
        throw new TeamsFileError(redirected ? "redirect" : "host", { host });
      case "status":
        throw new TeamsFileError("status", { status });
      default:
        throw new TeamsFileError(err.reason);
    }
  }
}

/** firstRequestWith is doFetch with an Authorization header on its first
 *  call, and only when that call is for href; every later call (a redirect)
 *  goes without it. downloadPinned checks each URL against the allowed hosts
 *  before it calls this. */
function firstRequestWith(doFetch: typeof fetch, href: string, authorization: string): typeof fetch {
  let first = true;
  return ((input: string | URL | Request, init?: RequestInit) => {
    const at = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    const send = first && at === href;
    first = false;
    if (!send) return doFetch(input, init);
    const headers = new Headers(init?.headers);
    headers.set("authorization", authorization);
    return doFetch(input, { ...init, headers });
  }) as typeof fetch;
}
