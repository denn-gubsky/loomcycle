// Package unpack walks a ZIP or RAR archive under the bounds of File
// op=unpack (RFC GV P5, D12) and hands each entry it keeps to a Dest. It is
// stdlib plus the RAR decoder, so the core runs it for an archive in the
// inbox or a volume, and a workspace worker's conversion child runs it for
// an archive in a workspace (RFC GW D34), with one set of rules.
//
// Nothing an archive says about itself is trusted. Its sizes are read only to
// skip an entry early; every byte inflated is counted as it comes, against
// the entry's own cap, its expansion ratio and the call's budget, and an
// entry name is never used as a path: each segment is sanitised as an inbox
// name is, and a name that climbs, is absolute, or is hidden is skipped.
package unpack

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nwaples/rardecode/v2"

	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/files/filenames"
)

const (
	// MaxDepth is the most folders-and-name segments an entry's path may
	// have.
	MaxDepth = 8
	// maxDirectory refuses a ZIP whose directory claims more records than
	// this before the directory is loaded: archive/zip holds every record in
	// memory, and a listing stops at Limits.Listed anyway.
	maxDirectory = 100_000
	// maxRARDictionary is the largest RAR dictionary (the window the decoder
	// allocates) taken. RAR 5 to 7 default to 32 MiB; a larger one would be
	// an allocation the header alone decides.
	maxRARDictionary = 64 << 20
	// Timeout bounds one walk.
	Timeout = 5 * time.Minute
	// Head is what is read of an entry to sniff its type, as much as the
	// inbox reads to settle one.
	Head = 4096
	// MaxSkippedListed caps the skipped entries a result names; the counts
	// stay complete.
	MaxSkippedListed = 500
	// maxNameShown bounds an entry name as a result shows it.
	maxNameShown = 200
)

// Limits are the bounds of one walk; tests use smaller ones.
type Limits struct {
	// Listed is the most entries read, Written the most files kept.
	Listed, Written int
	// Total is the most bytes inflated, kept or not.
	Total int64
	// Ratio is how many times its compressed size an entry may expand,
	// past RatioFree bytes.
	Ratio, RatioFree int64
}

// DefaultLimits are File op=unpack's bounds.
var DefaultLimits = Limits{
	Listed: 10_000, Written: 2_000, Total: 1 << 30,
	// extract's zip reader allows an entry this many times its compressed
	// size past the first MiB; the same bound holds here.
	Ratio: extract.MaxZipEntryRatio, RatioFree: 1 << 20,
}

// Options are one walk's settings.
type Options struct {
	Limits Limits
	// PerFile is the most one entry may inflate to.
	PerFile int64
	// Budget is what may be inflated, at most Limits.Total; zero or less
	// is nothing. The caller sets it: Limits.Total, lowered by the inbox's
	// free space when the inbox is written.
	Budget int64
	// DryRun lists what would be written, writing nothing; All keeps every
	// regular file, not only what extract reads.
	DryRun, All bool
	// Timeout bounds the walk; zero is Timeout.
	Timeout time.Duration
}

// Written is one entry kept.
type Written struct {
	Ref       string `json:"ref"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type"`
}

// Skipped is one entry not kept, and why.
type Skipped struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Counts are a walk's tallies.
type Counts struct {
	Listed        int   `json:"listed"`
	Written       int   `json:"written"`
	Skipped       int   `json:"skipped"`
	Folders       int   `json:"folders"`
	BytesWritten  int64 `json:"bytes_written"`
	BytesInflated int64 `json:"bytes_inflated"`
}

// Result is what a walk did.
type Result struct {
	Written []Written `json:"written,omitempty"`
	Skipped []Skipped `json:"skipped,omitempty"`
	Counts  Counts    `json:"counts"`
	Notes   []string  `json:"notes,omitempty"`
	// Stopped names the bound that ended the walk.
	Stopped string `json:"stopped,omitempty"`
	DryRun  bool   `json:"dry_run,omitempty"`
}

// Dest is where a walk's entries go.
type Dest interface {
	// Planned is the reference an entry would get, before de-duplication.
	Planned(segs []string) string
	// Write keeps one entry; the reader's bound errors come back as they
	// are (BoundOf). skip is a reason the entry alone was not written (a
	// hook).
	Write(segs []string, r io.Reader, head []byte, display string) (w Written, skip string, err error)
}

