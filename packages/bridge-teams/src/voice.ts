// Voice in for Teams (RFC GA §5.4, decision D12): an audio FILE a person
// attaches in their 1:1 chat with the bot. Teams offers no voice-note button
// in a bot chat, and a channel's voice notes are reachable only through
// tenant-wide Graph rights, so this is the most Teams allows a bot. Replies
// stay text: the port has no sendVoice.
//
// The file sits in the person's own OneDrive. The activity carries a
// pre-authorised downloadUrl that lives a few minutes, so it is fetched at
// once, with no credential of ours, and only from the SharePoint hosts the
// operator allows.

import { downloadPinned, PinnedDownloadError, type Logger, type VoiceIn, type VoicePort } from "@loomcycle/bridge-core";

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

/** hostAllowed says a URL is https on a host under one of the suffixes
 *  (each starting with a dot, so `evilsharepoint.com` never matches). */
export function hostAllowed(url: URL, suffixes: readonly string[]): boolean {
  if (url.protocol !== "https:" || url.username || url.password) return false;
  const host = url.hostname.toLowerCase();
  return suffixes.some((s) => host.endsWith(s));
}

/** A download that did not happen. It never carries the link, whose query
 *  is a credential while it lives: only the host refused, or the status. */
export class TeamsFileError extends Error {
  constructor(
    readonly reason: "host" | "redirect" | "status" | "type" | "size",
    readonly detail: { host?: string; status?: number } = {},
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
        bytes = await download(doFetch, ref.url, opts.hosts, maxBytes, signal);
      } catch (err) {
        if (err instanceof TeamsFileError) opts.log?.warn("teams file download refused", { reason: err.reason, ...err.detail });
        throw err;
      }
      return { bytes, mime: v.mime ?? "application/octet-stream", ...(ref.name ? { name: ref.name } : {}) };
    },
  };
}

/** download fetches a pre-authorised link with no credential and no added
 *  header, following a redirect only to another allowed host, and reads at
 *  most maxBytes (more is an error, found while streaming). */
async function download(doFetch: typeof fetch, link: string, hosts: readonly string[], maxBytes: number, signal: AbortSignal): Promise<Uint8Array> {
  try {
    const got = await downloadPinned(link, {
      allowed: (u) => hostAllowed(u, hosts),
      // An expired or refused link can answer 200 with a sign-in page.
      accept: (ct) => !ct.toLowerCase().startsWith("text/html"),
      maxBytes,
      signal,
      fetch: doFetch,
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
