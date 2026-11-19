package builtin

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
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nwaples/rardecode/v2"

	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/fsops"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// File op=unpack (RFC GV P5, D12): the files of a ZIP or RAR archive are
// copied, by the runtime, into the inbox (or a read-write volume), each to be
// extracted on its own afterwards. Nothing is extracted here: an archive's
// directory is read and its entries inflated, and that is the work the
// extraction slot bounds.
//
// Nothing an archive says about itself is trusted. Its sizes are read only to
// skip an entry early; every byte inflated is counted as it comes, against
// the entry's own cap, its expansion ratio and the call's budget, and an
// entry name is never used as a path: each segment is sanitised as an inbox
// name is, and a name that climbs, is absolute, or is hidden is skipped.

const (
	// unpackDir is the inbox folder unpacked archives go to.
	unpackDir = "unpacked"
	// unpackMaxDepth is the most folders-and-name segments an entry's path
	// may have.
	unpackMaxDepth = 8
	// unpackMaxDirectory refuses a ZIP whose directory claims more records
	// than this before the directory is loaded: archive/zip holds every
	// record in memory, and a listing stops at unpackMaxListed anyway.
	unpackMaxDirectory = 100_000
	// unpackMaxRARDictionary is the largest RAR dictionary (the window the
	// decoder allocates) taken. RAR 5 to 7 default to 32 MiB; a larger one
	// would be an allocation the header alone decides.
	unpackMaxRARDictionary = 64 << 20
	// unpackTimeout bounds one call: an extraction slot is held throughout.
	unpackTimeout = 5 * time.Minute
	// unpackHead is what is read of an entry to sniff its type, as much as
	// the inbox reads to settle one.
	unpackHead = 4096
	// unpackMaxSkippedListed caps the skipped entries the answer names; the
	// counts stay complete.
	unpackMaxSkippedListed = 500
	// unpackMaxOrigin bounds a capture record's origin.
	unpackMaxOrigin = 512
	// unpackMaxNameShown bounds an entry name as the answer shows it.
	unpackMaxNameShown = 200
)

// unpackLimits are the bounds of one call; tests use smaller ones.
type unpackLimits struct {
	listed, written int
	total           int64
	ratio           int64
	ratioFree       int64
}

var defaultUnpackLimits = unpackLimits{
	listed: 10_000, written: 2_000, total: 1 << 30,
	// extract's zip reader allows an entry this many times its compressed
	// size past the first MiB; the same bound holds here.
	ratio: extract.MaxZipEntryRatio, ratioFree: 1 << 20,
}

// unpackEntry is what an archive says of one entry, before any of it is read.
type unpackEntry struct {
	name      string
	dir       bool
	symlink   bool
	special   bool // a device, a pipe, a hard link or any other non-file
	encrypted bool
	size      int64 // declared; -1 unknown
	packed    int64 // compressed size; -1 unknown
}

type unpackWritten struct {
	Ref       string `json:"ref"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type"`
}

type unpackSkipped struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type unpackCounts struct {
	Listed        int   `json:"listed"`
	Written       int   `json:"written"`
	Skipped       int   `json:"skipped"`
	Folders       int   `json:"folders"`
	BytesWritten  int64 `json:"bytes_written"`
	BytesInflated int64 `json:"bytes_inflated"`
}

// unpackBound is a bound reached while inflating: the call stops there.
type unpackBound struct{ note string }

func (b *unpackBound) Error() string { return b.note }

// unpacker is one call's walk over an archive.
type unpacker struct {
	ctx     context.Context
	lim     unpackLimits
	perFile int64
	// budget is what may still be inflated, written or not: a skipped entry
	// of a solid RAR is decompressed all the same.
	budget  int64
	dryRun  bool
	all     bool
	archive string // the archive's reference, for origins
	dest    unpackDest

	written []unpackWritten
	skipped []unpackSkipped
	counts  unpackCounts
	notes   []string
	// stopped names the bound that ended the walk.
	stopped string
}

// unpackDest is where entries are written.
type unpackDest interface {
	// planned is the reference an entry would get, before de-duplication.
	planned(segs []string) string
	// write keeps one entry; the reader's bound errors come back as they
	// are. skip is a reason the entry alone was not written (a hook).
	write(segs []string, r io.Reader, head []byte, display string) (w unpackWritten, skip string, err error)
}

// ---- the op ----

