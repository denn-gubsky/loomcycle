package unpack

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/files/filenames"
)

// An unpack run away from its destination (RFC GW D34: in a workspace
// worker's conversion child) sends what it keeps as a tar: first "format"
// (the archive's kind), then one "entry/<path>" per file kept, its path the
// sanitised segments joined by '/' and the archive's own name for it in a
// PAX record, then "answer.json", the walk's Result. The side that writes
// the files reads it with a Replayer, which trusts none of it: every path is
// sanitised again, every size bounded, and the answer cut to its caps.
const (
	tarFormat   = "format"
	tarEntry    = "entry/"
	tarAnswer   = "answer.json"
	paxOriginal = "LOOMCYCLE.original"
	// maxAnswer bounds the answer: a dry run lists at most Written planned
	// paths, each of at most MaxDepth sanitised segments.
	maxAnswer = 16 << 20
	// maxNotes and maxText bound what the answer says in words.
	maxNotes = 16
	maxText  = 1024
)

// ErrStopped is a Replayer's write that failed: the unpack stopped there,
// and Result reports what was written before it.
var ErrStopped = errors.New("unpack: the destination stopped the unpack")

// WriteTar unpacks the archive r into a tar written to w. Each entry is
// spooled into scratch (empty: the system's temporary directory) before it
// is sent, as a tar header names its size first. Not an archive, or one
// refused whole, is a *Refusal with nothing written to w.
func WriteTar(ctx context.Context, r io.ReaderAt, size int64, opt Options, scratch string, w io.Writer) error {
	head := make([]byte, 512)
	n, _ := r.ReadAt(head, 0)
	head = head[:n]
	kind, why := Kind(head)
	if kind == "" {
		return &Refusal{fmt.Sprintf("is not a ZIP or RAR archive (%s)", why)}
	}
	if kind == "rar" {
		if refusal := RARRefusal(head); refusal != "" {
			return &Refusal{refusal}
		}
	}
	tw := tar.NewWriter(w)
	if err := writeTarBytes(tw, tarFormat, []byte(kind), nil); err != nil {
		return err
	}
	res, err := Run(ctx, kind, r, size, opt, &tarDest{tw: tw, scratch: scratch})
	if err != nil {
		return err
	}
	if !res.DryRun {
		// The entries name what was written; the receiver lists them itself.
		res.Written = nil
	}
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	if err := writeTarBytes(tw, tarAnswer, b, nil); err != nil {
		return err
	}
	return tw.Close()
}

func writeTarBytes(tw *tar.Writer, name string, data []byte, pax map[string]string) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Format: tar.FormatPAX, PAXRecords: pax}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// tarDest writes each entry kept as a tar entry.
type tarDest struct {
	tw      *tar.Writer
	scratch string
}

func (d *tarDest) Planned(segs []string) string { return path.Join(segs...) }

func (d *tarDest) Write(segs []string, r io.Reader, head []byte, display string) (Written, string, error) {
	f, err := os.CreateTemp(d.scratch, "unpack-entry-*")
	if err != nil {
		return Written{}, "", err
	}
	defer func() {
		f.Close()
		os.Remove(f.Name())
	}()
	n, err := io.Copy(f, io.MultiReader(bytes.NewReader(head), r))
	if err != nil {
		// A bound reached while inflating comes back as it is.
		return Written{}, "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Written{}, "", err
	}
	rel := path.Join(segs...)
	if err := d.tw.WriteHeader(&tar.Header{Name: tarEntry + rel, Mode: 0o644, Size: n, Format: tar.FormatPAX,
		PAXRecords: map[string]string{paxOriginal: display}}); err != nil {
		return Written{}, "", err
	}
	if _, err := io.CopyN(d.tw, f, n); err != nil {
		return Written{}, "", err
	}
	return Written{Ref: rel, Name: display, Size: n, MediaType: filenames.Sniff("", head, segs[len(segs)-1])}, "", nil
}

// Entry is one file of an unpack tar as a Replayer hands it on.
type Entry struct {
	// Segs is its sanitised path, Display the archive's name for it.
	Segs    []string
	Display string
	Size    int64
}

// Replayer reads an unpack tar an entry at a time, writes each file through
// Write, and makes the walk's Result from what Write kept and the answer.
type Replayer struct {
	// PerFile bounds an entry, MaxWritten the files a dry run may list:
	// what the receiver asked of the walk.
	PerFile    int64
	MaxWritten int
	// Planned is the reference a dry run's entry would get.
	Planned func(segs []string) string
	// Write keeps one entry: head is its first bytes (at most Head), body
	// the rest. skip is a reason the entry alone was not written.
	Write func(e Entry, head []byte, body io.Reader) (w Written, skip string, err error)

	kind    string
	mine    Result
	answer  *Result
	stopped bool
}

// Entry takes one tar entry. Anything the walk would never have sent is an
// error that ends the replay; a failed write is ErrStopped.
func (p *Replayer) Entry(h *tar.Header, r io.Reader) error {
	if p.answer != nil || p.stopped {
		return fmt.Errorf("unpack: an entry %q after the answer", clip(h.Name))
	}
	switch {
	case h.Name == tarFormat:
		b, err := readAll(h, r, 8)
		if err != nil {
			return err
		}
		if k := string(b); k != "zip" && k != "rar" {
			return fmt.Errorf("unpack: the format %q is not one an unpack reads", clip(k))
		}
		p.kind = string(b)
		return nil
	case h.Name == tarAnswer:
		b, err := readAll(h, r, maxAnswer)
		if err != nil {
			return err
		}
		var a Result
		if err := json.Unmarshal(b, &a); err != nil {
			return errors.New("unpack: the answer is not JSON")
		}
		p.answer = &a
		return nil
	case strings.HasPrefix(h.Name, tarEntry):
		return p.entry(h, r)
	}
	return fmt.Errorf("unpack: an unexpected entry %q", clip(h.Name))
}

