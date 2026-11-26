package gitrepo

import (
	"errors"
	"fmt"
	"sync"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// A workspace's repository (RFC GW §4.2, P11): a working tree whose own .git
// holds its refs, index and the objects it made, over a bare mirror on the
// same worker that holds what the forge sent. The two stores are layered
// here rather than through git's alternates file, which a command in the
// workspace's session can write: go-git would follow it to whatever it
// names, and spike A measured it 15 to 200 times slower.

// noAlternates is the filesystem both stores resolve an alternates file
// against: an empty one, so no path in one can name a directory that
// exists. The mirror layer comes from the caller, never from a file.
func noAlternates() billy.Filesystem { return memfs.New() }

// openStore opens the git directory dot as a store that reads no alternates
// file. Objects over a megabyte are read as streams, as newStorage does.
func openStore(dot billy.Filesystem) *filesystem.Storage {
	return filesystem.NewStorageWithOptions(dot, cache.NewObjectLRUDefault(), filesystem.Options{
		LargeObjectThreshold: 1 << 20,
		AlternatesFS:         noAlternates(),
	})
}

// layeredStorer is a workspace's store: references, index, shallow and every
// write are the workspace's; an object is read from the mirror, then from
// the workspace. An object id names its content, so where it is found does
// not change what is read, and the mirror, which only the forge fills, is
// asked first: its packs hold almost every object a workspace reads.
type layeredStorer struct {
	*filesystem.Storage
	mirror *filesystem.Storage
	cfg    *memConfig
}

var _ storage.Storer = (*layeredStorer)(nil)

func (s *layeredStorer) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	if o, err := s.mirror.EncodedObject(t, h); err == nil {
		return o, nil
	} else if !errors.Is(err, plumbing.ErrObjectNotFound) {
		return nil, err
	}
	return s.Storage.EncodedObject(t, h)
}

// DeltaObject is what the pack encoder reads to reuse a stored delta; it
// must find the mirror's objects as EncodedObject does.
func (s *layeredStorer) DeltaObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	if o, err := s.mirror.DeltaObject(t, h); err == nil {
		return o, nil
	} else if !errors.Is(err, plumbing.ErrObjectNotFound) {
		return nil, err
	}
	return s.Storage.DeltaObject(t, h)
}

func (s *layeredStorer) HasEncodedObject(h plumbing.Hash) error {
	if err := s.mirror.HasEncodedObject(h); err == nil {
		return nil
	}
	return s.Storage.HasEncodedObject(h)
}

func (s *layeredStorer) EncodedObjectSize(h plumbing.Hash) (int64, error) {
	if n, err := s.mirror.EncodedObjectSize(h); err == nil {
		return n, nil
	}
	return s.Storage.EncodedObjectSize(h)
}

// IterEncodedObjects walks the workspace's objects, then the mirror's. An
// object in both is visited twice; the library's walks that use this
// (pruning, repacking) are not run on a workspace.
func (s *layeredStorer) IterEncodedObjects(t plumbing.ObjectType) (storer.EncodedObjectIter, error) {
	own, err := s.Storage.IterEncodedObjects(t)
	if err != nil {
		return nil, err
	}
	shared, err := s.mirror.IterEncodedObjects(t)
	if err != nil {
		own.Close()
		return nil, err
	}
	return storer.NewMultiEncodedObjectIter([]storer.EncodedObjectIter{own, shared}), nil
}

// Config is the configuration the caller built from the workspace's row; the
// workspace's .git/config is never read for it (RFC GW §4.2: a session can
// write that file, and an insteadOf line in it redirected a push in spike A).
func (s *layeredStorer) Config() (*config.Config, error) { return s.cfg.get(), nil }

// SetConfig keeps a change the library makes (a branch's upstream) in memory
// for this handle's life; nothing reaches the file.
func (s *layeredStorer) SetConfig(c *config.Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	s.cfg.set(c)
	return nil
}

// Module refuses: a workspace's typed git ops do not follow submodules
// (P11 plan G13), so nothing opens .git/modules.
func (s *layeredStorer) Module(name string) (storage.Storer, error) {
	return nil, fmt.Errorf("%w: submodules are not opened in a workspace", ErrUnsupportedFeature)
}

// memConfig is a configuration held in memory, safe for concurrent use.
type memConfig struct {
	mu sync.Mutex
	c  *config.Config
}

// get returns a copy, so a caller that changes it changes nothing held.
func (m *memConfig) get() *config.Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneConfig(m.c)
}

func (m *memConfig) set(c *config.Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.c = cloneConfig(c)
}

// cloneConfig copies c through its own encoding, which is the one form every
// field of it round-trips through.
func cloneConfig(c *config.Config) *config.Config {
	b, err := c.Marshal()
	if err != nil {
		return c
	}
	out := config.NewConfig()
	if err := out.Unmarshal(b); err != nil {
		return c
	}
	return out
}
