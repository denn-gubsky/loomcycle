import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { LoomcycleClient } from "@loomcycle/client";
import { ErrorFormatterProvider, describeError, useErrorText } from "./errorText";
import { dataLayerFromConnection } from "./dataLayer";

const denied = Object.assign(new Error('{"code":"acl_denied","error":"author def:agent:a is denied by ops rule 0"}'), {
  status: 403,
});

// A host formatter that only knows access refusals, as the Web UI's does.
const hostFormat = (e: unknown) =>
  (e as { status?: number }).status === 403 ? "access denied — may not author def:agent:a" : undefined;

describe("describeError", () => {
  it("uses the host's wording for an error the host recognizes", () => {
    expect(describeError(denied, hostFormat)).toBe("access denied — may not author def:agent:a");
  });

  it("keeps the Library's own wording when the host has none", () => {
    expect(describeError(new Error("boom"), hostFormat)).toBe("boom");
    expect(describeError("plain", hostFormat)).toBe("plain");
  });

  it("keeps the Library's own wording when no host formatter is set", () => {
    expect(describeError(denied, undefined)).toBe(denied.message);
  });

  it("falls back to the caller's own explanation, not the bare message", () => {
    expect(describeError(new Error("x"), hostFormat, () => "explained")).toBe("explained");
    expect(describeError(denied, hostFormat, () => "explained")).toBe("access denied — may not author def:agent:a");
  });
});

function Probe({ error }: { error: unknown }) {
  const errorText = useErrorText();
  return createElement("span", null, errorText(error));
}

describe("useErrorText", () => {
  it("reads the host formatter the Library provides", () => {
    const html = renderToStaticMarkup(
      createElement(ErrorFormatterProvider, { value: hostFormat }, createElement(Probe, { error: denied })),
    );
    expect(html).toBe("<span>access denied — may not author def:agent:a</span>");
  });

  it("renders the bare message outside a provider", () => {
    expect(renderToStaticMarkup(createElement(Probe, { error: new Error("boom") }))).toBe("<span>boom</span>");
  });
});

describe("listHooks failure", () => {
  // The host formatter reads the status and the body, so the Library's own
  // fetch hands them on as the client's errors do.
  it("carries the status and the response body", async () => {
    const body = '{"code":"acl_denied","error":"no rule allows read def:hookdef:*"}';
    const fetchMock = async () => new Response(body, { status: 403 });
    const dl = dataLayerFromConnection({ baseUrl: "", fetch: fetchMock }, {} as LoomcycleClient);
    const err = await dl.listHooks!().then(
      () => null,
      (e: unknown) => e,
    );
    expect(err).toBeInstanceOf(Error);
    expect((err as Error).message).toBe("GET /v1/_hookdef/names: HTTP 403");
    expect(err).toMatchObject({ status: 403, bodyText: body });
  });
});
