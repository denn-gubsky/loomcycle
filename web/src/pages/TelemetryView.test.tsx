// @vitest-environment happy-dom
import { act } from "react";
import { type Root, createRoot } from "react-dom/client";
import { MemoryRouter, Outlet, Route, Routes } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Principal } from "../api";
import TelemetryView, { parseRatio } from "./TelemetryView";

// The page is mounted for real (effects run, buttons clicked) because what it
// shows is decided by the GET it makes on mount: the operator's gate, the stored
// policy, the replica's status.

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const operator: Principal = {
  tenant_id: "acme",
  subject: "ops",
  scopes: ["substrate:tenant"],
  is_admin: false,
  legacy: false,
};

const stored = {
  tenant: "acme",
  policy: {
    endpoint: "https://otel.acme.example:4317",
    protocol: "grpc",
    headers_credential: "otel-headers",
    signals: ["traces"],
    sampler_ratio: 0.25,
    updated_at: "2026-11-10T09:00:00Z",
    updated_by: "ops",
  },
  status: {
    replica: "replica-7",
    exporting: true,
    exported_spans: 1234,
    dropped_spans: 5,
    export_failures: 2,
    last_success_at: "2026-11-11T08:00:00Z",
    last_error: "collector answered 503",
    last_error_at: "2026-11-11T07:00:00Z",
  },
};

type Call = { method: string; url: string; body?: string };

// stubServer answers each route from `routes` (keyed "METHOD path") and records
// every call; anything unlisted is a 404 with no code.
function stubServer(routes: Record<string, () => Response>): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? "GET";
      calls.push({ method, url, body: typeof init?.body === "string" ? init.body : undefined });
      const hit = routes[`${method} ${url}`];
      return hit ? hit() : new Response("404 page not found", { status: 404 });
    }),
  );
  return calls;
}

const json = (status: number, body: unknown) => () => new Response(JSON.stringify(body), { status });

async function settle() {
  for (let i = 0; i < 5; i++) await act(async () => new Promise((r) => setTimeout(r, 0)));
}

async function mount(principal: Principal | null): Promise<{ root: Root; el: HTMLElement }> {
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () =>
    root.render(
      <MemoryRouter>
        <Routes>
          <Route element={<Outlet context={{ principal }} />}>
            <Route path="/" element={<TelemetryView />} />
          </Route>
        </Routes>
      </MemoryRouter>,
    ),
  );
  await settle();
  return { root, el };
}

function button(el: HTMLElement, text: string): HTMLButtonElement {
  const b = [...el.querySelectorAll("button")].find((x) => x.textContent === text);
  if (!b) throw new Error(`no "${text}" button in: ${el.textContent}`);
  return b;
}

async function click(b: HTMLElement) {
  await act(async () => b.click());
  await settle();
}

