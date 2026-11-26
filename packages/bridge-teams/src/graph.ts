// The Graph fallback for files Teams does not send (RFC HF §3.2–3.4, app-only
// mode): on a live tenant a file attached in a 1:1 chat reached the bot as
// one message with an empty text/html attachment and no
// file.download.info, so the bridge reads the same message through
// Microsoft Graph and takes its `reference` attachments.
//
// Not yet verified on a live tenant (RFC HF P0): the chat resolution through
// the person's installed apps, that a Teams activity id is the Graph message
// id, and the shape of a `reference` attachment. Each assumption is marked
// where it is made.
//
// The Graph token goes to the Graph host only: every JSON call refuses a
// redirect, and the file's own redirect to SharePoint is followed with no
// header at all, through bridge-core's downloadPinned. Nothing here logs a
// URL, a token or a Graph response: only reasons, statuses and counts.

import { setTimeout as sleep } from "node:timers/promises";

import { downloadPinned, extensionOf, PinnedDownloadError, type FileIn, type FilePort, type Logger } from "@loomcycle/bridge-core";

import { hostAllowed, TeamsFileError } from "./voice.js";

/** Graph's public-cloud root, for an SDK cloud that names none. */
export const GRAPH_PUBLIC_BASE = "https://graph.microsoft.com";

/** How long a lookup may hold a message before it is routed as text alone. */
export const DEFAULT_LOOKUP_TIMEOUT_MS = 5_000;

/** The waits before each re-read of a message Graph does not have yet. */
export const DEFAULT_RETRY_DELAYS_MS: readonly number[] = [300, 700, 1_500];

const DEFAULT_MAX_CHATS = 10_000;

/** What the file port downloads a Graph file from. */
export interface GraphFileRef {
  graph: "reference";
  /** The file in the sender's OneDrive, as Graph gave it. */
  contentUrl: string;
  name: string;
  /** The Entra tenant the token is asked for: the activity's. */
  tenantId?: string;
  /** The sender's Entra object id: the file must be in their OneDrive. */
  userId?: string;
}

export function isGraphFileRef(ref: unknown): ref is GraphFileRef {
  const r = ref as Partial<GraphFileRef> | null;
  return !!r && typeof r === "object" && r.graph === "reference" && typeof r.contentUrl === "string";
}

/** The one message a lookup reads, from fields the Bot Connector signed. */
export interface GraphLookup {
  /** The Bot Framework conversation id: the cache key, never sent to Graph. */
  conversationId: string;
  tenantId?: string;
  /** The sender's Entra object id. */
  userId: string;
  /** The activity id. */
  messageId: string;
}

export interface GraphFilesOptions {
  /** Graph's root (scheme and host), from the SDK's cloud environment. */
  base: string;
  /** The Teams app's manifest id. */
  appId: string;
  /** The app-only Graph token for a tenant; undefined when there is none. */
  token(tenantId?: string): Promise<string | undefined>;
  /** Allowed host suffixes for files (TEAMS_BRIDGE_FILE_HOSTS). */
  hosts: readonly string[];
  log: Logger;
  fetch?: typeof fetch;
  timeoutMs?: number;
  retryDelaysMs?: readonly number[];
  maxChats?: number;
}

/** A Graph call that did not answer 2xx: the status only. */
class GraphStatusError extends Error {
  constructor(readonly status: number) {
    super(`graph answered ${status}`);
    this.name = "GraphStatusError";
  }
}

/** A Graph file refused before any content was fetched. The core tells the
 *  person it could not download the file; the log says the reason only. */
export class GraphFileRefusal extends Error {
  constructor(readonly reason: "not_sender_drive") {
    super(`teams graph file refused: ${reason}`);
    this.name = "GraphFileRefusal";
  }
}

/** A lookup that stopped short, for the log. */
class LookupStop extends Error {
  constructor(readonly reason: "token" | "chat" | "sender") {
    super(`graph lookup stopped: ${reason}`);
    this.name = "LookupStop";
  }
}

interface GraphMessage {
  from?: { user?: { id?: unknown } | null } | null;
  attachments?: unknown;
}

export class GraphFiles {
  private readonly doFetch: typeof fetch;
  private readonly base: string;
  /** Graph chat id per (person, Bot Framework conversation). In memory: a
   *  restart costs one more resolution per conversation. */
  private readonly chats = new Map<string, string>();
  /** The OneDrive drive id per sender, kept the same way. */
  private readonly drives = new Map<string, string>();

