// copyToClipboard writes text to the system clipboard and returns whether it
// succeeded. It is a standalone helper (not inlined in the button) so the copy
// affordance's success/failure branch is unit-testable without a DOM — the UI
// only needs the boolean. It guards a missing clipboard (an insecure context, an
// old browser, or a non-browser test env) by returning false rather than
// throwing, so a copy button can never crash the viewer.
export async function copyToClipboard(text: string): Promise<boolean> {
  try {
    if (typeof navigator === "undefined" || !navigator.clipboard) return false;
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    return false;
  }
}
