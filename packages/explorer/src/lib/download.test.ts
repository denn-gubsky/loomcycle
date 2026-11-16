import { describe, expect, it } from "vitest";
import { errorMessageFromBody, fileStem, filenameFromContentDisposition } from "./download";
import { documentExportPath } from "./dataLayer";

describe("filenameFromContentDisposition", () => {
  it("prefers the UTF-8 filename* over the ASCII fallback", () => {
    const h = `attachment; filename="Zvit_2026.pdf"; filename*=UTF-8''%D0%97%D0%B2%D1%96%D1%82%202026.pdf`;
    expect(filenameFromContentDisposition(h)).toBe("Звіт 2026.pdf");
  });

  it("reads filename* whatever its case and language tag", () => {
    expect(filenameFromContentDisposition(`attachment; FILENAME*=utf-8'en'board%20report.docx`)).toBe(
      "board report.docx",
    );
  });

  it("falls back to the quoted filename when filename* cannot be decoded", () => {
    expect(filenameFromContentDisposition(`attachment; filename="plan.pdf"; filename*=UTF-8''%E0%A4%A`)).toBe(
      "plan.pdf",
    );
  });

  it("reads an unquoted filename token", () => {
    expect(filenameFromContentDisposition("attachment; filename=plan.html")).toBe("plan.html");
  });

  it("unescapes a quoted filename", () => {
    expect(filenameFromContentDisposition(`attachment; filename="a \\"b\\".md"`)).toBe(`a "b".md`);
  });

  it("drops any directory part of the name", () => {
    expect(filenameFromContentDisposition(`attachment; filename*=UTF-8''..%2F..%2Fetc%2Fpasswd`)).toBe("passwd");
    expect(filenameFromContentDisposition(`attachment; filename*=UTF-8''C%3A%5Cx%5Cy.pdf`)).toBe("y.pdf");
  });

  it("names nothing when the header names no file", () => {
    expect(filenameFromContentDisposition(null)).toBeUndefined();
    expect(filenameFromContentDisposition("attachment")).toBeUndefined();
    expect(filenameFromContentDisposition(`attachment; filename=""`)).toBeUndefined();
  });
});

describe("errorMessageFromBody", () => {
  it("reads the flat {code, error} envelope", () => {
    expect(errorMessageFromBody(`{"code":"not_found","error":"document not found"}`, 404)).toBe("document not found");
  });

  it("falls back to the status line for a body that is not an error envelope", () => {
    expect(errorMessageFromBody("<html>bad gateway</html>", 502, "Bad Gateway")).toBe("502 Bad Gateway");
    expect(errorMessageFromBody(`{"code":"busy"}`, 503, "Service Unavailable")).toBe("503 Service Unavailable");
  });
});

describe("fileStem", () => {
  it("keeps the .md download's file-name rule", () => {
    expect(fileStem("Board report: Q3/2026")).toBe("Board_report_Q3_2026");
    expect(fileStem("")).toBe("document");
  });
});

describe("documentExportPath", () => {
  it("carries the format, scope and browse subject, and only the options that are set", () => {
    expect(documentExportPath("doc 1", "tenant", "pdf", { scopeId: "alice", tenant: "acme" })).toBe(
      "/v1/_document/export/doc%201?format=pdf&scope=tenant&scope_id=alice&tenant=acme",
    );
    expect(documentExportPath("d", "user", "docx")).toBe("/v1/_document/export/d?format=docx&scope=user");
  });

  it("sends every export option by its wire name", () => {
    const p = documentExportPath("d", "user", "pdf", undefined, {
      chunk: "c1",
      toc: true,
      titlePage: true,
      pageSize: "letter",
      language: "uk",
      provenance: false,
    });
    expect(p).toBe(
      "/v1/_document/export/d?format=pdf&scope=user&chunk=c1&toc=true&title_page=true&page_size=letter&language=uk&provenance=false",
    );
  });
});