// Refusal is an archive (not one entry) a walk will not read.
type Refusal struct{ Reason string }

func (e *Refusal) Error() string { return e.Reason }

// bound is a bound reached while inflating: the walk stops there.
type bound struct{ note string }

func (b *bound) Error() string { return b.note }

// BoundOf is the bound an entry's reader stopped at, or nil: a Dest whose
// writer fails on the reader's error reports the bound rather than its own
// account of it.
func BoundOf(r io.Reader) error {
	if s, ok := r.(*stream); ok && s.bound != nil {
		return s.bound
	}
	return nil
}

// entry is what an archive says of one entry, before any of it is read.
type entry struct {
	name      string
	dir       bool
	symlink   bool
	special   bool // a device, a pipe, a hard link or any other non-file
	encrypted bool
	size      int64 // declared; -1 unknown
	packed    int64 // compressed size; -1 unknown
}

// walker is one walk over an archive.
type walker struct {
	ctx     context.Context
	lim     Limits
	perFile int64
	// budget is what may still be inflated, written or not: a skipped entry
	// of a solid RAR is decompressed all the same.
	budget int64
	all    bool
	dest   Dest
	res    Result
}

// Run walks an archive of kind ("zip" or "rar", as Kind names it) into
// dest. A *Refusal is an archive that could not be read with nothing
// written; ctx's own error is returned as it is. Anything else that stops
// the walk once a file was written is the result's Stopped.
func Run(ctx context.Context, kind string, r io.ReaderAt, size int64, opt Options, dest Dest) (*Result, error) {
	budget := min(opt.Budget, opt.Limits.Total)
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = Timeout
	}
	uctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	w := &walker{ctx: uctx, lim: opt.Limits, perFile: opt.PerFile, budget: budget, all: opt.All, dest: dest}
	w.res.DryRun = opt.DryRun
	var walkErr error
	switch kind {
	case "zip":
		walkErr = w.walkZip(r, size)
	case "rar":
		walkErr = w.walkRAR(r)
	default:
		return nil, &Refusal{fmt.Sprintf("is not a ZIP or RAR archive (%q)", kind)}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if errors.Is(walkErr, context.DeadlineExceeded) || uctx.Err() != nil {
		w.stop(fmt.Sprintf("time limit: unpacking stopped after %s", timeout))
		walkErr = nil
	}
	if walkErr != nil {
		reason := fmt.Sprintf("could not be read as a %s archive: %v", strings.ToUpper(kind), walkErr)
		var refused *Refusal
		if errors.As(walkErr, &refused) {
			reason = refused.Reason
		}
		if w.res.Counts.Written == 0 {
			return nil, &Refusal{reason}
		}
		// Files already written stay, and are reported.
		w.stop(reason)
	}
	return &w.res, nil
}

// Answer is File op=unpack's answer for a result.
func (r *Result) Answer(archive, kind, dest string) map[string]any {
	out := map[string]any{"archive": archive, "format": kind, "to": dest, "counts": r.Counts}
	list := r.Written
	if list == nil {
		list = []Written{}
	}
	if r.DryRun {
		out["dry_run"] = true
		out["would_write"] = list
	} else {
		out["written"] = list
	}
	skipped := r.Skipped
	if skipped == nil {
		skipped = []Skipped{}
	}
	out["skipped"] = skipped
	if r.Counts.Skipped > len(r.Skipped) {
		out["skipped_truncated"] = true
	}
	if r.Stopped != "" {
		out["stopped"] = r.Stopped
	}
	if len(r.Notes) > 0 {
		out["notes"] = r.Notes
	}
	return out
}

// Stop ends a result at a bound, keeping what was written before it.
func (r *Result) Stop(note string) {
	if r.Stopped == "" {
		r.Stopped = note
		r.Notes = append(r.Notes, "stopped: "+note+"; what was written before it stays")
	}
}

// Skip records one entry not kept.
func (r *Result) Skip(display, reason string) {
	r.Counts.Skipped++
	if len(r.Skipped) < MaxSkippedListed {
		r.Skipped = append(r.Skipped, Skipped{Name: display, Reason: reason})
	}
}

func (w *walker) stop(note string) { w.res.Stop(note) }

func (w *walker) skip(display, reason string) { w.res.Skip(display, reason) }

