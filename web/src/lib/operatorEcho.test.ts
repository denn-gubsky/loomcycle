import { describe, expect, it } from "vitest";
import { dropEchoedSteer } from "./operatorEcho";

describe("dropEchoedSteer", () => {
  it("drops a re-attach replay of a message this client echoed", () => {
    const echoed = ["hi"];
    expect(dropEchoedSteer(echoed, { type: "steer", user_input: { source: "replay", text: "hi" } })).toBe(true);
    expect(echoed).toEqual([]);
  });

  it("drops the live steer of a message this client sent to the running run", () => {
    // A detached run's stream is forwarded live: the operator's steer comes
    // back as the run drains it (source "api"), not as a replay. Shown as well
    // as the echo, every steer would appear twice.
    const echoed = ["keep going"];
    expect(dropEchoedSteer(echoed, { type: "steer", user_input: { source: "api", text: "keep going" } })).toBe(true);
    expect(echoed).toEqual([]);
  });

  it("shows a steer this client did not send", () => {
    // Another tab, another operator, a bridge, or an agent's own child.
    for (const source of ["api", "replay", "agent", "operator"]) {
      expect(dropEchoedSteer(["mine"], { type: "steer", user_input: { source, text: "theirs" } })).toBe(false);
    }
  });

  it("drops each echoed message once, so a genuine repeat still shows", () => {
    const echoed = ["again"];
    const ev = { type: "steer", user_input: { source: "api", text: "again" } };
    expect(dropEchoedSteer(echoed, ev)).toBe(true);
    expect(dropEchoedSteer(echoed, ev)).toBe(false);
  });

  it("leaves every frame that is not a steer alone", () => {
    expect(dropEchoedSteer(["hello"], { type: "text" })).toBe(false);
  });

  it("does not take a steer from another source for this client's echo", () => {
    // An agent's prompt to its resident child that happens to repeat the
    // operator's words is not the operator's message.
    const echoed = ["status?"];
    expect(dropEchoedSteer(echoed, { type: "steer", user_input: { source: "agent", text: "status?" } })).toBe(false);
    expect(echoed).toEqual(["status?"]);
  });
});
