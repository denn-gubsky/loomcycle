package builtin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// File op=checkout and op=commit (RFC GW §5.6, P7): a set of Documents
// written into a workspace as Markdown files laid out as the Path tree is,
// changed there by a script or by hand, and committed back as new editions.
//
// The manifest a checkout writes (.loom/checkout.json) is in the workspace,
// where a command can rewrite it, so it never chooses what a commit writes
// onto: a file is committed onto the document the store names at the file's
// mapped path, which must be the one the manifest says, and a new file only
// to a path no document is named at. What a commit reads back is a script's
// text: its loom metadata is dropped and the rest neutralised, as every
// import is.

const (
	// checkoutManifestRel is the manifest's path in the checkout's folder.
	checkoutManifestRel = ".loom/checkout.json"
	// checkoutManifestMax bounds the manifest a commit reads.
	checkoutManifestMax = 16 << 20
	checkoutVersion     = 1
	// importFieldCheckout names, in an edition's provenance, the checkout
	// its text came back from.
	importFieldCheckout = "checkout"
)

// checkoutManifest is .loom/checkout.json.
type checkoutManifest struct {
	Version    int    `json:"version"`
	CheckoutID string `json:"checkout_id"`
	Scope      string `json:"scope"`
	// Root is the Path folder the files are laid out from: the checkout's
	// under_path, or "/" for a checkout of named documents.
	Root      string         `json:"root"`
	Workspace string         `json:"workspace"`
	RunID     string         `json:"run_id"`
	TakenAt   string         `json:"taken_at"`
	Files     []checkoutFile `json:"files"`
}

// checkoutFile is one file of a checkout.
type checkoutFile struct {
	// Path is the file's path in the checkout's folder.
	Path       string `json:"path"`
	DocumentID string `json:"document_id"`
	// DocPath is the document's Path name the file is laid out from.
	DocPath string `json:"doc_path"`
	// Revision is the document's content fingerprint when it was taken
	// (checkoutFingerprint).
	Revision string `json:"revision"`
	// SHA256 is the file's digest as written.
	SHA256 string `json:"sha256"`
}

// checkoutLeftOut is a document a checkout did not write, and why.
type checkoutLeftOut struct {
	DocumentID string `json:"document_id"`
	Path       string `json:"path,omitempty"`
	Reason     string `json:"reason"`
}

// checkoutBounds are the operator's workspace.checkout bounds.
func (f *File) checkoutBounds() (int, int64) {
	if f.Workspaces != nil {
		return f.Workspaces.CheckoutLimits()
	}
	return lifecycle.DefaultCheckoutMaxDocuments, lifecycle.DefaultCheckoutMaxBytes
}

func checkoutScope(op, s string) (string, tools.Result, bool) {
	switch s {
	case "":
		return "user", tools.Result{}, true
	case "agent", "user", "tenant":
		return s, tools.Result{}, true
	}
	return "", errValidation(op+": scope must be agent, user or tenant", ""), false
}

// docAnswer runs a Document call in process, through Document's gates, and
// decodes its answer. A refusal comes back as the result to return.
func (f *File) docAnswer(ctx context.Context, op string, in docInput) (map[string]any, tools.Result, bool) {
	din, raw := checkoutDocCall(in)
	res, err := f.Docs.executeInput(ctx, din, raw)
	if err != nil {
		return nil, errTransient(op+": the document store failed", "Try again."), false
	}
	if res.IsError {
		return nil, prefixed(op, res), false
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		return nil, errTransient(op+": could not read the document store's answer", ""), false
	}
	return out, tools.Result{}, true
}

// checkoutExportOf is a document as a checkout writes it: the export, its
// fingerprint and its Path names.
func (f *File) checkoutExportOf(ctx context.Context, op, scope, docID string) (md, revision string, paths []string, res tools.Result, ok bool) {
	meta := true
	out, res, ok := f.docAnswer(ctx, op, docInput{Op: "export_md", Scope: scope, DocumentID: docID, IncludeMetadata: &meta, checkout: &checkoutExport{}})
	if !ok {
		return "", "", nil, res, false
	}
	for _, p := range jsonArray(out["paths"]) {
		paths = append(paths, asStr(p))
	}
	return asStr(out["markdown"]), asStr(out["content_sha256"]), paths, tools.Result{}, true
}