// visit decides one entry and writes it when it is wanted. open gives the
// entry's bytes through the walk's bounds; it is called at most once. It
// returns false once the walk must stop.
func (w *walker) visit(e entry, open func() (io.Reader, error)) bool {
	if err := w.ctx.Err(); err != nil {
		return false
	}
	c := &w.res.Counts
	if c.Listed >= w.lim.Listed {
		w.stop(fmt.Sprintf("entry limit: %d entries listed; the archive holds more", w.lim.Listed))
		return false
	}
	c.Listed++
	display := DisplayName(e.name)
	switch {
	case e.dir:
		c.Folders++
		return true
	case e.symlink:
		w.skip(display, "a symbolic link")
		return true
	case e.special:
		w.skip(display, "not a regular file")
		return true
	case e.encrypted:
		w.skip(display, "encrypted; a password is not taken")
		return true
	}
	segs, reason := Path(e.name)
	if reason != "" {
		w.skip(display, reason)
		return true
	}
	if e.size > w.perFile {
		w.skip(display, fmt.Sprintf("%d bytes, over the per-file limit of %d", e.size, w.perFile))
		return true
	}
	if lim := w.ratioLimit(e.packed); e.size >= 0 && lim >= 0 && e.size > lim {
		w.skip(display, fmt.Sprintf("expands more than %d times its compressed size", w.lim.Ratio))
		return true
	}
	if c.Written >= w.lim.Written {
		w.stop(fmt.Sprintf("write limit: %d files", w.lim.Written))
		return false
	}
	r, err := open()
	if err != nil {
		w.skip(display, "cannot be read: "+err.Error())
		return true
	}
	head := make([]byte, Head)
	n, err := io.ReadFull(r, head)
	head = head[:n]
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return w.readFailed(display, err)
	}
	mt := filenames.Sniff("", head, segs[len(segs)-1])
	if !w.all && !wanted(mt, segs[len(segs)-1]) {
		w.skip(display, fmt.Sprintf("not a format extract reads (%s); pass all: true to write every file", mt))
		return true
	}
	if w.res.DryRun {
		c.Written++
		size := e.size
		if size < 0 {
			size = 0
		}
		w.res.Written = append(w.res.Written, Written{Ref: w.dest.Planned(segs), Name: display, Size: size, MediaType: mt})
		return true
	}
	wr, skip, err := w.dest.Write(segs, r, head, display)
	if err != nil {
		return w.readFailed(display, err)
	}
	if skip != "" {
		w.skip(display, skip)
		return true
	}
	c.Written++
	c.BytesWritten += wr.Size
	w.res.Written = append(w.res.Written, wr)
	return true
}

// readFailed handles an error reading or writing an entry: a bound stops the
// walk, a cancelled walk stops it silently, a damaged entry stops it too (a
// stream past a damaged entry cannot be trusted to line up).
func (w *walker) readFailed(display string, err error) bool {
	var b *bound
	switch {
	case errors.As(err, &b):
		w.stop(b.note)
	case w.ctx.Err() != nil:
	default:
		w.stop(fmt.Sprintf("%s could not be written: %v", display, err))
	}
	return false
}

// ---- the bounded stream ----

// stream is one entry's bytes, counted as they are inflated against the
// per-file cap, the entry's expansion ratio and the walk's budget.
type stream struct {
	w          *walker
	r          io.Reader
	display    string
	n          int64
	ratioLimit int64 // -1 when the packed size is not known
	bound      error
}

func (w *walker) stream(r io.Reader, e entry) *stream {
	return &stream{w: w, r: r, display: DisplayName(e.name), ratioLimit: w.ratioLimit(e.packed)}
}

// ratioLimit is the most an entry of packed compressed bytes may inflate to,
// -1 when its packed size is unknown (or the bound would not fit an int64).
func (w *walker) ratioLimit(packed int64) int64 {
	if packed < 0 || packed > (math.MaxInt64-w.lim.RatioFree)/max(w.lim.Ratio, 1) {
		return -1
	}
	return packed*w.lim.Ratio + w.lim.RatioFree
}

