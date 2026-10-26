// Package inbox keeps the files a run's tools return (RFC FF).
//
// An agent acts through tool calls, and a tool call is text: a file that
// reached the model as base64 could not be produced, held or read by it. So a
// file a tool returns is written here, to an ephemeral volume of the run tree
// named "inbox", and the model is handed a one-line stub that names it. Every
// producer (MCP results, HTTP and WebFetch responses, File op=download) goes
// through Capture, so the caps, the names, the stub and the event cannot drift
// between them.
//
// The volume is provisioned on the first capture, through the same three
// steps an agent-created ephemeral volume takes (directory, row, the tree's
// set), and is removed with the tree's other ephemeral volumes. Writes here
// are the runtime's, not the agent's: a sub-agent whose ephemeral volumes are
// read-only still has its tool results kept.
package inbox

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
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/dynvol"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/redact"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Name is the inbox volume's name. It is reserved: VolumeDef refuses to
// create a volume by it.
const Name = "inbox"

// The folders at the inbox volume's root.
const (
	DirDocuments = "documents"
	DirImages    = "images"
	DirWeb       = "web"
	DirExtracted = "extracted"
)

// Defaults for the caps when the operator sets none.
const (
	DefaultMaxFileBytes = 64 << 20
	DefaultMaxTreeBytes = 1 << 30
)

// Store is what the inbox reads and writes of the store: the ephemeral volume
// rows a provisioned inbox is recorded in, and the persistent volumes a
// tenant may already have under the reserved name.
type Store interface {
	EphemeralVolumeCreate(ctx context.Context, row store.EphemeralVolumeDefRow) (store.EphemeralVolumeDefRow, error)
	EphemeralVolumeListByRun(ctx context.Context, rootRunID string) ([]store.EphemeralVolumeDefRow, error)
	VolumeDefGetByName(ctx context.Context, tenantID, name string) (store.VolumeDefRow, error)
}

// Service is the deployment's side of the inbox: where it may be made, what
// records it, and how much it may hold. One per run start; it holds no state.
type Service struct {
	// DynamicRoot is the operator's dynamic_root volume path; "" when none is
	// configured, and then nothing is kept.
	DynamicRoot string
	Store       Store
	// MaxFileBytes caps one file; MaxTreeBytes caps the run tree's inbox.
	// Zero takes the default.
	MaxFileBytes int64
	MaxTreeBytes int64
	// StaticVolumeNames are the operator's yaml volumes. One named "inbox"
	// would be shadowed by the ephemeral inbox, so its presence refuses.
	StaticVolumeNames []string
}

func (s Service) maxFile() int64 {
	if s.MaxFileBytes > 0 {
		return s.MaxFileBytes
	}
	return DefaultMaxFileBytes
}

func (s Service) maxTree() int64 {
	if s.MaxTreeBytes > 0 {
		return s.MaxTreeBytes
	}
	return DefaultMaxTreeBytes
}

// Tree is one run tree's inbox state: shared by every run of the tree in this
// process, as the tree's ephemeral volume set is.
type Tree struct {
	mu   sync.Mutex
	root string // the provisioned directory; "" until the first capture
	used int64
	// index holds what the runtime knows of each kept file, by its path
	// relative to the inbox root. A file it has no entry for (one the agent
	// wrote, or one kept before a resume) is still a file; stat recomputes.
	index map[string]Record
	// registered counts the secrets the tree's tools handed the redactor.
	registered int
	// shared is what other tools keep per run tree beside the inbox (Shared).
	shared map[any]any
}

// NewTree returns an empty tree state.
func NewTree() *Tree { return &Tree{index: map[string]Record{}} }

// Record is what the runtime recorded about one kept file.
type Record struct {
	Ref        string    `json:"ref"`
	Path       string    `json:"path"` // relative to the inbox root, slash-separated
	MediaType  string    `json:"media_type"`
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"`
	Origin     string    `json:"origin"`
	RunID      string    `json:"run_id,omitempty"`
	CapturedAt time.Time `json:"captured_at"`
	// Page and Slide are where in its document an extracted picture was:
	// the extractor says, and an import gives the image chunk the same.
	Page  int `json:"page,omitempty"`
	Slide int `json:"slide,omitempty"`
	// URL is where a download came from, kept so an import can link back to
	// it: without user information, and with its credential-bearing query
	// parameters masked (redact.MaskSignedURL) — never the URL that still
	// authorises a request.
	URL string `json:"url,omitempty"`
	// ExtractedFrom is the record of the file this one was extracted from,
	// copied when the extraction read bytes that still hashed to it: what an
	// import of the extracted text says the document came from.
	ExtractedFrom *Record `json:"extracted_from,omitempty"`
}

