// Test doubles for the Discord side: a signing key like the application's,
// and a recording fake of Discord's REST API behind `fetch`.

import { generateKeyPairSync, sign, type KeyObject } from "node:crypto";

export interface AppKey {
  privateKey: KeyObject;
  /** The public key as the Developer Portal shows it: 64 hex characters. */
  publicHex: string;
}

export function appKey(): AppKey {
  const { privateKey, publicKey } = generateKeyPairSync("ed25519");
  const x = publicKey.export({ format: "jwk" }).x!;
  return { privateKey, publicHex: Buffer.from(x, "base64url").toString("hex") };
}

/** signedHeaders signs body the way Discord does: timestamp, then the raw body. */
export function signedHeaders(key: KeyObject, body: string, timestamp: string): Record<string, string> {
  const sig = sign(null, Buffer.concat([Buffer.from(timestamp), Buffer.from(body)]), key).toString("hex");
  return { "x-signature-ed25519": sig, "x-signature-timestamp": timestamp, "content-type": "application/json" };
}

export interface ApiCall {
  method: string;
  /** The path after the API base, e.g. /channels/1/typing. */
  path: string;
  authorization?: string;
  userAgent?: string;
  body?: unknown;
  /** A multipart body: its payload_json, parsed, and its files. */
  form?: { payload: unknown; files: Array<{ field: string; name: string; type: string; bytes: Uint8Array }> };
}

/** A download from Discord's CDN, as the fake saw it. */
export interface Download {
  url: string;
  authorization?: string;
  redirect?: RequestInit["redirect"];
}

type Reply = { status: number; body?: unknown; headers?: Record<string, string> };

const CDN = /^https:\/\/(cdn\.discordapp\.com|media\.discordapp\.net)\//;

/** FakeDiscord records each call and answers 200 {} unless told otherwise; a
 *  message route answers with a message id. It also serves Discord's CDN
 *  from `files`, by URL. */
export class FakeDiscord {
  readonly calls: ApiCall[] = [];
  readonly downloads: Download[] = [];
  readonly files = new Map<string, { status?: number; body?: Uint8Array; headers?: Record<string, string> }>();
  private messageSeq = 0;
  /** Answers for the next calls whose "METHOD path" matches, in order. */
  private readonly scripted: Array<{ match: RegExp; reply: Reply }> = [];

  constructor(readonly base = "https://discord.test/api/v10") {}

  on(match: RegExp, reply: Reply): void {
    this.scripted.push({ match, reply });
  }

  fetch = async (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const url = String(input);
    if (CDN.test(url)) return this.cdn(url, init);
    if (!url.startsWith(this.base)) throw new Error(`unexpected host: ${url}`);
    const path = url.slice(this.base.length);
    const method = init?.method ?? "GET";
    const headers = new Headers(init?.headers);
    this.calls.push({
      method,
      path,
      authorization: headers.get("authorization") ?? undefined,
      userAgent: headers.get("user-agent") ?? undefined,
      body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
      form: init?.body instanceof FormData ? await formOf(init.body) : undefined,
    });
    const i = this.scripted.findIndex((s) => s.match.test(`${method} ${path}`));
    const message = /\/messages|^\/webhooks\/\d+\/[^/]+$/.test(path) ? { id: `70000000000000${String(++this.messageSeq).padStart(4, "0")}` } : {};
    const reply = i >= 0 ? this.scripted.splice(i, 1)[0].reply : { status: 200, body: message };
    return new Response(reply.body === undefined ? null : JSON.stringify(reply.body), { status: reply.status, headers: reply.headers });
  };

  private cdn(url: string, init?: RequestInit): Response {
    const headers = new Headers(init?.headers);
    this.downloads.push({ url, authorization: headers.get("authorization") ?? undefined, redirect: init?.redirect });
    const f = this.files.get(url);
    if (!f) return new Response("not found", { status: 404 });
    return new Response(f.body ?? null, { status: f.status ?? 200, headers: f.headers ?? { "content-type": "audio/ogg" } });
  }

  to(re: RegExp): ApiCall[] {
    return this.calls.filter((c) => re.test(`${c.method} ${c.path}`));
  }

  /** contents lists the message text of every message-carrying call. */
  contents(): string[] {
    return this.calls.flatMap((c) => {
      const content = (c.body as { content?: unknown } | undefined)?.content;
      return typeof content === "string" ? [content] : [];
    });
  }
}

async function formOf(form: FormData): Promise<NonNullable<ApiCall["form"]>> {
  const files: NonNullable<ApiCall["form"]>["files"] = [];
  let payload: unknown;
  for (const [field, v] of form.entries()) {
    if (typeof v === "string") {
      if (field === "payload_json") payload = JSON.parse(v);
    } else {
      files.push({ field, name: v.name, type: v.type, bytes: new Uint8Array(await v.arrayBuffer()) });
    }
  }
  return { payload, files };
}