func (p *Replayer) entry(h *tar.Header, r io.Reader) error {
	if p.kind == "" {
		return errors.New("unpack: an entry before the format")
	}
	rel := strings.TrimPrefix(h.Name, tarEntry)
	segs, reason := Path(rel)
	if reason != "" || path.Join(segs...) != rel {
		return fmt.Errorf("unpack: an entry path %q the walk would not make", clip(rel))
	}
	if h.Size < 0 || h.Size > p.PerFile {
		return fmt.Errorf("unpack: an entry of %d bytes, over the per-file limit of %d", h.Size, p.PerFile)
	}
	display := DisplayName(h.PAXRecords[paxOriginal])
	if display == "" {
		display = rel
	}
	head := make([]byte, min(int64(Head), h.Size))
	if _, err := io.ReadFull(r, head); err != nil {
		return fmt.Errorf("unpack: reading %s: %w", display, err)
	}
	w, skip, err := p.Write(Entry{Segs: segs, Display: display, Size: h.Size}, head, io.LimitReader(r, h.Size-int64(len(head))))
	c := &p.mine.Counts
	c.Listed++
	switch {
	case err != nil:
		p.mine.Stop(fmt.Sprintf("%s could not be written: %v", display, err))
		p.stopped = true
		return ErrStopped
	case skip != "":
		p.mine.Skip(display, skip)
	default:
		c.Written++
		c.BytesWritten += w.Size
		p.mine.Written = append(p.mine.Written, w)
	}
	return nil
}

// Stopped reports a write that failed: Result is then what was written
// before it, however the stream itself ended.
func (p *Replayer) Stopped() bool { return p.stopped }

// Result is the walk's result as the receiver kept it: the walk's own
// counts, notes and skips, with the files the receiver wrote (and the ones
// it skipped) in place of what the walk sent. After a failed write it is
// what was written before it. The format is "" when none came.
func (p *Replayer) Result() (string, *Result, error) {
	if p.stopped {
		res := p.mine
		res.Counts.BytesInflated = res.Counts.BytesWritten
		return p.kind, &res, nil
	}
	if p.answer == nil {
		return "", nil, errors.New("unpack: the stream ended before its answer")
	}
	res := clean(*p.answer, p.MaxWritten)
	if res.DryRun {
		planned := res.Written[:0]
		for _, w := range res.Written {
			segs, reason := Path(w.Ref)
			if reason != "" || path.Join(segs...) != w.Ref {
				continue
			}
			w.Ref = p.Planned(segs)
			planned = append(planned, w)
		}
		res.Written = planned
		res.Counts.Written = len(planned)
		return p.kind, &res, nil
	}
	res.Written = p.mine.Written
	res.Counts.Written, res.Counts.BytesWritten = p.mine.Counts.Written, p.mine.Counts.BytesWritten
	for _, s := range p.mine.Skipped {
		res.Skip(s.Name, s.Reason)
	}
	res.Counts.Skipped += p.mine.Counts.Skipped - len(p.mine.Skipped)
	return p.kind, &res, nil
}

// clean cuts an answer from the walk to the caps a walk here would keep.
func clean(r Result, maxWritten int) Result {
	out := Result{Stopped: clip(r.Stopped), DryRun: r.DryRun, Counts: r.Counts}
	c := &out.Counts
	c.Listed, c.Written, c.Skipped, c.Folders = max(c.Listed, 0), max(c.Written, 0), max(c.Skipped, 0), max(c.Folders, 0)
	c.BytesWritten, c.BytesInflated = max(c.BytesWritten, 0), max(c.BytesInflated, 0)
	for i, s := range r.Skipped {
		if i == MaxSkippedListed {
			break
		}
		out.Skipped = append(out.Skipped, Skipped{Name: DisplayName(s.Name), Reason: clip(s.Reason)})
	}
	c.Skipped = max(c.Skipped, len(out.Skipped))
	for i, n := range r.Notes {
		if i == maxNotes {
			break
		}
		out.Notes = append(out.Notes, clip(n))
	}
	if r.DryRun {
		for i, w := range r.Written {
			if i == maxWritten {
				break
			}
			out.Written = append(out.Written, Written{Ref: w.Ref, Name: DisplayName(w.Name), Size: max(w.Size, 0), MediaType: clip(w.MediaType)})
		}
	}
	return out
}

// clip cuts s to maxText bytes on a rune boundary.
func clip(s string) string {
	if len(s) <= maxText {
		return s
	}
	n := maxText
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func readAll(h *tar.Header, r io.Reader, limit int64) ([]byte, error) {
	if h.Size < 0 || h.Size > limit {
		return nil, fmt.Errorf("unpack: %q is %d bytes, over its %d-byte bound", clip(h.Name), h.Size, limit)
	}
	return io.ReadAll(io.LimitReader(r, limit))
}
