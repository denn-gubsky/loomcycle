// @vitest-environment happy-dom
import { act } from "react";
import { type Root, createRoot } from "react-dom/client";
import { MemoryRouter, Outlet, Route, Routes } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import SectionPage from "./SectionPages";

// The health page is mounted for real: the configured-teams table is what an
// operator has besides the boot log to learn that a configured team was not
// written (RFC GS).

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

function stub(teams: unknown | null) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url === "/healthz") return new Response(JSON.stringify({ ok: true, version: "v1" }), { status: 200 });
      if (url === "/v1/_config/teams" && teams !== null) return new Response(JSON.stringify(teams), { status: 200 });
      return new Response("404 page not found", { status: 404 });
    }),
  );
}

async function settle() {
  for (let i = 0; i < 5; i++) await act(async () => new Promise((r) => setTimeout(r, 0)));
}

async function mount(): Promise<{ root: Root; el: HTMLElement }> {
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () =>
    root.render(
      <MemoryRouter>
        <Routes>
          <Route element={<Outlet context={{ principal: null }} />}>
            <Route path="/" element={<SectionPage id="health" />} />
          </Route>
        </Routes>
      </MemoryRouter>,
    ),
  );
  await settle();
  return { root, el };
}

describe("HealthSection configured teams", () => {
  let mounted: { root: Root; el: HTMLElement } | null = null;
  afterEach(() => {
    if (mounted) act(() => mounted!.root.unmount());
    mounted?.el.remove();
    mounted = null;
    vi.unstubAllGlobals();
  });

  it("shows a name conflict with its reason beside a written team", async () => {
    stub({
      teams: [
        { name: "sdlc", status: "written", active_def_id: "td_1" },
        { name: "release", status: "conflict", active_def_id: "td_2", detail: "made over the API" },
      ],
    });
    mounted = await mount();
    const rows = [...mounted.el.querySelectorAll("tr")].filter((r) => r.querySelector("code")?.textContent !== "v1");
    const byName = Object.fromEntries(rows.map((r) => [r.querySelector("code")?.textContent, r]));
    expect(byName["sdlc"]?.querySelector(".status-ok")?.textContent).toBe("written");
    expect(byName["release"]?.querySelector(".status-warn")?.textContent).toBe("conflict");
    expect(byName["release"]?.textContent).toContain("made over the API");
  });

  it("lists the removed items stored definitions still name, with this replica's run counts", async () => {
    stub({
      teams: [],
      deprecations: {
        config: [],
        runs: [{ kind: "tool", name: "Bashbox", count: 3 }],
      },
    });
    mounted = await mount();
    expect(mounted.el.textContent).toContain("Removed items still named by stored definitions");
    expect(mounted.el.textContent).not.toContain("Deprecated");
    expect(mounted.el.textContent).not.toContain("future release");
    expect(mounted.el.querySelector(".deprecations-config")).toBeNull();
    expect(mounted.el.querySelector(".deprecations-runs")?.textContent).toContain("Bashbox3 runs on this replica");
  });

  it("shows no removed-items heading when no run named one", async () => {
    stub({ teams: [], deprecations: { config: [], runs: [] } });
    mounted = await mount();
    expect(mounted.el.textContent).not.toContain("Removed items");
  });

  it("shows liveness alone when the server has no such route", async () => {
    stub(null);
    mounted = await mount();
    expect(mounted.el.textContent).toContain("v1");
    expect(mounted.el.textContent).not.toContain("Configured teams");
  });
});
