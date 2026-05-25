// adapters/ts/tests/embeddings.test.ts — v0.11.4 OpenAI Embeddings
// compatibility shim. Mirrors llm-gateway.test.ts / substrate.test.ts
// patterns.

import { describe, expect, it } from "vitest";

import { jsonResponse, makeClient, errorResponse } from "./helpers.js";
import {
  AuthError,
  UnavailableError,
  type LLMEmbeddingsResponse,
} from "../src/index.js";

describe("embeddings", () => {
  it("POSTs to /v1/embeddings and returns the typed response", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        object: "list",
        data: [
          { object: "embedding", embedding: [0.1, 0.2, 0.3], index: 0 },
        ],
        model: "text-embedding-3-small",
        usage: { prompt_tokens: 8, total_tokens: 8 },
      } satisfies LLMEmbeddingsResponse),
    ]);

    const resp = await client.embeddings({
      model: "text-embedding-3-small",
      input: "hello",
    });

    expect(resp.object).toBe("list");
    expect(resp.data).toHaveLength(1);
    expect(resp.data[0]!.embedding).toEqual([0.1, 0.2, 0.3]);
    expect(resp.data[0]!.index).toBe(0);
    expect(resp.model).toBe("text-embedding-3-small");

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/embeddings");
    expect((call[1] as RequestInit).method).toBe("POST");
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body.model).toBe("text-embedding-3-small");
    expect(body.input).toBe("hello");
    // signal must NOT appear in the wire body (transport concern).
    expect(body.signal).toBeUndefined();
  });

  it("handles array input + base64 encoding format", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        object: "list",
        data: [
          { object: "embedding", embedding: "AABEAA==", index: 0 },
          { object: "embedding", embedding: "AACAQg==", index: 1 },
        ],
        model: "text-embedding-3-small",
        usage: { prompt_tokens: 0, total_tokens: 0 },
      } satisfies LLMEmbeddingsResponse),
    ]);

    const resp = await client.embeddings({
      model: "text-embedding-3-small",
      input: ["hello", "world"],
      encoding_format: "base64",
    });
    expect(resp.data).toHaveLength(2);
    expect(typeof resp.data[0]!.embedding).toBe("string");

    const body = JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string);
    expect(body.input).toEqual(["hello", "world"]);
    expect(body.encoding_format).toBe("base64");
  });

  it("forwards bearer auth in the Authorization header", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        object: "list", data: [], model: "x",
        usage: { prompt_tokens: 0, total_tokens: 0 },
      } satisfies LLMEmbeddingsResponse),
    ]);
    await client.embeddings({ model: "x", input: "y" });
    const headers = (fetchMock.mock.calls[0]![1] as RequestInit)
      .headers as Record<string, string>;
    expect(headers.Authorization).toBe("Bearer test-bearer");
  });

  it("raises AuthError on 401", async () => {
    const { client } = makeClient([errorResponse(401, "bad token")]);
    await expect(
      client.embeddings({ model: "x", input: "y" }),
    ).rejects.toBeInstanceOf(AuthError);
  });

  it("raises UnavailableError on 503 (no embedder configured)", async () => {
    const { client } = makeClient([
      errorResponse(503, "no embedder configured"),
    ]);
    await expect(
      client.embeddings({ model: "x", input: "y" }),
    ).rejects.toBeInstanceOf(UnavailableError);
  });

  it("passes signal through to fetch and strips it from the JSON body", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        object: "list", data: [], model: "x",
        usage: { prompt_tokens: 0, total_tokens: 0 },
      } satisfies LLMEmbeddingsResponse),
    ]);
    const ctrl = new AbortController();
    await client.embeddings({ model: "x", input: "y", signal: ctrl.signal });
    const init = fetchMock.mock.calls[0]![1] as RequestInit;
    const body = JSON.parse(init.body as string);
    expect(body.signal).toBeUndefined();
    expect(init.signal).toBe(ctrl.signal);
  });
});
