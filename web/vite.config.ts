import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// loomcycle UI build configuration.
//
// Output: `dist/` — embedded into the Go binary at build time via
// `internal/api/http/ui.go` (go:embed). The binary serves the SPA at
// `/ui` and the static assets under `/ui/assets/*`.
//
// Dev server: `npm run dev` runs at http://localhost:5173 with the
// proxy below forwarding /v1 calls to the loomcycle binary on
// http://localhost:8787 (the default LOOMCYCLE_LISTEN_ADDR). The
// production build embeds, so this proxy only matters during
// development.
export default defineConfig({
  plugins: [react()],
  base: "/ui/",
  build: {
    // Output INTO the Go package that owns the go:embed declaration.
    // `internal/webui` is the only Go package allowed to reference
    // these assets; api/http mounts the handler the package returns.
    // Path is relative to web/, the Vite project root.
    outDir: "../internal/webui/dist",
    // emptyOutDir: false preserves the committed `.gitkeep` placeholder
    // so go:embed always has at least one matching file, even when the
    // operator hasn't run npm build yet. The Makefile `build-ui` target
    // does an explicit clean before invoking Vite when a fresh build is
    // wanted.
    emptyOutDir: false,
    // No sourcemaps in production — they'd bloat the binary the SPA
    // is embedded into (~2 MB inlined source map for the v0.7.3
    // footprint). UI bugs are debugged locally with `npm run dev`,
    // which has full source maps via Vite's dev pipeline.
    sourcemap: false,
    // Cap chunk size: the v0.7.3 footprint is small (React + router);
    // the warning catches a future regression where someone bundles a
    // 500 KB chart library without thinking.
    chunkSizeWarningLimit: 512,
  },
  server: {
    port: 5173,
    proxy: {
      "/v1": "http://localhost:8787",
    },
  },
});