describe("TelemetryView", () => {
  let mounted: { root: Root; el: HTMLElement } | null = null;
  afterEach(() => {
    if (mounted) act(() => mounted!.root.unmount());
    mounted?.el.remove();
    mounted = null;
    vi.unstubAllGlobals();
  });

  it("says the feature is off, with the operator's switch, when the routes answer feature_disabled", async () => {
    stubServer({
      "GET /v1/_telemetry": json(404, {
        code: "feature_disabled",
        error: "tenant telemetry export is not enabled on this deployment",
      }),
    });
    mounted = await mount(operator);
    const text = mounted.el.textContent ?? "";
    expect(text).toContain("Tenant telemetry export is not enabled on this deployment");
    expect(text).toContain("LOOMCYCLE_TENANT_TELEMETRY=1");
    expect(mounted.el.querySelector("form")).toBeNull();
    expect(mounted.el.querySelector(".settings-error")).toBeNull();
  });

  it("fills the form from the stored policy and shows the replica's export status", async () => {
    const calls = stubServer({ "GET /v1/_telemetry": json(200, stored) });
    mounted = await mount(operator);
    const el = mounted.el;
    // A tenant operator is not offered a tenant to pick: its own is the only one.
    expect(calls[0]).toMatchObject({ method: "GET", url: "/v1/_telemetry" });
    expect(el.querySelector("input[placeholder='tenant id']")).toBeNull();
    expect((el.querySelector("input[name=endpoint]") as HTMLInputElement).value).toBe("https://otel.acme.example:4317");
    expect((el.querySelector("select[name=protocol]") as HTMLSelectElement).value).toBe("grpc");
    expect((el.querySelector("input[name=headers_credential]") as HTMLInputElement).value).toBe("otel-headers");
    expect((el.querySelector("input[name=sampler_ratio]") as HTMLInputElement).value).toBe("0.25");
    const traces = el.querySelector(".telemetry-signals input[type=checkbox]") as HTMLInputElement;
    expect(traces.checked && traces.disabled).toBe(true);
    const text = el.textContent ?? "";
    for (const want of ["replica-7", "1234", "export failures", "2026-11-11T08:00:00Z", "collector answered 503", "2026-11-11T07:00:00Z"]) {
      expect(text).toContain(want);
    }
    expect(text).toContain("Content capture is not available");
  });

  it("shows a failed test span's error text verbatim, and a delivered one's time", async () => {
    let answer = { ok: false, error: "collector answered 401 Unauthorized", duration_ms: 42 };
    const calls = stubServer({
      "GET /v1/_telemetry": json(200, stored),
      "POST /v1/_telemetry:test?tenant=acme": () => new Response(JSON.stringify({ ...stored, test: answer }), { status: 200 }),
    });
    mounted = await mount(operator);
    await click(button(mounted.el, "send test span"));
    expect(calls.at(-1)).toMatchObject({ method: "POST", url: "/v1/_telemetry:test?tenant=acme" });
    expect(mounted.el.textContent).toContain("test span failed after 42 ms: collector answered 401 Unauthorized");

    answer = { ok: true, error: "", duration_ms: 17 } as typeof answer;
    await click(button(mounted.el, "send test span"));
    expect(mounted.el.textContent).toContain("test span delivered in 17 ms");
    expect(mounted.el.textContent).not.toContain("401 Unauthorized");
  });

  it("saves the form as the PUT body and shows a refused policy's reason without the status line", async () => {
    const calls = stubServer({
      "GET /v1/_telemetry": json(200, stored),
      "PUT /v1/_telemetry?tenant=acme": json(400, {
        code: "bad_request",
        error: 'headers_credential "otel-headers" may be sent anywhere: restrict its allowed_urls to the endpoint https://otel.acme.example:4317',
      }),
    });
    mounted = await mount(operator);
    await click(button(mounted.el, "save"));
    const put = calls.find((c) => c.method === "PUT")!;
    expect(JSON.parse(put.body!)).toEqual({
      endpoint: "https://otel.acme.example:4317",
      protocol: "grpc",
      signals: ["traces"],
      headers_credential: "otel-headers",
      sampler_ratio: 0.25,
    });
    const err = mounted.el.querySelector(".settings-error")?.textContent ?? "";
    expect(err).toContain("restrict its allowed_urls to the endpoint");
    expect(err).not.toContain("400");
  });

  it("removes the policy only after the operator confirms", async () => {
    const calls = stubServer({
      "GET /v1/_telemetry": json(200, stored),
      "DELETE /v1/_telemetry?tenant=acme": json(200, { tenant: "acme", policy: null, deleted: true, status: { exporting: false, exported_spans: 0, dropped_spans: 0, export_failures: 0 } }),
    });
    const confirm = vi.fn(() => false);
    vi.stubGlobal("confirm", confirm);
    mounted = await mount(operator);
    await click(button(mounted.el, "remove"));
    expect(confirm).toHaveBeenCalledOnce();
    expect(calls.some((c) => c.method === "DELETE")).toBe(false);

    confirm.mockReturnValue(true);
    await click(button(mounted.el, "remove"));
    expect(calls.some((c) => c.method === "DELETE")).toBe(true);
    expect((mounted.el.querySelector("input[name=endpoint]") as HTMLInputElement).value).toBe("");
    expect(mounted.el.textContent).toContain("policy removed");
  });

  it("offers an admin the tenant to manage, and loads the one picked", async () => {
    const calls = stubServer({
      "GET /v1/_telemetry": json(400, {
        code: "bad_request",
        error: "name the tenant: a telemetry policy belongs to one tenant, and work of no tenant is never exported to one",
      }),
      "GET /v1/_telemetry?tenant=acme": json(200, stored),
    });
    mounted = await mount({ ...operator, scopes: ["substrate:admin"], is_admin: true, tenant_id: "" });
    expect(mounted.el.textContent).toContain("name the tenant");
    const field = mounted.el.querySelector("input[placeholder='tenant id']") as HTMLInputElement;
    expect(field).not.toBeNull();
    await act(async () => {
      Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!.call(field, "acme");
      field.dispatchEvent(new Event("input", { bubbles: true }));
    });
    await click(button(mounted.el, "load"));
    expect(calls.at(-1)).toMatchObject({ method: "GET", url: "/v1/_telemetry?tenant=acme" });
    expect((mounted.el.querySelector("input[name=endpoint]") as HTMLInputElement).value).toBe("https://otel.acme.example:4317");
  });
});

describe("parseRatio", () => {
  it("reads empty as the default, a number in [0, 1] as itself, and anything else as refused", () => {
    expect(parseRatio("")).toBeUndefined();
    expect(parseRatio(" 0 ")).toBe(0);
    expect(parseRatio("0.5")).toBe(0.5);
    expect(parseRatio("1")).toBe(1);
    expect(parseRatio("1.5")).toBeNull();
    expect(parseRatio("-0.1")).toBeNull();
    expect(parseRatio("half")).toBeNull();
  });
});
