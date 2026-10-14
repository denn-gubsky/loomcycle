/**
 * Verifying a signed loomcycle hook delivery.
 *
 * A HookDef whose webhook body names a `signing_secret` signs every call it
 * makes. The call carries
 *
 *     X-Loomcycle-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256>
 *
 * where the MAC is keyed by the secret and computed over `<t>.<raw body>` —
 * the exact bytes of the request body. `X-Loomcycle-Hook` names the hook and
 * `X-Loomcycle-Delivery` identifies the delivery (a retry keeps it), so a
 * receiver can drop one it has already handled.
 *
 * Web Crypto only (`crypto.subtle`), so the same function runs in Node and in
 * a browser or edge runtime. It is async because Web Crypto is.
 */

/** Options for {@link verifyLoomcycleSignature}. */
export interface VerifySignatureOptions {
  /** How far `t` may be from now, either way, in seconds. Default 300 — the
   *  window loomcycle's own receiver uses. A larger window lets a captured
   *  delivery be replayed for longer. */
  toleranceSec?: number;
  /** The current time in milliseconds since the epoch, as `Date.now()`
   *  returns. For tests; defaults to `Date.now()`. */
  now?: number;
  /** The Web Crypto implementation to use. Defaults to
   *  `globalThis.crypto.subtle`, which Node 19+ and every browser have; on
   *  Node 18 pass `require("node:crypto").webcrypto.subtle`. */
  subtle?: SubtleCrypto;
}

const DEFAULT_TOLERANCE_SEC = 300;
const HEX_RE = /^(?:[0-9a-fA-F]{2})+$/;
// What Go's strconv.ParseInt accepts, so a header loomcycle's own receiver
// would accept is accepted here too.
const UNIX_RE = /^[+-]?\d+$/;

/**
 * Reports whether `header` (the `X-Loomcycle-Signature` value) is a valid
 * signature of `rawBody` under `secret`, made within the tolerance of now.
 *
 * `rawBody` must be the request body exactly as received — the string or the
 * bytes, before any JSON parsing. A body parsed and re-serialized will not
 * match: key order, spacing and escapes all change the bytes. A string is
 * hashed as its UTF-8 encoding.
 *
 * Fails closed: a missing or repeated-garbage header, a `t` that is not an
 * integer, a stale or future `t`, a `v1` that is not hex, or a wrong MAC all
 * return `false`, with no reason given. The MAC is compared by Web Crypto's
 * HMAC verify, which compares in constant time. Throws only when no Web Crypto
 * implementation is available (a configuration problem, not a bad request).
 */
export async function verifyLoomcycleSignature(
  rawBody: string | Uint8Array,
  header: string,
  secret: string,
  opts: VerifySignatureOptions = {},
): Promise<boolean> {
  const subtle = opts.subtle ?? globalThis.crypto?.subtle;
  if (!subtle) {
    throw new TypeError(
      "verifyLoomcycleSignature: no Web Crypto implementation; pass opts.subtle (on Node 18: require('node:crypto').webcrypto.subtle)",
    );
  }
  if (typeof header !== "string" || typeof secret !== "string" || secret === "") {
    return false;
  }
  let ts = "";
  let macHex = "";
  for (const raw of header.split(",")) {
    const part = raw.trim();
    if (part.startsWith("t=")) ts = part.slice(2);
    else if (part.startsWith("v1=")) macHex = part.slice(3);
  }
  if (!UNIX_RE.test(ts) || !HEX_RE.test(macHex)) {
    return false;
  }
  const t = Number(ts);
  if (!Number.isSafeInteger(t)) {
    return false;
  }
  const tolerance = opts.toleranceSec ?? DEFAULT_TOLERANCE_SEC;
  const nowSec = (opts.now ?? Date.now()) / 1000;
  if (!(tolerance >= 0) || Math.abs(nowSec - t) > tolerance) {
    return false;
  }

  const enc = new TextEncoder();
  const body = typeof rawBody === "string" ? enc.encode(rawBody) : rawBody;
  // The MAC covers t exactly as written in the header, then ".", then the body.
  const prefix = enc.encode(ts + ".");
  const signed = new Uint8Array(prefix.length + body.length);
  signed.set(prefix, 0);
  signed.set(body, prefix.length);

  const mac = new Uint8Array(macHex.length / 2);
  for (let i = 0; i < mac.length; i++) {
    mac[i] = parseInt(macHex.slice(i * 2, i * 2 + 2), 16);
  }
  try {
    const key = await subtle.importKey("raw", enc.encode(secret), { name: "HMAC", hash: "SHA-256" }, false, ["verify"]);
    return await subtle.verify("HMAC", key, mac, signed);
  } catch {
    return false;
  }
}
