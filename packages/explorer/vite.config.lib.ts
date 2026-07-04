import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { fileURLToPath } from "node:url";

// Library build for the published @loomcycle/explorer package. Emits ESM + CJS
// to dist/; declarations come from `tsc -p tsconfig.lib.json` and styles.css is
// copied in — see the build:lib script. Everything imported by bare specifier
// (react, react-dom, @loomcycle/client) is externalized so it isn't duplicated
// into the consumer's bundle; only our own relative source is bundled.
export default defineConfig({
  plugins: [react()],
  build: {
    lib: {
      entry: fileURLToPath(new URL("./src/index.ts", import.meta.url)),
      formats: ["es", "cjs"],
      fileName: (format) => (format === "es" ? "index.js" : "index.cjs"),
    },
    outDir: "dist",
    sourcemap: true,
    emptyOutDir: true,
    rollupOptions: {
      external: (id) => !id.startsWith(".") && !id.startsWith("/"),
    },
  },
});