// Item is one file a producer hands over.
type Item struct {
	// Name is the producer's name for the file (a URI's last segment, a
	// field of the result). Only its last segment is used, sanitised.
	Name string
	// MediaType is the type the producer declared; "" or a generic type is
	// replaced by a sniff of the bytes.
	MediaType string
	// Origin is mcp:<server>/<tool>, http, webfetch or download:<host>.
	Origin string
	// Dir overrides the folder chosen from the media type (WebFetch assets
	// go under web/<host>). Slash-separated, relative, sanitised per segment.
	Dir string
	// Fallback names the file when Name is empty (a tool's name).
	Fallback string
	// Page and Slide place a picture an extraction found (see Record).
	Page, Slide int
	// URL is a download's URL; it is recorded masked (see Record.URL).
	URL string
	// ExtractedFrom is the trusted record of the file an extraction read.
	ExtractedFrom *Record
}

// Result is what a capture produced: the stub the model sees and, when the
// file was kept, its record.
type Result struct {
	Stub   string
	Kept   bool
	Record Record
}

type ctxKey struct{}

type handle struct {
	svc  Service
	tree *Tree
}

// With attaches the inbox to a run's context. Sub-agents inherit it through
// the context chain; a resumed run attaches its tree's existing state.
func With(ctx context.Context, svc Service, tree *Tree) context.Context {
	if tree == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, handle{svc: svc, tree: tree})
}

func from(ctx context.Context) (handle, bool) {
	h, ok := ctx.Value(ctxKey{}).(handle)
	return h, ok
}

// Available reports whether a capture on ctx can keep a file at all.
func Available(ctx context.Context) bool {
	h, ok := from(ctx)
	return ok && h.svc.DynamicRoot != ""
}

// Capture keeps data in the run tree's inbox and returns the stub that stands
// for it. It never fails outward: a file that cannot be kept yields a refusal
// stub saying why, so the bytes never reach the model either way.
func Capture(ctx context.Context, it Item, data []byte) Result {
	return CaptureReader(ctx, it, bytes.NewReader(data), int64(len(data)))
}

// CaptureReader is Capture over a stream. size is the length when known, -1
// when not; an unknown length is counted as it is written and the file is
// dropped when it passes a cap.
func CaptureReader(ctx context.Context, it Item, r io.Reader, size int64) Result {
	h, ok := from(ctx)
	if !ok {
		return refused(it, "", size, "this run has no inbox")
	}
	return h.capture(ctx, it, r, size)
}

func (h handle) capture(ctx context.Context, it Item, r io.Reader, size int64) Result {
	maxFile := h.svc.maxFile()
	if size > maxFile {
		return refused(it, it.MediaType, size, fmt.Sprintf("%s is over the %s per-file limit", humanBytes(size), humanBytes(maxFile)))
	}
	root, err := h.ensure(ctx)
	if err != nil {
		return refused(it, it.MediaType, size, err.Error())
	}
	if size >= 0 && !h.reserve(size) {
		return refused(it, it.MediaType, size, fmt.Sprintf("the run's inbox is full (%s)", humanBytes(h.svc.maxTree())))
	}
	reserved := max(size, 0)

	rec, err := h.write(ctx, root, it, r, maxFile, &reserved)
	if err != nil {
		h.release(reserved)
		return refused(it, it.MediaType, size, err.Error())
	}
	h.tree.mu.Lock()
	h.tree.index[rec.Path] = rec
	h.tree.mu.Unlock()

	tools.EventEmitter(ctx)(providers.Event{Type: providers.EventFileCaptured, File: &providers.FileCapturedInfo{
		Ref: rec.Ref, MediaType: rec.MediaType, Size: rec.Size, SHA256: rec.SHA256, Origin: rec.Origin, RunID: rec.RunID,
	}})
	return Result{Stub: Stub(rec), Kept: true, Record: rec}
}