func (s *stream) Read(p []byte) (int, error) {
	if s.bound != nil {
		return 0, s.bound
	}
	if err := s.w.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := s.r.Read(p)
	s.n += int64(n)
	s.w.budget -= int64(n)
	s.w.res.Counts.BytesInflated += int64(n)
	switch {
	case s.n > s.w.perFile:
		s.bound = &bound{fmt.Sprintf("per-file limit: %s passed %d bytes while inflating", s.display, s.w.perFile)}
	case s.ratioLimit >= 0 && s.n > s.ratioLimit:
		s.bound = &bound{fmt.Sprintf("expansion limit: %s expands more than %d times its compressed size", s.display, s.w.lim.Ratio)}
	case s.w.budget < 0:
		s.bound = &bound{"total limit: the bytes an unpack may inflate (1 GiB, and never past the inbox's free space) are spent"}
	}
	if s.bound != nil {
		return n, s.bound
	}
	return n, err
}

// drain reads what is left of an entry, as a solid RAR must before the next:
// it costs the decompression a written entry would, so it counts the same.
func (s *stream) drain() error {
	_, err := io.Copy(io.Discard, s)
	return err
}

// ---- ZIP ----

func (w *walker) walkZip(ra io.ReaderAt, size int64) error {
	if n, ok := zipDirectoryRecords(ra, size); ok && n > maxDirectory {
		return &Refusal{fmt.Sprintf("lists %d entries, more than the %d an unpack reads", n, maxDirectory)}
	}
	zr, err := zip.NewReader(ra, size)
	if err != nil && !(errors.Is(err, zip.ErrInsecurePath) && zr != nil) {
		// An insecure name is no reason to stop: every name is checked here.
		return err
	}
	for _, f := range zr.File {
		mode := f.Mode()
		e := entry{
			name:      f.Name,
			dir:       mode.IsDir() || strings.HasSuffix(f.Name, "/"),
			symlink:   mode&fs.ModeSymlink != 0,
			encrypted: f.Flags&0x1 != 0,
			size:      clampInt64(f.UncompressedSize64),
			packed:    clampInt64(f.CompressedSize64),
		}
		e.special = !e.dir && !e.symlink && !mode.IsRegular()
		var rc io.ReadCloser
		open := func() (io.Reader, error) {
			var err error
			if rc, err = f.Open(); err != nil {
				return nil, err
			}
			return w.stream(rc, e), nil
		}
		more := w.visit(e, open)
		if rc != nil {
			rc.Close()
		}
		if !more {
			return nil
		}
	}
	return nil
}

func clampInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// zipDirectoryRecords reads the record count an archive's end of central
// directory declares (its zip64 form when there is one), so a directory too
// large to hold is refused before archive/zip loads it. ok is false when no
// record is found; zip.NewReader then says what is wrong.
func zipDirectoryRecords(ra io.ReaderAt, size int64) (uint64, bool) {
	const eocdLen, maxComment = 22, 65535
	tail := int64(eocdLen + maxComment)
	if size < tail {
		tail = size
	}
	buf := make([]byte, tail)
	if _, err := ra.ReadAt(buf, size-tail); err != nil && !errors.Is(err, io.EOF) {
		return 0, false
	}
	i := bytes.LastIndex(buf, []byte("PK\x05\x06"))
	if i < 0 || len(buf)-i < eocdLen {
		return 0, false
	}
	n := uint64(binary.LittleEndian.Uint16(buf[i+10:]))
	if n != 0xFFFF || i < 20 || string(buf[i-20:i-16]) != "PK\x06\x07" {
		return n, true
	}
	// The zip64 locator names where the zip64 record is.
	off := binary.LittleEndian.Uint64(buf[i-20+8:])
	rec := make([]byte, 56)
	if off > math.MaxInt64 || int64(off) > size-56 {
		return n, true
	}
	if _, err := ra.ReadAt(rec, int64(off)); err != nil || string(rec[:4]) != "PK\x06\x06" {
		return n, true
	}
	return binary.LittleEndian.Uint64(rec[32:]), true
}

// ---- RAR ----

