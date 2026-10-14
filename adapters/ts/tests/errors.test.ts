/**
 * Tests for fetch-helpers.ts:raiseFromResponse — the single source
 * of truth for HTTP status + body-text → typed-error dispatch.
 *
 * Mirrors the Python adapter's test_errors.py pattern: synthesize
 * a Response with a specific (status, body) combination and assert
 * the right typed error class is thrown.
 */

import { describe, expect, it } from "vitest";
import {
  AgentIDInUseError,
  AgentNotFoundError,
  AlreadyPausingError,
  AuthError,
  BackpressureError,
  InvalidArgumentError,
  LoomcycleError,
  NotPausedError,
  PauseNotConfiguredError,
  PerUserQuotaExhaustedError,
  SessionBusyError,
  SessionNotFoundError,
  SnapshotNotFoundError,
  SnapshotTooLargeError,
  SnapshotVersionError,
  UnavailableError,
} from "../src/errors.js";
import { raiseFromResponse } from "../src/fetch-helpers.js";
import { jsonResponse, makeClient } from "./helpers.js";
// Through the package index on purpose: a class that is thrown but not
// exported cannot be named in a consumer's `instanceof`, and only an import
// from here fails when the export is dropped.
import {
  PromptTooLargeError,
  RateLimitedError,
  RequestTooLargeError,
  TokenLimitExceededError,
} from "../src/index.js";

async function expectErrorFor(status: number, body: string) {
  const resp = new Response(body, { status });
  try {
    await raiseFromResponse(resp);
    throw new Error("expected raiseFromResponse to throw");
  } catch (e) {
    return e;
  }
}

