import { defineConfig } from "vitest/config";
import { fileURLToPath } from "node:url";

// The tests run @loomcycle/code-editor from its sibling SOURCE, as the type
// check does (tsconfig paths) and as web's build does (its Vite alias), so the
// explorer is always tested against the editor in the same commit. Its
// @codemirror modules resolve from packages/code-editor/node_modules; react is
// deduped to this package's copy — a second React would break the hooks.
export default defineConfig({
  resolve: {
    // The styles key first: an alias key also matches every path under it.
    alias: {
      "@loomcycle/code-editor/styles.css": fileURLToPath(new URL("../code-editor/src/styles.css", import.meta.url)),
      "@loomcycle/code-editor": fileURLToPath(new URL("../code-editor/src/index.ts", import.meta.url)),
    },
    dedupe: ["react", "react-dom"],
  },
});
