// tokenAmount — human-friendly token amounts for the RFC AW limits editor.
//
// Token budgets are large (millions), so typing raw integers into the soft/hard
// fields is error-prone (a dropped or extra zero is a 10× budget mistake). These
// helpers let an operator type a shorthand — 500K, 5M, 2G — and see the exact
// value it resolves to, while the wire still carries a plain integer.

// The suffix multipliers (case-insensitive). B is an alias for G (billion), T is
// trillion — included for completeness though token counts rarely reach them.
const UNITS: Record<string, number> = {
  k: 1e3,
  m: 1e6,
  g: 1e9,
  b: 1e9,
  t: 1e12,
};

// parseTokenAmount converts a limits-editor entry to the wire value:
//   ""               → null       (tier unset — no ceiling)
//   "5000000"        → 5000000    (plain integer)
//   "500k" "5M" "1.5m" "2G" → the scaled integer (case-insensitive, optional
//                              decimal mantissa)
//   commas / underscores / spaces are ignored as digit separators
//   anything else    → undefined  (invalid — the caller flags it and never sends
//                                   garbage; the tri-state keeps "unset" (null)
//                                   distinct from "typo" (undefined))
export function parseTokenAmount(s: string): number | null | undefined {
  const t = s.trim().replace(/[_,\s]/g, "");
  if (t === "") return null;
  const m = /^(\d+(?:\.\d+)?)([kmgbt])?$/i.exec(t);
  if (!m) return undefined;
  const mult = m[2] ? UNITS[m[2].toLowerCase()] : 1;
  const n = Math.floor(parseFloat(m[1]) * mult);
  return Number.isFinite(n) && n >= 0 ? n : undefined;
}

// fmtTokens renders a counter compactly (1_500_000 → "1.50M"), K/M/G/T. The
// read-only `used` column uses it; it also mirrors the shorthand the editor
// accepts so display and input speak the same units.
export function fmtTokens(n: number): string {
  if (n >= 1e12) return (n / 1e12).toFixed(2) + "T";
  if (n >= 1e9) return (n / 1e9).toFixed(2) + "G";
  if (n >= 1e6) return (n / 1e6).toFixed(2) + "M";
  if (n >= 1e3) return (n / 1e3).toFixed(1) + "K";
  return String(n);
}

// amountHint is the live "recognition" feedback for an editor entry: the exact
// grouped integer once a shorthand or separator was applied ("5M" → "= 5,000,000"),
// or an invalid marker for a typo. Returns null when there's nothing worth
// showing — an empty field, or a plain integer that already reads as-is.
export function amountHint(s: string): { text: string; invalid: boolean } | null {
  const t = s.trim();
  if (t === "") return null;
  const v = parseTokenAmount(t);
  if (v === undefined) return { text: "not a number", invalid: true };
  if (v === null) return null;
  if (String(v) === t.replace(/[_,\s]/g, "")) return null; // plain integer, no transform
  return { text: "= " + v.toLocaleString("en-US"), invalid: false };
}