describe("raiseFromResponse — status + body-text → typed error", () => {
  it("400 → InvalidArgumentError", async () => {
    expect(await expectErrorFor(400, "bad timeout")).toBeInstanceOf(
      InvalidArgumentError,
    );
  });

  it("401 → AuthError", async () => {
    expect(await expectErrorFor(401, "invalid token")).toBeInstanceOf(AuthError);
  });

  it("404 + 'snapshot' → SnapshotNotFoundError", async () => {
    expect(
      await expectErrorFor(404, "no snapshot with id snap_xyz"),
    ).toBeInstanceOf(SnapshotNotFoundError);
  });

  it("404 + 'session' → SessionNotFoundError", async () => {
    expect(
      await expectErrorFor(404, "session not found"),
    ).toBeInstanceOf(SessionNotFoundError);
  });

  it("404 (other) → AgentNotFoundError (catch-all)", async () => {
    expect(
      await expectErrorFor(404, "no run found for agent_id ax"),
    ).toBeInstanceOf(AgentNotFoundError);
  });

  it("404 + both 'snapshot' AND 'session' → SnapshotNotFoundError (priority)", async () => {
    // Documented priority: "snapshot" wins over "session" wins over agent
    expect(
      await expectErrorFor(
        404,
        "no snapshot with id snap_sess_foo (session reference incidental)",
      ),
    ).toBeInstanceOf(SnapshotNotFoundError);
  });

  it("409 + 'already_pausing' → AlreadyPausingError", async () => {
    expect(
      await expectErrorFor(409, "already_pausing: runtime is already pausing"),
    ).toBeInstanceOf(AlreadyPausingError);
  });

  it("409 + 'already paused' → AlreadyPausingError", async () => {
    expect(
      await expectErrorFor(409, "runtime already paused"),
    ).toBeInstanceOf(AlreadyPausingError);
  });

  it("409 + 'not_paused' → NotPausedError", async () => {
    expect(
      await expectErrorFor(409, "not_paused: cannot resume"),
    ).toBeInstanceOf(NotPausedError);
  });

  it("409 + 'session' → SessionBusyError", async () => {
    expect(
      await expectErrorFor(409, "session busy: another request in flight"),
    ).toBeInstanceOf(SessionBusyError);
  });

  it("409 + 'agent_id' → AgentIDInUseError", async () => {
    expect(
      await expectErrorFor(409, "agent_id in use"),
    ).toBeInstanceOf(AgentIDInUseError);
  });

  it("409 (other) → LoomcycleError (base)", async () => {
    const e = await expectErrorFor(409, "some other conflict");
    expect(e).toBeInstanceOf(LoomcycleError);
    expect(e instanceof AlreadyPausingError).toBe(false);
  });

  it("413 → SnapshotTooLargeError", async () => {
    expect(
      await expectErrorFor(413, "snapshot exceeds size cap"),
    ).toBeInstanceOf(SnapshotTooLargeError);
  });

  it("422 → SnapshotVersionError", async () => {
    expect(
      await expectErrorFor(422, "snapshot section version too new"),
    ).toBeInstanceOf(SnapshotVersionError);
  });

  it("429 → BackpressureError", async () => {
    expect(await expectErrorFor(429, "queue full")).toBeInstanceOf(
      BackpressureError,
    );
  });

  it("429 + code:per_user_quota_exhausted → PerUserQuotaExhaustedError", async () => {
    // v0.10.1: the shape distinguishes from BackpressureError via the
    // JSON body's `code` field. PerUserQuotaExhaustedError carries
    // userId + cap + retryAfterMs derived from the body + header.
    const body = JSON.stringify({
      code: "per_user_quota_exhausted",
      error: "per-user quota exhausted: user=user_a cap=4",
      user_id: "user_a",
      cap: 4,
    });
    const resp = new Response(body, {
      status: 429,
      headers: { "Retry-After": "5", "Content-Type": "application/json" },
    });
    try {
      await raiseFromResponse(resp);
      throw new Error("expected raiseFromResponse to throw");
    } catch (e) {
      expect(e).toBeInstanceOf(PerUserQuotaExhaustedError);
      // It's NOT a BackpressureError — distinct branch.
      expect(e).not.toBeInstanceOf(BackpressureError);
      const pue = e as PerUserQuotaExhaustedError;
      expect(pue.userId).toBe("user_a");
      expect(pue.cap).toBe(4);
      expect(pue.retryAfterMs).toBe(5000);
    }
  });

  it("429 + code:backpressure → BackpressureError (not the v0.10.1 typed flavor)", async () => {
    // Sanity-check that the v0.9.x backpressure body still routes to
    // BackpressureError — the per_user_quota_exhausted branch must
    // ONLY match the explicit code.
    const body = JSON.stringify({ code: "backpressure", error: "queue full" });
    const resp = new Response(body, { status: 429 });
    try {
      await raiseFromResponse(resp);
      throw new Error("expected throw");
    } catch (e) {
      expect(e).toBeInstanceOf(BackpressureError);
      expect(e).not.toBeInstanceOf(PerUserQuotaExhaustedError);
    }
  });

  it("503 + 'pause manager not configured' → PauseNotConfiguredError", async () => {
    const e = await expectErrorFor(
      503,
      "pause manager not configured on this server",
    );
    expect(e).toBeInstanceOf(PauseNotConfiguredError);
    // back-compat: PauseNotConfiguredError IS-A UnavailableError
    expect(e).toBeInstanceOf(UnavailableError);
  });

  it("503 (other) → UnavailableError (not the more specific PauseNotConfiguredError)", async () => {
    const e = await expectErrorFor(503, "service unavailable");
    expect(e).toBeInstanceOf(UnavailableError);
    expect(e instanceof PauseNotConfiguredError).toBe(false);
  });

  it("500 → LoomcycleError (base) — unknown server error", async () => {
    expect(await expectErrorFor(500, "boom")).toBeInstanceOf(LoomcycleError);
  });

  it("error carries status + truncated bodyText", async () => {
    const e = (await expectErrorFor(401, "invalid token")) as AuthError;
    expect(e.status).toBe(401);
    expect(e.bodyText).toBe("invalid token");
  });

  it("LoomcycleError.bodyText is truncated to 1024 chars", async () => {
    const longBody = "x".repeat(5000);
    const e = (await expectErrorFor(500, longBody)) as LoomcycleError;
    expect(e.bodyText?.length).toBe(1024);
  });

  it("empty body falls back to a status-text message", async () => {
    const resp = new Response("", { status: 401 });
    let caught: unknown;
    try {
      await raiseFromResponse(resp);
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(AuthError);
    // empty body → message comes from status + statusText
    expect((caught as Error).message).toMatch(/401/);
  });
});

describe("raiseFromResponse — a JSON body's code", () => {
  it("is on the error, whichever class the status maps to", async () => {
    const cases: Array<[number, string]> = [
      [400, "model_not_allowed"],
      [403, "operator_key_restricted"],
      [413, "prompt_too_large"],
      [422, "tool_refused"],
      [429, "token_limit_exceeded"],
      [429, "per_user_quota_exhausted"],
      [502, "call_failed"],
      [503, "decision_not_configured"],
      [504, "timeout"],
    ];
    for (const [status, code] of cases) {
      const e = (await expectErrorFor(
        status,
        JSON.stringify({ code, error: "refused" }),
      )) as LoomcycleError;
      expect(e).toBeInstanceOf(LoomcycleError);
      expect(e.status).toBe(status);
      expect(e.code).toBe(code);
    }
  });

  it("survives a body longer than the 1 KiB bodyText cut", async () => {
    const body = JSON.stringify({ error: "x".repeat(5000), code: "prompt_too_large" });
    const e = (await expectErrorFor(413, body)) as LoomcycleError;
    expect(e.bodyText?.length).toBe(1024);
    expect(e.code).toBe("prompt_too_large");
  });

  it("is undefined for a plain-text body, a JSON non-object, or a non-string code", async () => {
    for (const body of ["bad timeout", '"model_not_allowed"', "null", '{"code":7}', '{"error":"x"}']) {
      const e = (await expectErrorFor(400, body)) as LoomcycleError;
      expect(e).toBeInstanceOf(InvalidArgumentError);
      expect(e.code).toBeUndefined();
    }
  });
});

// The body the server writes for a hard token budget, at run creation and at
// POST /v1/_decide (internal/api/http writeTokenLimitError).
const tokenLimitBody = JSON.stringify({
  code: "token_limit_exceeded",
  error: "tenant acme is at its monthly token limit",
  scope: "tenant",
  scope_id: "acme",
  used: 1_000_250,
  limit: 1_000_000,
  window: "month",
  message: "tenant acme is at its monthly token limit",
});

describe("raiseFromResponse — a 429 token budget is not plain backpressure", () => {
  it("429 + code:token_limit_exceeded → TokenLimitExceededError carrying the budget", async () => {
    const e = await expectErrorFor(429, tokenLimitBody);
    expect(e).toBeInstanceOf(TokenLimitExceededError);
    const err = e as TokenLimitExceededError;
    expect(err.name).toBe("TokenLimitExceededError");
    expect(err.status).toBe(429);
    expect(err.code).toBe("token_limit_exceeded");
    expect(err.scope).toBe("tenant");
    expect(err.scopeId).toBe("acme");
    expect(err.used).toBe(1_000_250);
    expect(err.limit).toBe(1_000_000);
    expect(err.window).toBe("month");
    expect(err.message).toContain("monthly token limit");
  });

  it("a token budget refusal is not caught by instanceof BackpressureError", async () => {
    // A `catch (e) { if (e instanceof BackpressureError) retry(); }` loop
    // would otherwise retry a refusal that holds for the rest of the month.
    const e = await expectErrorFor(429, tokenLimitBody);
    expect(e).not.toBeInstanceOf(BackpressureError);
    expect(e).toBeInstanceOf(LoomcycleError);
    expect(e).not.toBeInstanceOf(PerUserQuotaExhaustedError);
    expect((e as LoomcycleError).retryable).toBe(false);
  });

  it("a token budget refusal without the budget fields has them null", async () => {
    // The operator-wide budget has an empty scope_id, and one server path
    // sends only {code, error}.
    const operator = (await expectErrorFor(
      429,
      JSON.stringify({ code: "token_limit_exceeded", error: "x", scope: "operator", scope_id: "", used: 5, limit: 5, window: "month" }),
    )) as TokenLimitExceededError;
    expect(operator.scope).toBe("operator");
    expect(operator.scopeId).toBeNull();

    const bare = (await expectErrorFor(
      429,
      JSON.stringify({ code: "token_limit_exceeded", error: "x" }),
    )) as TokenLimitExceededError;
    expect(bare).toBeInstanceOf(TokenLimitExceededError);
    expect([bare.scope, bare.scopeId, bare.used, bare.limit, bare.window]).toEqual([
      null, null, null, null, null,
    ]);
  });

  it("a budget field of the wrong type is dropped, not passed through", async () => {
    const e = (await expectErrorFor(
      429,
      JSON.stringify({ code: "token_limit_exceeded", scope: 7, used: "many", limit: null, window: ["month"] }),
    )) as TokenLimitExceededError;
    expect([e.scope, e.used, e.limit, e.window]).toEqual([null, null, null, null]);
  });

  it("429 without that code stays a plain BackpressureError", async () => {
    for (const body of ["queue full", JSON.stringify({ code: "backpressure", error: "queue full" })]) {
      const e = await expectErrorFor(429, body);
      expect(e).toBeInstanceOf(BackpressureError);
      expect(e).not.toBeInstanceOf(TokenLimitExceededError);
      expect((e as Error).name).toBe("BackpressureError");
    }
  });

  it("429 + code:per_user_quota_exhausted is unchanged", async () => {
    const e = await expectErrorFor(
      429,
      JSON.stringify({ code: "per_user_quota_exhausted", user_id: "u", cap: 2 }),
    );
    expect(e).toBeInstanceOf(PerUserQuotaExhaustedError);
    expect(e).not.toBeInstanceOf(BackpressureError);
  });

  it("the budget fields are typed", () => {
    // Checked by `npm run typecheck:tests`; vitest strips types.
    const e = new TokenLimitExceededError("x");
    const scope: Exact<typeof e.scope, string | null> = true;
    const scopeId: Exact<typeof e.scopeId, string | null> = true;
    const used: Exact<typeof e.used, number | null> = true;
    const limit: Exact<typeof e.limit, number | null> = true;
    const window: Exact<typeof e.window, string | null> = true;
    const base: LoomcycleError = e;
    const tooLarge: SnapshotTooLargeError = new PromptTooLargeError("x");
    const request: RequestTooLargeError = new PromptTooLargeError("x");
    expect([scope, scopeId, used, limit, window]).toEqual([true, true, true, true, true]);
    expect([base, tooLarge, request]).toHaveLength(3);
  });
});

describe("raiseFromResponse — a 413 is named for what was too large", () => {
  it("413 + code:prompt_too_large → PromptTooLargeError", async () => {
    const e = await expectErrorFor(
      413,
      JSON.stringify({ code: "prompt_too_large", error: "Decision: prompt_too_large: 9000 tokens exceed 8192" }),
    );
    expect(e).toBeInstanceOf(PromptTooLargeError);
    expect((e as PromptTooLargeError).name).toBe("PromptTooLargeError");
    expect((e as PromptTooLargeError).code).toBe("prompt_too_large");
    expect((e as PromptTooLargeError).status).toBe(413);
  });

  it("413 + code:snapshot_too_large (the snapshot endpoint's body) → SnapshotTooLargeError itself", async () => {
    const e = await expectErrorFor(
      413,
      JSON.stringify({ code: "snapshot_too_large", error: "snapshot is 9 bytes, max 8" }),
    );
    expect(e).toBeInstanceOf(SnapshotTooLargeError);
    expect((e as Error).name).toBe("SnapshotTooLargeError");
    expect(e).not.toBeInstanceOf(RequestTooLargeError);
    expect(e).not.toBeInstanceOf(PromptTooLargeError);
  });

  it("any other 413 → RequestTooLargeError, not the prompt class", async () => {
    const bodies = [
      JSON.stringify({ code: "request_too_large", error: "body over 1 MiB" }),
      JSON.stringify({ code: "memory_quota_exceeded", error: "too big" }),
      "request body exceeds the 16777216-byte limit",
      "",
    ];
    for (const body of bodies) {
      const e = await expectErrorFor(413, body);
      expect(e).toBeInstanceOf(RequestTooLargeError);
      expect((e as Error).name).toBe("RequestTooLargeError");
      expect(e).not.toBeInstanceOf(PromptTooLargeError);
    }
  });

  it("every 413 is still caught by instanceof SnapshotTooLargeError", async () => {
    // The client raised that class for any 413 before the others existed.
    const bodies = [
      JSON.stringify({ code: "prompt_too_large", error: "x" }),
      JSON.stringify({ code: "request_too_large", error: "x" }),
      JSON.stringify({ code: "snapshot_too_large", error: "x" }),
      "snapshot exceeds size cap",
    ];
    for (const body of bodies) {
      expect(await expectErrorFor(413, body)).toBeInstanceOf(SnapshotTooLargeError);
    }
  });
});

/** A 429 (or any status) with a JSON body and optional headers. */
async function errorFor(
  status: number,
  body: Record<string, unknown>,
  headers: Record<string, string> = {},
): Promise<LoomcycleError> {
  const resp = new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });
  try {
    await raiseFromResponse(resp);
  } catch (e) {
    return e as LoomcycleError;
  }
  throw new Error("expected raiseFromResponse to throw");
}

