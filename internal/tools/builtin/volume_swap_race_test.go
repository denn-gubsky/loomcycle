//go:build linux || darwin

package builtin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The swap-race tests run a file tool many times while a goroutine flips a
// directory component of the target path between a real directory inside the
// volume and a symlink to a directory outside it. The tool resolves the path
// and then opens it; on code that re-walks the path string for the open, a
// flip between the two lands the open outside the volume.
const swapRaceIterations = 5000

const swapSentinel = "SENTINEL-outside-the-volume"

// swapRace is the fixture: root (the volume) holds d/secret.txt, outside
// holds secret.txt with content that carries the sentinel, and a goroutine
// swaps d for a symlink to outside and back until stop() is called.
type swapRace struct {
	root, outside, outsideContent string
	stop                          func()
}

func newSwapRace(t *testing.T, inside, outsideContent string) *swapRace {
	t.Helper()
	outside := realDir(t)
	return newSwapRaceAt(t, realDir(t), outside, outside, inside, outsideContent)
}

// newSwapRaceAt is newSwapRace with the outside directory and the symlink's
// target chosen by the caller: the fence test puts outside inside the volume
// and links to it relatively, so the link stays inside the root.
func newSwapRaceAt(t *testing.T, root, outside, linkTarget, inside, outsideContent string) *swapRace {
	t.Helper()
	if err := os.Mkdir(filepath.Join(root, "d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "d", "secret.txt"), []byte(inside), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(outsideContent), 0o600); err != nil {
		t.Fatal(err)
	}

	// d and d.swap trade places in one syscall each turn, so d is always
	// either the real directory or a symlink to outside, never missing.
	d, swap := filepath.Join(root, "d"), filepath.Join(root, "d.swap")
	if err := os.Symlink(linkTarget, swap); err != nil {
		t.Fatal(err)
	}
	if err := exchangePaths(d, swap); err != nil {
		t.Skipf("atomic rename exchange unsupported here: %v", err)
	}
	if err := exchangePaths(d, swap); err != nil {
		t.Fatal(err)
	}
	var done atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !done.Load() {
			_ = exchangePaths(d, swap)
		}
	}()
	s := &swapRace{root: root, outside: outside, outsideContent: outsideContent}
	s.stop = sync.OnceFunc(func() {
		done.Store(true)
		wg.Wait()
		// Leave d the real directory, whichever way the last swap went.
		if fi, err := os.Lstat(d); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			if err := exchangePaths(d, swap); err != nil {
				t.Errorf("restoring d: %v", err)
			}
		}
	})
	t.Cleanup(s.stop)
	return s
}

// outsideUntouched fails the test if anything outside the volume changed:
// the sentinel was rewritten, or a file (a temp file, a renamed target)
// appeared beside it.
func (s *swapRace) outsideUntouched(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(s.outside, "secret.txt"))
	if err != nil || string(got) != s.outsideContent {
		t.Errorf("the file outside the volume was rewritten: %q (err %v)", got, err)
	}
	ents, _ := os.ReadDir(s.outside)
	for _, e := range ents {
		if e.Name() != "secret.txt" {
			t.Errorf("a file was created outside the volume: %s", e.Name())
		}
	}
}

func swapCall(t *testing.T, tool tools.Tool, root string, args map[string]any) tools.Result {
	t.Helper()
	return swapCallFenced(t, tool, root, "", args)
}

