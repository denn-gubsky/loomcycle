// The explorer's navigator choice: the Path tree (the default) or the slot a
// host provides (the 3D star map). Remembered per browser; storage failures
// (private mode, blocked site data) fall back to the tree.

export type NavigatorMode = "tree" | "stars";

export const NAVIGATOR_KEY = "loomcycle.explorer.navigator";

export function readNavigatorMode(storage: Pick<Storage, "getItem"> | undefined): NavigatorMode {
  try {
    return storage?.getItem(NAVIGATOR_KEY) === "stars" ? "stars" : "tree";
  } catch {
    return "tree";
  }
}

export function writeNavigatorMode(
  storage: Pick<Storage, "setItem"> | undefined,
  mode: NavigatorMode,
): void {
  try {
    storage?.setItem(NAVIGATOR_KEY, mode);
  } catch {
    // Blocked storage: the choice lasts for this page only.
  }
}
