import { fileURLToPath } from "node:url";
import path from "node:path";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Consume @loomcycle/library from SOURCE (not its built dist) so this Vite
// build compiles it as part of the SPA — no separate package build step in the
// embed pipeline (make build-ui just runs this build). The package emits the
// same class names as web's global styles.css, so we deliberately do NOT alias
// in the package's own styles.css (see LibraryView.tsx). The styles alias is
// kept as a defensive resolution target and MUST precede the bare-package key:
// @rollup/plugin-alias matches a string key against `id === key || id starts
// with key + "/"`, so the bare "@loomcycle/library" key would otherwise capture
// "@loomcycle/library/styles.css" and rewrite it to "<index.ts>/styles.css".
const webRoot = path.dirname(fileURLToPath(import.meta.url));
const libSrc = path.resolve(webRoot, "../packages/library/src/index.ts");
const libStyles = path.resolve(webRoot, "../packages/library/src/styles.css");

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
  resolve: {
    // Order matters: the more-specific "/styles.css" key must come first (see
    // the note above the libSrc/libStyles definitions).
    alias: {
      "@loomcycle/library/styles.css": libStyles,
      "@loomcycle/library": libSrc,
    },
    // We consume @loomcycle/library from SOURCE, which imports `react` /
    // `react-dom` (and the JSX runtime). Because the package has its OWN
    // node_modules (for its standalone build), Vite would otherwise resolve
    // those bare imports to packages/library/node_modules — a SECOND React
    // copy, whose hooks dispatcher is null in web's render tree
    // ("Cannot read properties of null (reading 'useMemo')"). dedupe forces a
    // single copy from web/node_modules. (@loomcycle/client too, for good
    // measure — a duplicate is wasteful even if not fatal.)
    dedupe: ["react", "react-dom", "react/jsx-runtime", "@loomcycle/client"],
  },
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
