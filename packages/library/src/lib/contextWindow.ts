// contextWindowLabel renders an agent's starting context window (the agent
// library's context_window) compactly — 512, 8.2k, 200k, 1.0M — or null when
// it is unknown, so the caller renders nothing rather than "0".
//
// Kept local rather than shared with the Web UI's run metrics: this package is
// consumed on its own and depends on no sibling's source.
export function contextWindowLabel(n: number | undefined): string | null {
  if (!n || n <= 0) return null;
  if (n < 1000) return String(n);
  if (n < 1_000_000) return (n / 1000).toFixed(n < 10_000 ? 1 : 0) + "k";
  return (n / 1_000_000).toFixed(1) + "M";
}