  constructor(private readonly o: GraphFilesOptions) {
    this.doFetch = o.fetch ?? fetch;
    this.base = o.base.replace(/\/+$/, "");
  }

  /** lookup is the files the message carries in Graph, or none. It never
   *  throws: a failure or a timeout is logged and the message goes on as
   *  text. */
  async lookup(q: GraphLookup): Promise<FileIn[]> {
    const signal = AbortSignal.timeout(this.o.timeoutMs ?? DEFAULT_LOOKUP_TIMEOUT_MS);
    try {
      const token = await abortable(this.o.token(q.tenantId), signal);
      if (!token) throw new LookupStop("token");
      const chatId = await this.chatOf(q, token, signal);
      const msg = await this.messageOf(chatId, q.messageId, token, signal);
      const from = msg.from?.user?.id;
      if (typeof from !== "string" || from.toLowerCase() !== q.userId.toLowerCase()) throw new LookupStop("sender");
      const files = referencesOf(msg, q.tenantId, q.userId);
      this.o.log.info("teams graph lookup", { conversation: q.conversationId, files: files.length });
      return files;
    } catch (err) {
      this.o.log.warn("teams graph lookup failed", { conversation: q.conversationId, ...lookupFailure(err, signal) });
      return [];
    }
  }

  /** download fetches a Graph file: its metadata through the shares
   *  endpoint, then its content by item with the token, then the redirect to
   *  SharePoint with no credential, only to an allowed host. Refusals are
   *  TeamsFileErrors, as the attachment path throws, or a GraphFileRefusal.
   *
   *  The app token reads every file in the tenant, and a sender can post a
   *  chat message through Graph whose reference points at any of them, one
   *  they cannot read included (a confused deputy; every tenant file is on
   *  *.sharepoint.com, so the host check does not stop it). So a file is
   *  taken only from the sender's own OneDrive, where Teams keeps a file
   *  attached from a device ("Microsoft Teams Chat Files"). A file attached
   *  from SharePoint or a team site is refused in app mode. */
  async download(ref: GraphFileRef, maxBytes: number, signal: AbortSignal): Promise<Uint8Array> {
    let content: URL;
    try {
      content = new URL(ref.contentUrl);
    } catch {
      throw new TeamsFileError("host");
    }
    if (!hostAllowed(content, this.o.hosts)) throw new TeamsFileError("host", { host: content.hostname });
    if (!ref.userId) throw new GraphFileRefusal("not_sender_drive");
    const token = await this.o.token(ref.tenantId);
    if (!token) throw new Error("teams graph token unavailable");
    let item: { id?: unknown; parentReference?: { driveId?: unknown } | null };
    let own: string;
    try {
      item = (await this.get(`/v1.0/shares/${shareId(ref.contentUrl)}/driveItem?$select=id,name,parentReference`, token, signal)) as typeof item;
      own = await this.driveOf(ref.userId, token, signal);
    } catch (err) {
      if (err instanceof GraphStatusError) throw new TeamsFileError("status", { status: err.status });
      throw err;
    }
    const driveId = item.parentReference?.driveId;
    if (typeof item.id !== "string" || !item.id || typeof driveId !== "string" || driveId !== own) throw new GraphFileRefusal("not_sender_drive");
    const url = `${this.base}/v1.0/drives/${encodeURIComponent(driveId)}/items/${encodeURIComponent(item.id)}/content`;
    const res = await this.doFetch(url, { method: "GET", headers: { Authorization: `Bearer ${token}` }, redirect: "manual", signal });
    if (res.status >= 300 && res.status < 400) {
      await res.body?.cancel().catch(() => undefined);
      const loc = res.headers.get("location");
      if (!loc) throw new TeamsFileError("redirect");
      let next: URL;
      try {
        next = new URL(loc, url);
      } catch {
        throw new TeamsFileError("redirect");
      }
      return await pinned(next.href, this.o.hosts, maxBytes, signal, this.doFetch);
    }
    if (!res.ok) {
      await res.body?.cancel().catch(() => undefined);
      throw new TeamsFileError("status", { status: res.status });
    }
    // Graph served the bytes itself rather than redirecting.
    if (signInPage(res.headers.get("content-type") ?? "")) {
      await res.body?.cancel().catch(() => undefined);
      throw new TeamsFileError("type");
    }
    return await readCapped(res, maxBytes);
  }