describe("raiseFromResponse — a 429 says whether retrying can help", () => {
  it("token_limit_exceeded is TokenLimitExceededError, not retryable, not backpressure", async () => {
    const e = await errorFor(429, JSON.parse(tokenLimitBody) as Record<string, unknown>);
    expect(e).toBeInstanceOf(TokenLimitExceededError);
    expect(e).not.toBeInstanceOf(BackpressureError);
    expect(e.retryable).toBe(false);
  });

  it("rate_limited is a retryable RateLimitedError carrying Retry-After in ms", async () => {
    const e = await errorFor(
      429,
      { code: "rate_limited", error: "run-start rate limit reached" },
      { "Retry-After": "3" },
    );
    expect(e).toBeInstanceOf(RateLimitedError);
    expect(e).toBeInstanceOf(BackpressureError);
    expect(e.name).toBe("RateLimitedError");
    expect(e.code).toBe("rate_limited");
    expect(e.status).toBe(429);
    expect(e.retryable).toBe(true);
    expect((e as RateLimitedError).retryAfterMs).toBe(3000);
  });

  it("rate_limited without a usable Retry-After has retryAfterMs null", async () => {
    // The server always sends whole seconds; an HTTP-date form is not parsed.
    const cases: Array<Record<string, string>> = [
      {},
      { "Retry-After": "Wed, 21 Oct 2026 07:28:00 GMT" },
    ];
    for (const headers of cases) {
      const e = await errorFor(429, { code: "rate_limited", error: "x" }, headers);
      expect(e).toBeInstanceOf(RateLimitedError);
      expect(e.retryable).toBe(true);
      expect((e as RateLimitedError).retryAfterMs).toBeNull();
    }
  });

  it("per_user_quota_exhausted is retryable and keeps its Retry-After", async () => {
    const e = await errorFor(
      429,
      { code: "per_user_quota_exhausted", user_id: "u", cap: 2 },
      { "Retry-After": "5" },
    );
    expect(e).toBeInstanceOf(PerUserQuotaExhaustedError);
    expect(e.retryable).toBe(true);
    expect((e as PerUserQuotaExhaustedError).retryAfterMs).toBe(5000);
  });

  it("configured_run_cap stays a BackpressureError but is not retryable", async () => {
    // The body POST /v1/runs {start:false} writes at the per-user draft cap
    // (internal/api/http configured_runs.go): only starting or discarding a
    // configured run makes room, so waiting never does.
    const e = await errorFor(429, {
      code: "configured_run_cap",
      error: "this user already holds 2 configured runs (the cap is 2); start or discard one first",
    });
    expect(e.constructor).toBe(BackpressureError);
    expect(e.code).toBe("configured_run_cap");
    expect(e.retryable).toBe(false);
  });

  it("an unknown or absent 429 code is a plain, retryable BackpressureError", async () => {
    for (const code of ["backpressure", "provider_concurrency_exhausted", "something_new", undefined]) {
      const e = await errorFor(429, { code, error: "busy" });
      expect(e.constructor).toBe(BackpressureError);
      expect(e.retryable).toBe(true);
    }
    const plain = (await expectErrorFor(429, "queue full")) as LoomcycleError;
    expect(plain.constructor).toBe(BackpressureError);
    expect(plain.retryable).toBe(true);
  });
});