// documentHook runs the run's Document pre-hooks on the call File makes for
// it, less any text, as {op, scope, ..., via: File}.
func documentHook(ctx context.Context, op string, call map[string]any) (tools.Result, bool) {
	c := make(map[string]any, len(call)+1)
	for k, v := range call {
		c[k] = v
	}
	c["via"] = FileToolName
	b, _ := json.Marshal(c)
	if res, ok := tools.HookPreCheck(ctx, "Document", b); !ok {
		return classified(op+": refused by a Document hook: "+res.Text+". Nothing was written.", tools.CategoryPermission, false,
			"Ask the operator about the hook."), false
	}
	return tools.Result{}, true
}

func needDocumentTool(ctx context.Context, op string) (tools.Result, bool) {
	if !slices.Contains(tools.PolicyOf(ctx).Tools, "Document") {
		return errPermission(op+" needs the Document tool, which this run does not have", "Ask an operator to grant the tool."), false
	}
	return tools.Result{}, true
}

func newCheckoutID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// checkoutRel is where a document named at docPath is written in a
// checkout laid out from root.
func checkoutRel(root, docPath string) (string, bool) {
	prefix := dirPrefix(root)
	if !strings.HasPrefix(docPath, prefix) {
		return "", false
	}
	return strings.TrimPrefix(docPath, prefix) + ".md", true
}

// checkoutDocPath is the Path a checkout's file maps to: the inverse of
// checkoutRel. ok is false for a file that maps to no valid Path name.
func checkoutDocPath(root, rel string) (string, bool) {
	if !strings.HasSuffix(rel, ".md") || strings.HasPrefix(rel, ".loom/") {
		return "", false
	}
	p, err := normalizePath(dirPrefix(root) + strings.TrimSuffix(rel, ".md"))
	if err != nil || p == "/" || p != dirPrefix(root)+strings.TrimSuffix(rel, ".md") {
		return "", false
	}
	return p, true
}

// jsonArray is a decoded JSON array, or nil.
func jsonArray(v any) []any {
	s, _ := v.([]any)
	return s
}

// ---- checkout ----

// checkoutPlanned is one document a checkout will write.
type checkoutPlanned struct {
	id, docPath, rel string
}

