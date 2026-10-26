// The server masks the secret-bearing fields of a yaml MCP server before the
// Library sees them (header and env values, URL userinfo, query values and
// fragment become "****"). A fork seeded from that definition would save the
// mask as the real value and break the connection, so the editor refuses to
// submit while one is still there.
export const MASKED_VALUE = "****";

export function maskedSecretProblem(fields: {
  url?: string;
  headers?: { key: string; value: string }[];
  env?: { key: string; value: string }[];
}): string | null {
  const hint =
    "the Library never shows a configured secret. Enter the value, or a $cred:<name> or ${run.credentials.<name>} reference.";
  if (fields.url?.includes(MASKED_VALUE)) return `The URL holds a masked value (${MASKED_VALUE}) — ${hint}`;
  for (const [label, rows] of [
    ["Header", fields.headers],
    ["Env var", fields.env],
  ] as const) {
    for (const { key, value } of rows ?? []) {
      if (key.trim() && value.includes(MASKED_VALUE)) {
        return `${label} ${key.trim()} holds a masked value (${MASKED_VALUE}) — ${hint}`;
      }
    }
  }
  return null;
}
