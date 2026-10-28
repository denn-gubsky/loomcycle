import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { DeviceLoginError } from "../src/errors.js";
import type { DeviceLoginPrompt } from "../src/types.js";
import { jsonResponse, makeClient } from "./helpers.js";

// deviceLogin() drives the RFC 8628 device flow against /v1/_auth/device and
// /v1/_auth/device/token. These pin the wire shape (form bodies, no bearer),
// the polling rules (wait the interval, +5 s on slow_down) and how a refusal
// surfaces.

const BASE = "http://test-loomcycle:8787";

const START = {
  device_code: "dc_0123456789",
  user_code: "BCDF-GHJK",
  verification_uri: "https://lc.example/ui/device",
  verification_uri_complete: "https://lc.example/ui/device#BCDF-GHJK",
  expires_in: 600,
  interval: 5,
};

beforeEach(() => {
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
});

describe("deviceLogin", () => {
  it("shows the code, waits out pending and slow_down, and returns the token", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse(START),
      jsonResponse({ error: "authorization_pending" }, 400),
      jsonResponse({ error: "slow_down" }, 400),
      jsonResponse({ access_token: "lct_x", token_type: "Bearer", expires_in: 3600, scope: "substrate:user" }),
    ]);
    const prompts: DeviceLoginPrompt[] = [];
    const p = client.deviceLogin({ clientId: "my-cli", onPrompt: (pr) => void prompts.push(pr) });
    await vi.advanceTimersByTimeAsync(5_000 + 5_000 + 10_000);
    const res = await p;

    expect(res).toEqual({ accessToken: "lct_x", tokenType: "Bearer", expiresIn: 3600, scope: ["substrate:user"] });
    expect(prompts).toEqual([
      {
        userCode: "BCDF-GHJK",
        verificationUri: "https://lc.example/ui/device",
        verificationUriComplete: "https://lc.example/ui/device#BCDF-GHJK",
        expiresIn: 600,
      },
    ]);
    const calls = fetchMock.mock.calls;
    expect(calls).toHaveLength(4);
    const [startURL, startInit] = calls[0]!;
    expect(startURL).toBe(`${BASE}/v1/_auth/device`);
    expect(startInit.method).toBe("POST");
    expect(startInit.headers["Content-Type"]).toBe("application/x-www-form-urlencoded");
    expect(startInit.headers.Authorization).toBeUndefined();
    expect(Object.fromEntries(new URLSearchParams(startInit.body as string))).toEqual({ client_id: "my-cli" });
    const [pollURL, pollInit] = calls[1]!;
    expect(pollURL).toBe(`${BASE}/v1/_auth/device/token`);
    expect(pollInit.headers.Authorization).toBeUndefined();
    expect(Object.fromEntries(new URLSearchParams(pollInit.body as string))).toEqual({
      grant_type: "urn:ietf:params:oauth:grant-type:device_code",
      device_code: "dc_0123456789",
      client_id: "my-cli",
    });
  });

  it("lengthens the interval by five seconds after slow_down", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse(START),
      jsonResponse({ error: "slow_down" }, 400),
      jsonResponse({ access_token: "lct_x", token_type: "Bearer" }),
    ]);
    const p = client.deviceLogin({ onPrompt: () => {} });
    await vi.advanceTimersByTimeAsync(5_000);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    // The next poll waits 10 s, not 5.
    await vi.advanceTimersByTimeAsync(9_000);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(1_000);
    await p;
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it("throws DeviceLoginError with the server's code when the login is denied", async () => {
    const { client } = makeClient([
      jsonResponse(START),
      jsonResponse({ error: "access_denied", error_description: "the login was denied" }, 400),
    ]);
    const p = client.deviceLogin({ onPrompt: () => {} });
    const caught = p.catch((e: unknown) => e);
    await vi.advanceTimersByTimeAsync(5_000);
    const err = await caught;
    expect(err).toBeInstanceOf(DeviceLoginError);
    expect((err as DeviceLoginError).code).toBe("access_denied");
    expect((err as DeviceLoginError).status).toBe(400);
  });

  it("refuses a start answer that would print control characters", async () => {
    const { client } = makeClient([jsonResponse({ ...START, user_code: "AB\u001b[2JCD" })]);
    const onPrompt = vi.fn();
    await expect(client.deviceLogin({ onPrompt })).rejects.toBeInstanceOf(DeviceLoginError);
    expect(onPrompt).not.toHaveBeenCalled();
  });

  it("surfaces device_login_disabled when the server has no device gate", async () => {
    const { client } = makeClient([
      jsonResponse({ code: "device_login_disabled", error: "device login is not enabled on this server" }, 404),
    ]);
    await expect(client.deviceLogin({ onPrompt: () => {} })).rejects.toMatchObject({
      name: "DeviceLoginError",
      status: 404,
      code: "device_login_disabled",
    });
  });
});