// reserve takes n bytes of the tree's allowance, or reports that they are
// not there.
func (h handle) reserve(n int64) bool {
	h.tree.mu.Lock()
	defer h.tree.mu.Unlock()
	if h.tree.used+n > h.svc.maxTree() {
		return false
	}
	h.tree.used += n
	return true
}

func (h handle) release(n int64) {
	h.tree.mu.Lock()
	h.tree.used = max(h.tree.used-n, 0)
	h.tree.mu.Unlock()
}

// write streams r into a new file under the inbox. The bytes go to a hidden
// temporary first and are renamed over a name reserved with O_EXCL, so no
// reader sees a half-written file and two captures never share a name.
// reserved is the allowance already taken for this file; a stream of unknown
// length takes more as it is read.
func (h handle) write(ctx context.Context, root string, it Item, r io.Reader, maxFile int64, reserved *int64) (Record, error) {
	ir, err := os.OpenRoot(root)
	if err != nil {
		return Record{}, fmt.Errorf("could not open the inbox: %v", err)
	}
	defer ir.Close()

	head := make([]byte, 512)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return Record{}, fmt.Errorf("could not read the file: %v", err)
	}
	head = head[:n]
	name := SanitizeName(it.Name)
	if name == "" {
		name = SanitizeName(it.Fallback)
	}
	mt := settleMediaType(it.MediaType, head, name)
	name = withExtension(name, mt)

	dir := it.Dir
	if dir == "" {
		dir = DirFor(mt)
	}
	dir = sanitizeDir(dir)
	if err := ir.MkdirAll(dir, 0o700); err != nil {
		return Record{}, fmt.Errorf("could not make %s in the inbox: %v", dir, err)
	}
	final, err := reserveName(ir, dir, name)
	if err != nil {
		return Record{}, err
	}
	tmp, tmpName, err := createTemp(ir, dir)
	if err != nil {
		_ = ir.Remove(final)
		return Record{}, err
	}
	sum := sha256.New()
	cw := &countingWriter{w: io.MultiWriter(tmp, sum)}
	src := io.MultiReader(bytes.NewReader(head), r)
	// One byte past the cap is read, so a file that passes it is told apart
	// from one that ends exactly on it.
	_, copyErr := io.Copy(cw, &capReader{r: src, n: maxFile + 1})
	closeErr := tmp.Close()
	fail := func(err error) (Record, error) {
		_ = ir.Remove(tmpName)
		_ = ir.Remove(final)
		return Record{}, err
	}
	if copyErr != nil {
		return fail(fmt.Errorf("could not write the file: %v", copyErr))
	}
	if closeErr != nil {
		return fail(fmt.Errorf("could not write the file: %v", closeErr))
	}
	if cw.n > maxFile {
		return fail(fmt.Errorf("the file passed the %s per-file limit", humanBytes(maxFile)))
	}
	if extra := cw.n - *reserved; extra > 0 {
		if !h.reserve(extra) {
			return fail(fmt.Errorf("the run's inbox is full (%s)", humanBytes(h.svc.maxTree())))
		}
		*reserved += extra
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := ir.Rename(tmpName, final); err != nil {
		return fail(fmt.Errorf("could not keep the file: %v", err))
	}
	return Record{
		Ref:           Ref(final),
		Path:          final,
		MediaType:     mt,
		Size:          cw.n,
		SHA256:        hex.EncodeToString(sum.Sum(nil)),
		Origin:        it.Origin,
		RunID:         tools.RunID(ctx),
		CapturedAt:    time.Now().UTC(),
		Page:          max(it.Page, 0),
		Slide:         max(it.Slide, 0),
		URL:           recordURL(it.URL),
		ExtractedFrom: extractedFrom(it.ExtractedFrom),
	}, nil
}

// recordURL is the form of a URL a record keeps: no user information, and
// the credential-bearing query parameters masked. "" for what is not a URL,
// rather than a string that might hold a credential in clear.
func recordURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	u.User = nil
	return redact.MaskSignedURL(u.String())
}

// extractedFrom copies a source record one level deep: the original file,
// not a chain of every extraction between it and this one.
func extractedFrom(r *Record) *Record {
	if r == nil {
		return nil
	}
	c := *r
	c.ExtractedFrom = nil
	return &c
}

