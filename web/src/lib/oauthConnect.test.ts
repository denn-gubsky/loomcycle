import { describe, expect, it } from "vitest";
import type { CredentialMeta, OAuthConnectStart, OAuthState } from "../api";
import { ApiError } from "./apiError";
import {
  ConnectDeps,
  ConnectPhase,
  OAUTH_CODE_HINT,
  connectLabel,
  httpsLink,
  reauthItems,
  reconnectHref,
  runOAuthConnect,
  stateBadge,
} from "./oauthConnect";

const ref = { scope: "user" as const, name: "docs" };

function start(over: Partial<OAuthConnectStart> = {}): OAuthConnectStart {
  return {
    name: "docs",
    scope: "user",
    status: "pending_connect",
    user_code: "ABCD-EFGH",
    verification_uri: "https://auth.example.com/device",
    verification_uri_complete: "https://auth.example.com/device?user_code=ABCD-EFGH",
    expires_in: 300,
    interval: 5,
    ...over,
  };
}

function meta(state: OAuthState, extra: Partial<NonNullable<CredentialMeta["oauth"]>> = {}): CredentialMeta {
  return { name: "docs", scope: "user", usage: "oauth", oauth: { resource: "https://docs.example.com/mcp", state, ...extra } };
}

// fakeDeps answers status reads from `answers` in order (an Error is thrown),
// on a clock that only the sleeps move, and records every sleep.
function fakeDeps(first: OAuthConnectStart | Error, answers: (CredentialMeta | Error)[]) {
  let t = 1_000_000;
  const sleeps: number[] = [];
  let reads = 0;
  const deps: ConnectDeps = {
    start: async () => {
      if (first instanceof Error) throw first;
      return first;
    },
    status: async () => {
      const a = answers[Math.min(reads, answers.length - 1)];
      reads++;
      if (a instanceof Error) throw a;
      return a;
    },
    sleep: async (ms) => {
      sleeps.push(ms);
      t += ms;
    },
    now: () => t,
  };
  return { deps, sleeps, reads: () => reads };
}

async function run(deps: ConnectDeps, signal = new AbortController().signal): Promise<ConnectPhase[]> {
  const phases: ConnectPhase[] = [];
  await runOAuthConnect(ref, deps, (p) => phases.push(p), signal);
  return phases;
}

