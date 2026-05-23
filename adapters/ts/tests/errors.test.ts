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
