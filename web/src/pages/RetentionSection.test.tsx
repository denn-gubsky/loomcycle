// @vitest-environment happy-dom
import { act } from "react";
import { type Root, createRoot } from "react-dom/client";
import { MemoryRouter, Outlet, Route, Routes } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import SectionPage from "./SectionPages";

// The retention page is mounted for real: what it says about a restart is
// decided by the report it fetches on mount.

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

// What the sweeper runs with. The yaml may ask for something else; the report
// then carries pending_restart and still describes this.
const running = {
  no_trace_overrides: [],
  admin: true,
  enabled: true,
  interval_ms: 3_600_000,
  defs_mode: "off",
  defs_max_age_ms: 0,
  defs_keep_last_n: 0,
  chats_mode: "prune",
  chats_max_age_ms: 30 * 86_400_000,
  chats_internal_max_age_ms: 7 * 86_400_000,
  mem_mode: "off",
  mem_max_age_ms: 0,
  mem_content_mode: "off",
  mem_content_max_age_ms: 0,
  purgeable: { chats: 4 },
};

function stubReport(body: unknown) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) =>
      String(input) === "/v1/_retention"
        ? new Response(JSON.stringify(body), { status: 200 })
        : new Response("404 page not found", { status: 404 }),
    ),
  );
}

async function settle() {
  for (let i = 0; i < 5; i++) await act(async () => new Promise((r) => setTimeout(r, 0)));
}

// Open mode (no principal) is admin-equivalent, so the page is shown.
async function mount(): Promise<{ root: Root; el: HTMLElement }> {
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () =>
    root.render(
      <MemoryRouter>
        <Routes>
          <Route element={<Outlet context={{ principal: null }} />}>
            <Route path="/" element={<SectionPage id="retention" />} />
          </Route>
        </Routes>
      </MemoryRouter>,
    ),
  );
  await settle();
  return { root, el };
}

describe("RetentionSection", () => {
  let mounted: { root: Root; el: HTMLElement } | null = null;
  afterEach(() => {
    if (mounted) act(() => mounted!.root.unmount());
    mounted?.el.remove();
    mounted = null;
    vi.unstubAllGlobals();
  });

  it("says the yaml's settings wait for a restart when the report carries pending_restart", async () => {
    stubReport({ ...running, pending_restart: true });
    mounted = await mount();
    const notice = mounted.el.querySelector(".settings-warning");
    expect(notice?.textContent).toContain("take effect at the next restart");
    // The values shown are still the running sweeper's.
    expect(mounted.el.textContent).toContain("sweeper on, every 60 min");
    expect(mounted.el.textContent).toContain("older than 30d");
  });

  it("shows no restart notice when the yaml and the running sweeper agree", async () => {
    // The server omits the field in this state rather than sending false.
    stubReport(running);
    mounted = await mount();
    expect(mounted.el.textContent).toContain("sweeper on, every 60 min");
    expect(mounted.el.querySelector(".settings-warning")).toBeNull();
  });
});
