// CaptureDisabledBadge explains why a schedule or webhook restored from a
// snapshot is disabled. A snapshot never carries literal user_credentials, so
// the restore writes a server-set capture_disabled marker listing the stripped
// keys; the def stays disabled — whatever its `enabled` says — until a fork
// re-supplies every one. Without this the pane showed only "enabled: false"
// and nothing about how to fix it.

// captureDisabledKeys reads the marker's keys off a def body. Empty when the
// def is not marked.
export function captureDisabledKeys(def: Record<string, unknown> | undefined | null): string[] {
  const marker = def?.capture_disabled;
  if (!marker || typeof marker !== "object") return [];
  const keys = (marker as { stripped_credentials?: unknown }).stripped_credentials;
  return Array.isArray(keys) ? keys.filter((k): k is string => typeof k === "string") : [];
}

export default function CaptureDisabledBadge({
  def,
}: {
  def: Record<string, unknown> | undefined | null;
}) {
  const keys = captureDisabledKeys(def);
  if (keys.length === 0) return null;
  return (
    <div className="capture-disabled-badge" role="status">
      <strong>Disabled until credentials are re-supplied: {keys.join(", ")}</strong>
      <div className="capture-disabled-hint">
        A snapshot restore stripped these literal credentials. Fork this definition with
        enabled: true and every listed key, as a user_credentials value or a
        user_credentials_from_env variable the operator has allowlisted and set.
      </div>
    </div>
  );
}
