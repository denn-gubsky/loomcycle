package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// The File twin over HTTP, and the download of one inbox file:
//
//	POST /v1/runs/{run_id}/file              the twin (connector.File) on the run the path names
//	GET  /v1/runs/{run_id}/inbox/{path...}   one inbox file's bytes (HEAD for the headers alone)
//
// Both read the run through the twin's gate (fileTwinReadableRun), so a run
// the caller may not read is the missing run's not-found, and both hold the
// tree while they work. The route scope is runs:read for both; the twin
// refuses its writing ops to anyone but the run's owner or an admin itself.

// handleRunFile serves POST /v1/runs/{run_id}/file. The body is the twin's
// input; the run is the path's. A body may repeat run_id, but only with the
// path's value: two ids that disagree would leave which run was meant to the
// reader, so that is a 400. The response is the other tool routes': the
// tool's JSON on success, a 422 tool_refused envelope when it refuses.
func (s *Server) handleRunFile(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	guard := func(_ context.Context, body []byte) ([]byte, *guardError) {
		return fileRouteInput(body, runID)
	}
	s.dispatchSubstrateCtxGuarded(w, r, "File", s.File, fileRouteCtx, guard)
}

// fileRouteCtx is the context the twin runs in over HTTP, the MCP session's
// (mcpPrincipalCtx) on this transport: the caller's own subject and tenant
// with its isolated bit, the HTTP admin plane's agent identity (so an
// agent-scope import lands where /v1/_document reads it), and the Memory and
// SQL Memory scopes import's Document call needs. The twin itself narrows the
// tools and volumes to the op's own.
//
// Deliberately not substrateAdminCtx: that marks the call as the runtime
// operator's own (tools.WithSubstrateOperator), under which the File tool
// refuses import, and it grants def-family authority the twin never uses. A
// twin call is a principal's, as over MCP.
func fileRouteCtx(ctx context.Context) context.Context {
	p, ok := auth.PrincipalFromContext(ctx)
	ident := tools.RunIdentityValue{UserID: substrateAdminUserID, AgentID: substrateAdminAgentID}
	if !ok {
		// Open mode: no bearer is configured, so the caller is the operator.
		ctx = tools.WithUnauthenticatedOperator(ctx)
	} else {
		ident.TenantID = p.TenantID
		if p.Subject != "" {
			ident.UserID = p.Subject
		}
		ident.Isolated = auth.IsIsolated(p, ok)
	}
	ctx = tools.WithRunIdentity(ctx, ident)
	ctx = tools.WithAgentName(ctx, substrateAdminAgentName)
	return tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"agent", "user", "tenant"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"agent", "user", "tenant"}}
	})
}

// fileRouteInput puts the path's run_id into a body that has none, and
// refuses one whose run_id differs. The body's own keys are left for the
// twin's strict decoder to judge, so a case variant or a repeated key is
// refused there as on every other transport.
func fileRouteInput(body []byte, runID string) ([]byte, *guardError) {
	bad := func(msg string) ([]byte, *guardError) {
		return nil, &guardError{status: http.StatusBadRequest, code: "bad_request", msg: msg}
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) < 2 || trimmed[0] != '{' {
		return bad("body must be a JSON object: the File tool's input, without run_id")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	_, _ = dec.Token() // '{', checked above
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return bad("body: " + err.Error())
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return bad("body: " + err.Error())
		}
		if key, _ := tok.(string); key == "run_id" {
			var got string
			if json.Unmarshal(v, &got) != nil || got != runID {
				return bad("body run_id differs from the path's; leave it out")
			}
			return body, nil
		}
	}
	id, err := json.Marshal(runID)
	if err != nil {
		return bad("run_id: " + err.Error())
	}
	rest := bytes.TrimSpace(trimmed[1:])
	out := append([]byte(`{"run_id":`), id...)
	if len(rest) > 0 && rest[0] != '}' {
		out = append(out, ',')
	}
	return append(out, rest...), nil
}

// inboxDownloadTypes are the media types a download is served as. Each is one
// a browser saves, shows as an image, or plays — none it runs. Any other type
// (text/html, image/svg+xml, XML, scripts) is served as
// application/octet-stream, so a file a tool fetched can never execute in
// the origin that serves this API.
var inboxDownloadTypes = map[string]bool{
	"application/pdf":          true,
	"image/png":                true,
	"image/jpeg":               true,
	"image/gif":                true,
	"image/webp":               true,
	"video/mp4":                true,
	"text/plain":               true,
	"text/markdown":            true,
	"text/csv":                 true,
	"application/octet-stream": true,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   true,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         true,
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": true,
}

// inboxDownloadType is the Content-Type a file of media type mt is served
// as: mt without its parameters when it is safe, else application/octet-stream.
func inboxDownloadType(mt string) string {
	base, _, err := mime.ParseMediaType(mt)
	base = strings.ToLower(base)
	if err == nil && (inboxDownloadTypes[base] || strings.HasPrefix(base, "audio/")) {
		return base
	}
	return "application/octet-stream"
}

// inboxRelPath checks the {path...} of a download: a relative,
// slash-separated path of plain segments. "..", ".", empty segments, a
// leading "/", a backslash or a NUL are refused, and so is a hidden segment:
// the inbox writes a file under a hidden name until it is complete, and ls
// does not show those either. The os.Root the file is opened through is the
// fence; this is the clear refusal in front of it.
func inboxRelPath(p string) (string, bool) {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return "", false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || strings.HasPrefix(seg, ".") {
			return "", false
		}
	}
	return p, true
}

