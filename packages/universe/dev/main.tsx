// Dev harness (not published): the navigator over a synthetic scope, so the
// rendering, LOD and animations can be checked without a runtime. Size via
// ?docs=N&chunks=M&edges=K, theme via ?theme=light.
import { createRoot } from "react-dom/client";
import { UniverseView, type UniverseDataLayer } from "../src";
import { syntheticSnapshot } from "../src/lib/synthetic";
import "../src/styles.css";

const q = new URLSearchParams(location.search);
const docs = Number(q.get("docs") ?? 400);
const chunks = Number(q.get("chunks") ?? 18);
const edges = Number(q.get("edges") ?? 3);
const dirs = ["/loomcycle/rfcs", "/loomcycle/docs", "/loomcycle/docs/plans", "/loomcycle/marketing", "/facts", "/documents", "/documents/research", "/mtb/regulations", "/mtb/policies", "/mtb/policies/hr"];
const snap = syntheticSnapshot({
  docs,
  chunksPerDoc: chunks,
  edgesPerDoc: edges,
  dirs,
  projection: q.get("proj") === "0" ? undefined : { missingEvery: 50 },
});
// Recent updates on a few chunks, so the recency sparkle is visible.
const now = Date.now();
snap.chunks!.updated_at = snap.chunks!.ids.map((_, i) => (i % 37 === 0 ? now - (i % 5) * 3_600_000 : now - 30 * 86_400_000));
snap.documents!.type_code = snap.documents!.ids.map((_, i) => i % 2);
snap.codes.types = ["rfc", "research"];
snap.codes.statuses = ["draft", "confirmed", "done", "review"];
snap.documents!.status_code = snap.documents!.ids.map((_, i) => i % 4);
snap.chunks!.status_code = snap.chunks!.ids.map((_, i) => (i % 5) % 4);

const data: UniverseDataLayer = {
  graphSnapshot: async () => snap,
  search: async (query, _scope, opts) => {
    const idx = snap.chunks!.ids
      .map((_, i) => i)
      .filter((i) => snap.chunks!.titles[i].toLowerCase().includes(query.toLowerCase()))
      .slice(0, 20);
    // The query "lands" at the mean of its hits' projected points.
    let queryXyz: [number, number, number] | undefined;
    if (opts.withProjection && snap.projection && idx.length) {
      const p = snap.projection.xyz;
      const s = [0, 0, 0];
      for (const i of idx) for (let k = 0; k < 3; k++) s[k] += p[i * 3 + k];
      queryXyz = [s[0] / idx.length, s[1] / idx.length, s[2] / idx.length];
    }
    return { hits: idx.map((i, r) => ({ chunk_id: snap.chunks!.ids[i], score: 1 - r * 0.03 })), queryXyz };
  },
};

const theme = q.get("theme") === "light" ? "light" : undefined;
if (theme) document.body.style.background = "#f5f6f8";
const root = document.getElementById("root")!;
if (theme) root.setAttribute("data-theme", "light");
root.style.setProperty("--bg", theme ? "#f5f6f8" : "#0f1115");
createRoot(root).render(
  <UniverseView dataLayer={data} scope="user" theme={theme} onSelect={(s) => console.log("[universe] select", s)} />,
);
