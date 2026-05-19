import { defineConfig } from "vitest/config";

// Vitest configuration — minimal. The TS adapter targets Node (per
// engines >= 18), so we use the node environment rather than jsdom.
// Tests live alongside src/ under tests/, mocking `fetch` via vi.fn().
export default defineConfig({
  test: {
    environment: "node",
    include: ["tests/**/*.test.ts"],
  },
});
