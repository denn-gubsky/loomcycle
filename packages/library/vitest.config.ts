import { defineConfig } from "vitest/config";
import { fileURLToPath } from "node:url";

// Tests run against the def-fields SOURCE, as the typecheck does (see the paths
// note in tsconfig.json): the Library may use def-fields exports newer than the
// last published version, and a test pinned against a stale registry would be
// checking the wrong thing.
export default defineConfig({
  resolve: {
    alias: {
      "@loomcycle/def-fields": fileURLToPath(new URL("../def-fields/src/index.ts", import.meta.url)),
    },
  },
});