func (w *walker) walkRAR(r io.ReaderAt) error {
	head := make([]byte, 512)
	n, _ := r.ReadAt(head, 0)
	solid := rarSolid(head[:n])
	rr, err := rardecode.NewReader(io.NewSectionReader(r, 0, math.MaxInt64), rardecode.MaxDictionarySize(maxRARDictionary))
	if err != nil {
		return rarError(err)
	}
	for {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		h, err := rr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return rarError(err)
		}
		if h.Encrypted {
			return &Refusal{"is encrypted; a password is not taken"}
		}
		e := entry{
			name:    h.Name,
			dir:     h.IsDir,
			symlink: h.Mode()&fs.ModeSymlink != 0,
			special: h.LinkType != rardecode.LinkTypeNone,
			size:    h.UnPackedSize,
			packed:  h.PackedSize,
		}
		if h.UnKnownSize {
			e.size = -1
		}
		var s *stream
		open := func() (io.Reader, error) {
			s = w.stream(rr, e)
			return s, nil
		}
		if !w.visit(e, open) {
			return nil
		}
		// A solid archive's next entry is decoded from this one's: what was
		// not read is decompressed now, and charged.
		if solid && !e.dir {
			if s == nil {
				s = w.stream(rr, e)
			}
			if err := s.drain(); err != nil {
				var b *bound
				if errors.As(err, &b) {
					w.stop(b.note)
					return nil
				}
				return rarError(err)
			}
		}
	}
}

// rarError turns the decoder's refusals into reasons; anything else is the
// archive being damaged.
func rarError(err error) error {
	switch {
	case errors.Is(err, rardecode.ErrArchiveEncrypted):
		return &Refusal{"has encrypted headers; a password is not taken"}
	case errors.Is(err, rardecode.ErrArchivedFileEncrypted):
		return &Refusal{"is encrypted; a password is not taken"}
	case errors.Is(err, rardecode.ErrMultiVolume):
		return &Refusal{"is one volume of a multi-volume archive, which is not read"}
	case errors.Is(err, rardecode.ErrDictionaryTooLarge):
		return &Refusal{fmt.Sprintf("needs a dictionary larger than the %d MiB an unpack allows", maxRARDictionary>>20)}
	}
	return err
}

// ---- recognising an archive ----

const (
	rar4Signature = "Rar!\x1a\x07\x00"
	rar5Signature = "Rar!\x1a\x07\x01\x00"
)

// Kind is "zip" or "rar" by the file's first bytes, never its name; "" with
// what it looks like otherwise.
func Kind(head []byte) (kind, looksLike string) {
	switch {
	case bytes.HasPrefix(head, []byte("PK\x03\x04")), bytes.HasPrefix(head, []byte("PK\x05\x06")):
		return "zip", ""
	case bytes.HasPrefix(head, []byte(rar4Signature)), bytes.HasPrefix(head, []byte(rar5Signature)):
		return "rar", ""
	case bytes.HasPrefix(head, []byte("7z\xbc\xaf\x27\x1c")):
		return "", "a 7z archive"
	case bytes.HasPrefix(head, []byte("\x1f\x8b")):
		return "", "a gzip file"
	case len(head) >= 262 && string(head[257:262]) == "ustar":
		return "", "a tar archive"
	}
	return "", filenames.Sniff("", head, "")
}

// rarMainFlags reads the archive flags of a RAR's first headers: whether it
// is one volume of several, solid, or has its headers encrypted. The decoder
// would find a volume out only at its end, after the files before it were
// written; read here, it is refused before anything is.
func rarMainFlags(head []byte) (volume, solid, encryptedHeaders bool) {
	if bytes.HasPrefix(head, []byte(rar4Signature)) {
		// The archive header follows the 7-byte marker: CRC(2) TYPE(1)=0x73
		// FLAGS(2).
		b := head[len(rar4Signature):]
		if len(b) < 5 || b[2] != 0x73 {
			return false, false, false
		}
		flags := binary.LittleEndian.Uint16(b[3:])
		return flags&0x0001 != 0, flags&0x0008 != 0, flags&0x0080 != 0
	}
	if !bytes.HasPrefix(head, []byte(rar5Signature)) {
		return false, false, false
	}
	b := head[len(rar5Signature):]
	if len(b) < 4 {
		return false, false, false
	}
	b = b[4:] // CRC32
	if _, ok := rarVint(&b); !ok {
		return false, false, false
	}
	typ, ok := rarVint(&b)
	if !ok {
		return false, false, false
	}
	if typ == 4 { // the archive encryption header comes first
		return false, false, true
	}
	if typ != 1 {
		return false, false, false
	}
	hflags, ok := rarVint(&b)
	if !ok {
		return false, false, false
	}
	if hflags&0x1 != 0 { // extra area size
		if _, ok := rarVint(&b); !ok {
			return false, false, false
		}
	}
	if hflags&0x2 != 0 { // data size
		if _, ok := rarVint(&b); !ok {
			return false, false, false
		}
	}
	aflags, ok := rarVint(&b)
	if !ok {
		return false, false, false
	}
	return aflags&0x1 != 0, aflags&0x4 != 0, false
}

