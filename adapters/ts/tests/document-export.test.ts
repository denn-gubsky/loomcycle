/**
 * exportDocument() → GET /v1/_document/export/{document_id} (RFC GR).
 */

import { describe, it, expect } from "vitest";
import { makeClient, errorResponse } from "./helpers.js";
import { InvalidArgumentError, NotFoundError, UnavailableError } from "../src/errors.js";

const served =
  (body: string, headers: Record<string, string>) =>
  (): Response =>
    new Response(body, { status: 200, headers });

describe("exportDocument", () => {
  it("GETs the export route with the bearer and returns the file, its name, type and render id", async () => {
    const { client, fetchMock } = makeClient([
      served("%PDF-1.7", {
        "Content-Type": "application/pdf",
        "Content-Disposition": `attachment; filename="Zvit.pdf"; filename*=UTF-8''%D0%97%D0%B2%D1%96%D1%82.pdf`,
        "X-Loomcycle-Render-Id": "rnd_1",
      }),
    ]);
    const res = await client.exportDocument("doc 1", "pdf", { scope: "tenant", scopeId: "alice", tenant: "acme" });
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe(
      "http://test-loomcycle:8787/v1/_document/export/doc%201?format=pdf&scope=tenant&scope_id=alice&tenant=acme",
    );
    expect(init!.method).toBe("GET");
    expect((init!.headers as Record<string, string>).Authorization).toBe("Bearer test-bearer");
    expect(res.filename).toBe("Звіт.pdf");
    expect(res.mediaType).toBe("application/pdf");
    expect(res.renderId).toBe("rnd_1");
    expect(await res.blob.text()).toBe("%PDF-1.7");
  });

  it("defaults the scope to user and sends only the options that are set", async () => {
    const { client, fetchMock } = makeClient([served("<p/>", { "Content-Type": "text/html" })]);
    await client.exportDocument("d", "html");
    expect(fetchMock.mock.calls[0]![0]).toBe("http://test-loomcycle:8787/v1/_document/export/d?format=html&scope=user");
  });

  it("sends every export option by its wire name", async () => {
    const { client, fetchMock } = makeClient([served("x", {})]);
    await client.exportDocument("d", "docx", {
      chunk: "c1",
      toc: true,
      titlePage: true,
      pageSize: "letter",
      language: "uk",
      provenance: false,
    });
    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/_document/export/d?format=docx&scope=user&chunk=c1&toc=true&title_page=true&page_size=letter&language=uk&provenance=false",
    );
  });

  it("names the file by the document id and format when the server names none, with no render id", async () => {
    const { client } = makeClient([served("PK", { "Content-Type": "application/zip" })]);
    const res = await client.exportDocument("d1", "md");
    expect(res.filename).toBe("d1.md");
    expect(res.mediaType).toBe("application/zip");
    expect(res.renderId).toBeUndefined();
  });

  it("raises the typed error carrying the server's code and message", async () => {
    const msg = "export: this server cannot write PDF: the typesetter (Typst) is not installed where it converts files";
    const { client } = makeClient([
      errorResponse(503, JSON.stringify({ code: "unavailable", error: msg })),
      errorResponse(404, JSON.stringify({ code: "not_found", error: "not found" })),
      errorResponse(400, JSON.stringify({ code: "bad_request", error: "format: want pdf, docx, html or md" })),
    ]);
    const err = await client.exportDocument("d", "pdf").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(UnavailableError);
    expect((err as UnavailableError).code).toBe("unavailable");
    expect((err as UnavailableError).message).toContain(msg);
    await expect(client.exportDocument("d", "pdf")).rejects.toBeInstanceOf(NotFoundError);
    await expect(client.exportDocument("d", "pdf")).rejects.toBeInstanceOf(InvalidArgumentError);
  });
});