  /** chatOf is the Graph id of the person's 1:1 chat with the bot.
   *  ASSUMPTION (RFC HF P0): the Bot Framework conversation id (`a:…`) is not
   *  a Graph chat id; the chat is found through the app the person has
   *  installed, filtered by the manifest id as teamsApp/externalId. */
  private async chatOf(q: GraphLookup, token: string, signal: AbortSignal): Promise<string> {
    const key = `${q.userId.toLowerCase()}|${q.conversationId}`;
    const cached = this.chats.get(key);
    if (cached) return cached;
    const user = `/v1.0/users/${encodeURIComponent(q.userId)}/teamwork/installedApps`;
    const filter = encodeURIComponent(`teamsApp/externalId eq '${this.o.appId}'`);
    const list = (await this.get(`${user}?$expand=teamsApp&$filter=${filter}`, token, signal)) as { value?: Array<{ id?: unknown }> };
    const install = list.value?.[0]?.id;
    if (typeof install !== "string" || !install) throw new LookupStop("chat");
    const chat = (await this.get(`${user}/${encodeURIComponent(install)}/chat`, token, signal)) as { id?: unknown; chatType?: unknown };
    // The chat between this person and the bot, and no other: a group chat
    // would let a lookup read what others wrote.
    if (typeof chat.id !== "string" || !chat.id || chat.chatType !== "oneOnOne") throw new LookupStop("chat");
    this.remember(this.chats, key, chat.id);
    return chat.id;
  }

  /** driveOf is the id of the sender's own OneDrive. */
  private async driveOf(userId: string, token: string, signal: AbortSignal): Promise<string> {
    const key = userId.toLowerCase();
    const cached = this.drives.get(key);
    if (cached) return cached;
    const drive = (await this.get(`/v1.0/users/${encodeURIComponent(userId)}/drive?$select=id`, token, signal)) as { id?: unknown };
    if (typeof drive.id !== "string" || !drive.id) throw new GraphFileRefusal("not_sender_drive");
    this.remember(this.drives, key, drive.id);
    return drive.id;
  }

  /** remember keeps a value in a cache bounded to maxChats entries, dropping
   *  the oldest. */
  private remember(cache: Map<string, string>, key: string, value: string): void {
    if (cache.size >= (this.o.maxChats ?? DEFAULT_MAX_CHATS)) {
      const oldest = cache.keys().next().value;
      if (oldest !== undefined) cache.delete(oldest);
    }
    cache.set(key, value);
  }

  /** messageOf reads the message, re-reading a few times while Graph does
   *  not have it yet (404). ASSUMPTION (RFC HF P0): in Teams the activity id
   *  is the Graph chat message id. */
  private async messageOf(chatId: string, messageId: string, token: string, signal: AbortSignal): Promise<GraphMessage> {
    const path = `/v1.0/chats/${encodeURIComponent(chatId)}/messages/${encodeURIComponent(messageId)}`;
    const delays = this.o.retryDelaysMs ?? DEFAULT_RETRY_DELAYS_MS;
    for (let i = 0; ; i++) {
      try {
        return (await this.get(path, token, signal)) as GraphMessage;
      } catch (err) {
        if (!(err instanceof GraphStatusError) || err.status !== 404 || i >= delays.length) throw err;
        await sleep(delays[i], undefined, { signal });
      }
    }
  }

  /** get is one Graph JSON call. A redirect is an error, so the token is
   *  never sent anywhere but the Graph host. */
  private async get(path: string, token: string, signal: AbortSignal): Promise<unknown> {
    const res = await this.doFetch(`${this.base}${path}`, {
      method: "GET",
      headers: { Authorization: `Bearer ${token}`, Accept: "application/json" },
      redirect: "error",
      signal,
    });
    if (!res.ok) {
      await res.body?.cancel().catch(() => undefined);
      throw new GraphStatusError(res.status);
    }
    const body: unknown = await res.json();
    if (!body || typeof body !== "object") throw new GraphStatusError(res.status);
    return body;
  }
}

/** referencesOf is the message's files: attachments of contentType
 *  "reference" with a contentUrl and a name. ASSUMPTION (RFC HF P0): that is
 *  how Graph shows a file attached in a 1:1 chat. The type is the name's
 *  extension, which the core checks against BRIDGE_FILES_TYPES and the
 *  file's bytes as for any other file. */