func (f *File) unpack(ctx context.Context, in fileInput) (tools.Result, error) {
	r, err := fileRef(in.Path)
	if err != nil {
		return errValidation("unpack: "+err.Error(), "Pass path, e.g. volume:inbox:documents/books.zip."), nil
	}
	lim := defaultUnpackLimits
	if f.unpackLimits != nil {
		lim = *f.unpackLimits
	}
	vfs, rel, res, ok := openRef(ctx, r, false)
	if !ok {
		return res, nil
	}
	defer vfs.Close()
	fh, err := vfs.Open(rel)
	if err != nil {
		return errNotFound(fmt.Sprintf("unpack: no file at %s", r.raw), "List the folder with op=ls."), nil
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errValidation(fmt.Sprintf("unpack: %s is not a file", r.raw), ""), nil
	}
	head := make([]byte, 512)
	n, _ := fh.ReadAt(head, 0)
	head = head[:n]
	kind, why := archiveKind(head)
	if kind == "" {
		return errValidation(fmt.Sprintf("unpack: %s is not a ZIP or RAR archive (%s)", r.raw, why),
			"unpack reads ZIP and RAR archives only; 7z, tar, gzip and the rest are not read."), nil
	}
	if kind == "rar" {
		if refusal := rarRefusal(head); refusal != "" {
			return errValidation(fmt.Sprintf("unpack: %s %s", r.raw, refusal), "Unpack it with a tool of your own and upload the files."), nil
		}
	}

	name := filepath.Base(rel)
	slug := inbox.SanitizeName(strings.TrimSuffix(name, path.Ext(name)))
	if slug == "" {
		slug = "archive"
	}
	u := &unpacker{lim: lim, perFile: inbox.MaxFileBytes(ctx), budget: lim.total, dryRun: in.DryRun, all: in.All, archive: r.raw}
	var dest string
	if strings.TrimSpace(in.To) != "" {
		vd, res, ok := f.unpackVolumeDest(ctx, in.To)
		if !ok {
			return res, nil
		}
		defer vd.vfs.Close()
		u.dest, dest = vd, vd.prefix+vd.base
	} else {
		if !inbox.Available(ctx) {
			return errBusiness("unpack: this run cannot keep files, so there is nowhere to unpack to",
				"An operator must configure a dynamic_root volume for the run's inbox, or pass to: a read-write volume of yours."), nil
		}
		dir := path.Join(unpackDir, slug)
		u.dest, dest = &inboxDest{u: u, dir: dir}, inbox.Ref(dir)+"/"
		// min is not used: a test file of this package declares its own.
		if rest := inbox.Remaining(ctx); rest < u.budget {
			u.budget = rest
		}
	}

	release, err := f.extractSlot(ctx)
	if err != nil {
		return slotBusy(ctx, "unpack", f.slotWait)
	}
	defer release()
	uctx, cancel := context.WithTimeout(ctx, unpackTimeout)
	defer cancel()
	u.ctx = uctx

	var walkErr error
	if kind == "zip" {
		walkErr = u.walkZip(fh, info.Size())
	} else {
		walkErr = u.walkRAR(fh)
	}
	if err := ctx.Err(); err != nil {
		return tools.Result{}, err
	}
	if errors.Is(walkErr, context.DeadlineExceeded) || uctx.Err() != nil {
		u.stop(fmt.Sprintf("time limit: unpacking stopped after %s", unpackTimeout))
		walkErr = nil
	}
	if walkErr != nil {
		reason := fmt.Sprintf("could not be read as a %s archive: %v", strings.ToUpper(kind), walkErr)
		var refused *unpackRefusal
		if errors.As(walkErr, &refused) {
			reason = refused.reason
		}
		if u.counts.Written == 0 {
			return errValidation(fmt.Sprintf("unpack: %s %s", r.raw, reason), "Unpack it with a tool of your own and upload the files."), nil
		}
		// Files already written stay, and are reported.
		u.stop(reason)
	}
	return okJSON(u.answer(r.raw, kind, dest))
}

// unpackRefusal is an archive (not one entry) the call will not read on.
type unpackRefusal struct{ reason string }

func (e *unpackRefusal) Error() string { return e.reason }