describe("retryable on the other classes", () => {
  it("is false for a refusal that does not clear on its own", async () => {
    const cases: Array<[number, string]> = [
      [400, "bad request"],
      [401, "unauthorized"],
      [404, "agent not found"],
      [409, "session busy"],
      [409, "agent_id in use"],
      [413, JSON.stringify({ code: "prompt_too_large" })],
      [422, "snapshot version"],
      [500, "boom"],
      [503, JSON.stringify({ code: "decision_not_configured" })],
      [503, "pause manager not configured on this server"],
      [503, "service unavailable"],
    ];
    for (const [status, body] of cases) {
      const e = (await expectErrorFor(status, body)) as LoomcycleError;
      expect(e).toBeInstanceOf(LoomcycleError);
      expect(`${status} ${e.name} ${e.retryable}`).toBe(`${status} ${e.name} false`);
    }
  });

  it("is true for a decision call that failed (502 call_failed) or timed out (504 timeout)", async () => {
    // decide()'s own codes; the server classifies both as transient and
    // tells the caller to send the same call again.
    for (const [status, code] of [[502, "call_failed"], [504, "timeout"]] as const) {
      const e = await errorFor(status, { code, error: "Decision: " + code });
      expect(e.constructor).toBe(LoomcycleError);
      expect(`${status} ${code} ${e.retryable}`).toBe(`${status} ${code} true`);
    }
    // The same statuses with another code, or none, stay false.
    for (const [status, body] of [
      [502, JSON.stringify({ code: "model_not_found" })],
      [502, "bad gateway"],
      [504, "gateway timeout"],
    ] as const) {
      const e = (await expectErrorFor(status, body)) as LoomcycleError;
      expect(`${status} ${e.retryable}`).toBe(`${status} false`);
    }
  });

  it("is true for a 503 runtime_paused, which clears when the operator resumes", async () => {
    const e = await errorFor(503, { code: "runtime_paused", error: "runtime is paused" });
    expect(e).toBeInstanceOf(UnavailableError);
    expect(e.retryable).toBe(true);
  });

  it("is false on a bare LoomcycleError and read-only", () => {
    const e = new LoomcycleError("x");
    expect(e.retryable).toBe(false);
    // Checked by `npm run typecheck:tests`: a caller cannot set it.
    // @ts-expect-error retryable has no setter
    expect(() => { e.retryable = true; }).toThrow(TypeError);
    const typed: Exact<typeof e.retryable, boolean> = true;
    expect(typed).toBe(true);
  });
});

