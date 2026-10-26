import { describe, expect, it } from "vitest";
import { maskedSecretProblem } from "./maskedSecrets";

describe("maskedSecretProblem", () => {
  it("refuses a masked header, env value or URL", () => {
    expect(maskedSecretProblem({ headers: [{ key: "Authorization", value: "Bearer ****" }] })).toMatch(
      /Header Authorization holds a masked value/,
    );
    expect(maskedSecretProblem({ env: [{ key: "TOKEN", value: "****" }] })).toMatch(/Env var TOKEN/);
    expect(maskedSecretProblem({ url: "https://h.example/mcp?key=****" })).toMatch(/The URL holds a masked value/);
  });

  it("accepts real values, references and blank rows", () => {
    expect(
      maskedSecretProblem({
        url: "https://h.example/mcp",
        headers: [
          { key: "Authorization", value: "Bearer $cred:ms365" },
          { key: "", value: "****" },
        ],
        env: [{ key: "TOKEN", value: "${run.credentials.tok}" }],
      }),
    ).toBeNull();
  });
});