func rarSolid(head []byte) bool {
	_, solid, _ := rarMainFlags(head)
	return solid
}

// RARRefusal is why a RAR is not read at all, or "".
func RARRefusal(head []byte) string {
	volume, _, encrypted := rarMainFlags(head)
	switch {
	case encrypted:
		return "has encrypted headers; a password is not taken"
	case volume:
		return "is one volume of a multi-volume archive, which is not read"
	}
	return ""
}

// rarVint reads one RAR 5 variable-length integer off b.
func rarVint(b *[]byte) (uint64, bool) {
	var v uint64
	for i := 0; i < len(*b) && i < 10; i++ {
		c := (*b)[i]
		v |= uint64(c&0x7f) << (7 * i)
		if c&0x80 == 0 {
			*b = (*b)[i+1:]
			return v, true
		}
	}
	return 0, false
}

// ---- names ----

// Path turns an entry name into sanitised path segments, the last the file's
// name, or says why it is skipped. A backslash separates as a slash does:
// archives made on Windows carry them.
func Path(name string) (segs []string, reason string) {
	name = strings.ReplaceAll(name, "\\", "/")
	if strings.HasPrefix(name, "/") || (len(name) >= 2 && name[1] == ':' && isASCIILetter(name[0])) {
		return nil, "an absolute path"
	}
	for _, s := range strings.Split(name, "/") {
		switch {
		case s == "" || s == ".":
			continue
		case s == "..":
			return nil, "a path that climbs out of its folder (..)"
		case strings.HasPrefix(s, "."):
			return nil, "a hidden file"
		}
		clean := filenames.SanitizeName(s)
		if clean == "" {
			return nil, "a name with nothing left once sanitised"
		}
		segs = append(segs, clean)
	}
	if len(segs) == 0 {
		return nil, "a name with nothing left once sanitised"
	}
	if len(segs) > MaxDepth {
		return nil, fmt.Sprintf("nested more than %d levels deep", MaxDepth)
	}
	return segs, ""
}

func isASCIILetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// DisplayName is an entry name as a result and a record's origin show it:
// the archive's own text, so control and format characters, and the
// brackets and backquotes a stub line or Markdown would read, are replaced,
// and it is cut to a bounded length.
func DisplayName(name string) string {
	var b strings.Builder
	n := 0
	for _, r := range name {
		if n >= maxNameShown {
			b.WriteString("…")
			break
		}
		switch {
		case r == utf8.RuneError, unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			b.WriteByte('?')
		case strings.ContainsRune("[]<>`", r):
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
		n++
	}
	return b.String()
}

// archiveTypes are the media types of archives, written as files and never
// opened (an archive in an archive is listed; unpack it in its turn).
var archiveTypes = map[string]bool{
	"application/zip": true, "application/x-zip-compressed": true,
	"application/x-rar-compressed": true, "application/vnd.rar": true,
	"application/x-7z-compressed": true, "application/x-gzip": true, "application/gzip": true,
	"application/x-tar": true, "application/x-bzip2": true, "application/x-xz": true, "application/zstd": true,
}

var archiveExts = map[string]bool{
	".zip": true, ".rar": true, ".7z": true, ".gz": true, ".tgz": true, ".tar": true,
	".bz2": true, ".xz": true, ".zst": true, ".cbz": true, ".cbr": true, ".cb7": true,
}

// IsRecording is a media type the speech-to-text pass reads. It mirrors
// sttpass.IsRecording, which sits above this package; a test of the File
// tool holds the two equal.
func IsRecording(mt string) bool {
	return strings.HasPrefix(mt, "audio/") || strings.HasPrefix(mt, "video/") || mt == "application/ogg"
}

// wanted is the default filter: what extract reads (documents, books, DjVu,
// images, captions and text), by the sniffed type and then the name, and
// archives. A recording is not among them: extract hands one to transcribe,
// and an audiobook's files are what all: true is for.
func wanted(mt, name string) bool {
	if archiveTypes[mt] || archiveExts[strings.ToLower(path.Ext(name))] {
		return true
	}
	if IsRecording(mt) {
		return false
	}
	if extract.IsDjVu(mt, "") {
		return true
	}
	_, ok := extract.Default().For(mt, name)
	return ok
}
