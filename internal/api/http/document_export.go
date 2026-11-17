package http

import (
	"cmp"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// documentFileRoutes registers the document routes that answer with bytes
// rather than JSON.
func (s *Server) documentFileRoutes(mux *http.ServeMux) {
	// RFC BO — serve an image chunk's raw bytes. Same auth posture as
	// /v1/_document (bearer + ScopeTenant, scope resolved from the principal,
	// opaque-404 cross-scope). GET because the payload is binary, not JSON.
	mux.Handle("GET /v1/_document/asset/{chunk_id}", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleSubstrateDocumentAsset))))
	// RFC GR §4.4 — a Document rendered as a file (pdf, docx, html, md) and
	// streamed as an attachment, off-run: the Web UI's download menu and the
	// adapters' exportDocument. Gated like the asset GET above.
	mux.Handle("GET /v1/_document/export/{document_id}", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleDocumentExport))))
}

// handleDocumentExport serves GET /v1/_document/export/{document_id} (RFC GR
// §4.4): a Document rendered as a file (pdf, docx, html, md) and streamed as
// an attachment, for the Web UI's "Download as" menu and the adapters. No
// run is involved.
//
// SECURITY: gated like GET /v1/_document/asset/{chunk_id} — the scope,
// tenant and subject come from the principal (browseSubjectFor), never the
// wire, and the document is read through every gate Document op=export_md
// passes. A document that is missing, in a scope the caller may not browse,
// or not readable to it is the same opaque 404. Content-Type is the format's
// own (never sniffed) with nosniff, and the file is always an attachment, so
// a rendered HTML page is never shown in the server's origin.
func (s *Server) handleDocumentExport(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("document_id"))
	q := r.URL.Query()
	f := s.exportFileTool()
	if id == "" || f == nil || s.store == nil || s.sqlMem == nil {
		http.NotFound(w, r) // nothing to export from (opaque)
		return
	}
	scope := cmp.Or(strings.TrimSpace(q.Get("scope")), "user")
	b, ok := s.browseSubjectFor(r.Context(), "document", q.Get("scope_id"), q.Get("tenant"), false, scope)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := b.apply(r.Context())
	req := builtin.ExportRequest{
		Document: id, Scope: scope, Chunk: strings.TrimSpace(q.Get("chunk")), Format: q.Get("format"),
		TOC: queryBool(q, "toc"), TitlePage: queryBool(q, "title_page"),
		PageSize: q.Get("page_size"), Language: q.Get("language"), Metadata: q.Get("metadata"),
	}
	if v := q.Get("provenance"); v != "" {
		on := queryBool(q, "provenance")
		req.Provenance = &on
	}
	p, hasP := auth.PrincipalFromContext(r.Context())
	if hasP {
		req.Actor = p.Subject
	}
	file, xerr, err := f.Export(ctx, req)
	if err != nil {
		slog.ErrorContext(r.Context(), "document export failed", "document_id", id, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal", "export failed")
		return
	}
	if xerr != nil {
		writeExportError(w, r, xerr)
		return
	}

	if s.tokenAudit != nil {
		ev := audit.Event{Action: "document_exported", TargetName: "document:" + id,
			ExportFormat: strings.ToLower(req.Format), ExportScope: scope, ExportBytes: int64(len(file.Data))}
		if hasP {
			ev.ActorTenant, ev.ActorSubject, ev.ActorTokenSuffix, ev.TargetTenant = p.TenantID, p.Subject, p.TokenSuffix, p.TenantID
		}
		if aerr := s.tokenAudit.Record(ev); aerr != nil {
			// The bulk read is recorded or it is not served.
			slog.ErrorContext(r.Context(), "audit: a document export was refused because its record could not be written", "document_id", id, "err", aerr)
			writeAuditUnavailable(w)
			return
		}
	}

	h := w.Header()
	h.Set("Content-Type", file.MediaType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", attachmentDisposition(file.Name))
	h.Set("Content-Length", strconv.Itoa(len(file.Data)))
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	if file.RenderID != "" {
		h.Set("X-Loomcycle-Render-Id", file.RenderID)
	}
	w.WriteHeader(http.StatusOK)
	// #nosec G705 -- an attachment of the rendered format's own media type, nosniff and a sandboxing CSP: never shown as a page of this origin
	_, _ = w.Write(file.Data)
}

// exportFileTool is the server's File tool, which renders.
func (s *Server) exportFileTool() *builtin.File {
	for _, t := range s.tools {
		if f, ok := t.(*builtin.File); ok {
			return f
		}
	}
	return nil
}

func writeExportError(w http.ResponseWriter, r *http.Request, e *builtin.ExportError) {
	switch e.Kind {
	case builtin.ExportInvalid:
		writeJSONError(w, http.StatusBadRequest, "bad_request", e.Message)
	case builtin.ExportNotFound:
		http.NotFound(w, r) // opaque: missing, not readable and forbidden are one answer
	case builtin.ExportUnprocessable:
		// tool_refused: the code the adapters read as a refusal of what was
		// asked, not of how it was asked.
		writeJSONError(w, http.StatusUnprocessableEntity, "tool_refused", e.Message)
	case builtin.ExportTooLarge:
		writeJSONError(w, http.StatusRequestEntityTooLarge, "too_large", e.Message)
	case builtin.ExportBusy:
		w.Header().Set("Retry-After", "30")
		writeJSONError(w, http.StatusServiceUnavailable, "busy", e.Message)
	case builtin.ExportUnavailable:
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable", e.Message)
	default:
		writeJSONError(w, http.StatusInternalServerError, "internal", e.Message)
	}
}

// queryBool reads a flag: true, 1, yes or on.
func queryBool(q url.Values, key string) bool {
	switch strings.ToLower(strings.TrimSpace(q.Get(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
