import { describe, expect, it } from "vitest";
import { dropEchoedSteer, newEchoes } from "./operatorEcho";

const steer = (source: string, text: string) => ({ type: "steer", user_input: { source, text } });

describe("dropEchoedSteer", () => {
  it("drops a re-attach replay of a message this client echoed", () => {
    const echoes = newEchoes();
    echoes.prompts.push("hi");
    expect(dropEchoedSteer(echoes, steer("replay", "hi"))).toBe(true);
    expect(echoes.prompts).toEqual([]);
  });

  it("drops the live steer of a message this client sent to the running run", () => {
    // A detached run's stream is forwarded live: the operator's steer comes
    // back as the run drains it (source "api"), not as a replay. Shown as well
    // as the echo, every steer would appear twice.
    const echoes = newEchoes();
    echoes.steers.push("keep going");
    expect(dropEchoedSteer(echoes, steer("api", "keep going"))).toBe(true);
    expect(echoes.steers).toEqual([]);
  });

  it("keeps an opening or continuation prompt's echo from hiding a live steer it never gets", () => {
    // A prompt that starts or continues a run comes back as no live steer at
    // all, so its echo would wait for one forever — and hide the next live
    // steer of the same text from another tab or a bridge.
    const echoes = newEchoes();
    echoes.prompts.push("status?");
    expect(dropEchoedSteer(echoes, steer("api", "status?"))).toBe(false);
    expect(echoes.prompts).toEqual(["status?"]);
  });

  it("shows a steer this client did not send", () => {
    // Another tab, another operator, a bridge, or an agent's own child.
    for (const source of ["api", "replay", "agent", "operator"]) {
      const echoes = newEchoes();
      echoes.steers.push("mine");
      expect(dropEchoedSteer(echoes, steer(source, "theirs"))).toBe(false);
    }
  });

  it("drops each echoed message once, so a genuine repeat still shows", () => {
    const echoes = newEchoes();
    echoes.steers.push("again");
    expect(dropEchoedSteer(echoes, steer("api", "again"))).toBe(true);
    expect(dropEchoedSteer(echoes, steer("api", "again"))).toBe(false);
  });

  it("leaves every frame that is not a steer alone", () => {
    const echoes = newEchoes();
    echoes.steers.push("hello");
    expect(dropEchoedSteer(echoes, { type: "text" })).toBe(false);
  });

  it("does not take a steer from another source for this client's echo", () => {
    // An agent's prompt to its resident child that happens to repeat the
    // operator's words is not the operator's message.
    const echoes = newEchoes();
    echoes.steers.push("status?");
    expect(dropEchoedSteer(echoes, steer("agent", "status?"))).toBe(false);
    expect(echoes.steers).toEqual(["status?"]);
  });
});
