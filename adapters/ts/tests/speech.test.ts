/**
 * Tests for the speech client surface (RFC FK P3):
 *   - speech()      → POST /v1/audio/speech
 *   - transcribe()  → POST /v1/audio/transcriptions (multipart)
 *   - speechInfo()  → GET  /v1/_speech
 */

import { describe, it, expect, vi } from "vitest";
import { makeClient, jsonResponse } from "./helpers.js";
// Through the package index, as a consumer imports them: a type left out of
// the index fails this file's typecheck.
import { LoomcycleClient, LoomcycleError, TokenLimitExceededError } from "../src/index.js";
import type {
  SpeechFormat,
  SpeechInfo,
  SpeechOptions,
  SpeechResult,
  TranscribeOptions,
  TranscribeResult,
  TranscriptSegment,
  SpeechDirectionInfo,
  SpeechLanguageInfo,
  TranscriptionDirectionInfo,
} from "../src/index.js";

const wavBytes = new Uint8Array([0x52, 0x49, 0x46, 0x46, 1, 2, 3, 4]);

function audioResponse(headers: Record<string, string>): (req: Request) => Response {
  return () => new Response(wavBytes, { status: 200, headers: { "Content-Type": "audio/wav", ...headers } });
}

function errorJSON(status: number, code: string, error: string): (req: Request) => Response {
  return () => new Response(JSON.stringify({ code, error }), { status, headers: { "Content-Type": "application/json" } });
}

describe("speech", () => {
  it("POSTs the text with the OpenAI field names and returns the audio and its metadata", async () => {
    const { client, fetchMock } = makeClient([
      audioResponse({
        "X-Loomcycle-Speech-Language": "uk",
        "X-Loomcycle-Speech-Voice": "ukrainian_tts",
        "X-Loomcycle-Speech-Seconds": "3.25",
        "X-Loomcycle-Speech-Chars": "48",
        "X-Loomcycle-Speech-Pieces": "1",
      }),
    ]);
    const opts: SpeechOptions = { language: "uk", voice: "ukrainian_tts", format: "wav" };
    const res: SpeechResult = await client.speech("Привіт, це перевірка голосу.", opts);

    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/audio/speech");
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body as string)).toEqual({
      input: "Привіт, це перевірка голосу.",
      language: "uk",
      voice: "ukrainian_tts",
      response_format: "wav",
    });
    expect(new Headers(init.headers).get("Authorization")).toBe("Bearer test-bearer");
    expect(Array.from(res.bytes)).toEqual(Array.from(wavBytes));
    expect(res).toMatchObject({ contentType: "audio/wav", language: "uk", voice: "ukrainian_tts", seconds: 3.25, chars: 48, pieces: 1 });
  });

  it("sends only the input when no option is given, and reads absent headers as undefined", async () => {
    const { client, fetchMock } = makeClient([audioResponse({})]);
    const res = await client.speech("Hello.");
    expect(JSON.parse(fetchMock.mock.calls[0]![1]!.body as string)).toEqual({ input: "Hello." });
    expect(res.language).toBeUndefined();
    expect(res.seconds).toBeUndefined();
  });

  it("raises the server's code for a format the deployment does not make", async () => {
    const { client } = makeClient([
      errorJSON(400, "speech_encode_not_configured", "the format needs speech.encode: mp3 is encoded from the engine's WAV by speech.encode, which is not set; ask for wav"),
    ]);
    const err = await client.speech("Hello.", { format: "mp3" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(LoomcycleError);
    expect((err as LoomcycleError).status).toBe(400);
    expect((err as LoomcycleError).message).toContain("speech.encode");
  });

  it("maps a hard token budget to TokenLimitExceededError", async () => {
    const { client } = makeClient([errorJSON(429, "token_limit_exceeded", "token budget exhausted")]);
    await expect(client.speech("Hello.")).rejects.toBeInstanceOf(TokenLimitExceededError);
  });
});