func swapCallFenced(t *testing.T, tool tools.Tool, root, fence string, args map[string]any) tools.Result {
	t.Helper()
	in, _ := json.Marshal(args)
	res, err := tool.Execute(ctxWith(tools.VolumeBinding{Name: "default", Root: root, Default: true, Fence: fence}), in)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestRead_SymlinkSwapRaceNeverEscapes(t *testing.T) {
	s := newSwapRace(t, "inside the volume", swapSentinel)
	escapes, reads := 0, 0
	for range swapRaceIterations {
		res := swapCall(t, &Read{}, s.root, map[string]any{"path": "d/secret.txt"})
		if strings.Contains(res.Text, swapSentinel) {
			escapes++
		}
		if !res.IsError && res.Text == "inside the volume" {
			reads++
		}
	}
	s.stop()
	if escapes > 0 {
		t.Errorf("Read returned the file outside the volume %d times in %d calls", escapes, swapRaceIterations)
	}
	if reads == 0 {
		t.Errorf("no Read succeeded in %d calls; the race never let the tool run", swapRaceIterations)
	}
	s.outsideUntouched(t)
}

func TestWrite_SymlinkSwapRaceNeverEscapes(t *testing.T) {
	s := newSwapRace(t, "inside the volume", swapSentinel)
	writes := 0
	for range swapRaceIterations {
		res := swapCall(t, &Write{}, s.root, map[string]any{"path": "d/secret.txt", "content": "written by the tool"})
		if !res.IsError {
			writes++
		}
	}
	s.stop()
	if writes == 0 {
		t.Errorf("no Write succeeded in %d calls; the race never let the tool run", swapRaceIterations)
	}
	s.outsideUntouched(t)
}

func TestEdit_SymlinkSwapRaceNeverEscapes(t *testing.T) {
	// The outside file holds both edit strings once each, so an Edit that
	// opens it rewrites it; the inside file holds one, and alternates.
	s := newSwapRace(t, "alpha", "alpha beta "+swapSentinel)
	edits := 0
	for i := range swapRaceIterations {
		old, repl := "alpha", "beta"
		if i%2 == 1 {
			old, repl = "beta", "alpha"
		}
		res := swapCall(t, &Edit{}, s.root, map[string]any{"path": "d/secret.txt", "old_string": old, "new_string": repl})
		if !res.IsError {
			edits++
		}
	}
	s.stop()
	if edits == 0 {
		t.Errorf("no Edit succeeded in %d calls; the race never let the tool run", swapRaceIterations)
	}
	got, err := os.ReadFile(filepath.Join(s.root, "d", "secret.txt"))
	if err != nil || strings.Contains(string(got), swapSentinel) {
		t.Errorf("the file inside the volume took the outside file's content: %q (err %v)", got, err)
	}
	s.outsideUntouched(t)
}

// The fence (the directory inside a static volume that holds other runs'
// volumes) holds under the same race. The symlink swapped in is relative and
// stays inside the volume root, so containment in the root alone would follow
// it into the fence.
func TestRead_SymlinkSwapIntoFenceNeverReadsIt(t *testing.T) {
	root := realDir(t)
	fence := filepath.Join(root, "dynamic")
	other := filepath.Join(fence, "_ephemeral", "run-2")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	s := newSwapRaceAt(t, root, other, filepath.Join("dynamic", "_ephemeral", "run-2"), "inside the volume", swapSentinel)
	escapes, reads := 0, 0
	for range swapRaceIterations {
		res := swapCallFenced(t, &Read{}, s.root, fence, map[string]any{"path": "d/secret.txt"})
		if strings.Contains(res.Text, swapSentinel) {
			escapes++
		}
		if !res.IsError && res.Text == "inside the volume" {
			reads++
		}
	}
	s.stop()
	if escapes > 0 {
		t.Errorf("Read returned a file inside the fence %d times in %d calls", escapes, swapRaceIterations)
	}
	if reads == 0 {
		t.Errorf("no Read succeeded in %d calls; the race never let the tool run", swapRaceIterations)
	}
}

// The Grep and Glob walks hold under the same race: a directory listed as a
// real directory and swapped for a symlink before it is descended must not be
// listed or searched from outside the volume.
func TestGrepGlob_SymlinkSwapRaceNeverWalksOutside(t *testing.T) {
	s := newSwapRace(t, "needle inside the volume", swapSentinel)
	if err := os.WriteFile(filepath.Join(s.outside, "outside-only.txt"), []byte(swapSentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	listed, leakedNames, found, leakedContent := 0, 0, 0, 0
	for range swapRaceIterations {
		res := swapCall(t, &Glob{}, s.root, map[string]any{"pattern": "**/*.txt"})
		if strings.Contains(res.Text, "outside-only.txt") {
			leakedNames++
		}
		if strings.Contains(res.Text, "d/secret.txt") {
			listed++
		}
		res = swapCall(t, &Grep{}, s.root, map[string]any{"pattern": "needle|SENTINEL", "output_mode": "content"})
		if strings.Contains(res.Text, swapSentinel) {
			leakedContent++
		}
		if strings.Contains(res.Text, "needle inside") {
			found++
		}
	}
	s.stop()
	if leakedNames > 0 {
		t.Errorf("Glob listed a file outside the volume %d times in %d calls", leakedNames, swapRaceIterations)
	}
	if leakedContent > 0 {
		t.Errorf("Grep returned content from outside the volume %d times in %d calls", leakedContent, swapRaceIterations)
	}
	if listed == 0 || found == 0 {
		t.Errorf("Glob listed the inside file %d times and Grep found it %d times in %d calls; the race never let the tools run", listed, found, swapRaceIterations)
	}
}
