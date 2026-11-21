package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
)

// Conversions of a workspace's file (RFC GW D34) run on its worker; the core
// keeps the quarantine. A file that crashed the conversion child is refused
// for the same workspace, by its sha256, for the quarantine time, as RFC GO
// refuses one that crashed a conversion worker. It is kept per workspace,
// not per worker: another run's workspace on the same worker is another
// agent's, and one hostile file must not deny it.

// ErrQuarantined refuses a conversion of a file that crashed the
// conversion child of this workspace's worker within the quarantine time.
var ErrQuarantined = errors.New("this file crashed the workspace's file converter and will not be tried again here for now")

// DefaultConvertQuarantine is the quarantine time of a Limits that names
// none: config conversion.quarantine_ttl_s's default.
const DefaultConvertQuarantine = time.Hour

// maxQuarantined bounds the quarantine's entries; past it the oldest go.
const maxQuarantined = 10000

type quarantine struct {
	mu    sync.Mutex
	until map[string]time.Time
}

func (q *quarantine) key(workspaceID, sha string) string { return workspaceID + "|" + sha }

func (q *quarantine) has(key string, now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	t, ok := q.until[key]
	if ok && now.After(t) {
		delete(q.until, key)
		return false
	}
	return ok
}

func (q *quarantine) add(key string, until time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.until == nil {
		q.until = map[string]time.Time{}
	}
	if len(q.until) >= maxQuarantined {
		oldK, oldT := "", time.Time{}
		for k, t := range q.until {
			if oldK == "" || t.Before(oldT) {
				oldK, oldT = k, t
			}
		}
		delete(q.until, oldK)
	}
	q.until[key] = until
}

// ConvertArgs are a conversion op's arguments: the target the manager
// fills in.
type ConvertArgs interface {
	Target() *workspace.ConvertTarget
}

// ConvertRequest is one conversion of a file in the run's workspace Source.
type ConvertRequest struct {
	// Op is a workspace.OpConvert* op; Args its arguments.
	Op   string
	Args ConvertArgs
	// Source is the workspace the file is in; SHA256 the file's digest, as
	// Stat reported it, which the worker checks again.
	Source, SHA256 string
	// To, when set, is the run's workspace the result is written into on
	// the worker, at Args.Target().Path; it must be on Source's worker.
	// Empty streams the result to out.
	To string
	// MaxOutput bounds what streams back.
	MaxOutput int64
}

// SameWorker reports whether two of the run's workspaces are on one worker
// (and one tenant), so a conversion of a file in from can write into to
// there.
func (m *Manager) SameWorker(ctx context.Context, o Owner, from, to string) (bool, error) {
	a, err := m.ready(ctx, o, from)
	if err != nil {
		return false, err
	}
	b, err := m.ready(ctx, o, to)
	if err != nil {
		return false, err
	}
	return a.WorkerID == b.WorkerID && a.TenantID == b.TenantID, nil
}

// Convert runs a conversion of a file in one of the run's workspaces on its
// worker. A file quarantined for that workspace is refused before the
// worker is asked; one that crashes the conversion child is quarantined.
// Into a workspace (req.To), the answer is decoded into into; streamed, the
// output goes to out and into is not used.
func (m *Manager) Convert(ctx context.Context, o Owner, req ConvertRequest, out io.Writer, into any) error {
	src, err := m.ready(ctx, o, req.Source)
	if err != nil {
		return err
	}
	key := m.quarantine.key(src.ID, req.SHA256)
	if m.quarantine.has(key, m.now()) {
		return ErrQuarantined
	}
	t := req.Args.Target()
	t.Workspace = ""
	if req.To != "" {
		to, err := m.ready(ctx, o, req.To)
		if err != nil {
			return err
		}
		if to.WorkerID != src.WorkerID || to.TenantID != src.TenantID {
			return ErrNotSameWorker
		}
		t.Workspace = to.ID
	}
	raw, err := json.Marshal(req.Args)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	w := out
	limit := req.MaxOutput
	if req.To != "" || w == nil {
		w, limit = &buf, maxAnswer
	}
	_, err = m.disp.Stream(ctx, src.WorkerID, dispatch.Request{TenantID: src.TenantID, WorkspaceID: src.ID, Op: req.Op, Args: raw, MaxOutput: limit}, nil, w)
	if dispatch.ClassOf(err) == workspace.ClassConversionCrashed {
		ttl := m.limits.ConvertQuarantine
		m.quarantine.add(key, m.now().Add(ttl))
		m.log.Warn("a workspace's file crashed its worker's conversion child; it is quarantined for that workspace",
			"workspace_id", src.ID, "worker_id", src.WorkerID, "op", req.Op, "for", ttl)
		return fmt.Errorf("%w: %w", ErrQuarantined, err)
	}
	if err != nil || req.To == "" {
		return err
	}
	return decodeAnswer(req.Op, buf.Bytes(), into)
}

// maxSpliceInput bounds an md_splice's input: what a worker reads of an op's
// input before it runs it.
const maxSpliceInput = 8 << 20

// ErrSpliceTooLarge refuses OCR text over what one md_splice carries.
var ErrSpliceTooLarge = fmt.Errorf("the OCR text is over the %d MiB one splice carries", maxSpliceInput>>20)

// SpliceMarkdown writes OCR text into a Markdown file in the run's
// workspace name, on its worker (workspace.OpMDSplice): the file must still
// have args.SHA256, and is replaced in one rename. Only in crosses to the
// worker; the Markdown never leaves it.
func (m *Manager) SpliceMarkdown(ctx context.Context, o Owner, name string, args workspace.MDSpliceArgs, in workspace.MDSpliceInput) (workspace.MDSpliceAnswer, error) {
	var out workspace.MDSpliceAnswer
	r, err := m.ready(ctx, o, name)
	if err != nil {
		return out, err
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return out, err
	}
	body, err := json.Marshal(in)
	if err != nil {
		return out, err
	}
	if len(body) > maxSpliceInput {
		return out, ErrSpliceTooLarge
	}
	res, err := m.disp.Do(ctx, r.WorkerID, dispatch.Request{TenantID: r.TenantID, WorkspaceID: r.ID, Op: workspace.OpMDSplice, Args: raw,
		Input: bytes.NewReader(body), MaxOutput: maxAnswer})
	if err != nil {
		return out, err
	}
	return out, decodeAnswer(workspace.OpMDSplice, res.Output, &out)
}