func (u *unpacker) answer(archive, kind, dest string) map[string]any {
	out := map[string]any{"archive": archive, "format": kind, "to": dest, "counts": u.counts}
	list := u.written
	if list == nil {
		list = []unpackWritten{}
	}
	if u.dryRun {
		out["dry_run"] = true
		out["would_write"] = list
	} else {
		out["written"] = list
	}
	skipped := u.skipped
	if skipped == nil {
		skipped = []unpackSkipped{}
	}
	out["skipped"] = skipped
	if u.counts.Skipped > len(u.skipped) {
		out["skipped_truncated"] = true
	}
	if u.stopped != "" {
		out["stopped"] = u.stopped
	}
	if len(u.notes) > 0 {
		out["notes"] = u.notes
	}
	return out
}

func (u *unpacker) stop(note string) {
	if u.stopped == "" {
		u.stopped = note
		u.notes = append(u.notes, "stopped: "+note+"; what was written before it stays")
	}
}

func (u *unpacker) skip(display, reason string) {
	u.counts.Skipped++
	if len(u.skipped) < unpackMaxSkippedListed {
		u.skipped = append(u.skipped, unpackSkipped{Name: display, Reason: reason})
	}
}

// visit decides one entry and writes it when it is wanted. open gives the
// entry's bytes through the call's bounds; it is called at most once. It
// returns false once the walk must stop.
func (u *unpacker) visit(e unpackEntry, open func() (io.Reader, error)) bool {
	if err := u.ctx.Err(); err != nil {
		return false
	}
	if u.counts.Listed >= u.lim.listed {
		u.stop(fmt.Sprintf("entry limit: %d entries listed; the archive holds more", u.lim.listed))
		return false
	}
	u.counts.Listed++
	display := entryDisplayName(e.name)
	switch {
	case e.dir:
		u.counts.Folders++
		return true
	case e.symlink:
		u.skip(display, "a symbolic link")
		return true
	case e.special:
		u.skip(display, "not a regular file")
		return true
	case e.encrypted:
		u.skip(display, "encrypted; a password is not taken")
		return true
	}
	segs, reason := unpackPath(e.name)
	if reason != "" {
		u.skip(display, reason)
		return true
	}
	if e.size > u.perFile {
		u.skip(display, fmt.Sprintf("%d bytes, over the per-file limit of %d", e.size, u.perFile))
		return true
	}
	if lim := u.ratioLimit(e.packed); e.size >= 0 && lim >= 0 && e.size > lim {
		u.skip(display, fmt.Sprintf("expands more than %d times its compressed size", u.lim.ratio))
		return true
	}
	if u.counts.Written >= u.lim.written {
		u.stop(fmt.Sprintf("write limit: %d files", u.lim.written))
		return false
	}
	r, err := open()
	if err != nil {
		u.skip(display, "cannot be read: "+err.Error())
		return true
	}
	head := make([]byte, unpackHead)
	n, err := io.ReadFull(r, head)
	head = head[:n]
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return u.readFailed(display, err)
	}
	mt := inbox.Sniff("", head, segs[len(segs)-1])
	if !u.all && !unpackWanted(mt, segs[len(segs)-1]) {
		u.skip(display, fmt.Sprintf("not a format extract reads (%s); pass all: true to write every file", mt))
		return true
	}
	if u.dryRun {
		u.counts.Written++
		size := e.size
		if size < 0 {
			size = 0
		}
		u.written = append(u.written, unpackWritten{Ref: u.dest.planned(segs), Name: display, Size: size, MediaType: mt})
		return true
	}
	w, skip, err := u.dest.write(segs, r, head, display)
	if err != nil {
		return u.readFailed(display, err)
	}
	if skip != "" {
		u.skip(display, skip)
		return true
	}
	u.counts.Written++
	u.counts.BytesWritten += w.Size
	u.written = append(u.written, w)
	return true
}

// readFailed handles an error reading or writing an entry: a bound stops the
// walk, a cancelled call stops it silently, a damaged entry stops it too (a
// stream past a damaged entry cannot be trusted to line up).
func (u *unpacker) readFailed(display string, err error) bool {
	var b *unpackBound
	switch {
	case errors.As(err, &b):
		u.stop(b.note)
	case u.ctx.Err() != nil:
	default:
		u.stop(fmt.Sprintf("%s could not be written: %v", display, err))
	}
	return false
}

// ---- the bounded stream ----

// unpackStream is one entry's bytes, counted as they are inflated against
// the per-file cap, the entry's expansion ratio and the call's budget.
type unpackStream struct {
	u          *unpacker
	r          io.Reader
	display    string
	n          int64
	ratioLimit int64 // -1 when the packed size is not known
	bound      error
}

func (u *unpacker) stream(r io.Reader, e unpackEntry) *unpackStream {
	return &unpackStream{u: u, r: r, display: entryDisplayName(e.name), ratioLimit: u.ratioLimit(e.packed)}
}