describe("transcribe", () => {
  it("sends multipart FormData and leaves Content-Type for the runtime to write", async () => {
    const reply: TranscribeResult = { text: "привіт світ", language: "uk", duration: 90.5 };
    const { client, fetchMock } = makeClient([jsonResponse(reply)]);
    const opts: TranscribeOptions = { language: "uk", prompt: "loomcycle", contentType: "audio/wav" };
    const res = await client.transcribe(new Uint8Array([1, 2, 3]), opts);

    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/audio/transcriptions");
    expect(init.method).toBe("POST");
    expect(new Headers(init.headers).has("Content-Type")).toBe(false);
    const form = init.body as FormData;
    expect(form).toBeInstanceOf(FormData);
    expect(form.get("language")).toBe("uk");
    expect(form.get("prompt")).toBe("loomcycle");
    expect(form.has("model")).toBe(false);
    const file = form.get("file") as File;
    expect(file.name).toBe("recording.wav");
    expect(file.type).toBe("audio/wav");
    expect(new Uint8Array(await file.arrayBuffer())).toEqual(new Uint8Array([1, 2, 3]));
    expect(res).toEqual(reply);
  });

  it("names a browser's webm blob by its type and asks for segments", async () => {
    const seg: TranscriptSegment = { start: 0, end: 2.5, text: "hello", speaker: "A" };
    const { client, fetchMock } = makeClient([jsonResponse({ text: "hello", duration: 2.5, segments: [seg] })]);
    const blob = new Blob([new Uint8Array([0x1a, 0x45, 0xdf, 0xa3])], { type: "audio/webm;codecs=opus" });
    const res = await client.transcribe(blob, { format: "verbose_json" });
    const form = fetchMock.mock.calls[0]![1]!.body as FormData;
    expect((form.get("file") as File).name).toBe("recording.webm");
    expect(form.get("response_format")).toBe("verbose_json");
    expect(res.segments).toEqual([seg]);
  });

  it("calls fetch as fetchImpl(url, init), never detached from its receiver", async () => {
    // A browser's fetch throws "Illegal invocation" when called with a
    // `this` other than the window; the client must call it the way it was
    // given, as every other method does.
    const receivers: unknown[] = [];
    const host = {
      fetch: vi.fn(function (this: unknown) {
        receivers.push(this);
        return Promise.resolve(new Response(JSON.stringify({ text: "x" }), { status: 200, headers: { "Content-Type": "application/json" } }));
      }),
    };
    const client = new LoomcycleClient({ baseUrl: "http://h", fetch: host.fetch.bind(host) as unknown as typeof fetch });
    await client.transcribe(new Uint8Array([1]), { contentType: "audio/wav" });
    expect(receivers).toEqual([host]);
  });

  it("raises 415 for a type the deployment cannot take", async () => {
    const { client } = makeClient([errorJSON(415, "unsupported_media_type", "audio/webm cannot be transcribed here")]);
    const err = await client.transcribe(new Blob([new Uint8Array([1])], { type: "audio/webm" })).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(LoomcycleError);
    expect((err as LoomcycleError).status).toBe(415);
  });
});

describe("speechInfo", () => {
  it("GETs /v1/_speech", async () => {
    const lang: SpeechLanguageInfo = { voices: ["ukrainian_tts"], default_voice: "ukrainian_tts" };
    const sp: SpeechDirectionInfo = { enabled: true, default_language: "uk", languages: { uk: lang }, formats: ["wav"], max_chars_per_call: 4000, max_chars_per_request: 100000 };
    const tr: TranscriptionDirectionInfo = { enabled: true, accepts: ["audio/wav", "audio/webm"], max_upload_bytes: 16777216, whole_file_max_bytes: 26214400 };
    const info: SpeechInfo = { speech: sp, transcription: tr };
    const { client, fetchMock } = makeClient([jsonResponse(info)]);
    expect(await client.speechInfo()).toEqual(info);
    expect(fetchMock.mock.calls[0]![0]).toBe("http://test-loomcycle:8787/v1/_speech");
    const f: SpeechFormat = "opus";
    expect(f).toBe("opus");
  });
});
