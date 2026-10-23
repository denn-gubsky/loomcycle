package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// File op=download and op=upload move a file between a URL and a volume
// without its bytes passing through the model. Both go out on the HTTP tool's
// policy (its host check, as narrowed for the run, and its guarded dialer);
// download follows a redirect only to the same host, upload none.
//
// A pre-authenticated URL (a Microsoft Graph download or upload-session URL)
// is a credential for as long as it lives. Results show it with its query as
// "?…", the run's redactor is given the whole URL so a transcript that echoes
// it is masked, and an inbox record names only the host.

const (
	// fileTransferTimeout bounds one download or upload end to end; waiting
	// for a response's headers is bounded by the HTTP tool's timeout.
	// Assumed: 10 minutes moves the 64 MiB default file cap on a slow link.
	fileTransferTimeout = 10 * time.Minute
	// uploadChunkUnit is the multiple Microsoft Graph requires of every
	// chunk of an upload session but the last.
	uploadChunkUnit    = 320 << 10
	uploadChunkDefault = 12 * uploadChunkUnit  // 3.75 MiB
	uploadChunkCeiling = 192 * uploadChunkUnit // 60 MiB, Graph's largest
	// uploadExcerptBytes is how much of an upload's reply is returned.
	uploadExcerptBytes = 1024
)

type fileTransferInput struct {
	URL         string            `json:"url"`
	To          string            `json:"to"`
	From        string            `json:"from"`
	Headers     map[string]string `json:"headers"`
	MaxBytes    int64             `json:"max_bytes"`
	Overwrite   bool              `json:"overwrite"`
	Method      string            `json:"method"`
	ContentType string            `json:"content_type"`
	Ranged      bool              `json:"ranged"`
	ChunkBytes  int64             `json:"chunk_bytes"`
}

// transferURL checks the url and headers of a download or upload and hands
// the URL to the redactor.
func (f *File) transferURL(ctx context.Context, op string, in fileTransferInput) (*url.URL, tools.Result, bool) {
	if f.HTTP == nil {
		return nil, errBusiness(op+": not available in this deployment", "Ask the operator to enable the HTTP tool's host allowlist."), false
	}
	u, err := parseTransferURL(in.URL)
	if err != nil {
		return nil, errValidation(op+": "+err.Error(), "Pass url, e.g. https://example.com/files/report.pdf."), false
	}
	// The whole URL, query and all: a transcript that repeats it is masked
	// from here on, whether or not the request is made.
	if f.RegisterSecret != nil {
		f.RegisterSecret(u.String())
	}
	if !f.HTTP.hostPermitted(ctx, u.Hostname()) {
		return nil, errPermission(fmt.Sprintf("%s: host %q is not on the allowlist", op, u.Hostname()), "Use a host the operator allows."), false
	}
	if err := checkTransferHeaders(in.Headers); err != nil {
		return nil, errValidation(op+": "+err.Error(), "A pre-authenticated URL carries its own authorisation; pass only non-secret headers."), false
	}
	return u, tools.Result{}, true
}