// ratioLimit is the most an entry of packed compressed bytes may inflate to,
// -1 when its packed size is unknown (or the bound would not fit an int64).
func (u *unpacker) ratioLimit(packed int64) int64 {
	if packed < 0 || packed > (math.MaxInt64-u.lim.ratioFree)/max(u.lim.ratio, 1) {
		return -1
	}
	return packed*u.lim.ratio + u.lim.ratioFree
}

func (s *unpackStream) Read(p []byte) (int, error) {
	if s.bound != nil {
		return 0, s.bound
	}
	if err := s.u.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := s.r.Read(p)
	s.n += int64(n)
	s.u.budget -= int64(n)
	s.u.counts.BytesInflated += int64(n)
	switch {
	case s.n > s.u.perFile:
		s.bound = &unpackBound{fmt.Sprintf("per-file limit: %s passed %d bytes while inflating", s.display, s.u.perFile)}
	case s.ratioLimit >= 0 && s.n > s.ratioLimit:
		s.bound = &unpackBound{fmt.Sprintf("expansion limit: %s expands more than %d times its compressed size", s.display, s.u.lim.ratio)}
	case s.u.budget < 0:
		s.bound = &unpackBound{"total limit: the bytes an unpack may inflate (1 GiB, and never past the inbox's free space) are spent"}
	}
	if s.bound != nil {
		return n, s.bound
	}
	return n, err
}

// drain reads what is left of an entry, as a solid RAR must before the next:
// it costs the decompression a written entry would, so it counts the same.
func (s *unpackStream) drain() error {
	_, err := io.Copy(io.Discard, s)
	return err
}

// ---- ZIP ----