// handleRunInboxFile serves GET (and HEAD) /v1/runs/{run_id}/inbox/{path...}:
// the bytes of one file in the run tree's inbox, to any caller who may read
// the run. The response never lets a browser render what it serves: it is an
// attachment, nosniff, under a sandboxing CSP, and typed only from
// inboxDownloadTypes. X-Loomcycle-SHA256 is the hash of the served bytes, and
// X-Loomcycle-Origin the producer the runtime recorded — or "unknown" when the
// bytes no longer hash to the record (the run may write to its inbox).
//
// The hash is computed over the open file before it is served, and the file
// must not change while it is hashed. A writer rewriting the file in place
// while it streams is not detected; captures replace a file by renaming a new
// one in, which the open handle does not see.
func (s *Server) handleRunInboxFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	runID := r.PathValue("run_id")
	rel, ok := inboxRelPath(r.PathValue("path"))
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "the path must name a file inside the run's inbox: documents/a.pdf, with no \"..\", leading \"/\" or hidden segment")
		return
	}
	if !validIdent(runID) {
		writeJSONError(w, http.StatusNotFound, "not_found", fileTwinNotFound(runID).Text)
		return
	}
	run, res, ok, err := s.fileTwinReadableRun(ctx, runID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !ok {
		writeJSONError(w, http.StatusNotFound, "not_found", res.Text)
		return
	}
	box, res, ok := s.fileTwinHoldInbox(ctx, run)
	if !ok {
		if res.ErrorInfo != nil && res.ErrorInfo.Category == tools.CategoryTransient {
			writeJSONError(w, http.StatusServiceUnavailable, "unavailable", res.Text)
			return
		}
		writeJSONError(w, http.StatusNotFound, "not_found", res.Text)
		return
	}
	defer box.release()

	noFile := func() {
		writeJSONError(w, http.StatusNotFound, "not_found", "file: no file at "+inbox.Ref(rel)+" in run "+run.ID+"'s inbox")
	}
	root, err := os.OpenRoot(box.dir)
	if err != nil {
		noFile()
		return
	}
	defer root.Close()
	name := filepath.FromSlash(rel)
	// A symlink is not a file the runtime kept; the os.Root keeps one from
	// leading out, and this keeps it from being served at all.
	if fi, err := root.Lstat(name); err != nil || !fi.Mode().IsRegular() {
		noFile()
		return
	}
	f, err := root.Open(name)
	if err != nil {
		noFile()
		return
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() {
		noFile()
		return
	}
	sum := sha256.New()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	sum.Write(head)
	if _, err := io.Copy(sum, f); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable", "file: reading "+inbox.Ref(rel)+" failed; try again")
		return
	}
	after, err := f.Stat()
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable", "file: "+inbox.Ref(rel)+" changed while it was read; try again")
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable", "file: reading "+inbox.Ref(rel)+" failed; try again")
		return
	}
	digest := hex.EncodeToString(sum.Sum(nil))

	mediaType, origin := inbox.Sniff("", head, path.Base(rel)), "unknown"
	if tree, live := s.treeSets.liveInbox(box.root); live {
		if rec, ok := inbox.Lookup(inbox.With(ctx, s.inboxService(), tree), rel); ok && rec.SHA256 == digest {
			mediaType, origin = rec.MediaType, rec.Origin
		}
	}

	h := w.Header()
	h.Set("Content-Type", inboxDownloadType(mediaType))
	h.Set("Content-Disposition", attachmentDisposition(path.Base(rel)))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "private, no-store")
	h.Set("ETag", `"`+digest+`"`)
	h.Set("X-Loomcycle-SHA256", digest)
	h.Set("X-Loomcycle-Origin", origin)
	// The name is "" so ServeContent never types the response from it: the
	// Content-Type above is the only one.
	http.ServeContent(w, r, "", before.ModTime(), f)
}

// attachmentDisposition is a Content-Disposition that saves the file as name,
// encoded per RFC 8187 so any name is carried exactly and none can break out
// of the header.
func attachmentDisposition(name string) string {
	var b strings.Builder
	b.WriteString("attachment; filename*=UTF-8''")
	const hexDigits = "0123456789ABCDEF"
	for i := 0; i < len(name); i++ {
		c := name[i]
		if isAttrChar(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

// isAttrChar is RFC 8187's attr-char: the bytes a parameter value carries
// unencoded.
func isAttrChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("!#$&+-.^_`|~", c) >= 0
}

// isRunFileRoute matches the two routes above: POST /v1/runs/{run_id}/file,
// and GET or HEAD /v1/runs/{run_id}/inbox/... . Both read a run.
func isRunFileRoute(method, p string) bool {
	rest, ok := strings.CutPrefix(p, "/v1/runs/")
	if !ok {
		return false
	}
	id, sub, _ := strings.Cut(rest, "/")
	if id == "" {
		return false
	}
	switch method {
	case http.MethodPost:
		return sub == "file"
	case http.MethodGet, http.MethodHead:
		return sub == "inbox" || strings.HasPrefix(sub, "inbox/")
	}
	return false
}
