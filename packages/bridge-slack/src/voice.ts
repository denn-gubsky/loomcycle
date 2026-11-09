// Slack's side of voice messages (RFC GA): download a clip or an audio file
// someone shared, and post a spoken reply as an audio file in the thread.
//
// Slack has no API that posts a native clip, so a reply is an ordinary audio
// upload with Slack's inline player. Nothing is kept: the bytes live in
// memory for one request.

import { downloadPinned, PinnedDownloadError, VoiceRefusedError, type VoiceIn, type VoiceOut, type VoicePort } from "@loomcycle/bridge-core";

import { slackErrorCode, type SlackApi } from "./api.js";
import type { SlackRef } from "./identity.js";

/** The only host the bot token is sent to for a download. Slack serves
 *  url_private_download from it, and answers a request without the token
 *  with its login page. */
export const SLACK_FILES_HOST = "files.slack.com";

/** A shared file, as an event names it: VoiceIn.ref on Slack. */
export interface SlackFileRef {
  id: string;
  /** url_private_download. An event may leave it out (a file shown only
   *  through files.info); the download then fails and the person is told. */
  url?: string;
}

/** A download or upload that failed. Its message is a code: never a URL,
 *  which carries authority (an upload URL is pre-signed), and never the
 *  token. */
export class SlackFileError extends Error {
  constructor(readonly code: string) {
    super(`slack file: ${code}`);
    this.name = "SlackFileError";
  }
}

/** Slack's answers that mean the bot may not post a file in this
 *  conversation: spoken replies stop there (VoiceRefusedError). */
const REFUSED = new Set(["not_in_channel", "channel_not_found", "missing_scope"]);

/** The largest spoken reply uploaded. Slack takes far more; a reply capped at
 *  BRIDGE_VOICE_REPLY_MAX_CHARS is a few megabytes even as wav. */
export const MAX_UPLOAD_BYTES = 50 * 1024 * 1024;
/** A conversation answers one turn at a time, so a stalled upload must not
 *  hold it. */
const UPLOAD_TIMEOUT_MS = 60_000;

export interface SlackVoiceOptions {
  api: SlackApi;
  botToken: string;
  fetch?: typeof fetch;
  /** Whether a DM's reply streams. A streamed DM reply is a thread reply to
   *  the message it answers, so the audio goes there too; otherwise a DM
   *  reply is top-level, and so is the audio. */
  streamsDms: boolean;
}

export function createSlackVoice(o: SlackVoiceOptions): VoicePort<SlackRef> {
  const doFetch = o.fetch ?? globalThis.fetch;

  async function download(v: VoiceIn, maxBytes: number, signal: AbortSignal): Promise<{ bytes: Uint8Array; mime: string }> {
    const ref = v.ref as Partial<SlackFileRef> | undefined;
    const url = typeof ref?.url === "string" ? ref.url : "";
    if (!url) throw new SlackFileError("no_download_url");
    let got: { bytes: Uint8Array; contentType: string };
    try {
      // The host is checked before every request, the token's included: a
      // URL in an event is only as trustworthy as the event (unchecked in dev
      // mode), and a redirect may point anywhere.
      got = await downloadPinned(url, {
        allowed: onFilesHost,
        headers: { authorization: `Bearer ${o.botToken}` },
        // Without the token Slack answers 200 with its login page
        // (text/html), so a page or any other text is refused. A file host
        // may also serve the bytes as application/octet-stream; that is
        // taken, typed by the event's own mimetype, and loomcycle refuses
        // what is not audio.
        accept: (ct) => {
          const served = baseType(ct);
          return served.startsWith("audio/") || served.startsWith("video/") || served === "application/octet-stream";
        },
        maxBytes,
        signal,
        fetch: doFetch,
      });
    } catch (err) {
      throw err instanceof PinnedDownloadError ? new SlackFileError(slackCodeOf(err)) : err;
    }
    const served = baseType(got.contentType);
    const media = served.startsWith("audio/") || served.startsWith("video/");
    return { bytes: got.bytes, mime: media ? served : (v.mime ?? "application/octet-stream") };
  }

  async function sendVoice(ref: SlackRef, audio: VoiceOut): Promise<void> {
    try {
      const got = await o.api.callForm("files.getUploadURLExternal", { filename: fileNameOf(audio.mime), length: String(audio.bytes.length) });
      const uploadUrl = typeof got.upload_url === "string" ? got.upload_url : "";
      const fileId = typeof got.file_id === "string" ? got.file_id : "";
      if (!fileId || !uploadUrl.startsWith("https://")) throw new SlackFileError("no_upload_url");
      // The upload URL is Slack's own and carries its authority: the bot
      // token is not sent with the bytes.
      const res = await doFetch(uploadUrl, {
        method: "POST",
        headers: { "content-type": "application/octet-stream" },
        body: audio.bytes,
        redirect: "error",
        signal: AbortSignal.timeout(UPLOAD_TIMEOUT_MS),
      });
      await res.body?.cancel().catch(() => undefined);
      if (!res.ok) throw new SlackFileError(`upload_http_${res.status}`);
      const threadTs = ref.threadTs ?? (o.streamsDms ? ref.messageTs : undefined);
      await o.api.callForm("files.completeUploadExternal", {
        files: JSON.stringify([{ id: fileId, title: audio.label }]),
        channel_id: ref.channel,
        ...(threadTs ? { thread_ts: threadTs } : {}),
      });
    } catch (err) {
      const code = slackErrorCode(err);
      if (code && REFUSED.has(code)) throw new VoiceRefusedError(`slack refused the audio reply: ${code}`);
      throw err;
    }
  }

  return {
    download,
    sendVoice,
    // Slack has no recording or listening indicator for a bot outside its
    // assistant threads, which the bridge does not use.
    capabilities: {
      // mp3 first: the format Slack's inline player is most widely reported
      // to play (verify in P0).
      voiceFormats: ["mp3", "ogg", "wav"],
      out: "file",
      maxUploadBytes: MAX_UPLOAD_BYTES,
    },
  };
}

function onFilesHost(u: URL): boolean {
  return u.protocol === "https:" && u.hostname === SLACK_FILES_HOST && u.port === "" && !u.username && !u.password;
}

function baseType(ct: string): string {
  return ct.split(";")[0]!.trim().toLowerCase();
}

/** slackCodeOf is the SlackFileError code a refused download had before the
 *  download moved into bridge-core, so logs and callers read the same. */
function slackCodeOf(err: PinnedDownloadError): string {
  switch (err.reason) {
    case "url":
    case "host":
      return "not_slack_files_host";
    case "redirect":
      return "redirect";
    case "status":
      return `http_${err.detail.status}`;
    case "type":
      return "not_audio";
    case "size":
      return "too_large";
  }
}

/** fileNameOf names the upload by its format: Slack picks its player by the
 *  file's type. */
export function fileNameOf(mime: string): string {
  const m = mime.split(";")[0]!.trim().toLowerCase();
  if (m === "audio/mpeg" || m === "audio/mp3") return "reply.mp3";
  if (m === "audio/ogg" || m === "audio/opus") return "reply.ogg";
  if (m === "audio/wav" || m === "audio/x-wav" || m === "audio/wave") return "reply.wav";
  return "reply.audio";
}
