/**
 * Tests for calc() → POST /v1/_calc (RFC GY P4b): the request it sends, the
 * result it returns, and how each kind of refusal surfaces. The bodies are the
 * server's own (internal/api/http/calc_api.go calcRefusalBody).
 */

import { describe, it, expect } from "vitest";
import { makeClient, jsonResponse } from "./helpers.js";
// Through the package index, as a consumer imports them: a name left out of
// the index fails this file's typecheck.
import {
  CalcError,
  InvalidArgumentError,
  LoomcycleError,
  RateLimitedError,
} from "../src/index.js";
import type { CalcErrorCategory, CalcInput, CalcResult, CalcValueType } from "../src/index.js";

const category: CalcErrorCategory = "validation";
const valueType: CalcValueType = "number";

describe("calc", () => {
  it("POSTs the input unchanged to /v1/_calc and returns the tool's result", async () => {
    const result: CalcResult = {
      result: 0.33333,
      type: valueType,
      exact: true,
      fraction: "1/3",
      text: "0.33333",
    };
    const { client, fetchMock } = makeClient([jsonResponse(result)]);

    const input: CalcInput = { expr: "x / 3", vars: { x: "1" }, digits: 5, seed: 7 };
    const res = await client.calc(input);

    expect(res).toEqual(result);
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_calc");
    expect(init!.method).toBe("POST");
    const headers = init!.headers as Record<string, string>;
    expect(headers["Content-Type"]).toBe("application/json");
    expect(headers.Authorization).toBe("Bearer test-bearer");
    expect(JSON.parse(init!.body as string)).toEqual({ expr: "x / 3", vars: { x: "1" }, digits: 5, seed: 7 });
  });

  it("sends only expr when no option is given", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ result: 0.3, type: "number", exact: true, text: "0.3" }),
    ]);
    await client.calc({ expr: "0.1 + 0.2" });
    expect(JSON.parse(fetchMock.mock.calls[0]![1]!.body as string)).toEqual({ expr: "0.1 + 0.2" });
  });

  it("raises a validation refusal (400) as CalcError with the tool's code, hint, pos and category", async () => {
    const { client } = makeClient([
      jsonResponse(
        {
          code: "syntax",
          error: "`**` is not an operator",
          detail: "`**` is not an operator",
          hint: "power is `^`, e.g. 2^10",
          pos: 2,
          errorCategory: category,
          isRetryable: false,
          description: "power is `^`, e.g. 2^10",
        },
        400,
      ),
    ]);

    const err = await client.calc({ expr: "2 ** 3" }).catch((e: unknown) => e);

    expect(err).toBeInstanceOf(CalcError);
    expect(err).toBeInstanceOf(LoomcycleError);
    const e = err as CalcError;
    expect(e.name).toBe("CalcError");
    expect(e.status).toBe(400);
    expect(e.code).toBe("syntax");
    expect(e.message).toBe("`**` is not an operator");
    expect(e.detail).toBe("`**` is not an operator");
    expect(e.hint).toBe("power is `^`, e.g. 2^10");
    expect(e.pos).toBe(2);
    expect(e.category).toBe("validation");
    expect(e.retryable).toBe(false);
  });

  // 422 maps to SnapshotVersionError on every other route; a Calc business
  // refusal must not surface as one.
  it("raises a business refusal (422) as CalcError, not SnapshotVersionError", async () => {
    const { client } = makeClient([
      jsonResponse(
        {
          code: "domain",
          error: "sqrt(-1) is not a real number",
          detail: "sqrt(-1) is not a real number",
          hint: "there are no complex numbers in this version",
          pos: 4,
          errorCategory: "business",
          isRetryable: false,
        },
        422,
      ),
    ]);

    const e = (await client.calc({ expr: "sqrt(-1)" }).catch((x: unknown) => x)) as CalcError;

    expect(e).toBeInstanceOf(CalcError);
    expect(e.status).toBe(422);
    expect(e.code).toBe("domain");
    expect(e.category).toBe("business");
    expect(e.pos).toBe(4);
    expect(e.retryable).toBe(false);
  });

  it("reads retryable from the server on a transient refusal (503), and leaves pos and hint unset when absent", async () => {
    const { client } = makeClient([
      jsonResponse(
        {
          code: "canceled",
          error: "the evaluation was canceled",
          detail: "the evaluation was canceled",
          errorCategory: "transient",
          isRetryable: true,
        },
        503,
      ),
    ]);

    const e = (await client.calc({ expr: "1" }).catch((x: unknown) => x)) as CalcError;

    expect(e).toBeInstanceOf(CalcError);
    expect(e.category).toBe("transient");
    expect(e.retryable).toBe(true);
    expect(e.pos).toBeUndefined();
    expect(e.hint).toBeUndefined();
  });

  it("raises a refusal made before the tool ran (no category) through the shared status table", async () => {
    const { client } = makeClient([
      jsonResponse({ code: "bad_request", error: "empty body" }, 400),
    ]);

    const e = await client.calc({ expr: "" }).catch((x: unknown) => x);

    expect(e).toBeInstanceOf(InvalidArgumentError);
    expect(e).not.toBeInstanceOf(CalcError);
    expect((e as InvalidArgumentError).code).toBe("bad_request");
  });

  it("raises a spent run rate (429) as RateLimitedError with Retry-After", async () => {
    const { client } = makeClient([
      () =>
        new Response(JSON.stringify({ code: "rate_limited", error: "run rate exceeded" }), {
          status: 429,
          headers: { "Content-Type": "application/json", "Retry-After": "3" },
        }),
    ]);

    const e = (await client.calc({ expr: "1" }).catch((x: unknown) => x)) as RateLimitedError;

    expect(e).toBeInstanceOf(RateLimitedError);
    expect(e.retryAfterMs).toBe(3000);
    expect(e.retryable).toBe(true);
  });
});