func (f *File) checkout(ctx context.Context, in fileInput) (tools.Result, error) {
	const op = "checkout"
	if f.Docs == nil {
		return errBusiness(op+": documents are not available here", "An operator must enable SQL Memory for the Document tool."), nil
	}
	if strings.TrimSpace(in.To) == "" {
		return errValidation(op+": missing required field: to", "Pass to, a folder of a workspace, e.g. workspace:work:docs/."), nil
	}
	to, err := fileRef(in.To)
	if err != nil || to.kind != refWorkspace {
		return errValidation(op+": to must be a folder of a workspace, workspace:<name>:<folder>/", ""), nil
	}
	scope, res, ok := checkoutScope(op, in.Scope)
	if !ok {
		return res, nil
	}
	if (in.UnderPath == "") == (len(in.Documents) == 0) {
		return errValidation(op+": pass exactly one of under_path and documents", ""), nil
	}
	if len(in.Documents) > checkoutMaxListed {
		return errValidation(fmt.Sprintf("%s: at most %d documents", op, checkoutMaxListed), ""), nil
	}
	root := "/"
	if in.UnderPath != "" {
		if root, err = normalizePath(in.UnderPath); err != nil {
			return errValidation(op+": under_path: "+err.Error(), docPathFix), nil
		}
	}
	// Every grant before anything is read: the Document tool, the workspace
	// grant write (and the pin), a folder that is not there yet.
	if res, ok := needDocumentTool(ctx, op); !ok {
		return res, nil
	}
	l, res, ok := f.workspaceLoc(ctx, to, true, FileToolName)
	if !ok {
		return res, nil
	}
	if l.path == "." {
		return errValidation(op+": to is the workspace's root; name a folder in it, e.g. workspace:"+l.name+":docs/", ""), nil
	}
	e, res, ok := l.Stat(ctx)
	if !ok {
		return prefixed(op, res), nil
	}
	if e.Type != "" {
		return errBusiness(fmt.Sprintf("%s: %s already exists; nothing was written", op, l.Ref()),
			"Check out into a new folder, or remove this one with File op=rm."), nil
	}
	listCall := map[string]any{"op": "query_documents", "scope": scope}
	if in.UnderPath != "" {
		listCall["under_path"] = root
	} else {
		listCall["documents"] = in.Documents
	}
	if res, ok := documentHook(ctx, op, listCall); !ok {
		return res, nil
	}
	listed, res, ok := f.docAnswer(ctx, op, docInput{Op: "query_documents", Scope: scope, UnderPath: in.UnderPath,
		checkoutList: &checkoutListRequest{ids: in.Documents, max: checkoutMaxListed}})
	if !ok {
		return res, nil
	}
	planned, leftOut := checkoutPlan(listed, root)
	maxDocs, maxBytes := f.checkoutBounds()
	if len(planned) > maxDocs {
		for _, p := range planned[maxDocs:] {
			leftOut = append(leftOut, checkoutLeftOut{DocumentID: p.id, Path: p.docPath,
				Reason: fmt.Sprintf("past the %d documents one checkout writes (workspace.checkout.max_documents)", maxDocs)})
		}
		planned = planned[:maxDocs]
	}
	// Each read and each write is judged before the first of them: a hook
	// that refuses one refuses the checkout with nothing written.
	for _, p := range planned {
		if res, ok := documentHook(ctx, op, map[string]any{"op": "export_md", "scope": scope, "document_id": p.id}); !ok {
			return res, nil
		}
		if res, ok := l.Judge(ctx, p.rel, true); !ok {
			return prefixed(op, res), nil
		}
	}
	if res, ok := l.Judge(ctx, checkoutManifestRel, true); !ok {
		return prefixed(op, res), nil
	}
	return f.writeCheckout(ctx, l, scope, root, planned, leftOut, maxBytes)
}

// checkoutPlan lays the listed documents out from root, in path order, and
// says why each one it cannot lay out is left out.
func checkoutPlan(listed map[string]any, root string) ([]checkoutPlanned, []checkoutLeftOut) {
	var planned []checkoutPlanned
	leftOut := []checkoutLeftOut{}
	for _, id := range jsonArray(listed["missing"]) {
		leftOut = append(leftOut, checkoutLeftOut{DocumentID: asStr(id), Reason: "no such document, or you may not read it"})
	}
	for _, raw := range jsonArray(listed["documents"]) {
		d, _ := raw.(map[string]any)
		id := asStr(d["document_id"])
		var docPath, rel string
		for _, p := range jsonArray(d["paths"]) {
			if r, ok := checkoutRel(root, asStr(p)); ok {
				docPath, rel = asStr(p), r
				break
			}
		}
		switch {
		case docPath == "":
			leftOut = append(leftOut, checkoutLeftOut{DocumentID: id, Reason: "it has no Path name under " + root + ", so there is no place to write it; give it one with Document op=set_path"})
		case strings.HasPrefix(rel, ".loom/"):
			leftOut = append(leftOut, checkoutLeftOut{DocumentID: id, Path: docPath, Reason: "its name falls in the checkout's .loom folder"})
		default:
			planned = append(planned, checkoutPlanned{id: id, docPath: docPath, rel: rel})
		}
	}
	slices.SortFunc(planned, func(a, b checkoutPlanned) int { return strings.Compare(a.rel, b.rel) })
	return planned, leftOut
}