describe("the refusal classes reach a caller of the client", () => {
  it("run creation at a hard token budget throws TokenLimitExceededError", async () => {
    const { client } = makeClient([jsonResponse(JSON.parse(tokenLimitBody), 429)]);
    let caught: unknown;
    try {
      for await (const _ of client.runStreaming({ agent: "x", segments: [] })) {}
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(TokenLimitExceededError);
    expect((caught as TokenLimitExceededError).limit).toBe(1_000_000);
  });

  it("run creation over the run-start rate throws RateLimitedError", async () => {
    const { client } = makeClient([
      () =>
        new Response(JSON.stringify({ code: "rate_limited", error: "slow down" }), {
          status: 429,
          headers: { "Content-Type": "application/json", "Retry-After": "2" },
        }),
    ]);
    let caught: unknown;
    try {
      for await (const _ of client.runStreaming({ agent: "x", segments: [] })) {}
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(RateLimitedError);
    expect((caught as RateLimitedError).retryAfterMs).toBe(2000);
  });

  it("decide() over the model's context throws PromptTooLargeError", async () => {
    const { client } = makeClient([
      jsonResponse({ code: "prompt_too_large", error: "Decision: prompt_too_large: too long" }, 413),
    ]);
    const err = await client
      .decide({ state: {}, questions: { q: { type: "noul", instructions: "q" } } })
      .then(() => undefined, (e: unknown) => e);
    expect(err).toBeInstanceOf(PromptTooLargeError);
  });
});

/** True only when A and B are the same type, so a field widened to `unknown`
 *  or narrowed away from `null` fails the tests' typecheck. */
type Exact<A, B> =
  (<T>() => T extends A ? 1 : 2) extends (<T>() => T extends B ? 1 : 2) ? true : false;
