// A self-signed RSA certificate made in-process for tests, so they need no
// openssl binary (the bridge image's test stage has none) and no key file is
// committed. A minimal DER encoder for an X.509 v3 certificate without
// extensions, signed sha256WithRSAEncryption with the generated key.

import { generateKeyPairSync, sign, type KeyObject } from "node:crypto";

function len(n: number): Buffer {
  if (n < 0x80) return Buffer.from([n]);
  const bytes: number[] = [];
  for (let v = n; v > 0; v >>= 8) bytes.unshift(v & 0xff);
  return Buffer.from([0x80 | bytes.length, ...bytes]);
}

const tlv = (tag: number, body: Buffer): Buffer => Buffer.concat([Buffer.from([tag]), len(body.length), body]);
const seq = (...parts: Buffer[]) => tlv(0x30, Buffer.concat(parts));
const set = (...parts: Buffer[]) => tlv(0x31, Buffer.concat(parts));
const int = (n: number) => tlv(0x02, Buffer.from([n]));
const nul = () => Buffer.from([0x05, 0x00]);
const utf8 = (s: string) => tlv(0x0c, Buffer.from(s, "utf8"));
const bits = (b: Buffer) => tlv(0x03, Buffer.concat([Buffer.from([0]), b]));

function oid(dotted: string): Buffer {
  const [a, b, ...rest] = dotted.split(".").map(Number);
  const out = [40 * a! + b!];
  for (const v of rest) {
    const parts = [v & 0x7f];
    for (let x = v >> 7; x > 0; x >>= 7) parts.unshift(0x80 | (x & 0x7f));
    out.push(...parts);
  }
  return tlv(0x06, Buffer.from(out));
}

/** UTCTime, YYMMDDHHMMSSZ (years 1950–2049). */
function utcTime(d: Date): Buffer {
  const p = (n: number) => String(n).padStart(2, "0");
  const s = `${p(d.getUTCFullYear() % 100)}${p(d.getUTCMonth() + 1)}${p(d.getUTCDate())}${p(d.getUTCHours())}${p(d.getUTCMinutes())}${p(d.getUTCSeconds())}Z`;
  return tlv(0x17, Buffer.from(s, "ascii"));
}

/** selfSigned makes an RSA key and a certificate for it, both as PEM. */
export function selfSigned(cn = "bridge-teams-test"): { keyPem: string; certPem: string; key: KeyObject } {
  const { privateKey, publicKey } = generateKeyPairSync("rsa", { modulusLength: 2048 });
  const sha256WithRSA = seq(oid("1.2.840.113549.1.1.11"), nul());
  const name = seq(set(seq(oid("2.5.4.3"), utf8(cn))));
  const now = Date.now();
  const tbs = seq(
    tlv(0xa0, int(2)), // [0] EXPLICIT version v3
    int(1),
    sha256WithRSA,
    name,
    seq(utcTime(new Date(now - 60_000)), utcTime(new Date(now + 86_400_000))),
    name,
    publicKey.export({ type: "spki", format: "der" }),
  );
  const cert = seq(tbs, sha256WithRSA, bits(sign("sha256", tbs, privateKey)));
  const b64 = cert.toString("base64").replace(/.{64}/g, "$&\n");
  return {
    keyPem: privateKey.export({ type: "pkcs8", format: "pem" }) as string,
    certPem: `-----BEGIN CERTIFICATE-----\n${b64.endsWith("\n") ? b64 : `${b64}\n`}-----END CERTIFICATE-----\n`,
    key: privateKey,
  };
}
