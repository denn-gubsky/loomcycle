import { describe, expect, it } from "vitest";
import {
  asEventHooks,
  asToolHooks,
  entryProblem,
  headersProblem,
  nameProblem,
  pruneEventHooks,
  pruneToolHooks,
} from "./hooks";

describe("reading stored hooks", () => {
  it("keeps references as strings and webhooks as objects, in order", () => {
    const h = asEventHooks({
      pre: ["gate@3", { name: "scrub", url: "https://h.example/s", fail_mode: "closed", timeout_ms: 800, headers: { Authorization: "Bearer $cred:k" } }],
    });
    expect(h.pre).toEqual([
      "gate@3",
      { name: "scrub", url: "https://h.example/s", fail_mode: "closed", timeout_ms: 800, headers: { Authorization: "Bearer $cred:k" } },
    ]);
  });

  it("drops what the runtime could not have stored", () => {
    expect(asEventHooks(null)).toEqual({});
    expect(asEventHooks(["pre"])).toEqual({});
    expect(asEventHooks({ pre: "gate", post: [42, "ok"] })).toEqual({ post: ["ok"] });
    expect(asToolHooks({ WebFetch: { pre: ["gate"] } })).toEqual({ WebFetch: { pre: ["gate"] } });
  });
});

describe("writing hooks back", () => {
  // Clearing the last hook must leave the key UNSET, so a fork inherits the
  // parent's hooks rather than storing an empty map.
  it("an empty map is undefined, and empty events and tools are dropped", () => {
    expect(pruneEventHooks({ pre: [] })).toBeUndefined();
    expect(pruneEventHooks({ pre: [], run_end: ["log"] })).toEqual({ run_end: ["log"] });
    expect(pruneToolHooks({ WebFetch: { pre: [] } })).toBeUndefined();
    expect(pruneToolHooks({ WebFetch: { pre: [] }, Read: { post: ["x"] } })).toEqual({ Read: { post: ["x"] } });
  });
});

describe("the runtime's rules, checked beside the entry", () => {
  it("a reference is a valid name with an optional positive version", () => {
    expect(entryProblem("gate")).toBeNull();
    expect(entryProblem("team/gate@12")).toBeNull();
    expect(entryProblem("")).toMatch(/required/);
    expect(entryProblem("gate@0")).toMatch(/positive/);
    expect(entryProblem("gate@x")).toMatch(/positive/);
    expect(entryProblem("tenant:gate")).toMatch(/letters/);
    expect(entryProblem("a//b")).toMatch(/segment/);
  });

  it("a webhook needs a valid name and an http(s) url", () => {
    expect(entryProblem({ name: "gate", url: "https://h.example" })).toBeNull();
    expect(entryProblem({ name: "", url: "https://h.example" })).toMatch(/name/);
    expect(entryProblem({ name: "gate", url: "ftp://h.example" })).toMatch(/http/);
    expect(entryProblem({ name: "gate", url: "https://h", timeout_ms: -1 })).toMatch(/negative/);
  });

  it("headers follow the runtime's rule", () => {
    expect(headersProblem({ Authorization: "Bearer $cred:k" })).toBeNull();
    expect(headersProblem({ "": "x" })).toMatch(/name/);
    expect(headersProblem({ "X Bad": "x" })).toMatch(/letters/);
    expect(headersProblem({ "Content-Type": "x" })).toMatch(/set by the call/);
    expect(headersProblem({ X: "a\nb" })).toMatch(/line break/);
  });

  it("names are capped and segmented", () => {
    expect(nameProblem("a".repeat(129))).toMatch(/128/);
    expect(nameProblem("/lead")).toMatch(/segment/);
  });
});
