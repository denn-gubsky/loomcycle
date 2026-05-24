// adapters/ts/tests/llm-gateway.test.ts — v0.11.0 LLM Gateway client
// methods. Mirrors substrate.test.ts / library.test.ts patterns.

import { describe, expect, it } from "vitest";

import { jsonResponse, makeClient, errorResponse, sseResponse } from "./helpers.js";
import {
  AuthError,
  UnavailableError,
  type LLMChatResponse,
  type LLMChatStreamItem,
} from "../src/index.js";

describe("llmChat", () => {
  it("POSTs to /v1/_llm/chat with stream:false and unwraps the response", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        id: "llm_abc",
        request_id: "req_xyz",
        provider: "anthropic",
        model: "claude-sonnet-4-6",
        content: [{ type: "text", text: "Hello world" }],
        stop_reason: "end_turn",
        usage: { input_tokens: 10, output_tokens: 2 },
      } satisfies LLMChatResponse),
    ]);

    const resp = await client.llmChat({
      messages: [{ role: "user", content: "hi" }],
      provider: "anthropic",
      model: "claude-sonnet-4-6",
    });

    expect(resp.provider).toBe("anthropic");
    expect(resp.model).toBe("claude-sonnet-4-6");
    expect(resp.content).toEqual([{ type: "text", text: "Hello world" }]);
    expect(resp.stop_reason).toBe("end_turn");
    expect(resp.request_id).toBe("req_xyz");

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_llm/chat");
    expect((call[1] as RequestInit).method).toBe("POST");
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body.stream).toBe(false);
    expect(body.messages[0].content).toBe("hi");
    expect(body.signal).toBeUndefined();
  });

  it("forwards bearer auth in the Authorization header", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        id: "llm_a",
        request_id: "req_a",
        provider: "openai",
        model: "gpt-4o-mini",
        content: [],
        stop_reason: "end_turn",
        usage: { input_tokens: 0, output_tokens: 0 },
      } satisfies LLMChatResponse),
    ]);
    await client.llmChat({
      messages: [{ role: "user", content: "x" }],
      provider: "openai",
      model: "gpt-4o-mini",
    });
    const headers = (fetchMock.mock.calls[0]![1] as RequestInit)
      .headers as Record<string, string>;
    expect(headers.Authorization).toBe("Bearer test-bearer");
  });

  it("raises AuthError on 401", async () => {
    const { client } = makeClient([errorResponse(401, "invalid token")]);
    await expect(
      client.llmChat({
        messages: [{ role: "user", content: "x" }],
        provider: "openai",
        model: "gpt-4o-mini",
      }),
    ).rejects.toBeInstanceOf(AuthError);
  });

  it("raises UnavailableError on 503", async () => {
    const { client } = makeClient([
      errorResponse(503, "resolver not configured"),
    ]);
    await expect(
      client.llmChat({
        messages: [{ role: "user", content: "x" }],
      }),
    ).rejects.toBeInstanceOf(UnavailableError);
  });

  it("strips AbortSignal from the JSON body but passes it through fetch", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        id: "llm_x",
        request_id: "req_x",
        provider: "openai",
        model: "gpt-4o-mini",
        content: [],
        stop_reason: "end_turn",
        usage: { input_tokens: 0, output_tokens: 0 },
      } satisfies LLMChatResponse),
    ]);
    const controller = new AbortController();
    await client.llmChat({
      messages: [{ role: "user", content: "x" }],
      provider: "openai",
      model: "gpt-4o-mini",
      signal: controller.signal,
    });
    const init = fetchMock.mock.calls[0]![1] as RequestInit;
    const body = JSON.parse(init.body as string);
    expect(body.signal).toBeUndefined();
    expect(init.signal).toBe(controller.signal);
  });
});

