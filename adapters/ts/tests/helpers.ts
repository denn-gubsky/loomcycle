/**
 * Test helpers for vitest-based unit tests against LoomcycleClient.
 *
 * The pattern: build a `vi.fn()` fetch mock that returns canned
 * Response objects, instantiate LoomcycleClient with `fetch: mock`,
 * call a method, assert on the recorded calls + the unwrapped
 * response.
 *
 * Helpers here keep the per-test boilerplate small.
 */

import { vi, type Mock } from "vitest";
import { LoomcycleClient } from "../src/client.js";

/**
 * makeClient builds a LoomcycleClient with a captured fetch mock.
 * The mock is preloaded with a Response factory queue; each call
 * pops the next factory and uses it to build the Response.
 *
 * Returns: the client + the fetch mock + a `queue.push()` to enqueue
 * additional responses for chained-call tests.
 */
export function makeClient(
  responses: Array<(req: Request) => Response> = [],
): { client: LoomcycleClient; fetchMock: Mock; queue: typeof responses } {
  const queue = [...responses];
  const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
    const req = new Request(url, init as RequestInit);
    if (queue.length === 0) {
      throw new Error(
        `fetch mock: no more responses queued for ${init?.method ?? "GET"} ${url}`,
      );
    }
    const factory = queue.shift()!;
    return factory(req);
  });
  const client = new LoomcycleClient({
    baseUrl: "http://test-loomcycle:8787",
    authToken: "test-bearer",
    fetch: fetchMock as unknown as typeof fetch,
  });
  return { client, fetchMock, queue };
}

/** jsonResponse builds a Response factory that returns `body` as
 *  JSON with status 200 (or `status` if supplied). */
export function jsonResponse(
  body: unknown,
  status = 200,
): (req: Request) => Response {
  return () =>
    new Response(JSON.stringify(body), {
      status,
      headers: { "Content-Type": "application/json" },
    });
}

/** errorResponse builds a Response with non-2xx status + plain text
 *  body. Used to drive the typed-error dispatch in
 *  fetch-helpers.ts:raiseFromResponse. */
export function errorResponse(
  status: number,
  bodyText: string,
): (req: Request) => Response {
  return () =>
    new Response(bodyText, { status, statusText: defaultStatusText(status) });
}

/** noContentResponse builds a 204 No Content. */
export function noContentResponse(): (req: Request) => Response {
  return () => new Response(null, { status: 204 });
}

/** sseResponse builds a Response with a ReadableStream body that
 *  emits the given SSE frames. Use for runStreaming /
 *  continueSession tests. */
export function sseResponse(frames: string[]): (req: Request) => Response {
  const encoder = new TextEncoder();
  const body = new ReadableStream({
    start(controller) {
      for (const f of frames) controller.enqueue(encoder.encode(f));
      controller.close();
    },
  });
  return () =>
    new Response(body, {
      status: 200,
      headers: { "Content-Type": "text/event-stream" },
    });
}

function defaultStatusText(status: number): string {
  switch (status) {
    case 400: return "Bad Request";
    case 401: return "Unauthorized";
    case 404: return "Not Found";
    case 409: return "Conflict";
    case 413: return "Payload Too Large";
    case 422: return "Unprocessable Entity";
    case 429: return "Too Many Requests";
    case 503: return "Service Unavailable";
    default: return "";
  }
}