// ensure provisions the inbox the first time the tree needs it and returns
// its directory.
func (h handle) ensure(ctx context.Context) (string, error) {
	h.tree.mu.Lock()
	defer h.tree.mu.Unlock()
	if h.tree.root != "" {
		return h.tree.root, nil
	}
	ident := tools.RunIdentity(ctx)
	set := tools.EphemeralVolumes(ctx)
	if ident.RootRunID == "" || set == nil {
		return "", errors.New("this run has no run tree to keep files in")
	}
	// A resumed tree whose inbox was made before the pause finds it in the
	// set the resume rebuilt from the rows.
	if h.svc.DynamicRoot == "" {
		return "", errors.New("no dynamic_root volume is configured, so tool results cannot be kept as files — see docs/VOLUMES.md")
	}
	derived := dynvol.DerivedEphemeralPath(h.svc.DynamicRoot, ident.RootRunID, Name)
	if ref, ok := set.Get(Name); ok {
		// Taken only at the path the runtime derives for this tree. The set
		// is built from rows; whatever a row says, the files are written
		// nowhere else.
		if filepath.Clean(ref.Root) != filepath.Clean(derived) {
			return "", fmt.Errorf("the run's volume named %q is not its inbox", Name)
		}
		h.adopt(derived)
		return derived, nil
	}
	if slices.Contains(h.svc.StaticVolumeNames, Name) {
		return "", fmt.Errorf("a configured volume is named %q, which is reserved for the run's inbox", Name)
	}
	if h.svc.Store == nil {
		return "", errors.New("no store to record the inbox in")
	}
	for _, tenant := range dedupe(ident.TenantID, "") {
		_, err := h.svc.Store.VolumeDefGetByName(ctx, tenant, Name)
		if err == nil {
			return "", fmt.Errorf("a volume named %q already exists, and the name is reserved for the run's inbox; rename that volume", Name)
		}
		var nf *store.ErrNotFound
		if !errors.As(err, &nf) {
			return "", fmt.Errorf("could not check for a volume named %q: %v", Name, err)
		}
	}
	dir, created, undo, err := dynvol.ProvisionEphemeral(h.svc.DynamicRoot, ident.RootRunID, Name)
	if err != nil {
		return "", err
	}
	recorded := false
	if !created {
		// The directory is there and the set does not have it: a run of the
		// tree on this replica made it and the set was rebuilt without it.
		// The row decides, as it does for any ephemeral volume.
		rows, err := h.svc.Store.EphemeralVolumeListByRun(ctx, ident.RootRunID)
		if err != nil {
			return "", fmt.Errorf("could not read the run's volumes: %v", err)
		}
		recorded = slices.ContainsFunc(rows, func(r store.EphemeralVolumeDefRow) bool { return r.Name == Name })
	}
	if !recorded {
		body, _ := json.Marshal(dynvol.Body{Path: dir, Mode: "rw"})
		if _, err := h.svc.Store.EphemeralVolumeCreate(ctx, store.EphemeralVolumeDefRow{
			RootRunID: ident.RootRunID, Name: Name, TenantID: ident.TenantID, Definition: body,
		}); err != nil {
			// The sweeper finds ephemeral directories by their rows, so one
			// left without a row would never be removed.
			undo()
			return "", fmt.Errorf("could not record the inbox: %v", err)
		}
	}
	set.Add(Name, tools.EphemeralVolumeRef{Root: dir})
	if created {
		h.tree.root = dir
	} else {
		h.adopt(dir)
	}
	return dir, nil
}

// adopt takes an inbox that already holds files (a resumed tree's, or one a
// run of the tree made before this process's state existed) and counts what
// is in it, so the tree's cap holds across a resume. The tree's lock is held.
func (h handle) adopt(dir string) {
	h.tree.root = dir
	h.tree.used = max(h.tree.used, usedOnDisk(dir))
}

// usedOnDisk sums the regular files under dir, read through an os.Root so a
// link inside cannot make it count, or reach, anything outside.
func usedOnDisk(dir string) int64 {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return 0
	}
	defer r.Close()
	var n int64
	_ = fs.WalkDir(r.FS(), ".", func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			n += fi.Size()
		}
		return nil
	})
	return n
}

func dedupe(a, b string) []string {
	if a == b {
		return []string{a}
	}
	return []string{a, b}
}

