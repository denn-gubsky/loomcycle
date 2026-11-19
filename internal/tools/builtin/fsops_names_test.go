package builtin

import (
	"context"
	"io"
	"regexp"

	"github.com/denn-gubsky/loomcycle/internal/fsops"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The file tools' logic moved to internal/fsops (RFC GW P1). These are the
// package-local names this package's existing tests call it by, so those tests
// keep running unedited against the moved code. Production code calls fsops
// directly; nothing here is compiled into the binary.

func resolveInsideRoot(root, target string) (string, error) {
	return fsops.ResolveInsideRoot(root, target)
}

func resolveParentInsideRoot(root, target string) (string, error) {
	return fsops.ResolveParentInsideRoot(root, target)
}

func openVolumeFS(root, fence string) (*fsops.VolumeFS, error) {
	return fsops.OpenVolumeFS(root, fence)
}

var errInFence = fsops.ErrInFence

const grepDefaultMaxOutputBytes = fsops.DefaultGrepMaxOutputBytes

type lineWindow struct {
	offset   int
	limit    int
	numbers  bool
	maxBytes int64
	maxScan  int64
	maxSeek  int64
}

// readLines renders fsops.ReadLines the way the Read tool does.
func readLines(ctx context.Context, r io.Reader, w lineWindow) tools.Result {
	return readResult(fsops.ReadLines(ctx, r, fsops.LineWindow{
		Offset: w.offset, Limit: w.limit, Numbers: w.numbers,
		MaxBytes: w.maxBytes, MaxScan: w.maxScan, MaxSeek: w.maxSeek,
	}))
}

// grepWalk searches searchRoot under root as the Grep tool does with re's
// pattern and no flags.
func grepWalk(root, fence, searchRoot string, re *regexp.Regexp, glob, mode string, headLimit, maxBytes, before, after int) (string, int, error) {
	return fsops.Grep(root, fence, fsops.GrepRequest{
		Pattern: re.String(), Path: searchRoot, Glob: glob, OutputMode: mode,
		HeadLimit: headLimit, MaxOutputBytes: maxBytes, Before: before, After: after,
	})
}