func (u *unpacker) walkZip(ra io.ReaderAt, size int64) error {
	if n, ok := zipDirectoryRecords(ra, size); ok && n > unpackMaxDirectory {
		return &unpackRefusal{fmt.Sprintf("lists %d entries, more than the %d an unpack reads", n, unpackMaxDirectory)}
	}
	zr, err := zip.NewReader(ra, size)
	if err != nil && !(errors.Is(err, zip.ErrInsecurePath) && zr != nil) {
		// An insecure name is no reason to stop: every name is checked here.
		return err
	}
	for _, f := range zr.File {
		mode := f.Mode()
		e := unpackEntry{
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
			return u.stream(rc, e), nil
		}
		more := u.visit(e, open)
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

func (u *unpacker) walkRAR(r io.ReaderAt) error {
	head := make([]byte, 512)
	n, _ := r.ReadAt(head, 0)
	solid := rarSolid(head[:n])
	rr, err := rardecode.NewReader(io.NewSectionReader(r, 0, math.MaxInt64), rardecode.MaxDictionarySize(unpackMaxRARDictionary))
	if err != nil {
		return rarError(err)
	}
	for {
		if err := u.ctx.Err(); err != nil {
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
			return &unpackRefusal{"is encrypted; a password is not taken"}
		}
		e := unpackEntry{
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
		var s *unpackStream
		open := func() (io.Reader, error) {
			s = u.stream(rr, e)
			return s, nil
		}
		if !u.visit(e, open) {
			return nil
		}
		// A solid archive's next entry is decoded from this one's: what was
		// not read is decompressed now, and charged.
		if solid && !e.dir {
			if s == nil {
				s = u.stream(rr, e)
			}
			if err := s.drain(); err != nil {
				var b *unpackBound
				if errors.As(err, &b) {
					u.stop(b.note)
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
		return &unpackRefusal{"has encrypted headers; a password is not taken"}
	case errors.Is(err, rardecode.ErrArchivedFileEncrypted):
		return &unpackRefusal{"is encrypted; a password is not taken"}
	case errors.Is(err, rardecode.ErrMultiVolume):
		return &unpackRefusal{"is one volume of a multi-volume archive, which is not read"}
	case errors.Is(err, rardecode.ErrDictionaryTooLarge):
		return &unpackRefusal{fmt.Sprintf("needs a dictionary larger than the %d MiB an unpack allows", unpackMaxRARDictionary>>20)}
	}
	return err
}

// ---- recognising an archive ----

const (
	rar4Signature = "Rar!\x1a\x07\x00"
	rar5Signature = "Rar!\x1a\x07\x01\x00"
)

// archiveKind is "zip" or "rar" by the file's first bytes, never its name;
// "" with what it looks like otherwise.
func archiveKind(head []byte) (kind, looksLike string) {
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
	return "", inbox.Sniff("", head, "")
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

// rarRefusal is why a RAR is not read at all, or "".
func rarRefusal(head []byte) string {
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

// unpackPath turns an entry name into sanitised path segments, the last the
// file's name, or says why it is skipped. A backslash separates as a slash
// does: archives made on Windows carry them.
func unpackPath(name string) (segs []string, reason string) {
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
		clean := inbox.SanitizeName(s)
		if clean == "" {
			return nil, "a name with nothing left once sanitised"
		}
		segs = append(segs, clean)
	}
	if len(segs) == 0 {
		return nil, "a name with nothing left once sanitised"
	}
	if len(segs) > unpackMaxDepth {
		return nil, fmt.Sprintf("nested more than %d levels deep", unpackMaxDepth)
	}
	return segs, ""
}

func isASCIILetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// entryDisplayName is an entry name as the answer and a record's origin show
// it: the archive's own text, so control and format characters, and the
// brackets and backquotes a stub line or Markdown would read, are replaced,
// and it is cut to a bounded length.
func entryDisplayName(name string) string {
	var b strings.Builder
	n := 0
	for _, r := range name {
		if n >= unpackMaxNameShown {
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

// unpackOrigin is the capture record's origin of an entry: the archive and
// the entry, so extract, import and provenance name where it came from.
func unpackOrigin(archive, display string) string {
	o := "unpack:" + archive + "#" + display
	if len(o) <= unpackMaxOrigin {
		return o
	}
	cut := unpackMaxOrigin
	for cut > 0 && !utf8.RuneStart(o[cut]) {
		cut--
	}
	return o[:cut]
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

// unpackWanted is the default filter: what extract reads (documents, books,
// DjVu, images, captions and text), by the sniffed type and then the name,
// and archives. A recording is not among them: extract hands one to
// transcribe, and an audiobook's files are what all: true is for.
func unpackWanted(mt, name string) bool {
	if archiveTypes[mt] || archiveExts[strings.ToLower(path.Ext(name))] {
		return true
	}
	if isRecording(mt) {
		return false
	}
	if extract.IsDjVu(mt, "") {
		return true
	}
	_, ok := extract.Default().For(mt, name)
	return ok
}

// ---- destinations ----

// inboxDest writes through the inbox writer, a runtime write: a sub-agent
// whose inbox is read-only still unpacks into it, and each file gets a
// capture record naming the archive.
type inboxDest struct {
	u   *unpacker
	dir string
}

func (d *inboxDest) planned(segs []string) string {
	return inbox.Ref(path.Join(append([]string{d.dir}, segs...)...))
}

func (d *inboxDest) write(segs []string, r io.Reader, head []byte, display string) (unpackWritten, string, error) {
	dir := path.Join(append([]string{d.dir}, segs[:len(segs)-1]...)...)
	src := io.MultiReader(bytes.NewReader(head), r)
	res := inbox.CaptureReader(d.u.ctx, inbox.Item{Name: segs[len(segs)-1], Dir: dir, Origin: unpackOrigin(d.u.archive, display)}, src, -1)
	if !res.Kept {
		// The reader's own bound wins over the writer's account of it.
		if s, ok := r.(*unpackStream); ok && s.bound != nil {
			return unpackWritten{}, "", s.bound
		}
		return unpackWritten{}, "", errors.New(strings.Trim(res.Stub, "[]"))
	}
	return unpackWritten{Ref: res.Record.Ref, Name: display, Size: res.Record.Size, MediaType: res.Record.MediaType}, "", nil
}

// volumeDest writes into a folder of a read-write volume, as keep does: the
// run needs Write, and Write's hooks judge each file it writes.
type volumeDest struct {
	ctx    context.Context
	vfs    *fsops.VolumeFS
	volume string
	prefix string // volume:<name>:
	base   string // the folder, relative to the volume root, slash-separated, ending in /
}

func (f *File) unpackVolumeDest(ctx context.Context, to string) (*volumeDest, tools.Result, bool) {
	r, err := parseDataRef(strings.TrimSpace(to))
	if err != nil || r.kind != refVolume {
		return nil, errValidation("unpack: to must be a volume folder", "Pass to, e.g. volume:books:library/, or omit it to unpack into the inbox."), false
	}
	if r.volume == inbox.Name {
		return nil, errValidation("unpack: to is in the inbox", "Omit to to unpack into volume:inbox:unpacked/."), false
	}
	if res, ok := checkRefGrant(ctx, r, true, FileToolName, "Unpack into the inbox"); !ok {
		return nil, res, false
	}
	root, fence, err := effectiveRoot(ctx, r.volume, true)
	if err != nil {
		return nil, errPermission("unpack: "+err.Error(), "Name a read-write volume you are bound to."), false
	}
	resolved, err := fsops.ResolveInsideRoot(root, r.path)
	if err == nil {
		err = fsops.OutsideFence(fence, resolved)
	}
	if err != nil {
		return nil, errPermission("unpack: "+err.Error(), "Create the folder first, or name one that exists."), false
	}
	vfs, err := fsops.OpenVolumeFS(root, fence)
	if err != nil {
		return nil, errPermission("unpack: "+err.Error(), ""), false
	}
	rel, err := vfs.Rel(resolved)
	if err == nil {
		var d *os.Root
		if d, err = vfs.OpenDir(rel); err == nil {
			d.Close()
		}
	}
	if err != nil {
		vfs.Close()
		return nil, errNotFound("unpack: "+to+" is not a folder", "Create it first, or name one that exists."), false
	}
	base := filepath.ToSlash(rel)
	if base == "." {
		base = ""
	} else {
		base += "/"
	}
	return &volumeDest{ctx: ctx, vfs: vfs, volume: r.volume, prefix: refPrefix(r), base: base}, tools.Result{}, true
}

func (d *volumeDest) planned(segs []string) string {
	return d.prefix + d.base + path.Join(segs...)
}

func (d *volumeDest) ref(rel string) dataRef {
	return dataRef{raw: d.prefix + rel, kind: refVolume, volume: d.volume, path: rel}
}

func (d *volumeDest) write(segs []string, r io.Reader, head []byte, display string) (unpackWritten, string, error) {
	// Asked before any folder is made for it, and again below of the name
	// it gets: a hook that refuses the path refuses its folders too.
	if res, ok := checkRefGrant(d.ctx, d.ref(d.base+path.Join(segs...)), true, FileToolName, "Unpack into the inbox"); !ok {
		return unpackWritten{}, res.Text, nil
	}
	dir, err := d.vfs.OpenDir(strings.TrimSuffix(d.base, "/"))
	if err != nil {
		return unpackWritten{}, "", err
	}
	folder := d.base
	for _, s := range segs[:len(segs)-1] {
		if err := dir.Mkdir(s, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			dir.Close()
			return unpackWritten{}, "", err
		}
		next, err := d.vfs.Step(dir, s)
		dir.Close()
		if err != nil {
			return unpackWritten{}, "", fmt.Errorf("%s is not a folder: %w", folder+s, err)
		}
		dir, folder = next, folder+s+"/"
	}
	defer dir.Close()
	name, err := reserveVolumeName(dir, segs[len(segs)-1])
	if err != nil {
		return unpackWritten{}, "", err
	}
	rel := folder + name
	if name != segs[len(segs)-1] {
		if res, ok := checkRefGrant(d.ctx, d.ref(rel), true, FileToolName, "Unpack into the inbox"); !ok {
			_ = dir.Remove(name)
			return unpackWritten{}, res.Text, nil
		}
	}
	tmp, tmpName, err := fsops.CreateTemp(dir, ".loomcycle-unpack-")
	if err != nil {
		_ = dir.Remove(name)
		return unpackWritten{}, "", err
	}
	n, copyErr := io.Copy(tmp, io.MultiReader(bytes.NewReader(head), r))
	closeErr := tmp.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr == nil {
		copyErr = d.ctx.Err()
	}
	if copyErr == nil {
		copyErr = dir.Rename(tmpName, name)
	}
	if copyErr != nil {
		_ = dir.Remove(tmpName)
		_ = dir.Remove(name)
		return unpackWritten{}, "", copyErr
	}
	return unpackWritten{Ref: d.prefix + rel, Name: display, Size: n, MediaType: inbox.Sniff("", head, name)}, "", nil
}

// reserveVolumeName creates name, or the first free name-<n> variant, empty
// and O_EXCL in dir, so no existing file is ever replaced.
func reserveVolumeName(dir *os.Root, name string) (string, error) {
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i <= 1000; i++ {
		cand := name
		if i > 1 {
			cand = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		fh, err := dir.OpenFile(cand, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_ = fh.Close()
		return cand, nil
	}
	return "", fmt.Errorf("no free name for %s", name)
}
