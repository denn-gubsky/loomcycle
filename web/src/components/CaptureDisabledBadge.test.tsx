import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it } from "vitest";
import CaptureDisabledBadge, { captureDisabledKeys } from "./CaptureDisabledBadge";
import ScheduleDetailPane from "./ScheduleDetailPane";
import { renderWebhook } from "../pages/IntegrationsView";

const marked = {
  enabled: false,
  capture_disabled: { stripped_credentials: ["jobs", "slack"] },
};

describe("captureDisabledKeys", () => {
  it("reads the stripped keys off a marked def and nothing off an unmarked one", () => {
    expect(captureDisabledKeys(marked)).toEqual(["jobs", "slack"]);
    expect(captureDisabledKeys({ enabled: false })).toEqual([]);
    expect(captureDisabledKeys(undefined)).toEqual([]);
    expect(captureDisabledKeys({ capture_disabled: { stripped_credentials: "jobs" } })).toEqual([]);
  });
});

describe("CaptureDisabledBadge", () => {
  it("names every missing key and how to re-enable the def", () => {
    const html = renderToStaticMarkup(createElement(CaptureDisabledBadge, { def: marked }));
    expect(html).toContain("Disabled until credentials are re-supplied: jobs, slack");
    expect(html).toContain("Fork this definition");
  });

  it("renders nothing for a def without the marker", () => {
    expect(renderToStaticMarkup(createElement(CaptureDisabledBadge, { def: { enabled: false } }))).toBe("");
  });

  it("is shown in the schedule detail pane", () => {
    const html = renderToStaticMarkup(
      createElement(ScheduleDetailPane, {
        entry: {
          name: "digest",
          source: "static-only",
          in_static: true,
          in_substrate: false,
          static_definition: marked,
        },
        onMutated: () => {},
        onForkTemplate: () => {},
      }),
    );
    expect(html).toContain("Disabled until credentials are re-supplied: jobs, slack");
  });

  it("is shown in the webhook definition view", () => {
    const html = renderToStaticMarkup(
      createElement(
        MemoryRouter,
        null,
        renderWebhook({
          def_id: "wh_1",
          name: "gh",
          version: 2,
          created_at: "2026-10-01T00:00:00Z",
          definition: { delivery: "spawn", agent: "intake", ...marked },
        }),
      ),
    );
    expect(html).toContain("Disabled until credentials are re-supplied: jobs, slack");
  });
});
