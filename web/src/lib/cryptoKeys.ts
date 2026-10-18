// Pure logic behind the crypto-key rows of the credentials panel (RFC EP): which
// actions a key offers, the bounds of "retire versions below…", and the compact
// version list. Kept out of the component so it can be tested without a DOM.
//
// A crypto key is a credential with usage "crypto": a key type fixed at creation,
// numbered versions, and a min_version below which versions are retired (they no
// longer decrypt or verify, and may be deleted). Its material is never returned.

import type { CredentialMeta, CryptoKeyVersion } from "../api";

// The key types and import encodings the server accepts. Order is the select's.
export const CRYPTO_KEY_TYPES = [
  "aes-256-gcm",
  "xchacha20-poly1305",
  "hmac-sha256",
  "hmac-sha512",
  "ed25519",
  "ecdsa-p256",
  "totp",
  "x25519",
  "rsa-oaep-2048",
  "rsa-oaep-3072",
  "rsa-oaep-4096",
  "rsa-pss-2048",
  "rsa-pss-3072",
  "rsa-pss-4096",
] as const;
export type CryptoKeyType = (typeof CRYPTO_KEY_TYPES)[number];

export const CRYPTO_KEY_ENCODINGS = ["raw", "base64", "hex", "base32", "pem"] as const;
export type CryptoKeyEncoding = (typeof CRYPTO_KEY_ENCODINGS)[number];

export function isCryptoKey(m: Pick<CredentialMeta, "usage">): boolean {
  return m.usage === "crypto";
}

// isExternalKey: a crypto key an external KMS holds. Only a reference is
// stored; its versions live in the KMS and are rotated and retired there.
export function isExternalKey(m: Pick<CredentialMeta, "key_backend">): boolean {
  return !!m.key_backend;
}

const BACKEND_LABELS: Record<string, string> = {
  vault: "Vault Transit",
  aws_kms: "AWS KMS",
  gcp_kms: "Google Cloud KMS",
};

// keyBackendLabel names the KMS for display; an unknown one as the server sent it.
export function keyBackendLabel(backend: string): string {
  return BACKEND_LABELS[backend] ?? backend;
}

// current/min with the server's floor: a key always has version 1 at least, and
// min_version starts at 1. Absent fields (an older server) read as that floor.
function versionsOf(m: CredentialMeta): { current: number; min: number } {
  const current = Math.max(1, m.current_version ?? 1);
  // min_version may be current+1: every version retired, the key unusable
  // until it is rotated, and deletable.
  const min = Math.min(current + 1, Math.max(1, m.min_version ?? 1));
  return { current, min };
}

// MinVersionBounds is the inclusive range a new min_version may take: it only
// goes up, at most to current+1, which retires every version (the key then
// does nothing until rotated, and only then can it be deleted). null when
// every version is already retired.
export interface MinVersionBounds {
  lo: number;
  hi: number;
}

export function minVersionBounds(m: CredentialMeta): MinVersionBounds | null {
  const { current, min } = versionsOf(m);
  if (min > current) return null;
  return { lo: min + 1, hi: current + 1 };
}

// parseMinVersion reads the number input: a whole number inside the bounds, or
// null (the apply button stays disabled).
export function parseMinVersion(m: CredentialMeta, text: string): number | null {
  const b = minVersionBounds(m);
  if (!b) return null;
  const t = text.trim();
  if (!/^\d+$/.test(t)) return null;
  const n = Number(t);
  return n >= b.lo && n <= b.hi ? n : null;
}

// retireWarning names exactly which versions a new min_version retires and what
// that costs. Shown before the operator applies it.
export function retireWarning(m: CredentialMeta, newMin: number): string {
  const { min, current } = versionsOf(m);
  const first = min;
  const last = newMin - 1;
  const which = first === last ? `version ${first}` : `versions ${first}–${last}`;
  const all =
    newMin > current
      ? ` That is every version: "${m.name}" stops encrypting, signing and ` +
        `decrypting entirely until it is rotated, and can then be deleted.`
      : "";
  return (
    `This retires ${which} of "${m.name}". Anything encrypted or signed with a ` +
    `retired version stops decrypting or verifying unless it is rewrapped to a ` +
    `newer version first.${all} min_version can only be raised — this cannot be undone.`
  );
}