// Lookup returns the record of the file at rel (relative to the inbox root),
// if the runtime kept it.
func Lookup(ctx context.Context, rel string) (Record, bool) {
	h, ok := from(ctx)
	if !ok {
		return Record{}, false
	}
	h.tree.mu.Lock()
	defer h.tree.mu.Unlock()
	r, ok := h.tree.index[cleanRel(rel)]
	return r, ok
}

// Shared returns the value the run tree keeps under key, made by mk on the
// first call: state another tool holds per run tree beside the inbox, living
// as long as the tree's inbox state does — shared by the tree's sub-agents
// and by its runs started again on resume. ok is false when ctx carries no
// inbox. Use an unexported key type, as for a context key.
func Shared(ctx context.Context, key any, mk func() any) (any, bool) {
	h, ok := from(ctx)
	if !ok {
		return nil, false
	}
	h.tree.mu.Lock()
	defer h.tree.mu.Unlock()
	if v, ok := h.tree.shared[key]; ok {
		return v, true
	}
	if h.tree.shared == nil {
		h.tree.shared = map[any]any{}
	}
	v := mk()
	h.tree.shared[key] = v
	return v, true
}

// Forget drops the records of the files at rel or under it and gives their
// allowance back. Called after the File tool removed them.
func Forget(ctx context.Context, rel string) {
	h, ok := from(ctx)
	if !ok {
		return
	}
	rel = cleanRel(rel)
	h.tree.mu.Lock()
	defer h.tree.mu.Unlock()
	for p, r := range h.tree.index {
		if rel == "" || p == rel || strings.HasPrefix(p, rel+"/") {
			h.tree.used = max(h.tree.used-r.Size, 0)
			delete(h.tree.index, p)
		}
	}
}

func cleanRel(rel string) string {
	rel = path.Clean("/" + filepath.ToSlash(rel))
	return strings.TrimPrefix(rel, "/")
}

// Ref is the reference for a path relative to the inbox root, in the grammar
// Crypto's from/to and the File tool take: the path is relative to the
// volume's root, as Read's is (a leading "/" would name a host path).
func Ref(rel string) string {
	if rel = cleanRel(rel); rel == "" {
		rel = "."
	}
	return "volume:" + Name + ":" + rel
}

// Stub is the one line the model sees in place of a kept file.
func Stub(r Record) string {
	sum := r.SHA256
	if len(sum) > 12 {
		sum = sum[:12] + "…"
	}
	return fmt.Sprintf("[file: %s · %s · %s · sha256:%s · from %s]", r.Ref, r.MediaType, humanBytes(r.Size), sum, r.Origin)
}

// refused is the stub for a file that was not kept.
func refused(it Item, mt string, size int64, why string) Result {
	parts := []string{"file not kept: " + why}
	if mt != "" {
		parts = append(parts, mt)
	}
	if size >= 0 {
		parts = append(parts, humanBytes(size))
	}
	if it.Origin != "" {
		parts = append(parts, "from "+it.Origin)
	}
	return Result{Stub: "[" + strings.Join(parts, " · ") + "]"}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 3; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

// reserveName creates an empty file under dir with name or the first free
// name-<n> variant, O_EXCL, and returns its path relative to the root.
func reserveName(ir *os.Root, dir, name string) (string, error) {
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i <= 1000; i++ {
		cand := name
		if i > 1 {
			cand = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		rel := path.Join(dir, cand)
		f, err := ir.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("could not create %s in the inbox: %v", rel, err)
		}
		_ = f.Close()
		return rel, nil
	}
	return "", fmt.Errorf("no free name for %s in the inbox", name)
}

func createTemp(ir *os.Root, dir string) (*os.File, string, error) {
	for range 10 {
		var b [8]byte
		_, _ = rand.Read(b[:])
		rel := path.Join(dir, ".part-"+hex.EncodeToString(b[:]))
		f, err := ir.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("could not write into the inbox: %v", err)
		}
		return f, rel, nil
	}
	return nil, "", errors.New("could not write into the inbox")
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// capReader stops after n bytes; the writer's count tells the caller the
// file passed its cap without reading the rest of a stream that is too big.
type capReader struct {
	r io.Reader
	n int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	n, err := c.r.Read(p)
	c.n -= int64(n)
	return n, err
}