describe("runOAuthConnect", () => {
  it("shows the code and the link, then the account it connected as", async () => {
    const f = fakeDeps(start(), [meta("pending_connect"), meta("pending_connect"), meta("connected", { connected_as: "ada@example.com" })]);
    const phases = await run(f.deps);
    expect(phases[0]).toEqual({ kind: "starting" });
    expect(phases[1]).toMatchObject({
      kind: "waiting",
      userCode: "ABCD-EFGH",
      link: "https://auth.example.com/device?user_code=ABCD-EFGH",
      verificationURI: "https://auth.example.com/device",
    });
    expect(phases.at(-1)).toEqual({ kind: "connected", connectedAs: "ada@example.com" });
    // One status read per server interval, no faster.
    expect(f.sleeps).toEqual([5000, 5000, 5000]);
    expect(f.reads()).toBe(3);
  });

  it("spaces the reads further when the server asks to slow down", async () => {
    const tooMany = new ApiError("429 Too Many Requests: slow down", 429);
    const f = fakeDeps(start({ interval: 5 }), [tooMany, meta("pending_connect"), meta("connected")]);
    const phases = await run(f.deps);
    expect(f.sleeps).toEqual([5000, 10000, 10000]);
    expect(phases.at(-1)).toEqual({ kind: "connected" });
  });

  it("reports an expired code when the server marks it expired", async () => {
    const f = fakeDeps(start(), [meta("pending_connect"), meta("needs_reauth", { last_error: "device code expired; start again" })]);
    const phases = await run(f.deps);
    expect(phases.at(-1)).toEqual({ kind: "expired" });
  });

  it("gives up as expired once the code's lifetime is well past, even if the server never says so", async () => {
    const f = fakeDeps(start({ expires_in: 20, interval: 5 }), [meta("pending_connect")]);
    const phases = await run(f.deps);
    expect(phases.at(-1)).toEqual({ kind: "expired" });
    // 20 s of code + two intervals of grace, read every 5 s.
    expect(f.reads()).toBe(6);
  });

  it("reports the server's reason when the connect is refused", async () => {
    const f = fakeDeps(start(), [meta("needs_reauth", { last_error: "access_denied" })]);
    expect((await run(f.deps)).at(-1)).toEqual({ kind: "failed", reason: "access_denied" });
  });

  it("stops at once on a refusal that will not pass", async () => {
    const gone = new ApiError('404 Not Found: {"error":"no credential \\"docs\\" in user scope"}', 404);
    const f = fakeDeps(start(), [gone]);
    const phases = await run(f.deps);
    expect(phases.at(-1)).toEqual({ kind: "failed", reason: 'no credential "docs" in user scope' });
    expect(f.reads()).toBe(1);
  });

  it("reports a start the server refused without showing a code", async () => {
    const f = fakeDeps(new ApiError('422 Unprocessable: {"error":"connect: authorization server unavailable"}', 422), []);
    const phases = await run(f.deps);
    expect(phases.map((p) => p.kind)).toEqual(["starting", "failed"]);
  });

  it("reports nothing more once a newer connect aborted it", async () => {
    const ctl = new AbortController();
    const f = fakeDeps(start(), [meta("connected")]);
    const inner = f.deps.sleep;
    f.deps.sleep = async (ms, s) => {
      await inner(ms, s);
      ctl.abort();
    };
    const phases = await run(f.deps, ctl.signal);
    expect(phases.map((p) => p.kind)).toEqual(["starting", "waiting"]);
    expect(f.reads()).toBe(0);
  });

  it("reports nothing from a status read that answers after a newer connect aborted it", async () => {
    const ctl = new AbortController();
    const f = fakeDeps(start(), [meta("connected", { connected_as: "old@example.com" })]);
    const inner = f.deps.status;
    f.deps.status = async (r) => {
      const m = await inner(r);
      ctl.abort();
      return m;
    };
    const phases = await run(f.deps, ctl.signal);
    expect(phases.map((p) => p.kind)).toEqual(["starting", "waiting"]);
  });

  it("never offers a link that is not https", async () => {
    const f = fakeDeps(
      start({ verification_uri: "javascript:alert(1)", verification_uri_complete: "http://auth.example.com/x" }),
      [meta("connected")],
    );
    const phases = await run(f.deps);
    expect(phases[1]).toMatchObject({ kind: "waiting", link: null, verificationURI: "javascript:alert(1)" });
  });
});

describe("oauth helpers", () => {
  it("httpsLink keeps https URLs only", () => {
    expect(httpsLink("https://a.example/device")).toBe("https://a.example/device");
    expect(httpsLink("http://a.example/device")).toBeNull();
    expect(httpsLink("javascript:alert(1)")).toBeNull();
    expect(httpsLink("not a url")).toBeNull();
    expect(httpsLink(undefined)).toBeNull();
  });

  it("names the button and the badge for each state", () => {
    expect(connectLabel("pending_connect")).toBe("Connect account");
    expect(connectLabel("revoked")).toBe("Connect account");
    expect(connectLabel("connected")).toBe("Reconnect");
    expect(connectLabel("needs_reauth")).toBe("Reconnect");
    expect(stateBadge("needs_reauth")).toMatchObject({ label: "needs reauth", className: "status-bad" });
    expect(stateBadge("connected").className).toBe("status-ok");
    expect(stateBadge("pending_connect").className).toBe("status-warn");
  });

  it("reauthItems picks only OAuth credentials in needs_reauth, with their scope", () => {
    const plain: CredentialMeta = { name: "KEY", scope: "tenant" };
    const items = reauthItems([
      { scope: "tenant", credentials: [plain, { ...meta("needs_reauth", { last_error: "invalid_grant" }), name: "shared" }, meta("connected")] },
      { scope: "user", credentials: [meta("needs_reauth")] },
    ]);
    expect(items).toEqual([
      { scope: "tenant", name: "shared", reason: "invalid_grant" },
      { scope: "user", name: "docs" },
    ]);
    expect(reconnectHref("tenant")).toBe("/credentials");
    expect(reconnectHref("user")).toBe("/my-credentials");
  });

  it("the hint tells the owner to use the newest code", () => {
    expect(OAUTH_CODE_HINT).toBe("Codes last 5 minutes; use the newest.");
  });
});