// writeCheckout exports each planned document into a stage of the
// workspace, then the manifest, and commits the stage as the folder in one
// step: a failure leaves nothing.
func (f *File) writeCheckout(ctx context.Context, l *workspaceLoc, scope, root string, planned []checkoutPlanned, leftOut []checkoutLeftOut, maxBytes int64) (tools.Result, error) {
	const op = "checkout"
	stage, err := l.m.Stage(ctx, l.o, l.name)
	if err != nil {
		return prefixed(op, workspaceRefError(err, l.name)), nil
	}
	committed := false
	defer func() {
		if !committed {
			dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_ = l.m.Discard(dctx, l.o, l.name, stage)
		}
	}()
	man := checkoutManifest{Version: checkoutVersion, CheckoutID: newCheckoutID(), Scope: scope, Root: root,
		Workspace: l.name, RunID: l.o.RunID, TakenAt: time.Now().UTC().Format(time.RFC3339), Files: []checkoutFile{}}
	var total int64
	for _, p := range planned {
		md, rev, _, res, ok := f.checkoutExportOf(ctx, op, scope, p.id)
		if !ok {
			return res, nil
		}
		size := int64(len(md))
		switch {
		case size > fileImportMaxBytes:
			leftOut = append(leftOut, checkoutLeftOut{DocumentID: p.id, Path: p.docPath,
				Reason: fmt.Sprintf("its file would be %d bytes, over the %d a commit reads back", size, fileImportMaxBytes)})
			continue
		case total+size > maxBytes:
			leftOut = append(leftOut, checkoutLeftOut{DocumentID: p.id, Path: p.docPath,
				Reason: fmt.Sprintf("past the %d bytes one checkout writes (workspace.checkout.max_bytes)", maxBytes)})
			continue
		}
		if _, err := l.m.PutFile(ctx, l.o, l.name, workspace.PutArgs{Path: p.rel, Size: size, Stage: stage}, strings.NewReader(md)); err != nil {
			return prefixed(op, copyReadError(err)), nil
		}
		total += size
		sum := sha256.Sum256([]byte(md))
		man.Files = append(man.Files, checkoutFile{Path: p.rel, DocumentID: p.id, DocPath: p.docPath, Revision: rev, SHA256: hex.EncodeToString(sum[:])})
	}
	mb, _ := json.MarshalIndent(man, "", "  ")
	if _, err := l.m.PutFile(ctx, l.o, l.name, workspace.PutArgs{Path: checkoutManifestRel, Size: int64(len(mb)), Stage: stage}, bytes.NewReader(mb)); err != nil {
		return prefixed(op, copyReadError(err)), nil
	}
	if _, err := l.m.Commit(ctx, l.o, l.name, workspace.CommitArgs{Stage: stage, Path: l.path}); err != nil {
		return prefixed(op, workspaceRefError(err, l.name)), nil
	}
	committed = true
	return okJSON(map[string]any{"to": l.Ref(), "checkout_id": man.CheckoutID, "scope": scope, "root": root,
		"files": len(man.Files), "bytes": total, "manifest": l.child(checkoutManifestRel).Ref(), "left_out": leftOut})
}

// ---- commit ----

// The states a commit reports a file in.
const (
	commitUnchanged = "unchanged"
	commitChanged   = "changed"
	commitNew       = "new"
	commitMissing   = "missing"
	commitConflict  = "conflict"
	commitIgnored   = "ignored"
	commitCommitted = "committed"
	commitFailed    = "failed"
)

// commitItem is one file of a commit and what was found or done.
type commitItem struct {
	File         string `json:"file"`
	Status       string `json:"status"`
	DocumentID   string `json:"document_id,omitempty"`
	Path         string `json:"path,omitempty"`
	LinesAdded   int    `json:"lines_added,omitempty"`
	LinesRemoved int    `json:"lines_removed,omitempty"`
	Reason       string `json:"reason,omitempty"`
	// Edition is a committed file's: new (a new document) or the reimport's
	// report.
	Edition any `json:"edition,omitempty"`
	// action is what a real commit does with it (changed or new); sha is the
	// file's digest when compared; entry its manifest entry.
	action string
	sha    string
	entry  int
}