export function referencesOf(msg: GraphMessage, tenantId?: string, userId?: string): FileIn[] {
  const out: FileIn[] = [];
  for (const a of Array.isArray(msg.attachments) ? (msg.attachments as unknown[]) : []) {
    const att = a as { contentType?: unknown; contentUrl?: unknown; name?: unknown } | null;
    if (!att || att.contentType !== "reference") continue;
    if (typeof att.contentUrl !== "string" || !att.contentUrl || typeof att.name !== "string" || !att.name) continue;
    const ref: GraphFileRef = { graph: "reference", contentUrl: att.contentUrl, name: att.name, ...(tenantId ? { tenantId } : {}), ...(userId ? { userId } : {}) };
    const type = extensionOf(att.name);
    out.push({ ref, name: att.name, ...(type ? { type } : {}) });
  }
  return out;
}

/** shareId is Graph's encoding of a sharing URL: "u!" and the URL in
 *  unpadded base64url. */
export function shareId(url: string): string {
  return `u!${Buffer.from(url, "utf8").toString("base64url")}`;
}

/** withGraphFiles is the file port that downloads a Graph file through
 *  graph and any other through the attachment port. */
export function withGraphFiles(base: FilePort, graph: GraphFiles, log: Logger): FilePort {
  return {
    async download(f, maxBytes, signal) {
      if (!isGraphFileRef(f.ref)) return base.download(f, maxBytes, signal);
      try {
        return { bytes: await graph.download(f.ref, maxBytes, signal) };
      } catch (err) {
        if (err instanceof TeamsFileError) log.warn("teams file download refused", { reason: err.reason, via: "graph", ...err.detail });
        else if (err instanceof GraphFileRefusal) log.warn("teams file download refused", { reason: err.reason, via: "graph" });
        throw err;
      }
    },
  };
}

/** pinned follows Graph's redirect with no credential: every hop on an
 *  allowed host, a sign-in page refused, the cap while reading. The first
 *  hop is already a redirect, so a refused host is reported as one. */
async function pinned(link: string, hosts: readonly string[], maxBytes: number, signal: AbortSignal, doFetch: typeof fetch): Promise<Uint8Array> {
  try {
    const got = await downloadPinned(link, { allowed: (u) => hostAllowed(u, hosts), accept: (ct) => !signInPage(ct), maxBytes, signal, fetch: doFetch });
    return got.bytes;
  } catch (err) {
    if (!(err instanceof PinnedDownloadError)) throw err;
    const { host, status } = err.detail;
    switch (err.reason) {
      case "url":
      case "host":
        throw new TeamsFileError("redirect", host ? { host } : {});
      case "status":
        throw new TeamsFileError("status", { status });
      default:
        throw new TeamsFileError(err.reason);
    }
  }
}

function signInPage(contentType: string): boolean {
  return contentType.toLowerCase().startsWith("text/html");
}

/** readCapped reads a body, failing as soon as it passes maxBytes. */
async function readCapped(res: Response, maxBytes: number): Promise<Uint8Array> {
  const declared = Number(res.headers.get("content-length") ?? NaN);
  if (Number.isFinite(declared) && declared > maxBytes) {
    await res.body?.cancel().catch(() => undefined);
    throw new TeamsFileError("size");
  }
  if (!res.body) return new Uint8Array();
  const reader = res.body.getReader();
  const parts: Uint8Array[] = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.byteLength;
    if (total > maxBytes) {
      await reader.cancel().catch(() => undefined);
      throw new TeamsFileError("size");
    }
    parts.push(value);
  }
  const out = new Uint8Array(total);
  let at = 0;
  for (const p of parts) {
    out.set(p, at);
    at += p.byteLength;
  }
  return out;
}

/** abortable settles with p, or rejects once signal aborts. */
function abortable<T>(p: Promise<T>, signal: AbortSignal): Promise<T> {
  if (signal.aborted) return Promise.reject(signal.reason);
  return new Promise<T>((resolve, reject) => {
    const onAbort = () => reject(signal.reason);
    signal.addEventListener("abort", onAbort, { once: true });
    p.then(
      (v) => {
        signal.removeEventListener("abort", onAbort);
        resolve(v);
      },
      (e: unknown) => {
        signal.removeEventListener("abort", onAbort);
        reject(e);
      },
    );
  });
}

/** lookupFailure is what may be logged of a failed lookup: a reason, and a
 *  status for a Graph answer. Never a message, a URL or a token. */
function lookupFailure(err: unknown, signal: AbortSignal): { reason: string; status?: number } {
  if (err instanceof LookupStop) return { reason: err.reason };
  if (err instanceof GraphStatusError) return { reason: "status", status: err.status };
  if (signal.aborted) return { reason: "timeout" };
  return { reason: "error" };
}