describe("llmStream", () => {
  it("parses SSE frames into kind+payload items", async () => {
    const frames = [
      'event: provider_chosen\ndata: {"provider":"anthropic","model":"claude-sonnet-4-6","request_id":"req_a"}\n\n',
      'event: content_block_start\ndata: {"index":0,"block":{"type":"text","text":""}}\n\n',
      'event: content_block_delta\ndata: {"index":0,"delta":{"type":"text_delta","text":"Hi"}}\n\n',
      'event: content_block_delta\ndata: {"index":0,"delta":{"type":"text_delta","text":" there"}}\n\n',
      'event: content_block_stop\ndata: {"index":0}\n\n',
      'event: message_delta\ndata: {"delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":5,"output_tokens":2}}\n\n',
      'event: done\ndata: {"id":"llm_x","stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}\n\n',
    ];
    const { client } = makeClient([sseResponse(frames)]);
    const items: LLMChatStreamItem[] = [];
    for await (const it of client.llmStream({
      messages: [{ role: "user", content: "hi" }],
      provider: "anthropic",
      model: "claude-sonnet-4-6",
    })) {
      items.push(it);
    }
    expect(items.length).toBe(7);
    expect(items[0]!.kind).toBe("provider_chosen");
    expect(items[2]!.kind).toBe("content_block_delta");
    if (items[2]!.kind === "content_block_delta") {
      expect(items[2]!.payload.delta.text).toBe("Hi");
    }
    expect(items[6]!.kind).toBe("done");
  });

  it("yields an error frame and stops on terminal failure", async () => {
    const frames = [
      'event: provider_chosen\ndata: {"provider":"anthropic","model":"claude-sonnet-4-6","request_id":"req_a"}\n\n',
      'event: error\ndata: {"type":"provider_error","code":"anthropic_rate_limit","message":"backoff exhausted"}\n\n',
    ];
    const { client } = makeClient([sseResponse(frames)]);
    const items: LLMChatStreamItem[] = [];
    for await (const it of client.llmStream({
      messages: [{ role: "user", content: "x" }],
      provider: "anthropic",
      model: "claude-sonnet-4-6",
    })) {
      items.push(it);
    }
    expect(items.length).toBe(2);
    expect(items[1]!.kind).toBe("error");
    if (items[1]!.kind === "error") {
      expect(items[1]!.payload.code).toBe("anthropic_rate_limit");
    }
  });

  it("ignores SSE comment lines (keepalives)", async () => {
    const frames = [
      ': keepalive\n\n',
      'event: done\ndata: {"id":"llm_x","stop_reason":"end_turn","usage":{"input_tokens":0,"output_tokens":0}}\n\n',
    ];
    const { client } = makeClient([sseResponse(frames)]);
    const items: LLMChatStreamItem[] = [];
    for await (const it of client.llmStream({
      messages: [{ role: "user", content: "x" }],
      provider: "openai",
      model: "gpt-4o-mini",
    })) {
      items.push(it);
    }
    expect(items.length).toBe(1);
    expect(items[0]!.kind).toBe("done");
  });

  it("sends Accept: text/event-stream + stream:true on the request", async () => {
    const frames = ['event: done\ndata: {"id":"llm_x","stop_reason":"end_turn","usage":{"input_tokens":0,"output_tokens":0}}\n\n'];
    const { client, fetchMock } = makeClient([sseResponse(frames)]);
    const items: LLMChatStreamItem[] = [];
    for await (const it of client.llmStream({
      messages: [{ role: "user", content: "x" }],
      provider: "openai",
      model: "gpt-4o-mini",
    })) {
      items.push(it);
    }
    const init = fetchMock.mock.calls[0]![1] as RequestInit;
    const headers = init.headers as Record<string, string>;
    expect(headers.Accept).toBe("text/event-stream");
    expect(headers["Content-Type"]).toBe("application/json");
    const body = JSON.parse(init.body as string);
    expect(body.stream).toBe(true);
  });

  it("raises AuthError on 401 before the iterator yields", async () => {
    const { client } = makeClient([errorResponse(401, "bad token")]);
    await expect(
      (async () => {
        for await (const _ of client.llmStream({
          messages: [{ role: "user", content: "x" }],
          provider: "openai",
          model: "gpt-4o-mini",
        })) {
          // no-op
        }
      })(),
    ).rejects.toBeInstanceOf(AuthError);
  });
});
