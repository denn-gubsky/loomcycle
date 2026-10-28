import { describe, expect, it } from "vitest";
import { signedInDestination } from "./returnPath";

describe("signedInDestination", () => {
  it("follows a plain /ui path", () => {
    expect(signedInDestination("?next=/ui/device")).toBe("/ui/device");
    expect(signedInDestination("?next=/ui")).toBe("/ui");
  });
  it("never leaves this server's UI", () => {
    for (const bad of [
      "?next=https://evil.example/ui/device",
      "?next=//evil.example/ui",
      "?next=/\\evil.example",
      "?next=/ui/../../evil",
      "?next=/v1/_auth/logout",
      "?next=/ui/device?x=1",
      "?next=/ui/device%23x",
      "?next=javascript:alert(1)",
      "",
    ]) {
      expect(signedInDestination(bad)).toBe("/ui");
    }
  });
});