func (f *File) commit(ctx context.Context, in fileInput, raw json.RawMessage) (tools.Result, error) {
	const op = "commit"
	var opt struct {
		DryRun *bool `json:"dry_run"`
	}
	_ = json.Unmarshal(raw, &opt)
	dryRun := opt.DryRun == nil || *opt.DryRun
	if f.Docs == nil {
		return errBusiness(op+": documents are not available here", "An operator must enable SQL Memory for the Document tool."), nil
	}
	if strings.TrimSpace(in.From) == "" {
		return errValidation(op+": missing required field: from", "Pass from, the folder a checkout wrote, e.g. workspace:work:docs/."), nil
	}
	from, err := fileRef(in.From)
	if err != nil || from.kind != refWorkspace {
		return errValidation(op+": from must be the folder of a checkout in a workspace, workspace:<name>:<folder>/", ""), nil
	}
	if res, ok := needDocumentTool(ctx, op); !ok {
		return res, nil
	}
	l, res, ok := f.workspaceLoc(ctx, from, false, FileToolName)
	if !ok {
		return res, nil
	}
	if !dryRun {
		// A real commit rewrites the manifest.
		if _, res, ok := f.workspaceLoc(ctx, from, true, FileToolName); !ok {
			return res, nil
		}
	}
	ml := l.child(checkoutManifestRel)
	if res, ok := ml.hook(ctx, wsHookRead); !ok {
		return prefixed(op, res), nil
	}
	if !dryRun {
		if res, ok := ml.hook(ctx, wsHookWrite); !ok {
			return prefixed(op, res), nil
		}
	}
	man, res, ok := readCheckoutManifest(ctx, ml)
	if !ok {
		return prefixed(op, res), nil
	}
	items, res, ok := f.compareCheckout(ctx, l, man)
	if !ok {
		return prefixed(op, res), nil
	}
	if dryRun {
		return okJSON(commitAnswer(l, man, items, true))
	}
	// Every import is judged before the first one: a hook that refuses one
	// refuses the commit with nothing written.
	for _, it := range items {
		if it.action == "" {
			continue
		}
		if res, ok := documentHook(ctx, op, commitImportCall(man.Scope, it, "")); !ok {
			return res, nil
		}
	}
	committed := 0
	for i := range items {
		if items[i].action != "" {
			if f.commitOne(ctx, l, &man, &items[i]) {
				committed++
			}
		}
	}
	out := commitAnswer(l, man, items, false)
	if committed > 0 {
		mb, _ := json.MarshalIndent(man, "", "  ")
		if _, res, ok := ml.Create(ctx, int64(len(mb)), bytes.NewReader(mb), true); !ok {
			out["manifest_error"] = res.Text
		}
	}
	return okJSON(out)
}

// readCheckoutManifest reads and checks the manifest of a checkout.
func readCheckoutManifest(ctx context.Context, ml *workspaceLoc) (checkoutManifest, tools.Result, bool) {
	var man checkoutManifest
	e, res, ok := ml.Stat(ctx)
	if !ok {
		return man, res, false
	}
	if e.Type != workspace.EntryFile {
		return man, errNotFound("no checkout manifest at "+ml.Ref(), "Pass the folder File op=checkout wrote (its to)."), false
	}
	if e.Size > checkoutManifestMax {
		return man, errValidation(fmt.Sprintf("%s is over %d bytes", ml.Ref(), checkoutManifestMax), ""), false
	}
	rc, res, ok := ml.Open(ctx, ".")
	if !ok {
		return man, res, false
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, checkoutManifestMax+1))
	if err != nil {
		return man, copyReadError(err), false
	}
	bad := func(why string) (checkoutManifest, tools.Result, bool) {
		return man, errValidation(ml.Ref()+" is not a checkout manifest this runtime wrote: "+why, "Check the documents out again."), false
	}
	if err := json.Unmarshal(b, &man); err != nil {
		return bad(err.Error())
	}
	if man.Version != checkoutVersion {
		return bad(fmt.Sprintf("version %d", man.Version))
	}
	if _, res, ok := checkoutScope("commit", man.Scope); !ok || man.Scope == "" {
		if ok {
			res = errValidation("no scope", "")
		}
		return bad(res.Text)
	}
	if p, err := normalizePath(man.Root); err != nil || p != man.Root {
		return bad("root " + man.Root)
	}
	seen := map[string]bool{}
	for _, fe := range man.Files {
		if _, ok := checkoutDocPath(man.Root, fe.Path); !ok || fe.DocumentID == "" || seen[fe.Path] {
			return bad("file " + fe.Path)
		}
		seen[fe.Path] = true
	}
	return man, tools.Result{}, true
}

