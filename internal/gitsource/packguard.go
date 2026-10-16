package gitsource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// A limit on the bytes fetched does not limit what they become. A pack
// stores objects compressed, and a file of zeros compresses a thousand to
// one: 600 KiB on the wire is a 600 MiB object, and the library's pack
// reader holds each object whole in memory while it indexes the pack. A
// delta is worse, since a few bytes of "copy this again" instructions can
// name any result size at all.
//
// So no pack reaches the library before it has been read once here. The pack
// is spooled to a file, every object's header is checked against a size
// limit, and only a pack that passes is handed on. Reading it here costs no
// memory: the contents are inflated into a counter and thrown away.

// errObjectTooLarge is the guard's refusal; Fetch reports it as ErrTooLarge.
var errObjectTooLarge = errors.New("gitsource: the pack holds an object over the size limit")

// inflatedPerFetchedByte bounds what the packs of one fetch may become, in
// all, as a multiple of the bytes the fetch may read: every object's size as
// declared, and every delta's as built. A limit on each object alone does not
// do it: 256 MiB fetched is thousands of 64 MiB objects of zeros, or millions
// of few-byte deltas each building 64 MiB, and each is inflated here and then
// again by the library. Source code packs to a few times smaller than it is,
// and a history deepened for a merge base counts every version of a file it
// holds; 32 leaves room for both, and is still a bound.
const inflatedPerFetchedByte = 32

// inflatedBudget is the total a fetch limited to maxFetch bytes may inflate.
func inflatedBudget(maxFetch int64) int64 {
	if maxFetch > math.MaxInt64/inflatedPerFetchedByte {
		return math.MaxInt64
	}
	return maxFetch * inflatedPerFetchedByte
}

// guardedStorage is the library's file-system store with one change: a pack
// written to it is checked first.
type guardedStorage struct {
	*filesystem.Storage
	// ctx is the fetch's: the check of a pack stops at its end. The
	// library's writer interface takes no context, and the store lives for
	// one fetch, so it is kept here.
	ctx       context.Context
	spoolDir  string
	maxObject int64
	// mu guards inflated: what the packs still to come may inflate to, in
	// all (inflatedBudget less what the packs so far declared).
	mu       sync.Mutex
	inflated int64
	// refused is set when a pack was turned away. Fetch reads it rather
	// than rely on the library handing the writer's error back unchanged.
	refused atomic.Bool
}

// PackfileWriter returns a writer that spools the pack, and on Close checks
// it and, only then, writes it into the real store.
func (g *guardedStorage) PackfileWriter() (io.WriteCloser, error) {
	f, err := os.CreateTemp(g.spoolDir, "incoming-*.pack")
	if err != nil {
		return nil, err
	}
	return &spooledPack{f: f, g: g}, nil
}

type spooledPack struct {
	f *os.File
	g *guardedStorage
}

func (s *spooledPack) Write(p []byte) (int, error) { return s.f.Write(p) }

func (s *spooledPack) Close() (err error) {
	defer func() {
		_ = s.f.Close()
		_ = os.Remove(s.f.Name())
	}()
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	s.g.mu.Lock()
	err = checkPack(s.g.ctx, s.f, s.g.maxObject, &s.g.inflated)
	s.g.mu.Unlock()
	if err != nil {
		if errors.Is(err, errObjectTooLarge) {
			s.g.refused.Store(true)
		}
		return err
	}
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	w, err := s.g.Storage.PackfileWriter()
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, s.f); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

// checkPack reads a pack and fails with errObjectTooLarge when any object in
// it is, or would become, larger than maxObject, or when the objects together
// would become more than *inflated, which it lowers by what each declares. It
// stops at ctx's end, between objects.
//
// For an ordinary object the size is in its header, and the library refuses
// to inflate past what a header declares, so the header is what is checked.
// For a delta the header gives only the size of the instructions; the size
// of the object they build is the second number inside them, read here from
// the first bytes of the inflated instructions.
func checkPack(ctx context.Context, r io.Reader, maxObject int64, inflated *int64) error {
	s := packfile.NewScanner(r)
	_, count, err := s.Header()
	if err != nil {
		return fmt.Errorf("read the pack header: %w", err)
	}
	for i := uint32(0); i < count; i++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("check the pack: %w", err)
		}
		h, err := s.NextObjectHeader()
		if err != nil {
			return fmt.Errorf("read an object header: %w", err)
		}
		if h.Length > maxObject {
			return fmt.Errorf("%w (%d bytes declared, limit %d)", errObjectTooLarge, h.Length, maxObject)
		}
		head := &headCapture{}
		if _, _, err := s.NextObject(head); err != nil {
			return fmt.Errorf("read an object: %w", err)
		}
		size := h.Length
		if h.Type == plumbing.OFSDeltaObject || h.Type == plumbing.REFDeltaObject {
			result, ok := deltaResultSize(head.b)
			if !ok {
				return errors.New("read an object: a delta without a readable header")
			}
			if result > maxObject {
				return fmt.Errorf("%w (a delta that builds %d bytes, limit %d)", errObjectTooLarge, result, maxObject)
			}
			size = result
		}
		if size > *inflated {
			return fmt.Errorf("%w (the objects together build more than the fetch allows)", errObjectTooLarge)
		}
		*inflated -= size
	}
	return nil
}

// headCapture keeps the first bytes written to it and discards the rest.
type headCapture struct{ b []byte }

func (h *headCapture) Write(p []byte) (int, error) {
	if room := 32 - len(h.b); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		h.b = append(h.b, p[:room]...)
	}
	return len(p), nil
}

// deltaResultSize reads the size of the object a delta builds: the second of
// the two variable-length numbers a delta begins with (the first is the size
// of the object it is applied to).
func deltaResultSize(b []byte) (int64, bool) {
	readSize := func() (int64, bool) {
		var n int64
		for shift := uint(0); ; shift += 7 {
			if len(b) == 0 || shift > 56 {
				return 0, false
			}
			c := b[0]
			b = b[1:]
			n |= int64(c&0x7f) << shift
			if c&0x80 == 0 {
				return n, true
			}
		}
	}
	if _, ok := readSize(); !ok {
		return 0, false
	}
	return readSize()
}

// spoolDirFor returns the directory incoming packs are spooled in: beside
// the objects, inside the store, so it is removed with it.
func spoolDirFor(storeDir string) string { return filepath.Join(storeDir, "incoming") }
