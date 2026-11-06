// A stand-in for the Bot Framework's token service, so the Teams SDK app runs
// in tests with request authentication ON: it serves the signing keys the SDK
// checks an inbound activity's JWT against, signs those JWTs, and hands the
// app its own outbound token. A test never needs the SDK's unauthenticated
// mode, which a gate bridge refuses.

import { createSign, generateKeyPairSync, type KeyObject } from "node:crypto";
import http from "node:http";
import type { AddressInfo } from "node:net";

import { PUBLIC, withOverrides, type CloudEnvironment } from "@microsoft/teams.api";

/** The bot's Entra app id in tests: the audience of every inbound token. */
export const BOT_ID = "0b0b0b0b-0000-4000-8000-000000000b07";

const KID = "test-signing-key";

function b64url(v: string | Buffer): string {
  return Buffer.from(v).toString("base64url");
}

export class FakeBotFramework {
  private readonly privateKey: KeyObject;
  private readonly publicKey: KeyObject;
  private server?: http.Server;
  /** The cloud the app is built with: PUBLIC, with its keys served here. */
  cloud: CloudEnvironment = PUBLIC;

  constructor() {
    const { privateKey, publicKey } = generateKeyPairSync("rsa", { modulusLength: 2048 });
    this.privateKey = privateKey;
    this.publicKey = publicKey;
  }

  async start(): Promise<void> {
    const jwk = { ...this.publicKey.export({ format: "jwk" }), kid: KID, use: "sig", alg: "RS256" };
    this.server = http.createServer((req, res) => {
      req.resume();
      const base = `http://127.0.0.1:${(this.server!.address() as AddressInfo).port}`;
      const body = req.url?.endsWith("/keys") ? { keys: [jwk] } : { issuer: PUBLIC.tokenIssuer, jwks_uri: `${base}/v1/.well-known/keys` };
      res.writeHead(200, { "content-type": "application/json" });
      res.end(JSON.stringify(body));
    });
    await new Promise<void>((r) => this.server!.listen(0, "127.0.0.1", r));
    const base = `http://127.0.0.1:${(this.server.address() as AddressInfo).port}`;
    this.cloud = withOverrides(PUBLIC, { openIdMetadataUrl: `${base}/v1/.well-known/openidconfiguration` });
  }

  async stop(): Promise<void> {
    if (this.server) await new Promise((r) => this.server!.close(r));
  }

  /** sign makes an RS256 JWT under the served key, or under another key. */
  sign(claims: Record<string, unknown>, opts: { key?: KeyObject } = {}): string {
    const now = Math.floor(Date.now() / 1000);
    const header = b64url(JSON.stringify({ alg: "RS256", typ: "JWT", kid: KID }));
    const payload = b64url(JSON.stringify({ iat: now, nbf: now - 5, exp: now + 300, ...claims }));
    const signer = createSign("RSA-SHA256");
    signer.update(`${header}.${payload}`);
    return `${header}.${payload}.${b64url(signer.sign(opts.key ?? this.privateKey))}`;
  }

  /** authorization is the header the Bot Connector sends with an activity
   *  whose serviceUrl is serviceUrl. */
  authorization(serviceUrl: string, claims: Record<string, unknown> = {}, opts: { key?: KeyObject } = {}): string {
    return `Bearer ${this.sign({ iss: PUBLIC.tokenIssuer, aud: BOT_ID, serviceurl: serviceUrl, ...claims }, opts)}`;
  }

  /** token is the app's own outbound token (the fake Bot Connector takes any). */
  token = (): string => this.sign({ iss: "test", aud: "https://api.botframework.com", appid: BOT_ID });
}