// compareCheckout compares the folder's files with the manifest and the
// store, reading nothing a Workspace hook refuses: every file it may read
// is judged before the first is read.
func (f *File) compareCheckout(ctx context.Context, l *workspaceLoc, man checkoutManifest) ([]commitItem, tools.Result, bool) {
	entries, truncated, res, ok := l.Walk(ctx, workspace.MaxListLimit)
	if !ok {
		return nil, res, false
	}
	if truncated {
		return nil, errValidation(fmt.Sprintf("%s holds more than %d files", l.Ref(), workspace.MaxListLimit), "Commit a smaller checkout."), false
	}
	present := map[string]locEntry{}
	for _, e := range entries {
		if !strings.HasPrefix(e.Path, ".loom/") {
			present[e.Path] = e
		}
	}
	var items []commitItem
	inManifest := map[string]bool{}
	for i, fe := range man.Files {
		inManifest[fe.Path] = true
		it := commitItem{File: fe.Path, DocumentID: fe.DocumentID, Path: fe.DocPath, entry: i}
		if e, ok := present[fe.Path]; !ok || e.Type != workspace.EntryFile {
			it.Status, it.Reason = commitMissing, "not in the folder; a commit never deletes a document"
		}
		items = append(items, it)
	}
	var fresh []string
	for p, e := range present {
		if inManifest[p] {
			continue
		}
		it := commitItem{File: p, entry: -1}
		docPath, ok := checkoutDocPath(man.Root, p)
		switch {
		case e.Type != workspace.EntryFile:
			it.Status, it.Reason = commitIgnored, "not a regular file"
		case !ok:
			it.Status, it.Reason = commitIgnored, "not a .md file whose name maps to a Path name (segments of letters, digits, '.', '-', '_')"
		default:
			it.Path = docPath
			fresh = append(fresh, docPath)
		}
		items = append(items, it)
	}
	slices.SortFunc(items, func(a, b commitItem) int { return strings.Compare(a.File, b.File) })
	for _, it := range items {
		if it.Status == "" {
			if res, ok := l.Judge(ctx, it.File, false); !ok {
				return nil, res, false
			}
		}
	}
	named := map[string]bool{}
	if len(fresh) > 0 {
		out, res, ok := f.docAnswer(ctx, "commit", docInput{Op: "query_documents", Scope: man.Scope, checkoutList: &checkoutListRequest{paths: fresh}})
		if !ok {
			return nil, res, false
		}
		for _, p := range jsonArray(out["named"]) {
			named[asStr(p)] = true
		}
	}
	for i := range items {
		it := &items[i]
		if it.Status != "" {
			continue
		}
		var res tools.Result
		var ok bool
		if it.entry >= 0 {
			res, ok = f.compareTaken(ctx, l, man, it)
		} else {
			res, ok = f.compareFresh(ctx, l, it, named[it.Path])
		}
		if !ok {
			return nil, res, false
		}
	}
	return items, tools.Result{}, true
}