func (f *File) transferRequest(ctx context.Context, method string, u *url.URL, headers map[string]string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, errors.New("could not build the request")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// ---- download ----

func (f *File) download(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	var in fileTransferInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errValidation("download: invalid input: "+err.Error(), ""), nil
	}
	u, res, ok := f.transferURL(ctx, "download", in)
	if !ok {
		return res, nil
	}
	shown := displayURL(u)
	limit := inbox.MaxFileBytes(ctx)
	switch {
	case in.MaxBytes < 0:
		return errValidation("download: max_bytes must be positive", ""), nil
	case in.MaxBytes > limit:
		return errValidation(fmt.Sprintf("download: max_bytes is over the per-file limit of %d bytes", limit), "Omit max_bytes to take the limit."), nil
	case in.MaxBytes > 0:
		limit = in.MaxBytes
	}

	// Where it goes is settled before anything is requested, so a call that
	// cannot write sends nothing.
	to := dataRef{raw: inbox.Ref("."), kind: refVolume, volume: inbox.Name, path: "."}
	if strings.TrimSpace(in.To) != "" {
		r, err := fileRef(in.To)
		if err != nil || r.kind != refVolume {
			return errValidation("download: to must be a volume reference", "Pass to, e.g. volume:docs:reports/ — or omit it for the inbox."), nil
		}
		to = r
	}
	if to.volume == inbox.Name {
		if !inbox.Available(ctx) {
			return errBusiness("download: this run has no inbox to keep files in", "Pass to: a read-write volume of yours."), nil
		}
	} else {
		if !slices.Contains(tools.PolicyOf(ctx).Tools, "Write") {
			return errPermission(fmt.Sprintf("download: %s needs the Write tool, which this run does not have", to.raw), "Download into the inbox (omit to), or ask an operator to grant Write."), nil
		}
		if _, _, err := effectiveRoot(ctx, to.volume, true); err != nil {
			return errPermission("download: "+err.Error(), "Name a read-write volume you are bound to."), nil
		}
	}

	ctx, cancel := context.WithTimeout(ctx, fileTransferTimeout)
	defer cancel()
	client := f.HTTP.transferClient(ctx, redirectSameHost)
	defer client.CloseIdleConnections()
	req, err := f.transferRequest(ctx, http.MethodGet, u, in.Headers, nil)
	if err != nil {
		return errValidation("download: "+err.Error(), ""), nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return errBusiness(fmt.Sprintf("download: %s: %s", shown, transferError(err)), "Check that the URL's host is allowed and reachable."), nil
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return errBusiness(fmt.Sprintf("download: %s answered HTTP %d", shown, resp.StatusCode), "Check the URL; a pre-authenticated one may have expired."), nil
	}
	if resp.ContentLength > limit {
		return errValidation(fmt.Sprintf("download: %s is %d bytes, over the limit of %d", shown, resp.ContentLength, limit), ""), nil
	}
	body := io.Reader(&overLimitReader{r: resp.Body, n: limit})
	name := responseFileName(resp)
	origin := "download:" + u.Hostname()

	if to.volume == inbox.Name {
		it := inbox.Item{Name: name, MediaType: resp.Header.Get("Content-Type"), Origin: origin, Fallback: "download"}
		if p := strings.Trim(to.path, "/"); p != "" && p != "." {
			if strings.HasSuffix(to.path, "/") {
				it.Dir = p
			} else {
				it.Dir, it.Name = path.Dir(p), path.Base(p)
				if it.Dir == "." {
					it.Dir = ""
				}
			}
		}
		kept := inbox.CaptureReader(ctx, it, body, resp.ContentLength)
		if !kept.Kept {
			return errBusiness("download: "+strings.Trim(kept.Stub, "[]"), ""), nil
		}
		rec := kept.Record
		return okJSON(map[string]any{"url": shown, "status": resp.StatusCode, "ref": rec.Ref, "media_type": rec.MediaType,
			"size": rec.Size, "sha256": rec.SHA256, "origin": rec.Origin, "file": kept.Stub})
	}
	return f.downloadToVolume(ctx, to, name, in.Overwrite, body, resp, shown)
}

// downloadToVolume writes a response body to a durable volume as keep writes
// a copy: the target resolved through the volume's fence, Write's grant and
// hooks judged on the final path, a temporary file renamed into place.
func (f *File) downloadToVolume(ctx context.Context, to dataRef, name string, overwrite bool, body io.Reader, resp *http.Response, shown string) (tools.Result, error) {
	fileName := inbox.SanitizeName(name)
	if fileName == "" {
		fileName = "download"
	}
	root, fence, err := effectiveRoot(ctx, to.volume, true)
	if err != nil {
		return errPermission("download: "+err.Error(), "Name a read-write volume you are bound to."), nil
	}
	toPath := to.path
	if strings.HasSuffix(toPath, "/") {
		toPath += fileName
	}
	target, err := resolveParentInsideRoot(root, toPath)
	if err == nil {
		err = outsideFence(fence, target)
	}
	if err != nil {
		return errPermission("download: "+err.Error(), "Create the folder first, or name one that exists."), nil
	}
	dst, err := openVolumeFS(root, fence)
	if err != nil {
		return errPermission("download: "+err.Error(), ""), nil
	}
	defer dst.Close()
	rel, err := dst.rel(target)
	if err != nil {
		return errPermission("download: "+err.Error(), ""), nil
	}
	dir, err := dst.openDir(filepath.Dir(rel))
	if err != nil {
		return errNotFound("download: the folder for "+to.raw+" does not exist", "Create it first, or name one that exists."), nil
	}
	defer dir.Close()
	base := filepath.Base(rel)
	if fi, err := dir.Lstat(base); err == nil && fi.IsDir() {
		sub, err := dst.step(dir, base)
		if err != nil {
			return errPermission("download: "+err.Error(), ""), nil
		}
		defer sub.Close()
		dir, base, rel = sub, fileName, filepath.Join(rel, fileName)
	}
	final := dataRef{raw: refPrefix(to) + filepath.ToSlash(rel), kind: refVolume, volume: to.volume, path: filepath.ToSlash(rel)}
	if res, ok := checkRefGrant(ctx, final, true, FileToolName, "Download into the inbox"); !ok {
		return res, nil
	}
	if fi, err := dir.Lstat(base); err == nil && (!fi.Mode().IsRegular() || !overwrite) {
		return errBusiness(fmt.Sprintf("download: %s already exists", final.raw), "Pass overwrite: true to replace it, or choose another name."), nil
	}
	tmp, tmpName, err := createTemp(dir, ".loomcycle-download-")
	if err != nil {
		return errTransient("download: "+err.Error(), "Try again."), nil
	}
	defer dir.Remove(tmpName)
	sum := sha256.New()
	head := &headWriter{max: 512}
	n, err := io.Copy(io.MultiWriter(tmp, sum, head), body)
	closeErr := tmp.Close()
	if err != nil {
		return errBusiness(fmt.Sprintf("download: %s: %s", shown, transferError(err)), ""), nil
	}
	if closeErr != nil {
		return errTransient("download: "+closeErr.Error(), "Try again."), nil
	}
	if err := ctx.Err(); err != nil {
		return errTransient("download: the call ended before the file was complete", "Try again."), nil
	}
	if err := dir.Rename(tmpName, base); err != nil {
		return errTransient("download: "+err.Error(), "Try again."), nil
	}
	return okJSON(map[string]any{"url": shown, "status": resp.StatusCode, "ref": final.raw, "size": n,
		"sha256": hex.EncodeToString(sum.Sum(nil)), "media_type": inbox.Sniff(resp.Header.Get("Content-Type"), head.b, base)})
}

