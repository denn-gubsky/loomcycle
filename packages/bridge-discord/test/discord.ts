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
}

type Reply = { status: number; body?: unknown; headers?: Record<string, string> };

/** FakeDiscord records each call and answers 200 {} unless told otherwise. */
export class FakeDiscord {
  readonly calls: ApiCall[] = [];
  /** Answers for the next calls whose "METHOD path" matches, in order. */
  private readonly scripted: Array<{ match: RegExp; reply: Reply }> = [];

  constructor(readonly base = "https://discord.test/api/v10") {}

  on(match: RegExp, reply: Reply): void {
    this.scripted.push({ match, reply });
  }

  fetch = async (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const url = String(input);
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
    });
    const i = this.scripted.findIndex((s) => s.match.test(`${method} ${path}`));
    const reply = i >= 0 ? this.scripted.splice(i, 1)[0].reply : { status: 200, body: {} };
    return new Response(reply.body === undefined ? null : JSON.stringify(reply.body), { status: reply.status, headers: reply.headers });
  };

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