// compareTaken classifies a file the checkout wrote. ok false is a failure
// of the whole comparison (a store or worker fault), not of the file.
func (f *File) compareTaken(ctx context.Context, l *workspaceLoc, man checkoutManifest, it *commitItem) (tools.Result, bool) {
	fe := man.Files[it.entry]
	fl := l.child(it.File)
	info, err := l.m.Stat(ctx, l.o, l.name, workspace.FStatArgs{Path: fl.path, SHA256: true})
	if err != nil {
		return workspaceRefError(err, l.name), false
	}
	it.sha = info.SHA256
	if info.SHA256 == fe.SHA256 {
		it.Status = commitUnchanged
		return tools.Result{}, true
	}
	mapped, _ := checkoutDocPath(man.Root, it.File)
	it.Path = mapped
	md, rev, paths, res, ok := f.checkoutExportOf(ctx, "commit", man.Scope, fe.DocumentID)
	switch {
	case !ok:
		// Gone, no longer readable, or not to be read now: the file is
		// refused and listed, as any conflict is.
		it.Status, it.Reason = commitConflict, "the document cannot be read from the store now: "+strings.TrimPrefix(res.Text, "commit: ")
		return tools.Result{}, true
	case !slices.Contains(paths, mapped):
		it.Status, it.Reason = commitConflict, "the document is no longer named at "+mapped
		return tools.Result{}, true
	case rev != fe.Revision:
		it.Status, it.Reason = commitConflict, "the document changed in the store after the checkout"
		return tools.Result{}, true
	}
	b, _, res, ok := readWorkspaceMarkdown(ctx, fl, dataRef{raw: fl.Ref()})
	if !ok {
		it.Status, it.Reason = commitIgnored, strings.TrimPrefix(res.Text, "import: ")
		return tools.Result{}, true
	}
	it.Status, it.action = commitChanged, commitChanged
	it.LinesAdded, it.LinesRemoved = lineDelta(md, string(b))
	return tools.Result{}, true
}

// compareFresh classifies a new file: one the checkout did not write.
func (f *File) compareFresh(ctx context.Context, l *workspaceLoc, it *commitItem, taken bool) (tools.Result, bool) {
	if taken {
		it.Status, it.Reason = commitConflict, "a document is already named at "+it.Path+", and this checkout did not take it"
		return tools.Result{}, true
	}
	fl := l.child(it.File)
	b, info, res, ok := readWorkspaceMarkdown(ctx, fl, dataRef{raw: fl.Ref()})
	if !ok {
		it.Status, it.Reason = commitIgnored, strings.TrimPrefix(res.Text, "import: ")
		return tools.Result{}, true
	}
	it.sha = info.SHA256
	it.Status, it.action = commitNew, commitNew
	it.LinesAdded, _ = lineDelta("", string(b))
	return tools.Result{}, true
}

// commitImportCall is the import_md a commit makes for it; the hooks see
// it without the text (md "").
func commitImportCall(scope string, it commitItem, md string) map[string]any {
	call := map[string]any{"op": "import_md", "scope": scope}
	if it.action == commitChanged {
		call["document_id"], call["reimport"] = it.DocumentID, true
	} else {
		call["path"] = it.Path
	}
	if md != "" {
		call["markdown"] = md
	}
	return call
}

