import { describe, expect, it } from "vitest";
import { traceURL } from "./traceLink";

const tid = "4bf92f3577b34da6a3ce929d0e0e4736";

describe("traceURL", () => {
  it("fills the run's trace id into the template", () => {
    expect(traceURL("https://grafana.example/explore?traceId={trace_id}", tid)).toBe(
      `https://grafana.example/explore?traceId=${tid}`,
    );
  });
  it("gives no link without both a template and a trace id", () => {
    expect(traceURL("", tid)).toBeNull();
    expect(traceURL("https://g.example/{trace_id}", undefined)).toBeNull();
    expect(traceURL("https://g.example/{trace_id}", "")).toBeNull();
  });
  it("refuses a scheme that would run in an href, and a malformed id", () => {
    expect(traceURL("javascript:alert('{trace_id}')", tid)).toBeNull();
    expect(traceURL("https://g.example/{trace_id}", "\"><script>")).toBeNull();
  });
});