// overLimitReader fails once more than n bytes have come through, so a body
// over its cap is refused, never cut short and kept.
type overLimitReader struct {
	r io.Reader
	n int64
}

func (o *overLimitReader) Read(p []byte) (int, error) {
	n, err := o.r.Read(p)
	o.n -= int64(n)
	if o.n < 0 {
		return n, errors.New("the file passed its size limit")
	}
	return n, err
}

// headWriter keeps the first bytes written through it, for a sniff.
type headWriter struct {
	b   []byte
	max int
}

func (h *headWriter) Write(p []byte) (int, error) {
	if room := h.max - len(h.b); room > 0 {
		h.b = append(h.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// ---- upload ----

func (f *File) upload(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	var in fileTransferInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errValidation("upload: invalid input: "+err.Error(), ""), nil
	}
	method := strings.ToUpper(strings.TrimSpace(in.Method))
	if method == "" {
		method = http.MethodPut
	}
	if method != http.MethodPut && method != http.MethodPost {
		return errValidation("upload: method must be PUT or POST", ""), nil
	}
	if strings.ContainsAny(in.ContentType, "\r\n") {
		return errValidation("upload: content_type is not a valid header value", ""), nil
	}
	from, err := fileRef(in.From)
	if err != nil {
		return errValidation("upload: from: "+err.Error(), "Pass from, e.g. volume:inbox:documents/a.pdf."), nil
	}
	u, res, ok := f.transferURL(ctx, "upload", in)
	if !ok {
		return res, nil
	}
	shown := displayURL(u)
	src, rel, res, ok := openRef(ctx, from, false)
	if !ok {
		return res, nil
	}
	defer src.Close()
	fh, err := src.open(rel)
	if err != nil {
		return errNotFound(fmt.Sprintf("upload: no file at %s", from.raw), "List the folder with op=ls."), nil
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errValidation(fmt.Sprintf("upload: %s is not a file", from.raw), ""), nil
	}
	size := info.Size()

	ctx, cancel := context.WithTimeout(ctx, fileTransferTimeout)
	defer cancel()
	client := f.HTTP.transferClient(ctx, redirectNone)
	defer client.CloseIdleConnections()
	up := &uploader{f: f, client: client, u: u, method: method, headers: in.Headers, contentType: in.ContentType, shown: shown}
	var status int
	var sent int64
	var reply string
	if in.Ranged && size > 0 {
		chunk := in.ChunkBytes
		if chunk <= 0 {
			chunk = uploadChunkDefault
		}
		chunk = clampChunk(chunk)
		status, sent, reply, err = up.ranged(ctx, fh, size, chunk)
	} else {
		if up.contentType == "" {
			head := make([]byte, 512)
			n, _ := fh.ReadAt(head, 0)
			up.contentType = inbox.Sniff("", head[:n], info.Name())
		}
		status, sent, reply, err = up.single(ctx, fh, size)
	}
	if err != nil {
		return errBusiness(fmt.Sprintf("upload: %s: %s", shown, err.Error()), "Check that the URL's host is allowed and the URL still valid."), nil
	}
	out := map[string]any{"url": shown, "from": from.raw, "status": status, "bytes_sent": sent, "size": size, "response": reply}
	if status/100 != 2 {
		b, _ := json.Marshal(out)
		return errBusiness(fmt.Sprintf("upload: %s answered HTTP %d: %s", shown, status, b), "Check the URL; an upload URL may have expired."), nil
	}
	return okJSON(out)
}

type uploader struct {
	f           *File
	client      *http.Client
	u           *url.URL
	method      string
	headers     map[string]string
	contentType string
	shown       string
}

// send makes one request with body and returns its status and an excerpt of
// its reply (every URL's query in it shown as "?…").
func (up *uploader) send(ctx context.Context, body io.Reader, length int64, contentRange string) (int, string, []byte, error) {
	req, err := up.f.transferRequest(ctx, up.method, up.u, up.headers, body)
	if err != nil {
		return 0, "", nil, err
	}
	req.ContentLength = length
	if length == 0 {
		req.Body = http.NoBody
	}
	if up.contentType != "" {
		req.Header.Set("Content-Type", up.contentType)
	}
	if contentRange != "" {
		req.Header.Set("Content-Range", contentRange)
	}
	resp, err := up.client.Do(req)
	if err != nil {
		return 0, "", nil, errors.New(transferError(err))
	}
	defer resp.Body.Close()
	// Enough of the reply to read a session's next ranges; the rest is
	// drained up to a bound so the connection can be reused.
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, excerpt(reply), reply, nil
}

func (up *uploader) single(ctx context.Context, fh io.Reader, size int64) (int, int64, string, error) {
	status, reply, _, err := up.send(ctx, fh, size, "")
	if err != nil {
		return 0, 0, "", err
	}
	return status, size, reply, nil
}

// ranged sends the file in chunks of an upload session: each request carries
// Content-Range: bytes a-b/total; a 202 reply's nextExpectedRanges says where
// the next chunk starts (absent, it follows on from the last); 200 or 201
// ends it. Any other status is the answer. The number of requests is bounded,
// so a server that never finishes cannot hold the call.
func (up *uploader) ranged(ctx context.Context, fh io.ReaderAt, size, chunk int64) (int, int64, string, error) {
	var sent, next int64
	limit := 2*((size+chunk-1)/chunk) + 4
	for range limit {
		end := next + chunk - 1
		if end >= size {
			end = size - 1
		}
		cr := fmt.Sprintf("bytes %d-%d/%d", next, end, size)
		status, reply, raw, err := up.send(ctx, io.NewSectionReader(fh, next, end-next+1), end-next+1, cr)
		if err != nil {
			return 0, sent, "", err
		}
		sent += end - next + 1
		switch {
		case status == http.StatusOK || status == http.StatusCreated:
			return status, sent, reply, nil
		case status != http.StatusAccepted:
			return status, sent, reply, nil
		}
		n, ok := nextExpectedStart(raw)
		if !ok {
			n = end + 1
		}
		if n < 0 || n >= size {
			return status, sent, reply, fmt.Errorf("the server expects bytes from %d of a %d-byte file", n, size)
		}
		next = n
	}
	return 0, sent, "", fmt.Errorf("the upload did not finish in %d requests", limit)
}

// clampChunk rounds a chunk size down to a multiple of the unit, between one
// unit and the ceiling.
func clampChunk(n int64) int64 {
	n = n / uploadChunkUnit * uploadChunkUnit
	if n < uploadChunkUnit {
		return uploadChunkUnit
	}
	if n > uploadChunkCeiling {
		return uploadChunkCeiling
	}
	return n
}

// nextExpectedStart reads the start of the first range in an upload
// session's nextExpectedRanges ("26-", "26-1048575").
func nextExpectedStart(reply []byte) (int64, bool) {
	var s struct {
		Next []string `json:"nextExpectedRanges"`
	}
	if json.Unmarshal(reply, &s) != nil || len(s.Next) == 0 {
		return 0, false
	}
	start, _, _ := strings.Cut(s.Next[0], "-")
	n, err := strconv.ParseInt(strings.TrimSpace(start), 10, 64)
	return n, err == nil
}

// excerpt is the start of a reply as text, cut on a character, with every
// URL's query shown as "?…".
func excerpt(b []byte) string {
	s := string(b)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	if len(s) > uploadExcerptBytes {
		s, _ = cutOnRune(s, uploadExcerptBytes)
		s += "…"
	}
	return redactURLQueries(s)
}