// commitOne imports one changed or new file and records it in the
// manifest; false leaves it failed, with the reason on it.
func (f *File) commitOne(ctx context.Context, l *workspaceLoc, man *checkoutManifest, it *commitItem) bool {
	fail := func(why string) bool {
		it.Status, it.Reason = commitFailed, why
		return false
	}
	fl := l.child(it.File)
	b, info, res, ok := readWorkspaceMarkdown(ctx, fl, dataRef{raw: fl.Ref()})
	if !ok {
		return fail(strings.TrimPrefix(res.Text, "import: "))
	}
	if info.SHA256 != it.sha {
		return fail("the file changed while the commit ran")
	}
	md := commitText(string(b), path.Base(it.File))
	prov := workspaceImportProvenance(ctx, fl, info, b)
	prov.fields[importFieldCheckout] = man.CheckoutID
	prov.onNative = true
	ictx := withImportProvenance(withVolumeAssetReader(ctx, refuseCommitImage), prov)
	call, _ := json.Marshal(commitImportCall(man.Scope, *it, md))
	res, err := f.Docs.Execute(ictx, call)
	if err != nil {
		return fail("the document store failed")
	}
	if res.IsError {
		return fail(res.Text)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Text), &out)
	docID := asStr(out["document_id"])
	if it.action == commitNew {
		it.DocumentID, it.Edition = docID, "new"
	} else {
		it.Edition = out["reimport"]
	}
	tagCall, _ := json.Marshal(map[string]any{"op": "add_tags", "scope": man.Scope, "document_id": docID,
		"tags": []string{sourceTag("workspace:" + l.name), "untrusted"}})
	if tres, err := f.Docs.Execute(ctx, tagCall); err != nil || tres.IsError {
		it.Reason = "committed, but tagging it failed"
	}
	it.Status = commitCommitted
	// The manifest now says the file as it is, and the document as the
	// commit left it, so a second commit finds both unchanged. A document
	// the run can no longer export is left out of it, and so is a conflict
	// on the next commit.
	_, rev, _, _, ok := f.checkoutExportOf(ctx, "commit", man.Scope, docID)
	entry := checkoutFile{Path: it.File, DocumentID: docID, DocPath: it.Path, Revision: rev, SHA256: info.SHA256}
	if !ok {
		entry.Revision = ""
	}
	if it.entry >= 0 {
		man.Files[it.entry] = entry
	} else {
		man.Files = append(man.Files, entry)
	}
	return true
}

// refuseCommitImage is the image reader of a commit's import: a script's
// text may name a volume file, and the run's own Read grant must not attach
// it to a document. Pictures a checkout wrote are data: URLs, which need no
// reader.
func refuseCommitImage(context.Context, dataRef, int64) ([]byte, error) {
	return nil, errors.New("a commit attaches no file by reference; embed the picture as a data: URL")
}

// commitText is the Markdown a commit imports from a file: the loom
// metadata dropped (ids, types, relations: the store keeps its own, and a
// script's must not choose them), the rest neutralised as every import is,
// and a title from the file's name when the text has none.
func commitText(md, name string) string {
	md = extract.Neutralize(stripLoomMetadata(md), false)
	return withRootHeading(md, "", fileTitle(name))
}

// stripLoomMetadata drops the lines import_md acts on as metadata: a
// `<!-- loom: … -->` line and the `<!-- loom-edges:` block, outside fenced
// code, as parseLoomMarkdown reads them.
func stripLoomMetadata(md string) string {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	out := lines[:0]
	var fenceChar byte
	fenceRun := 0
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if c, run := mdFenceDelim(l); run > 0 {
			switch {
			case fenceRun == 0:
				fenceChar, fenceRun = c, run
			case c == fenceChar && run >= fenceRun:
				fenceRun = 0
			}
			out = append(out, l)
			continue
		}
		if fenceRun > 0 {
			out = append(out, l)
			continue
		}
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "<!-- loom-edges:") {
			for ; i < len(lines) && !strings.Contains(lines[i], "-->"); i++ {
			}
			continue
		}
		if strings.HasPrefix(t, "<!-- loom:") && strings.HasSuffix(t, "-->") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// lineDelta counts the lines added and removed from a to b, as multisets:
// exact counts, not an alignment.
func lineDelta(a, b string) (added, removed int) {
	count := map[string]int{}
	if a != "" {
		for _, l := range strings.Split(a, "\n") {
			count[l]++
		}
	}
	if b != "" {
		for _, l := range strings.Split(b, "\n") {
			count[l]--
		}
	}
	for _, n := range count {
		if n > 0 {
			removed += n
		} else {
			added -= n
		}
	}
	return added, removed
}

// commitAnswer is a commit's report: every file with its state, and the
// counts per state.
func commitAnswer(l *workspaceLoc, man checkoutManifest, items []commitItem, dryRun bool) map[string]any {
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Status]++
	}
	if items == nil {
		items = []commitItem{}
	}
	return map[string]any{"from": l.Ref(), "checkout_id": man.CheckoutID, "scope": man.Scope, "root": man.Root,
		"dry_run": dryRun, "counts": counts, "files": items}
}
