package http

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/text/unicode/norm"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/dynvol"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// A run owner's file put (RFC HE F1):
//
//	PUT /v1/runs/{run_id}/inbox/{name}   raw body, Content-Type = the file's type
//
// The way a person's own file reaches the run they are talking to: a chat
// bridge downloads an attachment and puts it here with the person's token, and
// tells the agent the ref. The agent then reads it with the File tool as it
// would a file a tool returned. Only the run's owner or an admin may put; a
// token limited to certain agents needs the inbox_put surface (the route is
// off the run plane).

// inboxPutOrigin is the origin a client put records.
const inboxPutOrigin = "client"

// inboxPutFallbackName names a file whose name sanitises to nothing.
const inboxPutFallbackName = "upload"

// inboxPutRoutes registers the run owner's file put beside the inbox's GET.
func (s *Server) inboxPutRoutes(mux *http.ServeMux) *http.ServeMux {
	mux.Handle("PUT /v1/runs/{run_id}/inbox/{name}", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleRunInboxPut))))
	return mux
}

// handleRunInboxPut serves PUT /v1/runs/{run_id}/inbox/{name}.
func (s *Server) handleRunInboxPut(w http.ResponseWriter, r *http.Request) {
	res, err := s.PutInboxFile(r.Context(), connector.InboxPutRequest{
		RunID:     r.PathValue("run_id"),
		Name:      r.PathValue("name"),
		MediaType: r.Header.Get("Content-Type"),
		Body:      r.Body,
		Size:      r.ContentLength,
	})
	var perr *connector.InboxPutError
	switch {
	case errors.As(err, &perr):
		writeJSONError(w, inboxPutHTTPStatus(perr.Kind), perr.Code, perr.Msg)
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// inboxPutHTTPStatus is a refused put's HTTP status.
func inboxPutHTTPStatus(k connector.InboxPutErrorKind) int {
	switch k {
	case connector.InboxPutNotFound:
		return http.StatusNotFound
	case connector.InboxPutBadRequest:
		return http.StatusBadRequest
	case connector.InboxPutNotLive:
		return http.StatusConflict
	case connector.InboxPutTooBig, connector.InboxPutInboxFull:
		return http.StatusRequestEntityTooLarge
	case connector.InboxPutElsewhere, connector.InboxPutUnavailable:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

func inboxPutRefused(kind connector.InboxPutErrorKind, code, msg string) *connector.InboxPutError {
	return &connector.InboxPutError{Kind: kind, Code: code, Msg: msg}
}

// PutInboxFile keeps req's bytes in the inbox of the run it names. The rules,
// in the order they are checked:
//
//   - The run is read through the gate every run read uses; a run the caller
//     may not read, and one it may read but does not own (unless it is an
//     admin), is the same not-found a missing run gets.
//   - The run, and its tree's top-level run, must not have ended (409): the
//     inbox goes with the tree.
//   - The tree must have a run in this process (503 otherwise): the inbox is
//     this process's disk and its index this process's memory.
//   - The bytes are streamed through the inbox's writer under the client put
//     cap and the tree's quota; over either, nothing is kept (413).
//   - The put is audited, without the file's name; if the record cannot be
//     written the file is removed again and the put refused.
func (s *Server) PutInboxFile(ctx context.Context, req connector.InboxPutRequest) (connector.InboxPutResult, error) {
	if !validIdent(req.RunID) {
		return connector.InboxPutResult{}, inboxPutRefused(connector.InboxPutNotFound, "not_found", fileTwinNotFound(req.RunID).Text)
	}
	if strings.TrimSpace(req.Name) == "" {
		return connector.InboxPutResult{}, inboxPutRefused(connector.InboxPutBadRequest, "bad_request", "the file needs a name: PUT /v1/runs/{run_id}/inbox/{name}")
	}
	run, res, ok, err := s.fileTwinReadableRun(ctx, req.RunID)
	if err != nil {
		return connector.InboxPutResult{}, err
	}
	if !ok {
		return connector.InboxPutResult{}, inboxPutRefused(connector.InboxPutNotFound, "not_found", res.Text)
	}
	p, hasP := auth.PrincipalFromContext(ctx)
	owner := hasP && p.Subject != "" && p.Subject == run.UserID && p.TenantID == run.TenantID
	if !owner && !fileTwinAdmin(p, hasP, run) {
		// Another user of the tenant may read the run (a tenant-wide read),
		// but not fill its inbox; told so the way a missing run is.
		return connector.InboxPutResult{}, inboxPutRefused(connector.InboxPutNotFound, "not_found", fileTwinNotFound(req.RunID).Text)
	}
	svc := s.inboxService()
	if svc.DynamicRoot == "" {
		return connector.InboxPutResult{}, inboxPutRefused(connector.InboxPutUnavailable, "unavailable",
			"no dynamic_root volume is configured, so no run keeps files")
	}

	root, _ := resumedRootRunID(ctx, s.store.GetRun, run)
	if !validIdent(root) {
		return connector.InboxPutResult{}, inboxPutRefused(connector.InboxPutNotFound, "not_found", fileTwinNotFound(req.RunID).Text)
	}
	if ended, err := s.inboxPutTreeEnded(ctx, run, root); err != nil {
		return connector.InboxPutResult{}, err
	} else if ended {
		return connector.InboxPutResult{}, inboxPutRefused(connector.InboxPutNotLive, "run_not_live",
			"run "+run.ID+" has ended, and its inbox with it; put the file into a live run")
	}
	// Held from here, so a tree that ends meanwhile is purged after the put
	// rather than under it.
	release := s.holdRunTree(root)
	defer release()
	set, tree, live := s.treeSets.live(root)
	if !live {
		return connector.InboxPutResult{}, inboxPutRefused(connector.InboxPutElsewhere, "run_elsewhere",
			"run "+run.ID+" is not served by this server; send the file to the server that runs it")
	}

	maxBytes := s.cfg().Env.FileClientPutMaxBytes
	if maxBytes <= 0 {
		maxBytes = svc.MaxFileBytes
	}
	uploader := p.Subject
	if uploader == "" {
		uploader = "operator" // open mode, or the legacy bearer
	}
	var mu sync.Mutex
	var captured []providers.Event
	pctx := tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: run.UserID, TenantID: run.TenantID, RootRunID: root})
	pctx = tools.WithRunID(pctx, run.ID)
	pctx = tools.WithEphemeralVolumes(pctx, set)
	pctx = s.withEphemeralRecord(pctx, root)
	pctx = inbox.With(pctx, svc, tree)
	// The event is written once the put is recorded, not when the bytes land.
	pctx = tools.WithEventEmitter(pctx, func(ev providers.Event) {
		mu.Lock()
		captured = append(captured, ev)
		mu.Unlock()
	})
	// NFC first: a name sent decomposed (и + combining breve) is the same
	// name as its composed form, and the sanitiser maps letters by code point.
	kept := inbox.CaptureReader(pctx, inbox.Item{
		Name:       norm.NFC.String(req.Name),
		Fallback:   inboxPutFallbackName,
		MediaType:  req.MediaType,
		Origin:     inboxPutOrigin,
		UploadedBy: uploader,
		MaxBytes:   maxBytes,
	}, req.Body, req.Size)
	if !kept.Kept {
		// The stub carries the reason and never the name; only the class is
		// logged.
		msg := strings.TrimSuffix(strings.TrimPrefix(kept.Stub, "["), "]")
		switch kept.Refusal {
		case inbox.RefusedTooBig:
			return connector.InboxPutResult{}, inboxPutRefused(connector.InboxPutTooBig, "client_put_max_bytes", msg)
		case inbox.RefusedInboxFull:
			return connector.InboxPutResult{}, inboxPutRefused(connector.InboxPutInboxFull, "inbox_full", msg)
		}
		slog.WarnContext(ctx, "inbox put: the file was not kept", "run_id", run.ID)
		return connector.InboxPutResult{}, inboxPutRefused(connector.InboxPutUnavailable, "unavailable", msg)
	}
	rec := kept.Record

	if s.tokenAudit != nil {
		ev := audit.Event{Action: "run_inbox_put", TargetTenant: run.TenantID,
			InboxRunID: run.ID, InboxBytes: rec.Size, InboxMediaType: rec.MediaType, InboxSHA256: rec.SHA256}
		if hasP {
			ev.ActorTenant, ev.ActorSubject, ev.ActorTokenSuffix = p.TenantID, p.Subject, p.TokenSuffix
		}
		if aerr := s.tokenAudit.Record(ev); aerr != nil {
			// A put is recorded or it is not kept.
			slog.ErrorContext(ctx, "audit: an inbox put was refused because its record could not be written", "run_id", run.ID, "err", aerr)
			s.inboxPutUndo(pctx, svc, root, rec.Path)
			return connector.InboxPutResult{}, inboxPutRefused(connector.InboxPutUnavailable, "audit_unavailable", auditUnavailableMessage)
		}
	}
	s.appendInboxPutEvents(ctx, run.ID, captured)
	return connector.InboxPutResult{
		Ref:       rec.Ref,
		Name:      filepath.Base(filepath.FromSlash(rec.Path)),
		Bytes:     rec.Size,
		SHA256:    rec.SHA256,
		MediaType: rec.MediaType,
	}, nil
}

// inboxPutTreeEnded reports whether the run, or its tree's top-level run, has
// ended.
func (s *Server) inboxPutTreeEnded(ctx context.Context, run store.Run, root string) (bool, error) {
	if store.IsTerminalRunStatus(run.Status) {
		return true, nil
	}
	if root == run.ID {
		return false, nil
	}
	top, err := s.store.GetRun(ctx, root)
	var nf *store.ErrNotFound
	switch {
	case errors.As(err, &nf):
		return true, nil
	case err != nil:
		return false, err
	}
	return store.IsTerminalRunStatus(top.Status), nil
}

// inboxPutUndo removes a kept file whose put could not be recorded, and gives
// its allowance back.
func (s *Server) inboxPutUndo(ctx context.Context, svc inbox.Service, root, rel string) {
	dir := dynvol.DerivedEphemeralPath(svc.DynamicRoot, root, inbox.Name)
	if r, err := os.OpenRoot(dir); err == nil {
		if rerr := r.Remove(filepath.FromSlash(rel)); rerr != nil {
			slog.ErrorContext(ctx, "inbox put: could not remove a file whose put was refused", "root_run_id", root, "err", rerr)
		}
		_ = r.Close()
	}
	inbox.Forget(ctx, rel)
}

// appendInboxPutEvents writes the put's file_captured event to the run's
// transcript, where a re-attached stream and the run views read it. Best
// effort: a kept file is not refused for a transcript line.
func (s *Server) appendInboxPutEvents(ctx context.Context, runID string, evs []providers.Event) {
	for _, ev := range evs {
		payload, err := json.Marshal(ev)
		if err != nil {
			slog.ErrorContext(ctx, "inbox put: could not encode the event", "run_id", runID, "err", err)
			continue
		}
		if err := s.store.AppendEvent(context.WithoutCancel(ctx), runID, string(ev.Type), payload); err != nil {
			slog.ErrorContext(ctx, "inbox put: the file was kept but its transcript line was not written", "run_id", runID, "err", err)
		}
	}
}