// CryptoKeyActions is what a key's row offers.
export interface CryptoKeyActions {
  rotate: boolean;
  // Retire versions below… — only when there is something to retire without
  // touching the current version, i.e. current > min.
  retire: boolean;
  // Versions the "delete version" control is offered for: retired ones only.
  deletableVersions: number[];
  // Delete is always offered; the server refuses it while any version is
  // unretired, and the panel shows that refusal.
  delete: boolean;
}

export function cryptoKeyActions(m: CredentialMeta): CryptoKeyActions {
  if (isExternalKey(m)) {
    // The server refuses every version action on it; delete removes only the
    // reference.
    return { rotate: false, retire: false, deletableVersions: [], delete: true };
  }
  return {
    rotate: true,
    retire: minVersionBounds(m) !== null,
    deletableVersions: sortedVersions(m)
      .filter((v) => isRetired(m, v))
      .map((v) => v.version),
    delete: true,
  };
}

// A version is retired when the server says so, or (a defensive fallback for a
// row without the flag) when it is below min_version.
function isRetired(m: CredentialMeta, v: CryptoKeyVersion): boolean {
  if (typeof v.retired === "boolean") return v.retired;
  return v.version < versionsOf(m).min;
}

function sortedVersions(m: CredentialMeta): CryptoKeyVersion[] {
  return [...(m.versions ?? [])].sort((a, b) => a.version - b.version);
}

// VersionChip is one entry of the compact version list: a single version, or a
// run of consecutive retired versions collapsed into one range.
export interface VersionChip {
  label: string;
  retired: boolean;
  current: boolean;
}

// versionChips renders the version list compactly, oldest first. Consecutive
// retired versions collapse into a range ("v1–v4"), since a long-lived key may
// have many; unretired versions are listed one by one, the current one marked.
export function versionChips(m: CredentialMeta): VersionChip[] {
  const { current } = versionsOf(m);
  const out: VersionChip[] = [];
  let run: number[] = [];
  const flush = () => {
    if (run.length === 0) return;
    const a = run[0];
    const b = run[run.length - 1];
    out.push({ label: a === b ? `v${a}` : `v${a}–v${b}`, retired: true, current: false });
    run = [];
  };
  for (const v of sortedVersions(m)) {
    if (isRetired(m, v)) {
      // A gap (a deleted version) breaks the run.
      if (run.length > 0 && v.version !== run[run.length - 1] + 1) flush();
      run.push(v.version);
      continue;
    }
    flush();
    out.push({ label: `v${v.version}`, retired: false, current: v.version === current });
  }
  flush();
  return out;
}

// versionSummary is the one-line "current v3 · min v2" shown under the name.
export function versionSummary(m: CredentialMeta): string {
  if (m.key_backend) {
    return `held in ${keyBackendLabel(m.key_backend)} · versions managed there`;
  }
  const { current, min } = versionsOf(m);
  return `current v${current} · min v${min}`;
}

// expiryToRFC3339 converts a <input type="datetime-local"> value (local wall
// time, no zone) to the RFC3339 UTC timestamp the server parses. "" → undefined
// (no expiry); an unparseable value → null so the form can refuse it.
export function expiryToRFC3339(local: string): string | undefined | null {
  const t = local.trim();
  if (!t) return undefined;
  const d = new Date(t);
  if (Number.isNaN(d.getTime())) return null;
  // Whole seconds: the server's layout is RFC3339 without a fraction.
  return d.toISOString().replace(/\.\d{3}Z$/, "Z");
}

// refusalText pulls the human-readable refusal out of a failed request's error
// message. jsonFetch throws "<status> <text>: <body, first 200 chars>", and the
// credential route's body is {"error": "...", "description"?: "..."}. The body
// may be cut mid-JSON, so the fields are matched rather than parsed. Anything
// else comes back unchanged.
export function refusalText(msg: string): string {
  const body = msg.replace(/^\d{3}[^:]*:\s*/, "");
  try {
    const env = JSON.parse(body) as { error?: unknown; description?: unknown };
    if (typeof env.error === "string" && env.error) {
      return typeof env.description === "string" && env.description
        ? `${env.error} — ${env.description}`
        : env.error;
    }
  } catch {
    const m = /"error"\s*:\s*"((?:[^"\\]|\\.)*)"?/.exec(body);
    if (m && m[1]) {
      try {
        return JSON.parse(`"${m[1]}"`) as string;
      } catch {
        return m[1];
      }
    }
  }
  return msg;
}
